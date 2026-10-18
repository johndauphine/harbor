package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func identityAPI(t *testing.T, a *App, route string, input any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "http://127.0.0.1:1234/api/"+route, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer fixture")
	w := httptest.NewRecorder()
	webHandler(a, "127.0.0.1:1234", "fixture").ServeHTTP(w, req)
	return w
}

func TestConnectionIdentityRejectsQueuedAndRecoveredProfileChanges(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		for _, change := range []string{"endpoint", "credentials"} {
			for _, restart := range []bool{false, true} {
				t.Run(direction+"/"+change+"/restart="+map[bool]string{true: "yes", false: "no"}[restart], func(t *testing.T) {
					var requests atomic.Int32
					server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
					defer server.Close()
					other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
					defer other.Close()
					vault := &journalVault{values: map[string]string{}}
					path := filepath.Join(t.TempDir(), "profiles.db")
					a := journalFixture(t, path, vault)
					p := journalProfile(t, a, server)
					tr := journalEntry(t, a, p, direction, "key")
					tr.Status = "queued"
					if direction == "upload" {
						if err := os.WriteFile(tr.Path, []byte("accepted payload"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					checkpoint(t, a, tr)
					secret := Secret{}
					if change == "endpoint" {
						p.Endpoint = other.URL
					} else {
						secret = Secret{AccessKey: "replacement-access", SecretKey: "replacement-secret"}
					}
					updated, err := a.SaveProfile(p, secret)
					if err != nil {
						t.Fatal(err)
					}
					if updated.Identity == "" || updated.Identity == tr.ConnectionIdentity {
						t.Fatal("saved connection did not expose changed identity")
					}
					if restart {
						tr.Status = "running" // Interrupted worker, with no live goroutine.
						checkpoint(t, a, tr)
						if err = a.shutdown(context.Background()); err != nil {
							t.Fatal(err)
						}
						a = journalFixture(t, path, vault)
						tr = a.transfers[tr.ID]
					} else {
						a.run(tr.ID, tr.attempt)
					}
					if tr.Status != "failed" || !strings.Contains(tr.Error, errConnectionChanged.Error()) {
						t.Fatalf("accepted changed connection: %+v", tr)
					}
					if err = a.RetryTransfer(tr.ID); !errors.Is(err, errConnectionChanged) {
						t.Fatalf("retry did not refuse changed connection: %v", err)
					}
					if requests.Load() != 0 {
						t.Fatalf("changed connection received %d requests", requests.Load())
					}
				})
			}
		}
	}
}

func TestCredentialRotationRetiresMultipartWithoutResending(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "restart"}[restart], func(t *testing.T) {
			var aborts, unexpected atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "DELETE" || r.URL.Query().Get("uploadId") != "owned-original-upload" || !strings.Contains(r.Header.Get("Authorization"), "replacement-access/") {
					unexpected.Add(1)
					w.WriteHeader(500)
					return
				}
				aborts.Add(1)
			}))
			defer server.Close()
			vault := &journalVault{values: map[string]string{}}
			path := filepath.Join(t.TempDir(), "profiles.db")
			a := journalFixture(t, path, vault)
			p := journalProfile(t, a, server)
			tr := journalEntry(t, a, p, "upload", "key")
			tr.Status = "failed"
			tr.multipartID, tr.multipartScope = "owned-original-upload", multipartScope(p.Endpoint, p.Region, p.PathStyle)
			tr.checkpoint = &transferCheckpoint{Version: 1, Kind: "upload", Scope: tr.multipartScope, Size: parallelDownloadThreshold, ChunkSize: resumableUploadPartSize, FileAttempt: 1, SourceHash: strings.Repeat("0", 64), Blocks: map[int]string{0: "part"}}
			checkpoint(t, a, tr)
			if _, err := a.SaveProfile(p, Secret{AccessKey: "replacement-access", SecretKey: "replacement-secret"}); err != nil {
				t.Fatal(err)
			}
			if restart {
				if err := a.shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
				a = journalFixture(t, path, vault)
				tr = a.transfers[tr.ID]
			}
			if err := a.RetryTransfer(tr.ID); !errors.Is(err, errConnectionChanged) {
				t.Fatal("changed credentials authorized a retry", err)
			}
			if tr.multipartID != "" || tr.checkpoint != nil || tr.Resumable || aborts.Load() != 1 || unexpected.Load() != 0 {
				t.Fatalf("cleanup-only did not retire original upload: %+v aborts=%d unexpected=%d", tr, aborts.Load(), unexpected.Load())
			}
			if err := a.DeleteProfile(p.ID); err != nil {
				t.Fatal("credential rotation stranded profile deletion", err)
			}
		})
	}
}

func TestLegacyUnavailablePartialPreservesRecoveryFilename(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	vault := &journalVault{values: map[string]string{}}
	path := filepath.Join(t.TempDir(), "profiles.db")
	a := journalFixture(t, path, vault)
	p := journalProfile(t, a, server)
	tr := journalEntry(t, a, p, "download", "key")
	tr.ConnectionIdentity = "" // Actual pre-identity journal, never rebound.
	tr.Status = "failed"
	tr.attempt = 2
	tr.checkpoint = &transferCheckpoint{Version: 1, Kind: "download", Scope: multipartScope(p.Endpoint, p.Region, p.PathStyle), Size: parallelDownloadThreshold, ChunkSize: downloadChunkSize, FileAttempt: 1, ETag: `"fixture"`, Blocks: map[int]string{0: strings.Repeat("0", 64)}}
	tr.partialPath = a.partialDownloadPath(*tr)
	if err := os.Mkdir(tr.partialPath, 0700); err != nil {
		t.Fatal(err)
	}
	obstruction := filepath.Join(tr.partialPath, "obstruction")
	if err := os.WriteFile(obstruction, []byte("prevents removal"), 0600); err != nil {
		t.Fatal(err)
	}
	checkpoint(t, a, tr)
	if err := a.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		a = journalFixture(t, path, vault)
		tr = a.transfers[tr.ID]
		if tr.CleanupError == "" || tr.Resumable || tr.checkpoint == nil || tr.checkpoint.FileAttempt != 1 || !strings.HasSuffix(tr.partialPath, "-1") {
			t.Fatalf("startup lost inaccessible legacy partial ownership: %+v", tr)
		}
		if err := a.RetryTransfer(tr.ID); !errors.Is(err, errUnverifiedTransfer) {
			t.Fatal("legacy transfer was rebound", err)
		}
		if err := a.shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	a = journalFixture(t, path, vault)
	tr = a.transfers[tr.ID]
	if err := os.Remove(obstruction); err != nil {
		t.Fatal(err)
	}
	if err := a.RetryTransfer(tr.ID); !errors.Is(err, errUnverifiedTransfer) {
		t.Fatal("legacy retry did not refuse payload", err)
	}
	if tr.partialPath != "" || tr.checkpoint != nil || tr.CleanupError != "" || requests.Load() != 0 {
		t.Fatal("legacy cleanup did not retire only its partial")
	}
	if err := a.DeleteProfile(p.ID); err != nil {
		t.Fatal("cleaned legacy transfer stranded profile", err)
	}
}

func TestMultipartCancellationAfterProfileEditUsesOriginalClient(t *testing.T) {
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	var aborts, otherRequests atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var first, unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(release) }) }
	defer unblock()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "DELETE":
			if r.URL.Query().Get("uploadId") != "original-upload" {
				t.Error("cleanup changed upload ID")
			}
			aborts.Add(1)
		case "POST":
			io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>original-upload</UploadId></InitiateMultipartUploadResult>`)
		case "PUT":
			io.Copy(io.Discard, r.Body)
			first.Do(func() { close(entered) })
			<-release
			w.Header().Set("ETag", `"part"`)
		default:
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherRequests.Add(1); w.WriteHeader(500) }))
	defer other.Close()
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), &journalVault{values: map[string]string{}})
	p := journalProfile(t, a, server)
	tr := journalEntry(t, a, p, "upload", "key")
	f, err := os.Create(tr.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(parallelDownloadThreshold); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	tr.Status = "queued"
	checkpoint(t, a, tr)
	done := make(chan struct{})
	go func() { a.run(tr.ID, tr.attempt); close(done) }()
	awaitRetrySignal(t, entered)
	p.Endpoint = other.URL
	if _, err = a.SaveProfile(p, Secret{}); err != nil {
		t.Fatal(err)
	}
	a.CancelTransfer(tr.ID)
	unblock()
	awaitRetrySignal(t, done)
	if tr.Status != "cancelled" || tr.multipartID != "" || tr.checkpoint != nil || aborts.Load() != 1 || otherRequests.Load() != 0 {
		t.Fatalf("cancellation cleanup escaped original client: %+v aborts=%d other=%d", tr, aborts.Load(), otherRequests.Load())
	}
}

func TestBrowserUploadProfileEditDuringStagingRejectsAndCleans(t *testing.T) {
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), &journalVault{values: map[string]string{}})
	p, err := a.SaveProfile(Profile{Name: "Staging", Region: "us-east-1"}, Secret{AccessKey: "fixture", SecretKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	req := httptest.NewRequest("POST", "http://127.0.0.1:1234/api/upload?profile="+p.ID+"&bucket=bucket&key=key&identity="+p.Identity, reader)
	req.Header.Set("Authorization", "Bearer fixture")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { webHandler(a, "127.0.0.1:1234", "fixture").ServeHTTP(w, req); close(done) }()
	if _, err = writer.Write([]byte("partially staged")); err != nil {
		t.Fatal(err)
	}
	p.Region = "us-west-2"
	if _, err = a.SaveProfile(p, Secret{}); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	awaitRetrySignal(t, done)
	if w.Code == 200 || !strings.Contains(w.Body.String(), errConnectionChanged.Error()) {
		t.Fatalf("staging accepted edited destination: %d %s", w.Code, w.Body)
	}
	if len(a.Transfers()) != 0 {
		t.Fatal("stale staged upload enqueued")
	}
	files, err := os.ReadDir(a.transferCache)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "upload-") {
			t.Fatal("rejected upload left staged payload")
		}
	}
}

func TestHTTPMutationsRequireConnectionIdentity(t *testing.T) {
	a := NewApp()
	a.ctx = context.Background()
	for _, route := range []string{"download", "upload-local", "delete-file", "upload?profile=fixture&bucket=bucket&key=key"} {
		w := identityAPI(t, a, route, map[string]string{"ProfileID": "fixture", "Bucket": "bucket", "Key": "key"})
		if w.Code != 400 || !strings.Contains(w.Body.String(), "Connection identity required") {
			t.Fatalf("%s accepted unbound request: %d %s", route, w.Code, w.Body)
		}
	}
}

func TestDeleteFileUsesReviewedListingAndImmutableClient(t *testing.T) {
	var heads, deletes, otherRequests atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var block atomic.Bool
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "HEAD":
			heads.Add(1)
			if block.Load() {
				close(entered)
				<-release
			}
			w.Header().Set("ETag", `"same"`)
			w.Header().Set("Content-Length", "1")
		case "DELETE":
			deletes.Add(1)
		default:
			io.WriteString(w, `<ListBucketResult><Contents><Key>key</Key><ETag>"same"</ETag><Size>1</Size></Contents></ListBucketResult>`)
		}
	}))
	defer server.Close()
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherRequests.Add(1); w.Header().Set("ETag", `"same"`) }))
	defer other.Close()
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), &journalVault{values: map[string]string{}})
	p := journalProfile(t, a, server)
	page, err := a.Browse(p.ID, "bucket", "", "")
	if err != nil || page.ConnectionIdentity != p.Identity {
		t.Fatal("listing identity missing", page, err)
	}
	p.Endpoint = other.URL
	if _, err = a.SaveProfile(p, Secret{}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DeleteFile(p.ID, "bucket", "key", `"same"`, page.ConnectionIdentity); !errors.Is(err, errConnectionChanged) {
		t.Fatal("stale selection accepted", err)
	}
	if _, err = a.Browse(p.ID, "bucket", "", "next", page.ConnectionIdentity); !errors.Is(err, errConnectionChanged) {
		t.Fatal("stale pagination accepted", err)
	}
	if heads.Load() != 0 || deletes.Load() != 0 || otherRequests.Load() != 0 {
		t.Fatal("stale selection contacted a storage endpoint")
	}
	p.Endpoint = server.URL
	p, err = a.SaveProfile(p, Secret{})
	if err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	deleted := make(chan error, 1)
	go func() { _, e := a.DeleteFile(p.ID, "bucket", "key", `"same"`, p.Identity); deleted <- e }()
	awaitRetrySignal(t, entered)
	p.Endpoint = other.URL
	if _, err = a.SaveProfile(p, Secret{}); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err = <-deleted; err != nil {
		t.Fatal(err)
	}
	if heads.Load() != 1 || deletes.Load() != 1 || otherRequests.Load() != 0 {
		t.Fatal("delete switched endpoint after HEAD")
	}
}
