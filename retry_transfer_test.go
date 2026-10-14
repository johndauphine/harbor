package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Install the queued entry before launching its worker to reproduce a goroutine
// that the scheduler has not started when the user cancels and retries.
func retryFixture(t *testing.T) (*App, string) {
	t.Helper()
	a := NewApp()
	a.ctx = context.Background()
	a.profiles = []Profile{{ID: "fixture", Name: "Fixture", Region: "us-east-1"}}
	transferID := "queued-fixture"
	a.transfers[transferID] = &Transfer{ID: transferID, ProfileID: "fixture", Status: "queued", attempt: 1}
	a.transferOrder = []string{transferID}
	return a, transferID
}

func awaitRetrySignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("transfer worker did not finish")
	}
}

func TestCancelledQueueWorkerCannotClaimNewAttempt(t *testing.T) {
	a, transferID := retryFixture(t)
	var calls atomic.Int32
	a.secretGet = func(string) (string, error) { calls.Add(1); return "", errors.New("fixture vault failure") }
	a.CancelTransfer(transferID)
	// Model the atomic retry transition, with both workers still unscheduled.
	a.mu.Lock()
	tr := a.transfers[transferID]
	tr.attempt++
	tr.Status = "queued"
	tr.Done = 7
	a.mu.Unlock()
	a.run(transferID, 1)
	if calls.Load() != 0 {
		t.Fatal("cancelled queue worker executed the new attempt")
	}
	if got := a.Transfers()[0]; got.Status != "queued" || got.Done != 7 {
		t.Fatalf("stale worker changed retry: %+v", got)
	}
	a.run(transferID, 2)
	if calls.Load() != 1 || a.Transfers()[0].Status != "failed" {
		t.Fatal("current attempt did not execute exactly once")
	}
}

func TestCancelRetryStartsOnlyOneWorker(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		w.Header().Set("Content-Length", "7")
		_, _ = w.Write([]byte("fixture"))
	}))
	defer server.Close()
	a := securityFixtureApp(t, server.URL, server)
	transferID := "queued-fixture"
	a.transfers[transferID] = &Transfer{ID: transferID, ProfileID: a.profiles[0].ID, Direction: "download", Bucket: "bucket", Key: "object", Path: filepath.Join(t.TempDir(), "download"), Status: "queued", attempt: 1}
	a.transferOrder = []string{transferID}
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer func() { unblock(); a.workers.Wait() }()
	a.CancelTransfer(transferID)
	var successes atomic.Int32
	var contenders sync.WaitGroup
	for i := 0; i < 20; i++ {
		contenders.Add(1)
		go func() {
			defer contenders.Done()
			if a.RetryTransfer(transferID) == nil {
				successes.Add(1)
			}
		}()
	}
	contenders.Wait()
	awaitRetrySignal(t, entered)
	returned := make(chan struct{})
	go func() { a.run(transferID, 1); a.run(transferID, 2); close(returned) }()
	awaitRetrySignal(t, returned)
	if successes.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("retry successes=%d, requests=%d", successes.Load(), calls.Load())
	}
	if got := a.Transfers()[0]; got.Status != "running" {
		t.Fatalf("stale runner changed status: %s", got.Status)
	}
	unblock()
	a.workers.Wait()
	if got := a.Transfers()[0]; got.Status != "completed" || got.Done != 7 {
		t.Fatalf("wrong final result: %+v", got)
	}
}

func TestRunningCancellationMustUnwindBeforeRetry(t *testing.T) {
	a, transferID := retryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.transfers[transferID].Status = "running"
	a.transfers[transferID].cancel = cancel
	a.CancelTransfer(transferID)
	if ctx.Err() == nil {
		t.Fatal("worker was not cancelled")
	}
	if err := a.RetryTransfer(transferID); err == nil {
		t.Fatal("retry allowed while cancelled worker was still unwinding")
	}
	if got := a.Transfers()[0]; got.Status != "running" {
		t.Fatal("cancellation published a retryable state before worker completion")
	}
}
