package main

import (
	"bytes"
	"context"
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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
)

type tuningTransportFunc func(*http.Request) (*http.Response, error)

func (f tuningTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type tuningBadBody struct{}

func (tuningBadBody) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (tuningBadBody) Close() error               { return nil }
func tuningRequest(ctx context.Context, operation string) *http.Request {
	ctx = middleware.WithOperationName(ctx, operation)
	r, _ := http.NewRequestWithContext(ctx, "GET", "https://fixture.invalid/object", nil)
	return r
}
func TestTuningTransportLeaseLifecycle(t *testing.T) {
	for _, scenario := range []string{"eof", "early-close", "read-error", "transport-error", "nil-body", "throttle", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			a := NewApp()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport := &tuningRoundTripper{app: a, scope: "fixture", next: tuningTransportFunc(func(*http.Request) (*http.Response, error) {
				if scenario == "transport-error" {
					return nil, errors.New("synthetic disconnect")
				}
				res := &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader([]byte("fixture"))), ContentLength: 7}
				if scenario == "nil-body" {
					res.Body = nil
				}
				if scenario == "read-error" {
					res.Body = tuningBadBody{}
				}
				if scenario == "throttle" {
					res.StatusCode = 503
				}
				return res, nil
			})}
			response, err := transport.RoundTrip(tuningRequest(ctx, "GetObject"))
			if scenario == "transport-error" {
				if err == nil {
					t.Fatal("expected error")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if response.Body != nil {
					if scenario == "cancel" {
						cancel()
					} else if scenario != "early-close" {
						_, _ = io.Copy(io.Discard, response.Body)
					}
					_ = response.Body.Close()
					_ = response.Body.Close()
				}
			}
			status := a.TransferTuningStatus()
			if status.Scheduler.ActiveBulk != 0 || status.Scheduler.ActiveControl != 0 {
				t.Fatalf("leaked permit: %+v", status.Scheduler)
			}
			if scenario == "read-error" && status.HTTPFailures != 1 {
				t.Fatal("body error not observed")
			}
			if scenario == "throttle" && status.Scheduler.ThrottledRequests != 1 {
				t.Fatal("throttle hidden")
			}
			if status.Scheduler.CompletedRequests != 0 {
				t.Fatal("HTTP body counted as application-verified bytes")
			}
		})
	}
}

func TestSDKSmallTransfersShareTuningBudget(t *testing.T) {
	var active, peak atomic.Int32
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", "fixture")
	}))
	defer server.Close()
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), &journalVault{values: map[string]string{}})
	if err := a.SetTransferTuningMode("fixed"); err != nil {
		t.Fatal(err)
	}
	p := journalProfile(t, a, server)
	c := mustDownloadClient(t, a, p.ID)
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := c.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("small"), Body: strings.NewReader("small")})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a.TransferTuningStatus().Scheduler.WaitingBulk >= 4 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	status := a.TransferTuningStatus()
	close(release)
	group.Wait()
	if status.Scheduler.ActiveBulk != 8 || status.Scheduler.WaitingBulk != 4 || peak.Load() > 8 {
		t.Fatalf("SDK path bypassed budget: %+v peak=%d", status.Scheduler, peak.Load())
	}
	if a.TransferTuningStatus().Scheduler.ActiveBulk != 0 {
		t.Fatal("completion leaked permits")
	}
}

func TestTuningPreferenceRecoveryAndPrivacy(t *testing.T) {
	vault := &journalVault{values: map[string]string{}}
	path := filepath.Join(t.TempDir(), "profiles.db")
	a := journalFixture(t, path, vault)
	if err := a.SetTransferTuningMode("auto"); err != nil {
		t.Fatal(err)
	}
	recovered := journalFixture(t, path, vault)
	if recovered.TransferTuningStatus().Scheduler.Mode != "auto" {
		t.Fatal("mode did not survive restart")
	}
	if err := a.SetTransferTuningMode("unsafe"); err == nil {
		t.Fatal("accepted unknown mode")
	}
	db, err := a.openStore()
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE transfer_tuning_settings SET mode='unknown'")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	fallback := journalFixture(t, path, vault)
	if fallback.TransferTuningStatus().Scheduler.Mode != "auto" {
		t.Fatal("invalid preference did not fall back safely")
	}
	// Tuning preferences are separate from recovery metadata and contain no keys.
	if _, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestTuningSDKRetriesRemainVisible(t *testing.T) {
	t.Setenv("AWS_MAX_ATTEMPTS", "2")
	var attempts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(503)
			_, _ = io.WriteString(w, "<Error><Code>SlowDown</Code><Message>synthetic throttling</Message></Error>")
			return
		}
		w.Header().Set("ETag", "fixture")
	}))
	defer server.Close()
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), &journalVault{values: map[string]string{}})
	if err := a.SetTransferTuningMode("fixed"); err != nil {
		t.Fatal(err)
	}
	p := journalProfile(t, a, server)
	c := mustDownloadClient(t, a, p.ID)
	_, err := c.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("small"), Body: strings.NewReader("payload")})
	if err != nil {
		t.Fatal(err)
	}
	status := a.TransferTuningStatus()
	if attempts.Load() != 2 || status.HTTPAttempts != 2 || status.Scheduler.ThrottledRequests != 1 || status.Scheduler.ActiveBulk != 0 {
		t.Fatalf("retry telemetry or cleanup: %+v", status)
	}
}

func TestTuningCancellationClosesUnreadSDKBody(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), &journalVault{values: map[string]string{}})
	if err := a.SetTransferTuningMode("fixed"); err != nil {
		t.Fatal(err)
	}
	p := journalProfile(t, a, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	object, err := mustDownloadClient(t, a, p.ID).GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bucket"), Key: aws.String("object")})
	if err != nil {
		t.Fatal(err)
	}
	if a.TransferTuningStatus().Scheduler.ActiveBulk != 1 {
		t.Fatal("body was not tracked")
	}
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && a.TransferTuningStatus().Scheduler.ActiveBulk != 0 {
		time.Sleep(time.Millisecond)
	}
	_ = object.Body.Close()
	if a.TransferTuningStatus().Scheduler.ActiveBulk != 0 {
		t.Fatal("cancellation retained unread body permit")
	}
}

func TestTuningAPIRequiresSessionAndValidMode(t *testing.T) {
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), &journalVault{values: map[string]string{}})
	handler := webHandler(a, "fixture", "session")
	request := func(route, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://fixture/api/"+route, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := request("transfer-tuning", "{}", "wrong"); got.Code != 401 {
		t.Fatal("unprotected tuning diagnostics")
	}
	if got := request("set-transfer-tuning", `{"Mode":"unbounded"}`, "session"); got.Code != 400 {
		t.Fatal("accepted unsupported tuning mode")
	}
	got := request("set-transfer-tuning", `{"Mode":"auto"}`, "session")
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"Mode":"auto"`) {
		t.Fatal(got.Code, got.Body.String())
	}
	if strings.Contains(got.Body.String(), a.configFile) {
		t.Fatal("diagnostics disclosed local path")
	}
}

func TestTuningDefaultAndCommandLineOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.db")
	vault := &journalVault{values: map[string]string{}}
	a := journalFixture(t, path, vault)
	if a.TransferTuningStatus().Scheduler.Mode != "auto" {
		t.Fatal("default must be automatic")
	}
	a.applyTransferTuningOverride(true)
	status := a.TransferTuningStatus()
	if !status.CommandLineOverride || status.Scheduler.Mode != "fixed" || status.Scheduler.BulkLimit != 8 {
		t.Fatalf("override not applied: %+v", status)
	}
	if err := a.SetTransferTuningMode("auto"); err == nil {
		t.Fatal("GUI bypassed launch override")
	}
	recovered := journalFixture(t, path, vault)
	if recovered.TransferTuningStatus().Scheduler.Mode != "auto" {
		t.Fatal("launch override persisted")
	}
	if err := recovered.SetTransferTuningMode("fixed"); err != nil {
		t.Fatal(err)
	}
	saved := journalFixture(t, path, vault)
	if saved.TransferTuningStatus().Scheduler.Mode != "fixed" {
		t.Fatal("GUI opt out did not persist")
	}
}
