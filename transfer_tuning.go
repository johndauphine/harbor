package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

var errCheckpointPersistence = errors.New("Save transfer recovery state")
var errStagedUploadChanged = errors.New("Staged upload changed")

func resetTuningAfterValidation(a *App, ctx context.Context, err error) {
	var local *os.PathError
	if !errors.Is(err, errDownloadIdentity) && !errors.Is(err, errCheckpointPersistence) && !errors.Is(err, errStagedUploadChanged) && !errors.As(err, &local) {
		return
	}
	if identity, ok := ctx.Value(tuningContextKey{}).(tuningIdentity); ok {
		a.tuning().scheduler.ResetScope(identity.scope, identity.direction)
	}
}

type tuningContextKey struct{}
type tuningIdentity struct{ id, scope, direction string }
type tuningPhaseMetric struct {
	Count        uint64
	Milliseconds float64
}
type transferTuningStatus struct {
	CommandLineOverride                       bool
	Scheduler                                 tuningSchedulerSnapshot
	Resources                                 tuningResources
	ResourceReason                            string
	Phases                                    map[string]tuningPhaseMetric
	HTTPAttempts, HTTPFailures, HTTPBodyBytes uint64
}
type appTransferTuning struct {
	modeMu                        sync.Mutex
	commandLineOverride           bool
	mu                            sync.Mutex
	scheduler                     *transferScheduler
	resources                     tuningResources
	previous                      *tuningResourceReading
	resourceReason                string
	phases                        map[string]tuningPhaseMetric
	attempts, failures, bodyBytes uint64
	preparation                   chan struct{}
	activeTransfers               chan struct{}
}

func (a *App) tuning() *appTransferTuning {
	a.tuningMu.Lock()
	defer a.tuningMu.Unlock()
	if a.transferTuning == nil {
		a.transferTuning = &appTransferTuning{scheduler: newTransferScheduler(), phases: map[string]tuningPhaseMetric{}, preparation: make(chan struct{}, 2), activeTransfers: make(chan struct{}, 8)}
		_ = a.transferTuning.scheduler.SetMode("auto")
	}
	return a.transferTuning
}
func (a *App) sampleTransferResources() {
	t := a.tuning()
	t.mu.Lock()
	if time.Since(t.resources.SampledAt) < 5*time.Second {
		t.mu.Unlock()
		return
	}
	t.resources, t.previous = sampleTuningResources(a.transferCache, t.previous)
	limit, why := tuningResourceCeiling(t.resources)
	t.resourceReason = why
	t.scheduler.SetResourceCeiling(limit)
	t.mu.Unlock()
}
func (a *App) TransferTuningStatus() transferTuningStatus {
	a.sampleTransferResources()
	t := a.tuning()
	t.mu.Lock()
	out := transferTuningStatus{Resources: t.resources, ResourceReason: t.resourceReason, Phases: map[string]tuningPhaseMetric{}, HTTPAttempts: t.attempts, HTTPFailures: t.failures, HTTPBodyBytes: t.bodyBytes}
	for k, v := range t.phases {
		out.Phases[k] = v
	}
	t.mu.Unlock()
	t.modeMu.Lock()
	out.CommandLineOverride = t.commandLineOverride
	out.Scheduler = t.scheduler.Snapshot()
	t.modeMu.Unlock()
	return out
}
func (a *App) SetTransferTuningMode(mode string) error {
	state := a.tuning()
	state.modeMu.Lock()
	defer state.modeMu.Unlock()
	if state.commandLineOverride {
		return errors.New("Automatic tuning is disabled by --no-auto-tuning for this launch")
	}
	if mode != "fixed" && mode != "observe" && mode != "auto" {
		return errors.New("Choose Fixed, Observe or Automatic")
	}
	db, err := a.openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE IF NOT EXISTS transfer_tuning_settings(singleton INTEGER PRIMARY KEY CHECK(singleton=1),mode TEXT NOT NULL)"); err != nil {
		return err
	}
	if _, err = db.Exec("INSERT INTO transfer_tuning_settings(singleton,mode) VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET mode=excluded.mode", mode); err != nil {
		return err
	}
	return a.tuning().scheduler.SetMode(mode)
}
func (a *App) restoreTransferTuning() error {
	db, err := a.openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE IF NOT EXISTS transfer_tuning_settings(singleton INTEGER PRIMARY KEY CHECK(singleton=1),mode TEXT NOT NULL)"); err != nil {
		return err
	}
	var mode string
	if err = db.QueryRow("SELECT mode FROM transfer_tuning_settings WHERE singleton=1").Scan(&mode); err != nil {
		return nil
	}
	// An unknown preference cannot prevent recovery of existing transfers.
	if mode != "fixed" && mode != "observe" && mode != "auto" {
		mode = "auto"
	}
	return a.tuning().scheduler.SetMode(mode)
}
func (a *App) recordTransferPhase(name string, started time.Time) {
	switch name {
	case "staging", "source verification", "download verification", "disk sync", "checkpoint":
	default:
		return
	}
	t := a.tuning()
	t.mu.Lock()
	defer t.mu.Unlock()
	v := t.phases[name]
	v.Count++
	v.Milliseconds += float64(time.Since(started)) / float64(time.Millisecond)
	t.phases[name] = v
}
func (a *App) acquireTransferPreparation(ctx context.Context) (func(), error) {
	t := a.tuning()
	select {
	case t.preparation <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-t.preparation }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (a *App) observeUsefulTransfer(ctx context.Context, bytes int64, started time.Time) {
	identity, ok := ctx.Value(tuningContextKey{}).(tuningIdentity)
	if !ok || bytes <= 0 {
		return
	}
	a.tuning().scheduler.Observe(transferSample{Class: transferRequestClass{TransferID: identity.id, Scope: identity.scope, Direction: identity.direction}, Started: started, Finished: time.Now(), Bytes: bytes, Success: true})
}
func tuningScope(p Profile) string {
	raw, _ := json.Marshal([]any{p.ID, credentialID(p), p.Endpoint, p.Region, p.PathStyle, p.CAFile})
	if p.CAFile != "" {
		pem, err := os.ReadFile(p.CAFile)
		if err == nil {
			trust := sha256.Sum256(pem)
			raw = append(raw, trust[:]...)
		}
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (a *App) transferTuningContext(ctx context.Context, t Transfer) context.Context {
	a.profileMu.Lock()
	var scope string
	for _, p := range a.profiles {
		if p.ID == t.ProfileID {
			scope = tuningScope(p)
		}
	}
	a.profileMu.Unlock()
	return context.WithValue(ctx, tuningContextKey{}, tuningIdentity{id: t.ID, scope: scope, direction: t.Direction})
}
func (a *App) tuneClient(c *s3.Client, p Profile) *s3.Client {
	options := c.Options()
	client, ok := options.HTTPClient.(*http.Client)
	if !ok {
		return c
	}
	clone := *client
	clone.Transport = &tuningRoundTripper{next: client.Transport, app: a, scope: tuningScope(p)}
	return s3.New(options, func(o *s3.Options) {
		o.HTTPClient = &clone
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			// Inside Retry, so each attempt waits before generating a fresh signature.
			return stack.Finalize.Insert(&tuningAdmissionMiddleware{app: a, scope: tuningScope(p)}, "Signing", middleware.Before)
		})
	})
}

type tuningAdmissionKey struct{}
type tuningAdmission struct {
	lease   *transferLease
	state   *appTransferTuning
	class   transferRequestClass
	started time.Time
	claimed atomic.Bool
}
type tuningAdmissionMiddleware struct {
	app   *App
	scope string
}

func (*tuningAdmissionMiddleware) ID() string { return "HarborTransferAdmission" }
func (m *tuningAdmissionMiddleware) HandleFinalize(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (out middleware.FinalizeOutput, metadata middleware.Metadata, err error) {
	request, ok := in.Request.(*smithyhttp.Request)
	if !ok {
		return out, metadata, errors.New("Unsupported storage request transport")
	}
	admission, err := m.app.admitTuningRequest(request.Request.WithContext(ctx), m.scope)
	if err != nil {
		return out, metadata, err
	}
	// Signing/authentication may fail before HTTP sees the request. In that case
	// there is no response body to release the lease on our behalf.
	defer func() {
		if !admission.claimed.Load() {
			admission.lease.Release()
		}
	}()
	return next.HandleFinalize(context.WithValue(ctx, tuningAdmissionKey{}, admission), in)
}

type tuningRoundTripper struct {
	next                              http.RoundTripper
	app                               *App
	scope                             string
	controlTimeout, inactivityTimeout time.Duration
}

func (a *App) admitTuningRequest(r *http.Request, scope string) (*tuningAdmission, error) {
	a.sampleTransferResources()
	state := a.tuning()
	op := middleware.GetOperationName(r.Context())
	identity, hasIdentity := r.Context().Value(tuningContextKey{}).(tuningIdentity)
	bulk := op == "GetObject" || op == "UploadPart" || op == "PutObject"
	// Ordinary folder markers are control traffic; transfer and standalone SDK
	// uploads/downloads use the same bounded network budget.
	if op == "PutObject" && !hasIdentity && r.ContentLength == 0 {
		bulk = false
	}
	class := transferRequestClass{Scope: scope, Control: !bulk}
	if hasIdentity {
		class.TransferID = identity.id
		class.Direction = identity.direction
	} else {
		class.TransferID = "standalone"
		if op == "GetObject" {
			class.Direction = "download"
		} else {
			class.Direction = "upload"
		}
	}
	started := time.Now()
	lease, err := state.scheduler.Acquire(r.Context(), class)
	if err != nil {
		return nil, err
	}
	return &tuningAdmission{lease: lease, state: state, class: class, started: started}, nil
}

func (t *tuningRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	admission, _ := r.Context().Value(tuningAdmissionKey{}).(*tuningAdmission)
	if admission == nil {
		// Also support direct transport callers. SDK clients always admit before signing.
		var err error
		admission, err = t.app.admitTuningRequest(r, t.scope)
		if err != nil {
			return nil, err
		}
	}
	admission.claimed.Store(true)
	lease, state, class, started := admission.lease, admission.state, admission.class, admission.started
	networkStarted := time.Now()
	interval := t.controlTimeout
	inactivity := !class.Control || middleware.GetOperationName(r.Context()) == "CompleteMultipartUpload"
	if inactivity {
		interval = t.inactivityTimeout
		if interval <= 0 {
			interval = transferInactivityTimeout
		}
	} else if interval <= 0 {
		interval = controlRequestTimeout
	}
	attempt := newNetworkAttempt(r.Context(), interval, inactivity)
	request := r.Clone(attempt.ctx)
	if request.Body != nil && request.Body != http.NoBody {
		request.Body = &networkProgressBody{ReadCloser: request.Body, attempt: attempt}
	}
	state.mu.Lock()
	state.attempts++
	state.mu.Unlock()
	response, err := t.next.RoundTrip(request)
	if err != nil {
		err = attempt.err(err)
		attempt.stop()
		lease.Release()
		state.mu.Lock()
		state.failures++
		state.mu.Unlock()
		state.scheduler.Observe(transferSample{Class: class, Started: networkStarted, Finished: time.Now(), QueueDelay: networkStarted.Sub(started), TransportError: true, Cancelled: r.Context().Err() != nil})
		return response, err
	}
	// Failure responses include SDK retries that eventually succeed. The SDK
	// retains its own retry/backoff policy; the scheduler adds no operation retry.
	if response == nil {
		attempt.stop()
		lease.Release()
		return nil, errors.New("Storage returned no HTTP response")
	}
	if response.StatusCode >= 300 {
		state.mu.Lock()
		state.failures++
		state.mu.Unlock()
		state.scheduler.Observe(transferSample{Class: class, Started: networkStarted, Finished: time.Now(), Throttled: response.StatusCode == 429 || response.StatusCode == 503, Cancelled: r.Context().Err() != nil})
	}
	if response.Body == nil || response.Body == http.NoBody {
		err = attempt.err(nil)
		attempt.stop()
		lease.Release()
		return response, err
	}
	body := &tuningResponseBody{ReadCloser: response.Body, lease: lease, state: state, class: class, ctx: r.Context(), started: networkStarted, attempt: attempt}
	response.Body = body
	stop := context.AfterFunc(attempt.ctx, func() { _ = body.Close() })
	body.cancelMu.Lock()
	body.stop = stop
	if body.finished {
		stop()
	}
	body.cancelMu.Unlock()
	if attempt.ctx.Err() != nil {
		_ = body.Close()
	}
	return response, nil
}

type tuningResponseBody struct {
	io.ReadCloser
	lease     *transferLease
	state     *appTransferTuning
	once      sync.Once
	cancelMu  sync.Mutex
	stop      func() bool
	class     transferRequestClass
	ctx       context.Context
	started   time.Time
	failure   sync.Once
	finished  bool
	closeOnce sync.Once
	closeErr  error
	attempt   *networkAttempt
}

func (b *tuningResponseBody) finish() {
	b.once.Do(func() {
		b.cancelMu.Lock()
		b.finished = true
		if b.stop != nil {
			b.stop()
		}
		b.cancelMu.Unlock()
		b.attempt.stop()
		b.lease.Release()
	})
}
func (b *tuningResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		// Close may finish concurrently with a pending Read. Account each read
		// directly so that cancellation cannot retire its byte count too early.
		b.state.mu.Lock()
		b.state.bodyBytes += uint64(n)
		b.state.mu.Unlock()
	}
	b.attempt.progress(n)
	err = b.attempt.err(err)
	if err != nil {
		if err != io.EOF {
			b.observeFailure()
		}
		b.finish()
	}
	return n, err
}
func (b *tuningResponseBody) Close() error {
	b.closeOnce.Do(func() { b.closeErr = b.ReadCloser.Close() })
	if b.attempt.err(nil) != nil {
		b.observeFailure()
	}
	b.finish()
	return b.closeErr
}
func (b *tuningResponseBody) observeFailure() {
	b.failure.Do(func() {
		b.state.mu.Lock()
		b.state.failures++
		b.state.mu.Unlock()
		b.state.scheduler.Observe(transferSample{Class: b.class, Started: b.started, Finished: time.Now(), TransportError: true, Cancelled: b.ctx.Err() != nil})
	})
}

func (a *App) acquireActiveTransfer(ctx context.Context) (func(), error) {
	t := a.tuning()
	select {
	case t.activeTransfers <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-t.activeTransfers }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// applyTransferTuningOverride affects this launch only; saved GUI preferences survive.
func (a *App) applyTransferTuningOverride(disabled bool) {
	if !disabled {
		return
	}
	state := a.tuning()
	state.modeMu.Lock()
	defer state.modeMu.Unlock()
	state.commandLineOverride = true
	_ = state.scheduler.SetMode("fixed")
}
