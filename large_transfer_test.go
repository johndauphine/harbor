package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitLargeTransfer(t *testing.T, a *App, id, status string) Transfer {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		tr := *a.transfers[id]
		a.mu.Unlock()
		if tr.Done < 0 || (tr.Total > 0 && tr.Done > tr.Total) {
			t.Fatalf("invalid progress %d/%d", tr.Done, tr.Total)
		}
		if tr.Status == status {
			return tr
		}
		if tr.Status == "failed" && status != "failed" {
			t.Fatalf("transfer failed: %s", tr.Error)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("transfer did not reach %s", status)
	return Transfer{}
}

func TestLargeMultipartUploadFailureAndRetries(t *testing.T) {
	payload := bytes.Repeat([]byte("multipart-fixture-"), 750000) // Over 12 MiB: three SDK parts.
	var mode atomic.Int32
	partStarted := make(chan struct{}, 1)
	var aborts, completes, partRetries atomic.Int32
	var mu sync.Mutex
	parts := map[int][]byte{}
	var uploaded []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case r.Method == "POST" && q.Has("uploads"):
			mu.Lock()
			parts = map[int][]byte{}
			mu.Unlock()
			fmt.Fprint(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>large</Key><UploadId>fixture-upload</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == "DELETE" && q.Get("uploadId") != "":
			aborts.Add(1)
			w.WriteHeader(204)
		case r.Method == "PUT" && q.Get("partNumber") != "":
			n, _ := strconv.Atoi(q.Get("partNumber"))

			body, err := io.ReadAll(r.Body)
			if err != nil {
				return
			}
			if mode.Load() == 2 {
				select {
				case partStarted <- struct{}{}:
				default:
				}
				<-r.Context().Done()
				return
			}
			if n == 2 && mode.Load() == 0 {
				// Drop the socket after receiving a part: the client must retain
				// staging, abort this upload, and restart cleanly on user retry.
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			if n == 2 && partRetries.Add(1) == 1 {
				w.WriteHeader(503)
				fmt.Fprint(w, `<Error><Code>SlowDown</Code><Message>fixture interruption</Message></Error>`)
				return
			}
			// SDK checksum streaming may use aws-chunked encoding; decoding is handled below.
			body = decodeFixtureUpload(t, r, body)
			mu.Lock()
			parts[n] = body
			mu.Unlock()
			w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, n))
		case r.Method == "POST" && q.Get("uploadId") != "":
			mu.Lock()
			for n := 1; n <= len(parts); n++ {
				uploaded = append(uploaded, parts[n]...)
			}
			mu.Unlock()
			completes.Add(1)
			fmt.Fprint(w, `<CompleteMultipartUploadResult><Location>https://localhost/bucket/large</Location><Bucket>bucket</Bucket><Key>large</Key><ETag>fixture-etag</ETag></CompleteMultipartUploadResult>`)
		default:
			t.Errorf("unexpected fixture request %s %s", r.Method, r.URL)
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	a := securityFixtureApp(t, server.URL, server)
	t.Setenv("AWS_MAX_ATTEMPTS", "2")
	defer a.shutdown(context.Background())
	source := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(source, payload, 0600); err != nil {
		t.Fatal(err)
	}
	id, err := a.QueueLocalUpload(context.Background(), a.profiles[0].ID, "bucket", "large", source)
	if err != nil {
		t.Fatal(err)
	}
	failed := waitLargeTransfer(t, a, id, "failed")
	if aborts.Load() != 1 || completes.Load() != 0 {
		t.Fatalf("failed upload not aborted: abort=%d complete=%d", aborts.Load(), completes.Load())
	}
	staged, err := os.ReadFile(failed.Path)
	if err != nil || !bytes.Equal(staged, payload) {
		t.Fatal("failed upload lost retry staging")
	}
	mode.Store(2)
	if err = a.RetryTransfer(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-partStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("multipart cancellation fixture did not start")
	}
	a.CancelTransfer(id)
	waitLargeTransfer(t, a, id, "cancelled")
	if aborts.Load() != 2 {
		t.Fatal("cancelled multipart upload was not aborted")
	}
	staged, err = os.ReadFile(failed.Path)
	if err != nil || !bytes.Equal(staged, payload) {
		t.Fatal("cancelled upload lost retry staging")
	}
	mode.Store(1)
	if err = a.RetryTransfer(id); err != nil {
		t.Fatal(err)
	}
	done := waitLargeTransfer(t, a, id, "completed")
	mu.Lock()
	equal := bytes.Equal(uploaded, payload)
	partCount := len(parts)
	mu.Unlock()
	if !equal || partCount != 3 || partRetries.Load() < 2 || completes.Load() != 1 {
		t.Fatalf("multipart retry incorrect: equal=%v parts=%d retries=%d complete=%d", equal, partCount, partRetries.Load(), completes.Load())
	}
	if done.Done != int64(len(payload)) || done.Total != int64(len(payload)) {
		t.Fatal("final upload progress incorrect")
	}
	if _, err = os.Stat(failed.Path); !os.IsNotExist(err) {
		t.Fatal("successful upload left staging behind")
	}
	original, _ := os.ReadFile(source)
	if !bytes.Equal(original, payload) {
		t.Fatal("upload changed source")
	}
}

func decodeFixtureUpload(t *testing.T, r *http.Request, body []byte) []byte {
	t.Helper()
	if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
		decoded, err := io.ReadAll(httputil.NewChunkedReader(bytes.NewReader(body)))
		if err != nil {
			t.Errorf("invalid SDK chunk framing: %v", err)
		}
		return decoded
	}

	return body
}

func TestLargeDownloadInterruptionCancellationAndRetry(t *testing.T) {
	payload := bytes.Repeat([]byte("download-fixture-"), 750000)
	var mode atomic.Int32
	partial := make(chan struct{}, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		if mode.Load() == 2 {
			_, _ = w.Write(payload)
			return
		}
		_, _ = w.Write(payload[:64*1024])
		w.(http.Flusher).Flush()
		if mode.Load() == 1 {
			partial <- struct{}{}
			<-r.Context().Done()
			return
		}
		// Closing before advertised Content-Length exercises a real interrupted stream.
	}))
	defer server.Close()
	a := securityFixtureApp(t, server.URL, server)
	defer a.shutdown(context.Background())
	dir := t.TempDir()
	destination := filepath.Join(dir, "existing.bin")
	original := []byte("existing destination must survive")
	if err := os.WriteFile(destination, original, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := a.QueueDownloadTo(a.profiles[0].ID, "bucket", "large", dir, "existing.bin", true)
	if err != nil {
		t.Fatal(err)
	}
	assertPreserved := func() {
		t.Helper()
		got, _ := os.ReadFile(destination)
		if !bytes.Equal(got, original) {
			t.Fatal("interrupted download replaced destination")
		}
		tmp, _ := filepath.Glob(filepath.Join(dir, ".harbor-download-*"))
		if len(tmp) != 0 {
			t.Fatalf("temporary downloads leaked: %v", tmp)
		}
	}
	waitLargeTransfer(t, a, result.ID, "failed")
	assertPreserved()
	mode.Store(1)
	if err = a.RetryTransfer(result.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-partial:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel fixture did not stream")
	}
	a.CancelTransfer(result.ID)
	waitLargeTransfer(t, a, result.ID, "cancelled")
	assertPreserved()
	mode.Store(2)
	if err = a.RetryTransfer(result.ID); err != nil {
		t.Fatal(err)
	}
	done := waitLargeTransfer(t, a, result.ID, "completed")
	got, _ := os.ReadFile(destination)
	if !bytes.Equal(got, payload) {
		t.Fatal("retried large download differs")
	}
	if done.SavedPath != destination || done.Done != int64(len(payload)) || done.Total != int64(len(payload)) {
		t.Fatal("download completion metadata incorrect")
	}
}

func TestCancelledMultipartCleanupFailureReported(t *testing.T) {
	started := make(chan struct{}, 1)
	var aborts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			fmt.Fprint(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>large</Key><UploadId>fixture-upload</UploadId></InitiateMultipartUploadResult>`)
		case "PUT":
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				return
			}
			select {
			case started <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		case "DELETE":
			aborts.Add(1)
			w.WriteHeader(403)
			fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>fixture denial</Message></Error>`)
		default:
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	a := securityFixtureApp(t, server.URL, server)
	defer a.shutdown(context.Background())
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, bytes.Repeat([]byte("x"), 6<<20), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := a.QueueLocalUpload(context.Background(), a.profiles[0].ID, "bucket", "large", source)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("upload not started")
	}
	a.CancelTransfer(id)
	failed := waitLargeTransfer(t, a, id, "failed")
	if aborts.Load() != 1 || !strings.Contains(failed.Error, "Incomplete upload parts may remain") {
		t.Fatalf("cleanup failure hidden: aborts=%d error=%q", aborts.Load(), failed.Error)
	}
	if _, err = os.Stat(failed.Path); err != nil {
		t.Fatal("cleanup failure removed staged upload")
	}
}
