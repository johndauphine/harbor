package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
)

func TestSigningWaitsForCapacityOnEveryAttempt(t *testing.T) {
	t.Setenv("AWS_MAX_ATTEMPTS", "2")
	var requests, signs atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if requests.Add(1) == 1 {
			w.WriteHeader(503)
			io.WriteString(w, `<Error><Code>SlowDown</Code></Error>`)
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
	var leases []*transferLease
	for i := 0; i < 8; i++ {
		lease, err := a.tuning().scheduler.Acquire(context.Background(), transferRequestClass{Scope: tuningScope(p), TransferID: "held", Direction: "upload"})
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	release := func() {
		for _, lease := range leases {
			lease.Release()
		}
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var previousAdmission *tuningAdmission
	go func() {
		_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("queued"), Body: strings.NewReader("payload")}, func(o *s3.Options) {
			o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
				return stack.Finalize.Insert(middleware.FinalizeMiddlewareFunc("CheckFreshAdmission", func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
					signs.Add(1)
					admission, _ := ctx.Value(tuningAdmissionKey{}).(*tuningAdmission)
					if admission == nil || admission.lease == nil || admission.state != a.tuning() || admission.lease.scheduler != a.tuning().scheduler || admission.class.Scope != tuningScope(p) || admission.class.Direction != "upload" || admission.class.Control || admission.claimed.Load() {
						return middleware.FinalizeOutput{}, middleware.Metadata{}, errors.New("signing without a matching unclaimed admission lease")
					}
					if previousAdmission != nil && (admission == previousAdmission || admission.lease == previousAdmission.lease) {
						return middleware.FinalizeOutput{}, middleware.Metadata{}, errors.New("retry reused a prior attempt's admission lease")
					}
					previousAdmission = admission
					// Seven unrelated fixture leases remain held throughout both
					// attempts. Only this attempt can occupy the eighth slot.
					if a.TransferTuningStatus().Scheduler.ActiveBulk != 8 {
						return middleware.FinalizeOutput{}, middleware.Metadata{}, errors.New("signing without an exclusive per-attempt lease")
					}
					return next.HandleFinalize(ctx, in)
				}), "Signing", middleware.Before)
			})
		})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for a.TransferTuningStatus().Scheduler.WaitingBulk != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	before := signs.Load()
	waiting := a.TransferTuningStatus().Scheduler.WaitingBulk
	// Release only one fixture lease. Releasing all eight in a loop races the
	// signing observer, since the first release can immediately admit it.
	leases[0].Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if waiting != 1 || before != 0 {
		t.Fatalf("request signed before capacity: waiting=%d signs=%d", waiting, before)
	}
	if signs.Load() != 2 || requests.Load() != 2 {
		t.Fatalf("retry bypassed admission/signing: signs=%d requests=%d", signs.Load(), requests.Load())
	}
	if a.TransferTuningStatus().Scheduler.ActiveBulk != 7 {
		t.Fatal("attempt leaked capacity")
	}
	release()
	if a.TransferTuningStatus().Scheduler.ActiveBulk != 0 {
		t.Fatal("fixture leaked capacity")
	}
}

func TestSigningFailureReleasesCapacity(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("failed signing reached network") }))
	defer server.Close()
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), &journalVault{values: map[string]string{}})
	p := journalProfile(t, a, server)
	c := mustDownloadClient(t, a, p.ID)
	sentinel := errors.New("synthetic signing failure")
	_, err := c.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("fixture"), Body: strings.NewReader("payload")}, func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			return stack.Finalize.Insert(middleware.FinalizeMiddlewareFunc("FailBeforeSigning", func(context.Context, middleware.FinalizeInput, middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
				return middleware.FinalizeOutput{}, middleware.Metadata{}, sentinel
			}), "Signing", middleware.Before)
		})
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("wrong error: %v", err)
	}
	if a.TransferTuningStatus().Scheduler.ActiveBulk != 0 {
		t.Fatal("pre-HTTP failure leaked capacity")
	}
}

func TestCancelledSigningWaitNeverReachesNetwork(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("cancelled waiter reached network") }))
	defer server.Close()
	a := journalFixture(t, filepath.Join(t.TempDir(), "profiles.db"), &journalVault{values: map[string]string{}})
	if err := a.SetTransferTuningMode("fixed"); err != nil {
		t.Fatal(err)
	}
	p := journalProfile(t, a, server)
	c := mustDownloadClient(t, a, p.ID)
	var leases []*transferLease
	for i := 0; i < 8; i++ {
		lease, err := a.tuning().scheduler.Acquire(context.Background(), transferRequestClass{Scope: tuningScope(p), Direction: "upload", TransferID: "held"})
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
		defer lease.Release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("cancelled"), Body: strings.NewReader("payload")})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for a.TransferTuningStatus().Scheduler.WaitingBulk != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	waiting := a.TransferTuningStatus().Scheduler.WaitingBulk
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("wrong cancellation: %v", err)
	}
	status := a.TransferTuningStatus().Scheduler
	if waiting != 1 || status.WaitingBulk != 0 || status.ActiveBulk != 8 {
		t.Fatalf("cancelled admission leaked or released another lease: %+v", status)
	}
}
