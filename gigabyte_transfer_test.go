package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This intentionally transfers several GiB through real loopback HTTPS and uses
// temporary disk space. Keep it opt-in so ordinary unit tests stay lightweight.
func TestStreamingMultiGiBTransfers(t *testing.T) {
	if os.Getenv("HARBOR_GIB_TEST") != "1" {
		t.Skip("set HARBOR_GIB_TEST=1 for multi-GiB loopback HTTPS verification")
	}
	const size int64 = (2 << 30) + (5 << 20) + 123
	const partSize int64 = 5 << 20 // SDK default; source is an io.Reader.
	root := t.TempDir()
	source := filepath.Join(root, "source.bin")
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	// Distinct per-part markers catch reordering/offset mistakes while retaining
	// a sparse source and constant-memory generation.
	var marker [8]byte
	for offset := int64(0); offset < size-8; offset += partSize {
		binary.LittleEndian.PutUint64(marker[:], uint64(offset+1))
		if _, err = f.WriteAt(marker[:], offset); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.WriteAt([]byte{0xa5}, size-1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	expected := hashGiBFile(t, source)
	assembled, err := os.Create(filepath.Join(root, "assembled.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer assembled.Close()
	var uploadMode, downloadMode atomic.Int32
	var aborts, completions atomic.Int32
	uploadStarted, downloadStarted := make(chan struct{}, 1), make(chan struct{}, 1)
	var partMu sync.Mutex
	parts := map[int]int64{}
	fixtureDone := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case r.Method == "POST" && q.Has("uploads"):
			partMu.Lock()
			parts = map[int]int64{}
			partMu.Unlock()
			fmt.Fprint(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>large</Key><UploadId>gib-fixture</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == "DELETE" && q.Get("uploadId") != "":
			aborts.Add(1)
			w.WriteHeader(204)
		case r.Method == "PUT" && q.Get("partNumber") != "":
			n, err := strconv.Atoi(q.Get("partNumber"))
			if err != nil || n < 1 {
				w.WriteHeader(400)
				return
			}
			var body io.Reader = r.Body
			if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
				body = httputil.NewChunkedReader(body)
			}
			count, err := io.CopyBuffer(io.NewOffsetWriter(assembled, int64(n-1)*partSize), body, make([]byte, 64<<10))
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			if uploadMode.Load() == 0 {
				select {
				case uploadStarted <- struct{}{}:
				default:
				}
				select {
				case <-r.Context().Done():
				case <-fixtureDone:
				}
				return
			}
			partMu.Lock()
			parts[n] = count
			partMu.Unlock()
			w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, n))
		case r.Method == "POST" && q.Get("uploadId") != "":
			completions.Add(1)
			fmt.Fprint(w, `<CompleteMultipartUploadResult><Location>https://localhost/bucket/large</Location><Bucket>bucket</Bucket><Key>large</Key><ETag>gib-fixture</ETag></CompleteMultipartUploadResult>`)
		case r.Method == "GET":
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			reader, err := os.Open(source)
			if err != nil {
				t.Error(err)
				return
			}
			defer reader.Close()
			limit := size
			switch downloadMode.Load() {
			case 0:
				limit = (2 << 30) + (64 << 10)
			case 1:
				limit = 64 << 10
			}
			_, _ = io.CopyBuffer(w, io.LimitReader(reader, limit), make([]byte, 64<<10))
			if downloadMode.Load() == 1 {
				w.(http.Flusher).Flush()
				select {
				case downloadStarted <- struct{}{}:
				default:
				}
				select {
				case <-r.Context().Done():
				case <-fixtureDone:
				}
			}
		default:
			t.Errorf("unexpected fixture request %s %s", r.Method, r.URL)
			w.WriteHeader(400)
		}
	}))
	defer func() { close(fixtureDone); server.Close() }()
	a := securityFixtureApp(t, server.URL, server)
	defer a.shutdown(context.Background())
	var peak atomic.Uint64
	stopMemory := make(chan struct{})
	go func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopMemory:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				for old := peak.Load(); m.HeapAlloc > old; old = peak.Load() {
					if peak.CompareAndSwap(old, m.HeapAlloc) {
						break
					}
				}
			}
		}
	}()
	defer close(stopMemory)
	started := time.Now()
	uploadID, err := a.QueueLocalUpload(context.Background(), a.profiles[0].ID, "bucket", "large", source)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-uploadStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("multipart upload did not start")
	}
	a.CancelTransfer(uploadID)
	cancelled := waitGiBTransfer(t, a, uploadID, "cancelled")
	if aborts.Load() != 1 || completions.Load() != 0 {
		t.Fatal("cancelled multipart upload was not aborted")
	}
	staged, err := os.Stat(cancelled.Path)
	if err != nil || staged.Size() != size {
		t.Fatal("cancelled upload staging not retained")
	}
	t.Log("cancelled multipart upload aborted; full staging retained")
	uploadMode.Store(1)
	if err = a.RetryTransfer(uploadID); err != nil {
		t.Fatal(err)
	}
	uploaded := waitGiBTransfer(t, a, uploadID, "completed")
	if uploaded.Done != size || uploaded.Total != size || completions.Load() != 1 {
		t.Fatal("large upload completion or progress mismatch")
	}
	partMu.Lock()
	var received int64
	for _, n := range parts {
		received += n
	}
	partCount := len(parts)
	partMu.Unlock()
	if received != size || partCount != int((size+partSize-1)/partSize) {
		t.Fatalf("multipart coverage: bytes=%d parts=%d", received, partCount)
	}
	if err = assembled.Sync(); err != nil {
		t.Fatal(err)
	}
	if hashGiBFile(t, assembled.Name()) != expected {
		t.Fatal("assembled multipart SHA-256 differs")
	}
	if _, err = os.Stat(cancelled.Path); !os.IsNotExist(err) {
		t.Fatal("completed upload staging leaked")
	}
	t.Logf("uploaded and verified %d bytes in %d parts", size, partCount)
	destination := filepath.Join(root, "download.bin")
	const original = "preserve this existing destination"
	if err = os.WriteFile(destination, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := a.QueueDownloadTo(a.profiles[0].ID, "bucket", "large", root, "download.bin", true)
	if err != nil {
		t.Fatal(err)
	}
	assertPreserved := func() {
		t.Helper()
		b, err := os.ReadFile(destination)
		if err != nil || string(b) != original {
			t.Fatal("interrupted download replaced existing destination")
		}
		paths, _ := filepath.Glob(filepath.Join(root, ".harbor-download-*"))
		if len(paths) != 0 {
			t.Fatal("partial download temporary file leaked")
		}
	}
	failed := waitGiBTransfer(t, a, result.ID, "failed")
	if failed.Done < 2<<30 {
		t.Fatalf("interruption did not cross 2GiB: %d", failed.Done)
	}
	assertPreserved()
	t.Logf("download interruption after %d bytes preserved destination and removed temporary file", failed.Done)
	downloadMode.Store(1)
	if err = a.RetryTransfer(result.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-downloadStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("download cancellation fixture did not start")
	}
	a.CancelTransfer(result.ID)
	waitGiBTransfer(t, a, result.ID, "cancelled")
	assertPreserved()
	downloadMode.Store(2)
	if err = a.RetryTransfer(result.ID); err != nil {
		t.Fatal(err)
	}
	downloaded := waitGiBTransfer(t, a, result.ID, "completed")
	if downloaded.Done != size || downloaded.Total != size || downloaded.SavedPath != destination {
		t.Fatal("download completion metadata incorrect")
	}
	if hashGiBFile(t, destination) != expected || hashGiBFile(t, source) != expected {
		t.Fatal("large download or source SHA-256 differs")
	}
	if peak.Load() > 512<<20 {
		t.Fatalf("streaming heap unexpectedly exceeded512MiB: %d", peak.Load())
	}
	t.Logf("verified %d-byte upload/download SHA-256=%x; sampled peak Go heap %.1fMiB; elapsed %s", size, expected, float64(peak.Load())/(1<<20), time.Since(started).Round(time.Second))
}

func hashGiBFile(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.CopyBuffer(h, f, make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}
func waitGiBTransfer(t *testing.T, a *App, id, status string) Transfer {
	t.Helper()
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		tr := *a.transfers[id]
		a.mu.Unlock()
		if tr.Done < 0 || (tr.Total > 0 && tr.Done > tr.Total) {
			t.Fatalf("invalid large-file progress %d/%d", tr.Done, tr.Total)
		}
		if tr.Status == status {
			return tr
		}
		if tr.Status == "failed" && status != "failed" {
			t.Fatalf("large transfer failed: %s", tr.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("large transfer did not reach %s", status)
	return Transfer{}
}

func TestUploadSizeLimitRejectsBeforeStaging(t *testing.T) {
	a := securityFixtureApp(t, "https://127.0.0.1:1")
	source := filepath.Join(t.TempDir(), "too-large.bin")
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate((5 << 30) + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	id, err := a.QueueLocalUpload(context.Background(), a.profiles[0].ID, "bucket", "large", source)
	if id != "" || err == nil || !strings.Contains(err.Error(), "5 GiB") || len(a.Transfers()) != 0 {
		t.Fatalf("oversize upload not rejected: id=%q error=%v", id, err)
	}
}
