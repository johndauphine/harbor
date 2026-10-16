package main

import (
	"context"
	"crypto/sha256"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Real request durations exceed the old idle threshold. The test never changes
// resource ceilings, controller clocks, or the production multipart settings.
func TestMinIOAdaptiveSlowPartContinuity(t *testing.T) {
	if os.Getenv("HARBOR_ADAPTIVE_SLOW_MINIO") != "1" || os.Getenv("HARBOR_MINIO_BINARY") == "" {
		t.Skip("set HARBOR_ADAPTIVE_SLOW_MINIO=1 and HARBOR_MINIO_BINARY for real long-part timing")
	}
	f := newMinIOResilience(t)
	ctx, cancel := context.WithTimeout(context.Background(), 175*time.Second)
	defer cancel()
	target, _ := url.Parse("http://" + f.address)
	reverse := httputil.NewSingleHostReverseProxy(target)
	var throttles atomic.Int32
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && r.URL.Query().Get("partNumber") != "" {
			if throttles.CompareAndSwap(0, 1) {
				// Drain the upload so a real HTTP 503 reaches the SDK, rather
				// than a reset caused by closing an unread large request body.
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					http.Error(w, "fault fixture could not read upload", http.StatusBadGateway)
					return
				}
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, "<Error><Code>SlowDown</Code><Message>Slow-part fixture</Message></Error>")
				return
			}
			timer := time.NewTimer(32 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
		}
		reverse.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { cancel(); _ = f.a.shutdown(context.Background()); proxy.Close() })
	ca := filepath.Join(f.root, "slow-part-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: proxy.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	profile, err := f.a.SaveProfile(Profile{Name: "Disposable long-part MinIO", Region: "us-east-1", Endpoint: proxy.URL, CAFile: ca, PathStyle: true}, Secret{AccessKey: "harbor-resilience", SecretKey: "synthetic-resilience-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if f.a.TransferTuningStatus().Scheduler.Mode != "auto" {
		t.Fatal("slow-part test requires production Automatic default")
	}
	const size = 256 << 20
	source := filepath.Join(f.root, "slow-distinct-source.bin")
	file, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	block := make([]byte, 1<<20)
	for i := range block {
		block[i] = byte((i*31 + i/7919) % 251)
	}
	writer := io.MultiWriter(file, h)
	for i := 0; i < size/len(block); i++ {
		block[0], block[1] = byte(i), byte(i>>8)
		if _, err = writer.Write(block); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	want := h.Sum(nil)
	key := "slow-part-continuity.bin"
	f.keys = append(f.keys, key)
	started := time.Now()
	tid, err := f.a.QueueLocalUpload(ctx, profile.ID, f.bucket, key, source)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := f.a.tuning().scheduler
	scope := schedulerScope{tuningScope(profile), "upload"}
	var before slowPartPolicyState
	longGaps, preserved := 0, 0
	sawBackoff := false
	for {
		state := readSlowPartPolicy(scheduler, scope)
		if state.target == 4 {
			sawBackoff = true
		}
		if !state.last.IsZero() && !state.last.Equal(before.last) {
			if !before.last.IsZero() && state.last.Sub(before.last) > time.Second {
				t.Logf("completion cohort: gap=%s active window=%t target=%d baseline=%.0f successes=%d", state.last.Sub(before.last).Round(time.Millisecond), !state.start.IsZero(), state.target, state.previous, state.successes)
			}
			// Reason text can describe an earlier low-demand callback while a
			// fresh window is already collecting data. Compare active windows;
			// a genuine low-demand reset has no current measurement window.
			if !before.start.IsZero() && !state.start.IsZero() && state.last.Sub(before.last) > 30*time.Second {
				longGaps++
				// A completed measurement window may advance start, but it must
				// retain a useful baseline. The old idle reset loses both.
				if !state.start.Equal(before.start) && state.previous == 0 {
					t.Fatalf("active slow request discarded learning after %s: before=%+v after=%+v", state.last.Sub(before.last), before, state)
				}
				preserved++
				t.Logf("actual completion gap=%s retained measurement baseline=%.0f bytes/s successes=%d", state.last.Sub(before.last).Round(time.Millisecond), state.previous, state.successes)
			}
			before = state
		}
		f.a.mu.Lock()
		tr := *f.a.transfers[tid]
		active := tr.cancel != nil
		f.a.mu.Unlock()
		if !active && (tr.Status == "completed" || tr.Status == "failed" || tr.Status == "cancelled") {
			if tr.Status != "completed" {
				t.Fatalf("slow transfer failed: %s %s", tr.Status, tr.Error)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("slow transfer deadline", scheduler.Snapshot())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if longGaps == 0 || preserved == 0 || throttles.Load() != 1 || !sawBackoff {
		t.Fatalf("missing live regression evidence: gaps=%d preserved=%d throttles=%d backoff=%t finalPolicy=%+v scheduler=%+v", longGaps, preserved, throttles.Load(), sawBackoff, before, scheduler.Snapshot())
	}
	object, err := f.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	h.Reset()
	n, err := io.Copy(h, object.Body)
	object.Body.Close()
	if err != nil || n != size || string(h.Sum(nil)) != string(want) {
		t.Fatalf("slow-part SHA-256 mismatch: bytes=%d error=%v", n, err)
	}
	f.assertNoMultipart()
	status := scheduler.Snapshot()
	if status.ActiveBulk != 0 || status.WaitingBulk != 0 || status.ActiveControl != 0 || status.ThrottledRequests != 1 {
		t.Fatal("slow-part admission or throttle accounting", status)
	}
	t.Logf("real MinIO 256MiB SHA-256 matched after %s, %d long completion gaps retained learning", time.Since(started).Round(time.Millisecond), preserved)
}

type slowPartPolicyState struct {
	last, start time.Time
	previous    float64
	successes   int
	target      int
	reason      string
}

func readSlowPartPolicy(s *transferScheduler, key schedulerScope) slowPartPolicyState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.learning[key]; p != nil {
		return slowPartPolicyState{last: p.policy.last, start: p.policy.start, previous: p.policy.previous, successes: p.policy.successes, target: s.scopeLimit(key), reason: p.policy.reason}
	}
	return slowPartPolicyState{}
}
