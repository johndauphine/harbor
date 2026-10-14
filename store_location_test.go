package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBoundStoreCopyRejectedBeforeVaultAccess(t *testing.T) {
	a, vault := credentialFixture(t)
	if err := a.startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := fixtureProfile(t, a)
	before := vault[credentialID(p)]
	raw, err := os.ReadFile(a.configFile)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "copied.db")
	if err = os.WriteFile(copyPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	b := NewApp()
	b.configFile = copyPath
	calls := 0
	b.secretGet = func(string) (string, error) { calls++; return "", nil }
	b.secretSet = func(string, string) error { calls++; return nil }
	b.secretDelete = func(string) error { calls++; return nil }
	if err = b.startup(context.Background()); !errors.Is(err, errStoreLocationChanged) {
		t.Fatalf("copy accepted: %v", err)
	}
	if calls != 0 || len(b.Profiles()) != 0 || vault[credentialID(p)] != before {
		t.Fatal("copied store touched vault or exposed profiles")
	}
	if got := restartCredentialFixture(t, a); len(got.Profiles()) != 1 {
		t.Fatal("original no longer opens")
	}
}

func TestBoundStoreMoveRejectedWithoutRebinding(t *testing.T) {
	a, _ := credentialFixture(t)
	if err := a.startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := a.configFile
	moved := filepath.Join(filepath.Dir(original), "moved.db")
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	a.configFile = moved
	if err := a.startup(context.Background()); !errors.Is(err, errStoreLocationChanged) {
		t.Fatalf("moved store accepted: %v", err)
	}
	if err := os.Rename(moved, original); err != nil {
		t.Fatal(err)
	}
	a.configFile = original
	if err := a.startup(context.Background()); err != nil {
		t.Fatal("failed startup rebound the moved store:", err)
	}
}

func TestBoundStoreSymlinkReopensOriginal(t *testing.T) {
	a, _ := credentialFixture(t)
	if err := a.startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixtureProfile(t, a)
	alias := filepath.Join(filepath.Dir(a.configFile), "alias.db")
	if err := os.Symlink(a.configFile, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	a.configFile = alias
	if b := restartCredentialFixture(t, a); len(b.Profiles()) != 1 {
		t.Fatal("symlink alias did not reopen")
	}
}

func TestBoundStoreLegacyFirstOpenBindsOnce(t *testing.T) {
	a, _, _ := legacyUpgradeFixture(t)
	if err := a.startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(a.configFile)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "legacy-copy-after-upgrade.db")
	if err = os.WriteFile(copyPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.configFile = copyPath
	if err = a.startup(context.Background()); !errors.Is(err, errStoreLocationChanged) {
		t.Fatalf("upgraded legacy copy bypassed location guard: %v", err)
	}
}
