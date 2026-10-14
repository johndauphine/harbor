package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func credentialFixture(t *testing.T) (*App, map[string]string) {
	t.Helper()
	a := NewApp()
	a.configFile = filepath.Join(t.TempDir(), "profiles.db")
	vault := map[string]string{}
	a.secretGet = func(k string) (string, error) {
		v, ok := vault[k]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return v, nil
	}
	a.secretSet = func(k, v string) error { vault[k] = v; return nil }
	a.secretDelete = func(k string) error { delete(vault, k); return nil }
	return a, vault
}
func fixtureProfile(t *testing.T, a *App) Profile {
	t.Helper()
	p, err := a.SaveProfile(Profile{Name: "fixture", Region: "us-east-1"}, Secret{AccessKey: "synthetic-access", SecretKey: "synthetic-secret"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func restartCredentialFixture(t *testing.T, a *App) *App {
	t.Helper()
	b := NewApp()
	b.configFile = a.configFile
	b.secretGet = a.secretGet
	b.secretSet = a.secretSet
	b.secretDelete = a.secretDelete
	b.startup(context.Background())
	return b
}
func TestCredentialSaveFailureKeepsCommittedPair(t *testing.T) {
	for _, kind := range []string{"open", "write", "vault", "commit"} {
		t.Run(kind, func(t *testing.T) {
			a, vault := credentialFixture(t)
			p := fixtureProfile(t, a)
			old := vault[credentialID(p)]
			path := a.configFile
			switch kind {
			case "open":
				a.configFile = filepath.Join(path, "invalid.db")
			case "write":
				db, err := a.openStore()
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`CREATE TRIGGER fail_profile BEFORE UPDATE ON profiles BEGIN SELECT RAISE(ABORT,'synthetic write failure'); END`)
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "vault":
				a.secretSet = func(k, v string) error { vault[k] = v; return errors.New("synthetic vault write failure") }
			case "commit":
				a.profileCommit = func(*sql.Tx) error { return errors.New("synthetic commit failure") }
			}
			p.Name = "changed"
			if _, err := a.SaveProfile(p, Secret{AccessKey: "replacement"}); err == nil {
				t.Fatal("expected failure")
			}
			if a.Profiles()[0].Name != "fixture" || vault[credentialID(p)] != old {
				t.Fatal("failed save changed committed metadata or credentials")
			}
			if len(vault) != 1 {
				t.Fatal("staged secret was not cleaned")
			}
			a.configFile = path
			b := restartCredentialFixture(t, a)
			if len(b.Profiles()) != 1 || b.Profiles()[0].Name != "fixture" || credentialID(b.Profiles()[0]) != credentialID(p) {
				t.Fatal("restart did not preserve old pair")
			}
		})
	}
}
func TestCredentialNewSaveCommitFailureLeavesNoConnection(t *testing.T) {
	a, vault := credentialFixture(t)
	a.profileCommit = func(*sql.Tx) error { return errors.New("synthetic commit failure") }
	if _, err := a.SaveProfile(Profile{Name: "new", Region: "us-east-1"}, Secret{AccessKey: "fake", SecretKey: "fake"}); err == nil {
		t.Fatal("expected failure")
	}
	if len(vault) != 0 || len(a.Profiles()) != 0 {
		t.Fatal("failed new profile left credentials or connection")
	}
	if len(restartCredentialFixture(t, a).Profiles()) != 0 {
		t.Fatal("failed new connection persisted")
	}
}
func TestCredentialCleanupFailureIsCommittedAndRetryable(t *testing.T) {
	a, vault := credentialFixture(t)
	p := fixtureProfile(t, a)
	oldRef := credentialID(p)
	a.secretDelete = func(string) error { return errors.New("synthetic vault locked") }
	p.Name = "changed"
	updated, err := a.SaveProfile(p, Secret{SecretKey: "replacement"})
	var warning *credentialCleanupError
	if !errors.As(err, &warning) || warning.Operation != "saved" {
		t.Fatalf("expected committed warning: %v", err)
	}
	if updated.Name != "changed" || credentialID(updated) == oldRef || len(vault) != 2 {
		t.Fatal("replacement not committed independently")
	}
	b := restartCredentialFixture(t, a)
	if credentialID(b.Profiles()[0]) != credentialID(updated) {
		t.Fatal("restart lost committed reference")
	}
	b.secretDelete = func(k string) error { delete(vault, k); return nil }
	if err = b.RetryCredentialCleanup(); err != nil {
		t.Fatal(err)
	}
	if _, exists := vault[oldRef]; exists || vault[credentialID(updated)] == "" {
		t.Fatal("cleanup removed wrong credential")
	}
}
func TestCredentialFailedStageCleanupRecoversOnRestart(t *testing.T) {
	a, vault := credentialFixture(t)
	p := fixtureProfile(t, a)
	oldRef := credentialID(p)
	a.profileCommit = func(*sql.Tx) error { return errors.New("synthetic commit failure") }
	a.secretDelete = func(string) error { return errors.New("synthetic vault locked") }
	if _, err := a.SaveProfile(p, Secret{SecretKey: "replacement"}); err == nil || !strings.Contains(err.Error(), "cleanup is pending") {
		t.Fatalf("cleanup failure not explicit: %v", err)
	}
	if len(vault) != 2 {
		t.Fatal("expected old and staged entries")
	}
	a.secretDelete = func(k string) error { delete(vault, k); return nil }
	b := restartCredentialFixture(t, a)
	if len(vault) != 1 || credentialID(b.Profiles()[0]) != oldRef || vault[oldRef] == "" {
		t.Fatal("restart cleanup failed")
	}
}
func TestCredentialLegacyMigrationAndPrivateReference(t *testing.T) {
	a, vault := credentialFixture(t)
	p := Profile{ID: "legacy", Name: "legacy", Region: "us-east-1"}
	raw, _ := json.Marshal(p)
	db, err := a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO profiles(id,metadata) VALUES(?,?)", p.ID, string(raw))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	vault[p.ID] = `{"AccessKey":"legacy-access","SecretKey":"legacy-secret"}`
	a = restartCredentialFixture(t, a)
	if credentialID(a.Profiles()[0]) != p.ID {
		t.Fatal("legacy reference changed")
	}
	p.credentialID = "caller-controlled"
	updated, err := a.SaveProfile(p, Secret{AccessKey: "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	var got Secret
	json.Unmarshal([]byte(vault[credentialID(updated)]), &got)
	if got.AccessKey != "replacement" || got.SecretKey != "legacy-secret" {
		t.Fatal("partial replacement failed")
	}
	if _, ok := vault[p.ID]; ok {
		t.Fatal("legacy key not cleaned")
	}
	raw, _ = json.Marshal(updated)
	if strings.Contains(string(raw), credentialID(updated)) || strings.Contains(string(raw), "legacy-secret") {
		t.Fatal("private reference or secret exposed")
	}
	if credentialID(restartCredentialFixture(t, a).Profiles()[0]) != credentialID(updated) {
		t.Fatal("migration lost after restart")
	}
}
func TestCredentialDeleteCommitFailureRetainsPair(t *testing.T) {
	a, vault := credentialFixture(t)
	p := fixtureProfile(t, a)
	a.profileCommit = func(*sql.Tx) error { return errors.New("synthetic commit failure") }
	if err := a.DeleteProfile(p.ID); err == nil {
		t.Fatal("expected failure")
	}
	if len(a.Profiles()) != 1 || vault[credentialID(p)] == "" {
		t.Fatal("failed deletion changed pair")
	}
	if len(restartCredentialFixture(t, a).Profiles()) != 1 {
		t.Fatal("failed deletion persisted")
	}
}

func TestCredentialFullReplacementRecoversMissingVaultEntry(t *testing.T) {
	a, vault := credentialFixture(t)
	p := fixtureProfile(t, a)
	delete(vault, credentialID(p))
	updated, err := a.SaveProfile(p, Secret{AccessKey: "replacement-access", SecretKey: "replacement-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if vault[credentialID(updated)] == "" {
		t.Fatal("full replacement did not recover missing credential")
	}
}
func TestCredentialDeletedCleanupRecoversOnRestart(t *testing.T) {
	a, vault := credentialFixture(t)
	p := fixtureProfile(t, a)
	a.secretDelete = func(string) error { return errors.New("synthetic vault locked") }
	var warning *credentialCleanupError
	if err := a.DeleteProfile(p.ID); !errors.As(err, &warning) || warning.Operation != "deleted" {
		t.Fatalf("expected committed deletion warning: %v", err)
	}
	a.secretDelete = func(k string) error { delete(vault, k); return nil }
	b := restartCredentialFixture(t, a)
	if len(b.Profiles()) != 0 || len(vault) != 0 {
		t.Fatal("restart did not finish deleted credential cleanup")
	}
}

func TestCredentialClientSnapshotSurvivesConcurrentReplacement(t *testing.T) {
	a, vault := credentialFixture(t)
	p := fixtureProfile(t, a)
	entered, release := make(chan struct{}), make(chan struct{})
	get := a.secretGet
	a.secretGet = func(k string) (string, error) { close(entered); <-release; return get(k) }
	result := make(chan error, 1)
	go func() {
		c, err := a.client(context.Background(), p.ID)
		if err == nil {
			creds, e := c.Options().Credentials.Retrieve(context.Background())
			err = e
			if err == nil && (creds.AccessKeyID != "synthetic-access" || creds.SecretAccessKey != "synthetic-secret") {
				err = errors.New("client mixed credentials from different saves")
			}
		}
		result <- err
	}()
	<-entered
	saveResult := make(chan error, 1)
	go func() {
		_, err := a.SaveProfile(p, Secret{AccessKey: "replacement-access", SecretKey: "replacement-secret"})
		saveResult <- err
	}()
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := <-saveResult; err != nil {
		t.Fatal(err)
	}
	if len(vault) != 1 {
		t.Fatal("replacement did not retire old credentials")
	}
}
