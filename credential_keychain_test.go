package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zalando/go-keyring"
)

// Opt-in integration with exact, randomly named test-only Keychain entries.
// It never queries the HarborS3 service or enumerates a user's vault.
func TestMacOSKeychainProfileLifecycle(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("HARBOR_KEYCHAIN_TEST") != "1" {
		t.Skip("opt in on macOS with HARBOR_KEYCHAIN_TEST=1")
	}
	service := "Harbor-Isolated-Integration-" + id()
	accounts := map[string]bool{}
	t.Cleanup(func() {
		for account := range accounts {
			err := keyring.Delete(service, account)
			if err != nil && !errors.Is(err, keyring.ErrNotFound) {
				t.Error("test-only Keychain cleanup failed")
				continue
			}
			if _, err = keyring.Get(service, account); !errors.Is(err, keyring.ErrNotFound) {
				t.Error("test-only Keychain entry still present or cleanup verification failed")
			}
		}
	})
	newApp := func(path string) *App {
		a := NewApp()
		a.configFile = path
		a.secretGet = func(account string) (string, error) { return keyring.Get(service, account) }
		a.secretSet = func(account, value string) error {
			accounts[account] = true
			return keyring.Set(service, account, value)
		}
		a.secretDelete = func(account string) error { return keyring.Delete(service, account) }
		return a
	}
	path := filepath.Join(t.TempDir(), "profiles.db")
	a := newApp(path)
	if err := a.startup(context.Background()); err != nil {
		t.Fatal("isolated startup failed:", err)
	}
	initial := Secret{AccessKey: "synthetic-access", SecretKey: "synthetic-secret", SessionToken: "synthetic-token"}
	p, err := a.SaveProfile(Profile{Name: "Isolated Keychain test", Region: "us-east-1"}, initial)
	if err != nil {
		t.Fatal("test-only Keychain save failed:", err)
	}
	check := func(a *App, p Profile, want Secret) {
		t.Helper()
		raw, err := a.secretGet(credentialID(p))
		if err != nil {
			t.Fatal("test-only Keychain read failed:", err)
		}
		var got Secret
		if json.Unmarshal([]byte(raw), &got) != nil || got != want {
			t.Fatal("test-only credential value did not round-trip")
		}
	}
	check(a, p, initial)
	ref := credentialID(p)
	p.Name = "Renamed test"
	p, err = a.SaveProfile(p, Secret{})
	if err != nil {
		t.Fatal("metadata-only save failed:", err)
	}
	if credentialID(p) != ref {
		t.Fatal("metadata-only save replaced credentials")
	}
	check(a, p, initial)
	p, err = a.SaveProfile(p, Secret{SecretKey: "synthetic-partial-replacement"})
	if err != nil {
		t.Fatal("partial key replacement failed:", err)
	}
	check(a, p, Secret{AccessKey: initial.AccessKey, SecretKey: "synthetic-partial-replacement"})
	if _, err = a.secretGet(ref); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatal("superseded test-only credential was not removed")
	}
	replacement := Secret{AccessKey: "synthetic-new-access", SecretKey: "synthetic-new-secret", SessionToken: "synthetic-new-token"}
	p, err = a.SaveProfile(p, replacement)
	if err != nil {
		t.Fatal("full key replacement failed:", err)
	}
	b := newApp(path)
	if err = b.startup(context.Background()); err != nil {
		t.Fatal("reopening isolated store failed:", err)
	}
	if len(b.Profiles()) != 1 || b.Profiles()[0].Name != "Renamed test" {
		t.Fatal("reopened profile metadata mismatch")
	}
	check(b, b.Profiles()[0], replacement)
	if err = b.DeleteProfile(p.ID); err != nil {
		t.Fatal("test-only profile deletion failed:", err)
	}
	if _, err = b.secretGet(credentialID(p)); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatal("deleted test-only credential still present")
	}
	c := newApp(path)
	if err = c.startup(context.Background()); err != nil {
		t.Fatal("reopen after deletion failed:", err)
	}
	if len(c.Profiles()) != 0 {
		t.Fatal("deleted profile reappeared")
	}
}
