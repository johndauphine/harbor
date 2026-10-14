package main

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVersionWithoutOpeningConfiguration(t *testing.T) {
	if os.Getenv("HARBOR_VERSION_TEST") == "1" {
		flag.CommandLine = flag.NewFlagSet("harbor", flag.ExitOnError)
		os.Args = []string{"harbor", "--version", "--db", os.Getenv("HARBOR_VERSION_DB")}
		main()
		return
	}
	path := filepath.Join(t.TempDir(), "new-directory", "profiles.db")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestVersionWithoutOpeningConfiguration$")
	cmd.Env = append(os.Environ(), "HARBOR_VERSION_TEST=1", "HARBOR_VERSION_DB="+path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("version failed: %v %s", err, output)
	}
	if !strings.Contains(string(output), "Harbor ") || strings.Contains(string(output), "Open http://") {
		t.Fatalf("unexpected output: %s", output)
	}
	if _, err = os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("version initialized configuration", err)
	}
}
