package main

import (
	"context"
	"database/sql"
	"encoding/json"
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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestJournalRetryCannotRaceProfileDeletion(t *testing.T) {
	vault := &journalVault{values: map[string]string{}}
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), vault)
	p, err := a.SaveProfile(Profile{Name: "Fixture", Region: "us-east-1"}, Secret{AccessKey: "fixture-access", SecretKey: "fixture-secret"})
	if err != nil {
		t.Fatal(err)
	}
	tr := journalEntry(t, a, p, "upload", "key")
	tr.Status = "failed"
	_ = os.WriteFile(tr.Path, []byte("payload"), 0600)
	checkpoint(t, a, tr)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	a.profileCommit = func(tx *sql.Tx) error { close(entered); <-release; return tx.Commit() }
	deleted := make(chan error, 1)
	go func() { deleted <- a.DeleteProfile(p.ID) }()
	<-entered
	retried := make(chan error, 1)
	go func() { retried <- a.RetryTransfer(tr.ID) }()
	// Hold deletion after the active-work check. A retry must not acquire a.mu
	// and recreate its row while the deletion transaction is being committed.
	time.Sleep(100 * time.Millisecond)
	unblock()
	if err = <-deleted; err != nil {
		t.Fatal(err)
	}
	if err = <-retried; err == nil {
		t.Fatal("admitted retry for a deleted connection")
	}
	db, err := a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRow("SELECT COUNT(*) FROM transfers WHERE id=?", tr.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("orphan transfer journal row", count, err)
	}
	if len(a.Transfers()) != 0 {
		t.Fatal("deleted transfer still in memory")
	}
}

func TestJournalMissingStageStillRetriesPendingMultipartCleanup(t *testing.T) {
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	var offline atomic.Bool
	offline.Store(true)
	var cleaned, uploads atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			if r.URL.Query().Get("uploadId") != "owned-upload" {
				t.Error("wrong upload")
			}
			if offline.Load() {
				w.WriteHeader(503)
				_, _ = io.WriteString(w, "<Error><Code>ServiceUnavailable</Code></Error>")
				return
			}
			cleaned.Add(1)
			w.WriteHeader(204)
			return
		}
		uploads.Add(1)
		w.WriteHeader(500)
	}))
	defer server.Close()
	vault := &journalVault{values: map[string]string{}}
	path := filepath.Join(t.TempDir(), "profiles.db")
	a := journalFixture(t, path, vault)
	p := journalProfile(t, a, server)
	tr := journalEntry(t, a, p, "upload", "key")
	tr.multipartID = "owned-upload"
	tr.multipartScope = a.multipartScopeFor(p.ID)
	checkpoint(t, a, tr)
	recovered := journalFixture(t, path, vault)
	offline.Store(false)
	if err := recovered.RetryTransfer(tr.ID); err != nil {
		t.Fatal("missing staging blocked remote cleanup", err)
	}
	got := awaitJournalTransfer(t, recovered, tr.ID)
	if got.Status != "failed" || !strings.Contains(got.Error, "Staged upload unavailable") {
		t.Fatalf("missing source not reported: %+v", got)
	}
	if cleaned.Load() != 1 || uploads.Load() != 0 || recovered.transfers[tr.ID].multipartID != "" {
		t.Fatal("pending cleanup not retired before reporting missing source")
	}
	if err := recovered.DeleteProfile(p.ID); err != nil {
		t.Fatal("connection remains undeletable after cleanup", err)
	}
}

func TestJournalDeletionRetainsFailedLocalCleanup(t *testing.T) {
	vault := &journalVault{values: map[string]string{}}
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), vault)
	p, err := a.SaveProfile(Profile{Name: "Fixture", Region: "us-east-1"}, Secret{AccessKey: "fixture-access", SecretKey: "fixture-secret"})
	if err != nil {
		t.Fatal(err)
	}
	upload := journalEntry(t, a, p, "upload", "upload")
	upload.Status = "failed"
	_ = os.WriteFile(upload.Path, []byte("keep retry data"), 0600)
	checkpoint(t, a, upload)
	download := journalEntry(t, a, p, "download", "download")
	download.Status = "failed"
	download.destination = &downloadTarget{Path: filepath.Join(t.TempDir(), "saved.bin")}
	download.partialPath = a.partialDownloadPath(*download)
	// A nonempty directory supplies a portable unlink failure, equivalent to a
	// locked destination partial on Windows, without depending on runner ACLs.
	if err = os.Mkdir(download.partialPath, 0700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(download.partialPath, "locked")
	_ = os.WriteFile(blocker, []byte("fixture"), 0600)
	checkpoint(t, a, download)
	if err = a.DeleteProfile(p.ID); err == nil {
		t.Fatal("discarded cleanup ownership on unlink failure")
	}
	if len(a.Profiles()) != 1 || len(a.Transfers()) != 2 {
		t.Fatal("failed deletion discarded local state")
	}
	if _, err = os.Stat(upload.Path); err != nil {
		t.Fatal("failed deletion discarded staged upload", err)
	}
	db, err := a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	err = db.QueryRow("SELECT metadata FROM transfers WHERE id=?", download.ID).Scan(&raw)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	var record transferRecord
	if err = json.Unmarshal([]byte(raw), &record); err != nil || record.PartialPath != download.partialPath {
		t.Fatal("lost durable partial path", err)
	}
	if err = os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err = a.DeleteProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(download.partialPath); !os.IsNotExist(err) {
		t.Fatal("partial not cleaned", err)
	}
	if _, err = os.Stat(upload.Path); !os.IsNotExist(err) {
		t.Fatal("discarded source not cleaned", err)
	}
}

func TestJournalCreateCheckpointAndAbortFailureRetainsMultipart(t *testing.T) {
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	var a *App
	var aborts atomic.Int32
	var parts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>key</Key><UploadId>unrecorded-upload</UploadId></InitiateMultipartUploadResult>`)
		case "DELETE":
			if r.URL.Query().Get("uploadId") != "unrecorded-upload" {
				t.Error("wrong upload ID")
			}
			if aborts.Add(1) == 1 {
				db, err := a.openStore()
				if err != nil {
					t.Error(err)
				} else {
					_, err = db.Exec("DROP TRIGGER fail_multipart_checkpoint")
					db.Close()
					if err != nil {
						t.Error(err)
					}
				}
				w.WriteHeader(503)
				_, _ = io.WriteString(w, "<Error><Code>ServiceUnavailable</Code></Error>")
				return
			}
			w.WriteHeader(204)
		default:
			parts.Add(1)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	vault := &journalVault{values: map[string]string{}}
	path := filepath.Join(t.TempDir(), "profiles.db")
	a = journalFixture(t, path, vault)
	p := journalProfile(t, a, server)
	tr := journalEntry(t, a, p, "upload", "key")
	_ = os.WriteFile(tr.Path, []byte("payload"), 0600)
	checkpoint(t, a, tr)
	db, err := a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TRIGGER fail_multipart_checkpoint BEFORE UPDATE ON transfers WHEN instr(NEW.metadata,'"MultipartID":"unrecorded-upload"')>0 BEGIN SELECT RAISE(FAIL,'fixture checkpoint failure'); END`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	client, err := a.client(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := &cleanupUploadClient{Client: client, app: a, transfer: *tr, scope: multipartClientScope(client)}
	out, err := wrapper.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{Bucket: aws.String("bucket"), Key: aws.String("key")})
	if err == nil || out != nil || parts.Load() != 0 {
		t.Fatal("released an unjournaled multipart upload to its caller")
	}
	if tr.multipartID != "unrecorded-upload" || tr.CleanupError == "" {
		t.Fatal("abort failure lost ownership in memory")
	}
	db, err = a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	err = db.QueryRow("SELECT metadata FROM transfers WHERE id=?", tr.ID).Scan(&raw)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	var record transferRecord
	if err = json.Unmarshal([]byte(raw), &record); err != nil || record.MultipartID != "unrecorded-upload" {
		t.Fatal("checkpoint retry lost ownership", err)
	}
	recovered := journalFixture(t, path, vault)
	if aborts.Load() != 2 || recovered.transfers[tr.ID].multipartID != "" {
		t.Fatal("startup did not retire retained upload")
	}
}

func TestJournalRetainsOwnershipDuringRepeatedWriteFailures(t *testing.T) {
	vault := &journalVault{values: map[string]string{}}
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), vault)
	tr := journalEntry(t, a, Profile{ID: "fixture"}, "upload", "key")
	checkpoint(t, a, tr)
	db, err := a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("DROP TABLE transfers")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.retainMultipart(*tr, "owned-upload", "scope"); err == nil {
		t.Fatal("expected storage failure")
	}
	if tr.multipartID != "owned-upload" || tr.multipartScope != "scope" || tr.CleanupError == "" {
		t.Fatal("persistent storage failure discarded ownership")
	}
	db, err = a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE transfers(id TEXT PRIMARY KEY,profile_id TEXT NOT NULL,metadata TEXT NOT NULL)")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint(t, a, tr)
	db, err = a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var raw string
	if err = db.QueryRow("SELECT metadata FROM transfers WHERE id=?", tr.ID).Scan(&raw); err != nil || !strings.Contains(raw, "owned-upload") {
		t.Fatal(fmt.Sprint("later checkpoint lost retained ownership: ", err))
	}
}
