package main

import (
	"bufio"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreLockExcludesAliasesAndAllowsSQLite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.db")
	lock, err := acquireStoreLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err = acquireStoreLock(path); !errors.Is(err, errStoreInUse) {
		t.Fatalf("second lock: %v", err)
	}

	alias := filepath.Join(dir, "alias.db")
	if err = os.Symlink(path, alias); err != nil {
		t.Logf("symlink unavailable: %v", err)
	} else {
		if got, err := acquireStoreLock(alias); !errors.Is(err, errStoreInUse) {
			if got != nil {
				got.Close()
			}
			t.Fatalf("symlink did not contend: %v", err)
		}
	}
	hardlink := filepath.Join(dir, "hardlink.db")
	if err = os.Link(path, hardlink); err != nil {
		t.Fatal(err)
	}
	if got, err := acquireStoreLock(hardlink); err == nil || !strings.Contains(err.Error(), "Hard-linked") {
		if got != nil {
			got.Close()
		}
		t.Fatalf("hardlink bypassed lock: %v", err)
	}
	if err = os.Remove(hardlink); err != nil {
		t.Fatal(err)
	}
	a, vault := credentialFixture(t)
	a.configFile = lock.path
	p := fixtureProfile(t, a)
	if vault[credentialID(p)] == "" {
		t.Fatal("SQLite save under lock failed")
	}
	if err = a.DeleteProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	other, err := acquireStoreLock(filepath.Join(dir, "independent.db"))
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := acquireStoreLock(path)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}

func storeLockChild(t *testing.T, mode, path string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreLockSubprocess$")
	cmd.Env = append(os.Environ(), "HARBOR_LOCK_TEST_MODE="+mode, "HARBOR_LOCK_TEST_PATH="+path)
	return cmd
}

func TestStoreLockRejectsSecondExecutableBeforeStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.db")
	lock, err := acquireStoreLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	cmd := storeLockChild(t, "main", path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("second executable unexpectedly started")
	}
	if !strings.Contains(string(out), errStoreInUse.Error()) || strings.Contains(string(out), "Open http://") {
		t.Fatalf("expected clean startup rejection, got %q", out)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Fatalf("second executable initialized database before lock: %v", err)
	}
}

func TestStoreLockReleasedOnProcessExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.db")
	cmd := storeLockChild(t, "hold", path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "locked\n" {
			t.Fatalf("child lock failed: %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child failed to lock")
	}
	if l, err := acquireStoreLock(path); !errors.Is(err, errStoreInUse) {
		if l != nil {
			l.Close()
		}
		t.Fatalf("child lock not exclusive: %v", err)
	}
	// Child calls os.Exit without Close: release must come from the OS.
	stdin.Close()
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireStoreLock(path)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestStoreLockSubprocess(t *testing.T) {
	mode := os.Getenv("HARBOR_LOCK_TEST_MODE")
	if mode == "" {
		return
	}
	path := os.Getenv("HARBOR_LOCK_TEST_PATH")
	if mode == "main" {
		flag.CommandLine = flag.NewFlagSet("harbor", flag.ExitOnError)
		os.Args = []string{"harbor", "--db", path}
		main()
		os.Exit(0)
	}
	lock, err := acquireStoreLock(path)
	if err != nil {
		os.Exit(2)
	}
	_ = lock // Deliberately leave the descriptor open until process exit.
	os.Stdout.WriteString("locked\n")
	io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
