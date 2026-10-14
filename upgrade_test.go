package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Seed exactly the legacy schema, without invoking current store initialization.
func legacyUpgradeFixture(t *testing.T) (*App, map[string]string, []Profile) {
	t.Helper()
	a, vault := credentialFixture(t)
	db, err := sql.Open("sqlite", a.configFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE profiles(id TEXT PRIMARY KEY,metadata TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	profiles := []Profile{{ID: "legacy-one", Name: "Legacy AWS", Region: "us-east-1"}, {ID: "legacy-two", Name: "Legacy compatible", Region: "test-region", Endpoint: "https://storage.example.invalid", PathStyle: true, CAFile: "synthetic-ca.pem"}}
	for i, p := range profiles {
		raw, _ := json.Marshal(p)
		if _, err = db.Exec("INSERT INTO profiles VALUES(?,?)", p.ID, string(raw)); err != nil {
			t.Fatal(err)
		}
		secret := Secret{AccessKey: "synthetic-" + p.ID, SecretKey: "synthetic-secret", SessionToken: "synthetic-token"}
		b, _ := json.Marshal(secret)
		vault[p.ID] = string(b)
		profiles[i] = p
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	return a, vault, profiles
}

func TestUpgradeLegacyStoreRetainsConnectionsAndUntouchedCredentials(t *testing.T) {
	a, vault, legacy := legacyUpgradeFixture(t)
	writes, deletes := 0, 0
	set, del := a.secretSet, a.secretDelete
	a.secretSet = func(k, v string) error { writes++; return set(k, v) }
	a.secretDelete = func(k string) error { deletes++; return del(k) }
	if err := a.startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.Profiles(); len(got) != 2 || got[1].Endpoint != legacy[1].Endpoint || got[1].CAFile != legacy[1].CAFile || !got[1].PathStyle {
		t.Fatal("legacy settings changed")
	}
	if writes != 0 || deletes != 0 {
		t.Fatal("opening legacy store accessed credential mutation functions")
	}
	p := a.Profiles()[0]
	original := vault[p.ID]
	p.Name = "Renamed"
	p, err := a.SaveProfile(p, Secret{})
	if err != nil {
		t.Fatal(err)
	}
	if credentialID(p) != p.ID || vault[p.ID] != original || writes != 0 || deletes != 0 {
		t.Fatal("metadata-only migration changed credentials")
	}
	b := restartCredentialFixture(t, a)
	if b.Profiles()[0].Name != "Renamed" || credentialID(b.Profiles()[0]) != p.ID {
		t.Fatal("metadata-only migration failed to reopen")
	}
	updated, err := b.SaveProfile(b.Profiles()[0], Secret{AccessKey: "new-access", SecretKey: "new-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if credentialID(updated) == updated.ID || vault[legacy[0].ID] != "" || vault[legacy[1].ID] == "" {
		t.Fatal("rotation failed or changed untouched legacy connection")
	}
	c := restartCredentialFixture(t, b)
	if credentialID(c.Profiles()[0]) != credentialID(updated) || credentialID(c.Profiles()[1]) != legacy[1].ID {
		t.Fatal("mixed legacy/current store failed to reopen")
	}
}

func TestUpgradeFailedLegacyRotationRetainsOriginalPair(t *testing.T) {
	a, vault, legacy := legacyUpgradeFixture(t)
	original := vault[legacy[0].ID]
	if err := a.startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.profileCommit = func(*sql.Tx) error { return errors.New("synthetic failed migration commit") }
	p := a.Profiles()[0]
	p.Name = "Uncommitted"
	if _, err := a.SaveProfile(p, Secret{SecretKey: "replacement"}); err == nil {
		t.Fatal("expected failed migration")
	}
	b := restartCredentialFixture(t, a)
	if b.Profiles()[0].Name != legacy[0].Name || credentialID(b.Profiles()[0]) != legacy[0].ID || vault[legacy[0].ID] != original || len(vault) != 2 {
		t.Fatal("failed migration altered old pair")
	}
}

func TestUpgradeInvalidConfigurationFailsBeforeCleanup(t *testing.T) {
	for _, kind := range []string{"corrupt-file", "invalid-json", "mismatched-id", "missing-region"} {
		t.Run(kind, func(t *testing.T) {
			a, _, _ := legacyUpgradeFixture(t)
			if kind == "corrupt-file" {
				if err := os.WriteFile(a.configFile, []byte("synthetic invalid database"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				db, err := a.openStore()
				if err != nil {
					t.Fatal(err)
				}
				raw := `{`
				if kind == "mismatched-id" {
					raw = `{"ID":"wrong-id","Name":"Test","Region":"test"}`
				}
				if kind == "missing-region" {
					raw = `{"ID":"legacy-two","Name":"Test"}`
				}
				if _, err = db.Exec("UPDATE profiles SET metadata=? WHERE id='legacy-two'", raw); err != nil {
					t.Fatal(err)
				}
				db.Close()
			}
			deleted := 0
			a.secretDelete = func(string) error { deleted++; return nil }
			if err := a.startup(context.Background()); err == nil {
				t.Fatal("invalid database started successfully")
			}
			if len(a.Profiles()) != 0 || deleted != 0 {
				t.Fatal("failed startup exposed partial profiles or touched credentials")
			}
		})
	}
}

func TestUpgradeExecutableRejectsCorruptStoreWithoutServing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.db")
	if err := os.WriteFile(path, []byte("synthetic invalid database"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := storeLockChild(t, "main", path).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "Open configuration database") || strings.Contains(string(out), "Open http://") {
		t.Fatalf("invalid config was not rejected cleanly: %v %s", err, out)
	}
}

func TestUpgradeMetadataBackupDoesNotRestoreRotatedCredentials(t *testing.T) {
	a, vault, _ := legacyUpgradeFixture(t)
	if err := a.startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(a.configFile)
	if err != nil {
		t.Fatal(err)
	}
	p := a.Profiles()[0]
	if _, err = a.SaveProfile(p, Secret{AccessKey: "rotated-access", SecretKey: "rotated-secret"}); err != nil {
		t.Fatal(err)
	}
	// Restore only configuration bytes at the bound original path after stopping
	// the isolated fixture. Copying to a different path is deliberately rejected.
	restored := NewApp()
	if err = a.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored.configFile = a.configFile
	restored.secretGet = a.secretGet
	restored.secretSet = a.secretSet
	restored.secretDelete = a.secretDelete
	if err = os.WriteFile(restored.configFile, backup, 0600); err != nil {
		t.Fatal(err)
	}
	if err = restored.startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = restored.client(context.Background(), p.ID); err == nil {
		t.Fatal("metadata-only backup unexpectedly restored removed secret")
	}
	recovered, err := restored.SaveProfile(restored.Profiles()[0], Secret{AccessKey: "reentered-access", SecretKey: "reentered-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if vault[credentialID(recovered)] == "" {
		t.Fatal("explicit credential re-entry did not recover restored profile")
	}
}
