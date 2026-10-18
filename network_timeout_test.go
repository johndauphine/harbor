package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const testNetworkInterval = 750 * time.Millisecond

func timeoutTransport(t *testing.T, handler http.HandlerFunc) (*tuningRoundTripper, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	base := server.Client().Transport.(*http.Transport).Clone()
	t.Cleanup(base.CloseIdleConnections)
	a := NewApp()
	_ = a.tuning().scheduler.SetMode("fixed")
	return &tuningRoundTripper{next: base, app: a, scope: "timeout-fixture", controlTimeout: testNetworkInterval, inactivityTimeout: testNetworkInterval}, server
}
func networkRequest(t *testing.T, ctx context.Context, server *httptest.Server, operation, path string) *http.Request {
	t.Helper()
	r := tuningRequest(ctx, operation)
	url, err := r.URL.Parse(server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	r.URL = url
	return r
}
func assertNetworkTimeout(t *testing.T, err error) {
	t.Helper()
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() || !timeout.Temporary() {
		t.Fatalf("expected retryable network timeout, got %v", err)
	}
}
func assertNoNetworkLeases(t *testing.T, tr *tuningRoundTripper) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s := tr.app.tuning().scheduler.Snapshot()
		if s.ActiveBulk == 0 && s.ActiveControl == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("network timeout retained scheduler lease")
}

func TestNetworkTimeoutStalledHeaders(t *testing.T) {
	tr, server := timeoutTransport(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthy" {
			_, _ = io.WriteString(w, "healthy")
			return
		}
		<-r.Context().Done()
	})
	for _, op := range []string{"HeadObject", "GetObject", "CompleteMultipartUpload"} {
		t.Run(op, func(t *testing.T) {
			parent := context.Background()
			done := make(chan error, 1)
			go func() { _, err := tr.RoundTrip(networkRequest(t, parent, server, op, "/stall")); done <- err }()
			res, err := tr.RoundTrip(networkRequest(t, parent, server, "GetObject", "/healthy"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				assertNetworkTimeout(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("headers never timed out")
			}
			if parent.Err() != nil {
				t.Fatal("timeout cancelled SDK parent")
			}
			assertNoNetworkLeases(t, tr)
		})
	}
}

func TestNetworkTimeoutStalledDownloadAndUnreadBody(t *testing.T) {
	tr, server := timeoutTransport(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	for _, unread := range []bool{false, true} {
		t.Run(map[bool]string{false: "reading", true: "unread"}[unread], func(t *testing.T) {
			res, err := tr.RoundTrip(networkRequest(t, context.Background(), server, "GetObject", "/"))
			if err != nil {
				t.Fatal(err)
			}
			if unread {
				time.Sleep(2 * testNetworkInterval)
			}
			_, err = io.ReadAll(res.Body)
			assertNetworkTimeout(t, err)
			res.Body.Close()
			assertNoNetworkLeases(t, tr)
		})
	}
}

type endlessNetworkBody struct{}

func (endlessNetworkBody) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type progressingUploadBody struct{ remaining int }

func (b *progressingUploadBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	time.Sleep(testNetworkInterval / 3)
	n := min(len(p), 1024)
	clear(p[:n])
	b.remaining--
	return n, nil
}
func TestNetworkUploadProgressExtendsAttempt(t *testing.T) {
	tr, server := timeoutTransport(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) })
	r := networkRequest(t, context.Background(), server, "PutObject", "/")
	r.Method = "PUT"
	r.Body = io.NopCloser(&progressingUploadBody{remaining: 8})
	r.ContentLength = 8 * 1024
	started := time.Now()
	res, err := tr.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || time.Since(started) <= 2*testNetworkInterval {
		t.Fatalf("upload progress did not extend attempt: %v", err)
	}
	assertNoNetworkLeases(t, tr)
}

func TestNetworkBodyParentCancellation(t *testing.T) {
	tr, server := timeoutTransport(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := tr.RoundTrip(networkRequest(t, ctx, server, "GetObject", "/"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = io.ReadAll(res.Body)
	res.Body.Close()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("body did not preserve parent cancellation: %v", err)
	}
	assertNoNetworkLeases(t, tr)
}

func TestNetworkTimeoutStalledUpload(t *testing.T) {
	release := make(chan struct{})
	tr, server := timeoutTransport(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	defer close(release)
	r := networkRequest(t, context.Background(), server, "UploadPart", "/")
	r.Method = "PUT"
	r.Body = io.NopCloser(io.LimitReader(endlessNetworkBody{}, 1<<30))
	r.ContentLength = 1 << 30
	done := make(chan error, 1)
	go func() { _, err := tr.RoundTrip(r); done <- err }()
	select {
	case err := <-done:
		assertNetworkTimeout(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("upload never timed out")
	}
	assertNoNetworkLeases(t, tr)
}

func TestNetworkProgressExtendsStreamAndMultipartCompletion(t *testing.T) {
	tr, server := timeoutTransport(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		for i := 0; i < 8; i++ {
			_, _ = io.WriteString(w, " ")
			w.(http.Flusher).Flush()
			time.Sleep(testNetworkInterval / 3)
		}
		_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><ETag>fixture</ETag></CompleteMultipartUploadResult>`)
	})
	for _, op := range []string{"GetObject", "CompleteMultipartUpload"} {
		started := time.Now()
		res, err := tr.RoundTrip(networkRequest(t, context.Background(), server, op, "/"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || time.Since(started) <= 2*testNetworkInterval {
			t.Fatalf("progress was bounded by total duration: %v", err)
		}
		assertNoNetworkLeases(t, tr)
	}
}

func TestNetworkControlTotalTimeout(t *testing.T) {
	tr, server := timeoutTransport(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		for {
			_, err := io.WriteString(w, " ")
			if err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(testNetworkInterval / 3):
			}
		}
	})
	res, err := tr.RoundTrip(networkRequest(t, context.Background(), server, "ListBuckets", "/"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(res.Body)
	assertNetworkTimeout(t, err)
	res.Body.Close()
	assertNoNetworkLeases(t, tr)
}

func TestNetworkTimeoutStartsAfterAdmission(t *testing.T) {
	tr, server := timeoutTransport(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	var leases []*transferLease
	for i := 0; i < 8; i++ {
		lease, err := tr.app.tuning().scheduler.Acquire(context.Background(), transferRequestClass{Scope: "occupied", TransferID: "occupied", Direction: "download"})
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	defer func() {
		for _, lease := range leases {
			lease.Release()
		}
	}()
	done := make(chan error, 1)
	go func() {
		res, err := tr.RoundTrip(networkRequest(t, context.Background(), server, "GetObject", "/"))
		if err == nil {
			_, err = io.ReadAll(res.Body)
			res.Body.Close()
		}
		done <- err
	}()
	time.Sleep(2 * testNetworkInterval)
	leases[0].Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued request did not run")
	}
}

func TestNetworkTimeoutSDKRetriesAndCompletionKeepalives(t *testing.T) {
	var attempts atomic.Int32
	tr, server := timeoutTransport(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" && attempts.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "10")
			return
		}
		w.WriteHeader(200)
		for i := 0; i < 8; i++ {
			_, _ = io.WriteString(w, " ")
			w.(http.Flusher).Flush()
			time.Sleep(testNetworkInterval / 3)
		}
		_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><ETag>fixture</ETag></CompleteMultipartUploadResult>`)
	})
	c := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(server.URL), UsePathStyle: true, Credentials: aws.AnonymousCredentials{}, HTTPClient: &http.Client{Transport: tr}, Retryer: retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = 2
		o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
	})})
	ctx := context.Background()
	_, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key")})
	if err != nil || attempts.Load() != 2 {
		t.Fatalf("SDK did not retry timeout: attempts=%d err=%v", attempts.Load(), err)
	}
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("bucket"), Key: aws.String("key"), UploadId: aws.String("upload"), MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: aws.String("fixture")}}}})
	if err != nil {
		t.Fatal(err)
	}
	assertNoNetworkLeases(t, tr)
}

func TestNetworkParentCancellationTakesPrecedence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempt := newNetworkAttempt(ctx, time.Hour, true)
	defer attempt.stop()
	attempt.mu.Lock()
	attempt.expired = true
	attempt.mu.Unlock()
	cancel()
	if !errors.Is(attempt.err(errors.New("transport")), context.Canceled) {
		t.Fatal("parent cancellation masked by timeout")
	}
}

func TestNetworkProgressResetRejectsStaleTimer(t *testing.T) {
	a := newNetworkAttempt(context.Background(), time.Hour, true)
	defer a.stop()
	a.progress(1)
	a.expire() // Simulate a callback already queued before the progress reset.
	if err := a.err(nil); err != nil {
		t.Fatal(err)
	}
	if a.ctx.Err() != nil {
		t.Fatal("stale timer cancelled progressing attempt")
	}
	if strings.Contains(networkTimeoutError{}.Error(), "context canceled") {
		t.Fatal("timeout misclassified")
	}
}
