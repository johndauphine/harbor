package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFailedCompletionCommitRetainsRecoveryState(t *testing.T) {
	for _, direction := range []string{"download", "upload"} {
		t.Run(direction, func(t *testing.T) {
			a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), &journalVault{values: map[string]string{}})
			p, err := a.SaveProfile(Profile{Name: "Completion", Region: "us-east-1"}, Secret{AccessKey: "fixture", SecretKey: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			tr := journalEntry(t, a, p, direction, "key")
			tr.checkpoint = &transferCheckpoint{Version: 1, Kind: direction, Scope: multipartScope(p.Endpoint, p.Region, p.PathStyle), Size: parallelDownloadThreshold, ChunkSize: downloadChunkSize, ETag: `"fixture"`, FileAttempt: 1, Blocks: map[int]string{}}
			if direction == "upload" {
				tr.checkpoint.ChunkSize = resumableUploadPartSize
				tr.checkpoint.SourceHash = strings.Repeat("0", 64)
				tr.multipartID, tr.multipartScope = "owned-upload", tr.checkpoint.Scope
			} else {
				tr.partialPath = a.partialDownloadPath(*tr)
			}
			tr.Resumable = true
			checkpoint(t, a, tr)
			db, err := a.openStore()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.Exec(`CREATE TRIGGER fail_completion BEFORE UPDATE ON transfers WHEN json_extract(NEW.metadata,'$.Status')='completed' BEGIN SELECT RAISE(FAIL,'injected completion failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err = a.finishCheckpointedTransfer(*tr, parallelDownloadThreshold); !errors.Is(err, errCheckpointPersistence) {
				t.Fatal("expected failed durable completion", err)
			}
			if tr.Status != "running" || tr.checkpoint == nil || !tr.Resumable || direction == "upload" && tr.multipartID != "owned-upload" || direction == "download" && tr.partialPath == "" {
				t.Fatalf("failed completion discarded recovery state: %+v", tr)
			}
			var raw string
			if err = db.QueryRow("SELECT metadata FROM transfers WHERE id=?", tr.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(raw, `"Checkpoint":{`) || direction == "upload" && !strings.Contains(raw, "owned-upload") {
				t.Fatal("durable recovery state was lost", raw)
			}
		})
	}
}

func TestCheckpointedCompletionRemainsDurableAndCannotRetry(t *testing.T) {
	for _, direction := range []string{"download", "upload"} {
		t.Run(direction, func(t *testing.T) {
			t.Setenv("AWS_MAX_ATTEMPTS", "1")
			const size int64 = 64 << 20
			var gets, initiates, completes atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if direction == "download" {
					gets.Add(1)
					w.Header().Set("ETag", "\"fixture\"")
					var start, end int64
					if r.Header.Get("Range") != "" {
						if _, e := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); e != nil {
							t.Error(e)
							w.WriteHeader(400)
							return
						}
						w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
						w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
						w.WriteHeader(206)
						_, _ = io.CopyN(w, zeroStream{}, end-start+1)
					} else {
						w.Header().Set("Content-Length", fmt.Sprint(size))
						_, _ = io.CopyN(w, zeroStream{}, size)
					}
					return
				}
				switch {
				case r.Method == "POST" && r.URL.Query().Has("uploads"):
					uploadID := fmt.Sprintf("upload-%d", initiates.Add(1))
					_, _ = fmt.Fprintf(w, "<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>key</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", uploadID)
				case r.Method == "PUT":
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("ETag", fmt.Sprintf("\"part-%s\"", r.URL.Query().Get("partNumber")))
				case r.Method == "POST":
					_, _ = io.Copy(io.Discard, r.Body)
					completes.Add(1)
					_, _ = io.WriteString(w, "<CompleteMultipartUploadResult><Location>fixture</Location><Bucket>bucket</Bucket><Key>key</Key><ETag>\"complete\"</ETag></CompleteMultipartUploadResult>")
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "profiles.db")
			vault := &journalVault{values: map[string]string{}}
			a := journalFixture(t, path, vault)
			p := journalProfile(t, a, server)
			tr := journalEntry(t, a, p, direction, "key")
			tr.Status = "queued"
			destination := filepath.Join(t.TempDir(), "saved.bin")
			if direction == "download" {
				tr.destination = &downloadTarget{Path: destination, Overwrite: false}
			} else {
				f, e := os.Create(tr.Path)
				if e != nil {
					t.Fatal(e)
				}
				if e = f.Truncate(size); e != nil {
					t.Fatal(e)
				}
				if e = f.Close(); e != nil {
					t.Fatal(e)
				}
			}
			checkpoint(t, a, tr)
			db, e := a.openStore()
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			_, e = db.Exec(`CREATE TRIGGER reject_redundant_completion BEFORE UPDATE ON transfers WHEN json_extract(OLD.metadata,'$.Status')='completed' AND json_extract(NEW.metadata,'$.Status')='completed' BEGIN SELECT RAISE(FAIL,'injected second completion failure'); END`)
			if e != nil {
				t.Fatal(e)
			}
			defer db.Exec("DROP TRIGGER IF EXISTS reject_redundant_completion")
			a.run(tr.ID, tr.attempt)
			if tr.Status != "completed" || tr.Error != "" || tr.checkpoint != nil || tr.multipartID != "" {
				t.Fatalf("completed transfer was downgraded: status=%s error=%s checkpoint=%v multipart=%s", tr.Status, tr.Error, tr.checkpoint, tr.multipartID)
			}
			var persisted string
			if e = db.QueryRow("SELECT json_extract(metadata,'$.Status') FROM transfers WHERE id=?", tr.ID).Scan(&persisted); e != nil {
				t.Fatal(e)
			}
			if persisted != "completed" {
				t.Fatalf("first completion was not durable: %s", persisted)
			}
			if _, e = db.Exec("DROP TRIGGER reject_redundant_completion"); e != nil {
				t.Fatal(e)
			}
			beforeGets := gets.Load()
			if direction == "download" {
				fi, e := os.Stat(destination)
				if e != nil || fi.Size() != size {
					t.Fatalf("download was not published: %v %v", fi, e)
				}
			} else if initiates.Load() != 1 || completes.Load() != 1 {
				t.Fatalf("first upload did not complete: starts=%d completes=%d", initiates.Load(), completes.Load())
			}
			if e = a.RetryTransfer(tr.ID); e == nil {
				t.Fatal("completed transfer accepted Retry")
			}
			if e = a.shutdown(context.Background()); e != nil {
				t.Fatal(e)
			}
			recovered := journalFixture(t, path, vault)
			if got := recovered.transfers[tr.ID]; got == nil || got.Status != "completed" || got.checkpoint != nil || got.multipartID != "" {
				t.Fatalf("restart lost durable completion: %+v", got)
			}
			if e = recovered.RetryTransfer(tr.ID); e == nil {
				t.Fatal("recovered completion accepted Retry")
			}
			if gets.Load() != beforeGets || direction == "upload" && (initiates.Load() != 1 || completes.Load() != 1) {
				t.Fatal("retry or restart repeated completed payload")
			}
		})
	}
}
