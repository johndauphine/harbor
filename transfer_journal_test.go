package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type journalVault struct {
	mu     sync.Mutex
	values map[string]string
}

func journalFixture(t *testing.T, path string, vault *journalVault) *App {
	t.Helper()
	a := NewApp()
	a.configFile = path
	a.secretGet = func(k string) (string, error) {
		vault.mu.Lock()
		defer vault.mu.Unlock()
		v, ok := vault.values[k]
		if !ok {
			return "", fmt.Errorf("missing fixture secret")
		}
		return v, nil
	}
	a.secretSet = func(k, v string) error { vault.mu.Lock(); defer vault.mu.Unlock(); vault.values[k] = v; return nil }
	a.secretDelete = func(k string) error { vault.mu.Lock(); defer vault.mu.Unlock(); delete(vault.values, k); return nil }
	if err := a.startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return a
}
func journalProfile(t *testing.T, a *App, server *httptest.Server) Profile {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := a.SaveProfile(Profile{Name: "Journal fixture", Region: "us-east-1", Endpoint: server.URL, CAFile: ca, PathStyle: true}, Secret{AccessKey: "fixture-access", SecretKey: "fixture-secret"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func journalEntry(t *testing.T, a *App, p Profile, direction, key string) *Transfer {
	t.Helper()
	cache, err := a.transferCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	prefix := "download-"
	if direction == "upload" {
		prefix = "upload-"
	}
	tr := &Transfer{ConnectionIdentity: connectionIdentity(p), ID: id(), attempt: 1, Direction: direction, ProfileID: p.ID, Bucket: "bucket", Key: key, Path: filepath.Join(cache, prefix+id()), Status: "running", CreatedAt: time.Now()}
	a.transfers[tr.ID] = tr
	a.transferOrder = append(a.transferOrder, tr.ID)
	return tr
}
func checkpoint(t *testing.T, a *App, tr *Transfer) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.persistTransferLocked(tr); err != nil {
		t.Fatal(err)
	}
}
func awaitJournalTransfer(t *testing.T, a *App, transferID string) Transfer {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, tr := range a.Transfers() {
			if tr.ID == transferID && (tr.Status == "completed" || tr.Status == "failed") {
				return tr
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("transfer did not terminate")
	return Transfer{}
}

func TestJournalRestartRestoresRetryAndDownloadDestination(t *testing.T) {
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	payload := []byte("persistent fixture payload")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("ETag", "fixture")
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	vault := &journalVault{values: map[string]string{}}
	dbPath := filepath.Join(t.TempDir(), "profiles.db")
	a := journalFixture(t, dbPath, vault)
	p := journalProfile(t, a, server)
	upload := journalEntry(t, a, p, "upload", "upload")
	if err := os.WriteFile(upload.Path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	checkpoint(t, a, upload)
	download := journalEntry(t, a, p, "download", "download")
	download.destination = &downloadTarget{Path: filepath.Join(t.TempDir(), "saved.bin"), Overwrite: false}
	download.partialPath = a.partialDownloadPath(*download)
	if err := os.WriteFile(download.partialPath, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	checkpoint(t, a, download)
	orphan := filepath.Join(a.transferCache, "upload-orphan")
	_ = os.WriteFile(orphan, []byte("orphan"), 0600)
	foreign := filepath.Join(t.TempDir(), "upload-other-store")
	_ = os.WriteFile(foreign, []byte("keep"), 0600)
	// Construct a fresh application without shutdown: the journal is the only
	// source of transfer state, just as after a killed process.
	recovered := journalFixture(t, dbPath, vault)
	transfers := recovered.Transfers()
	if len(transfers) != 2 {
		t.Fatalf("transfers=%+v", transfers)
	}
	for _, tr := range transfers {
		if tr.Status != "failed" || !strings.Contains(tr.Error, "Interrupted") {
			t.Fatalf("not recoverable: %+v", tr)
		}
	}
	if _, err := os.Stat(upload.Path); err != nil {
		t.Fatal("retry staging lost", err)
	}
	for _, path := range []string{download.partialPath, orphan} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("orphan survived", path, err)
		}
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("another cache was touched", err)
	}
	if err := recovered.RetryTransfer(upload.ID); err != nil {
		t.Fatal(err)
	}
	if tr := awaitJournalTransfer(t, recovered, upload.ID); tr.Status != "completed" {
		t.Fatal(tr.Error)
	}
	if _, err := os.Stat(upload.Path); !os.IsNotExist(err) {
		t.Fatal("successful upload staging retained")
	}
	if err := recovered.RetryTransfer(download.ID); err != nil {
		t.Fatal(err)
	}
	if tr := awaitJournalTransfer(t, recovered, download.ID); tr.Status != "completed" || tr.SavedPath != download.destination.Path {
		t.Fatalf("retry failed: %+v", tr)
	}
	got, err := os.ReadFile(download.destination.Path)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("destination not restored", err)
	}
	db, err := a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var raw string
	if err = db.QueryRow("SELECT metadata FROM transfers WHERE id=?", upload.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "fixture-access") || strings.Contains(raw, "fixture-secret") {
		t.Fatal("credentials in journal")
	}
}

func TestJournalMultipartCleanupRetriesExactOwnedID(t *testing.T) {
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	var offline atomic.Bool
	offline.Store(true)
	var aborts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			if r.URL.Query().Get("uploadId") != "owned-upload" || r.URL.Path != "/bucket/key" {
				t.Error("cleanup touched a different upload")
				w.WriteHeader(400)
				return
			}
			if offline.Load() {
				w.WriteHeader(503)
				_, _ = io.WriteString(w, "<Error><Code>ServiceUnavailable</Code></Error>")
				return
			}
			aborts.Add(1)
			w.WriteHeader(204)
			return
		}
		if r.Method != "PUT" {
			t.Error("unexpected request (cleanup must not list/abort by prefix)", r.Method, r.URL.Path)
		}
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer server.Close()
	vault := &journalVault{values: map[string]string{}}
	dbPath := filepath.Join(t.TempDir(), "profiles.db")
	a := journalFixture(t, dbPath, vault)
	p := journalProfile(t, a, server)
	tr := journalEntry(t, a, p, "upload", "key")
	_ = os.WriteFile(tr.Path, []byte("payload"), 0600)
	tr.multipartID = "owned-upload"
	tr.multipartScope = a.multipartScopeFor(p.ID)
	checkpoint(t, a, tr)
	recovered := journalFixture(t, dbPath, vault)
	if got := recovered.Transfers()[0]; got.CleanupError == "" || got.Status != "failed" {
		t.Fatalf("offline cleanup not retained: %+v", got)
	}
	if recovered.transfers[tr.ID].multipartID != "owned-upload" {
		t.Fatal("lost cleanup journal")
	}
	offline.Store(false)
	if err := recovered.RetryTransfer(tr.ID); err != nil {
		t.Fatal(err)
	}
	if got := awaitJournalTransfer(t, recovered, tr.ID); got.Status != "completed" || got.CleanupError != "" {
		t.Fatalf("recovery failed: %+v", got)
	}
	if aborts.Load() != 1 || recovered.transfers[tr.ID].multipartID != "" {
		t.Fatal("cleanup not retired exactly once")
	}
}

func TestJournalRejectsUnownedCleanupPaths(t *testing.T) {
	vault := &journalVault{values: map[string]string{}}
	dbPath := filepath.Join(t.TempDir(), "profiles.db")
	a := journalFixture(t, dbPath, vault)
	victim := filepath.Join(t.TempDir(), "keep.bin")
	_ = os.WriteFile(victim, []byte("keep"), 0600)
	tr := Transfer{ID: id(), Direction: "upload", Path: victim, Status: "running"}
	raw, _ := json.Marshal(transferRecord{Transfer: tr, Attempt: 1})
	db, err := a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO transfers(id,profile_id,metadata) VALUES(?,?,?)", tr.ID, "fixture", string(raw))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	recovered := NewApp()
	recovered.configFile = dbPath
	if err = recovered.startup(context.Background()); err == nil {
		t.Fatal("accepted outside cache path")
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Fatal("deleted unrelated file")
	}
}

func TestJournalWriteFailureRejectsNewWork(t *testing.T) {
	vault := &journalVault{values: map[string]string{}}
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), vault)
	p, err := a.SaveProfile(Profile{Name: "Fixture", Region: "us-east-1"}, Secret{AccessKey: "fixture-access", SecretKey: "fixture-secret"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("DROP TABLE transfers")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.queueTransfer("upload", p.ID, "bucket", "key", filepath.Join(a.transferCache, "upload-fixture")); err == nil {
		t.Fatal("admitted non-durable transfer")
	}
	if len(a.Transfers()) != 0 {
		t.Fatal("worker admitted after journal failure")
	}
}

func TestJournalGracefulShutdownRetainsRetryableUpload(t *testing.T) {
	vault := &journalVault{values: map[string]string{}}
	dbPath := filepath.Join(t.TempDir(), "profiles.db")
	a := journalFixture(t, dbPath, vault)
	tr := journalEntry(t, a, Profile{ID: "fixture"}, "upload", "key")
	tr.Status = "queued"
	_ = os.WriteFile(tr.Path, []byte("retry"), 0600)
	checkpoint(t, a, tr)
	if err := a.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tr.Path); err != nil {
		t.Fatal("shutdown deleted retryable source", err)
	}
	recovered := journalFixture(t, dbPath, vault)
	if got := recovered.Transfers()[0]; got.Status != "cancelled" {
		t.Fatalf("shutdown state not retained: %+v", got)
	}
}

func TestJournalMultipartIsDurableBeforeSendingParts(t *testing.T) {
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	var a *App
	var parts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Query().Has("uploads"):
			_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>key</Key><UploadId>journal-upload</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == "PUT":
			db, err := a.openStore()
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			var raw string
			err = db.QueryRow("SELECT metadata FROM transfers WHERE profile_id=?", a.profiles[0].ID).Scan(&raw)
			db.Close()
			var record transferRecord
			if err != nil || json.Unmarshal([]byte(raw), &record) != nil || record.MultipartID != "journal-upload" {
				t.Error("part sent before upload ID was durable", err)
				w.WriteHeader(500)
				return
			}
			parts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("ETag", `"part-etag"`)
		case r.Method == "POST":
			_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><Bucket>bucket</Bucket><Key>key</Key><ETag>"complete-etag"</ETag></CompleteMultipartUploadResult>`)
		default:
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	vault := &journalVault{values: map[string]string{}}
	a = journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), vault)
	p := journalProfile(t, a, server)
	source := filepath.Join(t.TempDir(), "source")
	_ = os.WriteFile(source, bytes.Repeat([]byte("x"), 6<<20), 0600)
	transferID, err := a.QueueLocalUpload(context.Background(), p.ID, "bucket", "key", source)
	if err != nil {
		t.Fatal(err)
	}
	if tr := awaitJournalTransfer(t, a, transferID); tr.Status != "completed" {
		t.Fatal(tr.Error)
	}
	if parts.Load() < 2 {
		t.Fatal("multipart fixture did not exercise parts")
	}
	a.mu.Lock()
	remaining := a.transfers[transferID].multipartID
	a.mu.Unlock()
	if remaining != "" {
		t.Fatal("successful multipart ID not retired")
	}
}

func TestJournalDoesNotCleanMultipartAtChangedEndpoint(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(204) }))
	defer server.Close()
	vault := &journalVault{values: map[string]string{}}
	dbPath := filepath.Join(t.TempDir(), "profiles.db")
	a := journalFixture(t, dbPath, vault)
	p := journalProfile(t, a, server)
	tr := journalEntry(t, a, p, "upload", "key")
	_ = os.WriteFile(tr.Path, []byte("payload"), 0600)
	tr.multipartID = "owned-upload"
	tr.multipartScope = a.multipartScopeFor(p.ID)
	checkpoint(t, a, tr)
	p.Region = "us-west-2"
	if _, err := a.SaveProfile(p, Secret{}); err != nil {
		t.Fatal(err)
	}
	recovered := journalFixture(t, dbPath, vault)
	if requests.Load() != 0 || recovered.transfers[tr.ID].multipartID != "owned-upload" {
		t.Fatal("cleanup reached changed connection")
	}
	if recovered.Transfers()[0].CleanupError == "" {
		t.Fatal("missing cleanup warning")
	}
}

func TestJournalRetryDoesNotOverwriteExistingDestination(t *testing.T) {
	payload := []byte("replacement")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	vault := &journalVault{values: map[string]string{}}
	dbPath := filepath.Join(t.TempDir(), "profiles.db")
	a := journalFixture(t, dbPath, vault)
	p := journalProfile(t, a, server)
	tr := journalEntry(t, a, p, "download", "key")
	tr.destination = &downloadTarget{Path: filepath.Join(t.TempDir(), "existing.bin"), Overwrite: false}
	_ = os.WriteFile(tr.destination.Path, []byte("original"), 0600)
	checkpoint(t, a, tr)
	recovered := journalFixture(t, dbPath, vault)
	if err := recovered.RetryTransfer(tr.ID); err != nil {
		t.Fatal(err)
	}
	if got := awaitJournalTransfer(t, recovered, tr.ID); got.Status != "failed" {
		t.Fatal("overwrote existing destination")
	}
	if got, _ := os.ReadFile(tr.destination.Path); string(got) != "original" {
		t.Fatal("existing destination changed")
	}
}
