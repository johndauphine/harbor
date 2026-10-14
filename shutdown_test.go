package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func shutdownFixture(t *testing.T) (*App, <-chan struct{}, func(), string) {
	t.Helper()
	a := NewApp()
	a.ctx = context.Background()
	a.profiles = []Profile{{ID: "fixture", Name: "Fixture", Region: "us-east-1"}}
	entered, release := make(chan struct{}), make(chan struct{})
	a.secretGet = func(string) (string, error) { close(entered); <-release; return "", errors.New("fixture unavailable") }
	path := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); _ = a.shutdown(context.Background()) })
	transferID := a.queue("upload", "fixture", "bucket", "key", path)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	return a, entered, unblock, transferID
}

func TestShutdownWaitsForWorkerBeforeRemovingCache(t *testing.T) {
	a, _, unblock, transferID := shutdownFixture(t)
	path := a.Transfers()[0].Path
	done := a.beginShutdown()
	select {
	case <-done:
		t.Fatal("shutdown returned with worker blocked")
	default:
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("cache removed while worker active", err)
	}
	if a.queue("upload", "fixture", "bucket", "new", "unused") != "" {
		t.Fatal("admitted work during shutdown")
	}
	if err := a.RetryTransfer(transferID); err == nil {
		t.Fatal("admitted retry during shutdown")
	}
	unblock()
	if err := a.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cache not removed after workers drained", err)
	}
	if got := a.Transfers()[0].Status; got != "cancelled" {
		t.Fatalf("status=%s", got)
	}
}

func TestShutdownTimeoutPreservesActiveCacheAndCanFinishLater(t *testing.T) {
	a, _, unblock, _ := shutdownFixture(t)
	path := a.Transfers()[0].Path
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown error=%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("timeout removed active cache", err)
	}
	unblock()
	if err := a.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownCancelsRegisteredCopiesAndRejectsNewOperations(t *testing.T) {
	a := NewApp()
	ctx, finish, err := a.beginOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := a.beginShutdown()
	if ctx.Err() == nil {
		t.Fatal("active operation was not cancelled")
	}
	select {
	case <-done:
		t.Fatal("did not wait for copy cleanup")
	default:
	}
	if _, _, err := a.beginOperation(context.Background()); !errors.Is(err, errShuttingDown) {
		t.Fatal(err)
	}
	if _, err := a.QueueLocalUpload(context.Background(), "", "", "", ""); !errors.Is(err, errShuttingDown) {
		t.Fatal("local staging admitted", err)
	}
	if _, err := a.SaveDownload(context.Background(), "", "", "", false); !errors.Is(err, errShuttingDown) {
		t.Fatal("save admitted", err)
	}
	finish()
	if err := a.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownRejectsAPIRequests(t *testing.T) {
	a := NewApp()
	a.beginShutdown()
	req := httptest.NewRequest("POST", "http://127.0.0.1:1234/api/profiles", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer fixture")
	recorder := httptest.NewRecorder()
	webHandler(a, "127.0.0.1:1234", "fixture").ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", recorder.Code)
	}
}

func TestServerSignalShutdownWaitsForTransfers(t *testing.T) {
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			a, _, unblock, _ := shutdownFixture(t)
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.NotFoundHandler()}
			stop := make(chan os.Signal, 1)
			returned := make(chan error, 1)
			go func() { returned <- serveUntilStopped(a, server, listener, stop) }()
			stop <- sig
			// Admission closure provides a deterministic observation of shutdown begun.
			deadline := time.After(5 * time.Second)
			for {
				a.mu.Lock()
				closing := a.closing
				a.mu.Unlock()
				if closing {
					break
				}
				select {
				case <-deadline:
					t.Fatal("shutdown never began")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			select {
			case err := <-returned:
				t.Fatalf("server returned before transfer: %v", err)
			default:
			}
			unblock()
			select {
			case err := <-returned:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("server failed to finish")
			}
		})
	}
}

func TestShutdownCancelsApplicationStorageContext(t *testing.T) {
	a := NewApp()
	a.configFile = filepath.Join(t.TempDir(), "profiles.db")
	a.startup(context.Background())
	if a.ctx.Err() != nil {
		t.Fatal(a.ctx.Err())
	}
	if err := a.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.ctx.Err() != context.Canceled {
		t.Fatal("storage operations were not cancelled")
	}
}

func TestShutdownUnblocksStagedBrowserUploadAndRemovesPartialFile(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	a := NewApp()
	a.ctx = context.Background()
	a.profiles = []Profile{{ID: "fixture", Name: "Fixture", Region: "us-east-1"}}
	a.secretGet = func(string) (string, error) { return `{"AccessKey":"fixture","SecretKey":"fixture"}`, nil }
	reader, writer := io.Pipe()
	defer writer.Close()
	req := httptest.NewRequest("POST", "http://127.0.0.1:1234/api/upload?profile=fixture&bucket=bucket&key=fixture", reader)
	req.Header.Set("Authorization", "Bearer fixture")
	recorder := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() { webHandler(a, "127.0.0.1:1234", "fixture").ServeHTTP(recorder, req); close(handlerDone) }()
	if _, err := writer.Write([]byte("partial fixture")); err != nil {
		t.Fatal(err)
	}
	// io.Pipe Write returns only after the handler starts reading its staged body.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handlerDone:
	case <-ctx.Done():
		t.Fatal("upload handler did not unwind")
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "HarborS3", "upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatal("partial upload cache left after shutdown")
	}
	if len(a.Transfers()) != 0 {
		t.Fatal("partial upload queued during shutdown")
	}
	if recorder.Code == http.StatusOK {
		t.Fatal("interrupted upload reported success")
	}
}
