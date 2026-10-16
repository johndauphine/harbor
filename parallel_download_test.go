package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A real TLS endpoint exercises SDK range handling and the durable app journal.
// Every block differs, so swapped ranges cannot pass the final comparison.
func TestParallelDownloadRecovery(t *testing.T) {
	for _, scenario := range []string{"retry", "restart", "corrupt", "changed", "cancel", "published", "linked"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("AWS_MAX_ATTEMPTS", "1")
			payload := make([]byte, 96<<20)
			for i := range payload {
				payload[i] = byte(i / 4096 % 251)
			}
			var app *App
			var tr *Transfer
			var fail atomic.Bool
			fail.Store(true)
			var changed atomic.Bool
			var mu sync.Mutex
			requests := map[int64]int{}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tag := "\"original\""
				if changed.Load() {
					tag = "\"changed\""
				}
				w.Header().Set("ETag", tag)
				if r.Method == "HEAD" {
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
					return
				}
				if r.Header.Get("Range") == "" {
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
					_, _ = w.Write(payload)
					return
				}
				var start, end int64
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
					t.Errorf("missing range: %v", err)
					w.WriteHeader(400)
					return
				}
				if r.Header.Get("If-Match") != tag {
					w.WriteHeader(412)
					return
				}
				mu.Lock()
				requests[start]++
				mu.Unlock()
				if start > 0 && fail.Load() {
					deadline := time.Now().Add(5 * time.Second)
					for time.Now().Before(deadline) {
						app.mu.Lock()
						cp := app.transfers[tr.ID].checkpoint
						ready := cp != nil && len(cp.Blocks) > 0
						app.mu.Unlock()
						if ready {
							break
						}
						time.Sleep(time.Millisecond)
					}
					w.WriteHeader(503)
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
				w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
				w.WriteHeader(206)
				_, _ = w.Write(payload[start : end+1])
			}))
			defer server.Close()
			vault := &journalVault{values: map[string]string{}}
			db := filepath.Join(t.TempDir(), "profiles.db")
			app = journalFixture(t, db, vault)
			p := journalProfile(t, app, server)
			tr = journalEntry(t, app, p, "download", "large")
			target := filepath.Join(t.TempDir(), "saved.bin")
			tr.destination = &downloadTarget{Path: target, Overwrite: true}
			if err := os.WriteFile(target, []byte("original destination"), 0600); err != nil {
				t.Fatal(err)
			}
			checkpoint(t, app, tr)
			err := app.execute(context.Background(), *tr)
			if err == nil {
				t.Fatal("expected injected failure")
			}
			if !tr.Resumable || checkpointBytes(tr.checkpoint) != downloadChunkSize {
				t.Fatalf("checkpoint not retained: %+v %v", tr, err)
			}
			original, err := os.ReadFile(target)
			if err != nil || string(original) != "original destination" {
				t.Fatal("destination modified before completion", err)
			}
			partial := tr.partialPath
			if scenario == "cancel" {
				app.mu.Lock()
				tr.userCancelled = true
				app.mu.Unlock()
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, _ = app.parallelDownload(ctx, mustDownloadClient(t, app, p.ID), *tr, nil)
				if tr.checkpoint != nil {
					t.Fatal("explicit cancel retained checkpoint")
				}
				if _, err := os.Stat(partial); !os.IsNotExist(err) {
					t.Fatal("cancel retained partial", err)
				}
				return
			}
			if scenario == "corrupt" {
				file, err := os.OpenFile(partial, os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteAt([]byte("corrupt"), 0)
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "changed" {
				changed.Store(true)
			}
			if scenario == "published" || scenario == "linked" {
				file, err := os.OpenFile(partial, os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = file.WriteAt(payload, 0); err != nil {
					t.Fatal(err)
				}
				cp := cloneCheckpoint(tr.checkpoint)
				for i := 0; i < checkpointChunks(cp); i++ {
					start := int64(i) * cp.ChunkSize
					cp.Blocks[i], err = digestRange(file, start, min(cp.ChunkSize, cp.Size-start))
					if err != nil {
						t.Fatal(err)
					}
				}
				file.Close()
				if err = app.storeCheckpoint(*tr, cp, partial); err != nil {
					t.Fatal(err)
				}
				if scenario == "linked" {
					tr.destination.Overwrite = false
					if err = os.Remove(target); err != nil {
						t.Fatal(err)
					}
					err = os.Link(partial, target)
				} else {
					err = os.Rename(partial, target)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "restart" || scenario == "published" || scenario == "linked" {
				// No in-memory transfer objects are reused by the recovered application.
				app = journalFixture(t, db, vault)
				tr = app.transfers[tr.ID]
			} else {
				app.mu.Lock()
				tr.Status = "failed"
				app.mu.Unlock()
			}
			fail.Store(false)
			if err = app.RetryTransfer(tr.ID); err != nil {
				t.Fatal(err)
			}
			result := awaitJournalTransfer(t, app, tr.ID)
			if result.Status != "completed" {
				t.Fatalf("retry failed: %+v", result)
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatal("assembled data mismatch", err)
			}
			mu.Lock()
			firstRequests := requests[0]
			mu.Unlock()
			expected := 1
			if scenario == "corrupt" || scenario == "changed" {
				expected = 2
			}
			if firstRequests != expected {
				t.Fatalf("first block downloaded %d times, want %d", firstRequests, expected)
			}
			if scenario != "corrupt" && scenario != "changed" && scenario != "published" && scenario != "linked" && result.ResumedBytes != downloadChunkSize {
				t.Fatalf("resumed bytes=%d", result.ResumedBytes)
			}
			if result.Resumable || tr.checkpoint != nil {
				t.Fatal("completed checkpoint not retired")
			}
		})
	}
}

func mustDownloadClient(t *testing.T, a *App, profile string) *s3.Client {
	t.Helper()
	c, err := a.client(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParallelDownloadFallbackAndInvalidRanges(t *testing.T) {
	for _, mode := range []string{"ignored", "invalid", "identity", "parallel"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("AWS_MAX_ATTEMPTS", "1")
			var active, peak atomic.Int32
			var ranges atomic.Int32
			const size = 96 << 20
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", "\"fixture\"")
				if r.Header.Get("Range") == "" || mode == "ignored" {
					w.Header().Set("Content-Length", fmt.Sprint(size))
					_, _ = io.CopyN(w, zeroStream{}, size)
					return
				}
				ranges.Add(1)
				now := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); now > old; old = peak.Load() {
					if peak.CompareAndSwap(old, now) {
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
				var start, end int64
				fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
				contentRange := fmt.Sprintf("bytes %d-%d/%d", start, end, size)
				if mode == "invalid" {
					contentRange = "bytes 0-1/2"
				}
				if mode == "identity" {
					w.Header().Set("ETag", "\"other\"")
				}
				w.Header().Set("Content-Range", contentRange)
				w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
				w.WriteHeader(206)
				_, _ = io.CopyN(w, zeroStream{}, end-start+1)
			}))
			defer server.Close()
			a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), &journalVault{values: map[string]string{}})
			p := journalProfile(t, a, server)
			tr := journalEntry(t, a, p, "download", "large")
			checkpoint(t, a, tr)
			err := a.execute(context.Background(), *tr)
			if mode == "invalid" || mode == "identity" {
				if err == nil {
					t.Fatal("accepted invalid response")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(tr.Path)
			if err != nil || info.Size() != size {
				t.Fatal("wrong final file", err)
			}
			if mode == "parallel" && (ranges.Load() != 3 || peak.Load() < 2 || peak.Load() > 8) {
				t.Fatalf("ranges=%d concurrent=%d", ranges.Load(), peak.Load())
			}
			if tr.checkpoint != nil {
				t.Fatal("retained completed checkpoint")
			}
		})
	}
}

type zeroStream struct{}

func (zeroStream) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestParallelDownloadGlobalRequestBudget(t *testing.T) {
	var active, peak atomic.Int32
	const size = 96 << 20
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", "\"fixture\"")
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", fmt.Sprint(size))
			_, _ = io.CopyN(w, zeroStream{}, size)
			return
		}
		count := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); count > old; old = peak.Load() {
			if peak.CompareAndSwap(old, count) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(206)
		_, _ = io.CopyN(w, zeroStream{}, end-start+1)
	}))
	defer server.Close()
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), &journalVault{values: map[string]string{}})
	p := journalProfile(t, a, server)
	transfers := []Transfer{}
	for i := 0; i < 4; i++ {
		tr := journalEntry(t, a, p, "download", fmt.Sprint(i))
		checkpoint(t, a, tr)
		transfers = append(transfers, *tr)
	}
	var group sync.WaitGroup
	for _, tr := range transfers {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := a.execute(context.Background(), tr); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if peak.Load() > 8 || peak.Load() < 2 {
		t.Fatalf("global concurrency=%d", peak.Load())
	}
}
