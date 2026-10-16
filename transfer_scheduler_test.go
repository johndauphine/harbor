package main

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

func schedulerWait(t *testing.T, s *transferScheduler, n int) {
	t.Helper()
	end := time.Now().Add(2 * time.Second)
	for time.Now().Before(end) {
		if s.Snapshot().WaitingBulk == n {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("waiting count", s.Snapshot())
}
func TestTransferSchedulerBudgetsAndCancellation(t *testing.T) {
	s := newTransferScheduler()
	if x := s.Snapshot(); x.Mode != "observe" || x.BulkLimit != 8 {
		t.Fatal(x)
	}
	var leases []*transferLease
	for i := 0; i < 8; i++ {
		l, e := s.Acquire(context.Background(), transferRequestClass{TransferID: "one", Scope: "scope"})
		if e != nil {
			t.Fatal(e)
		}
		leases = append(leases, l)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		l, e := s.Acquire(ctx, transferRequestClass{TransferID: "two"})
		if l != nil {
			l.Release()
		}
		done <- e
	}()
	schedulerWait(t, s, 1)
	c1, e := s.Acquire(context.Background(), transferRequestClass{Control: true})
	if e != nil {
		t.Fatal(e)
	}
	c2, e := s.Acquire(context.Background(), transferRequestClass{Control: true})
	if e != nil {
		t.Fatal(e)
	}
	if x := s.Snapshot(); x.ActiveControl != 2 || x.ActiveBulk != 8 {
		t.Fatal(x)
	}
	cancel()
	if <-done != context.Canceled {
		t.Fatal("cancel error")
	}
	schedulerWait(t, s, 0)
	for _, l := range leases {
		l.Release()
		l.Release()
	}
	c1.Release()
	c2.Release()
	if x := s.Snapshot(); x.ActiveBulk != 0 || x.ActiveControl != 0 {
		t.Fatal(x)
	}
}
func TestTransferSchedulerFairRoundRobin(t *testing.T) {
	s := newTransferScheduler()
	s.SetMode("auto")
	s.SetResourceCeiling(1)
	first, _ := s.Acquire(context.Background(), transferRequestClass{TransferID: "held"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan string, 3)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for _, id := range []string{"a", "a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			l, e := s.Acquire(ctx, transferRequestClass{TransferID: id})
			if e == nil {
				out <- id
				<-release
				l.Release()
			}
		}(id)
	}
	schedulerWait(t, s, 3)
	first.Release()
	a := <-out
	release <- struct{}{}
	b := <-out
	if a == b {
		t.Fatal("one transfer monopolized consecutive rounds", a, b)
	}
	release <- struct{}{}
	<-out
	release <- struct{}{}
	wg.Wait()
}
func TestTransferSchedulerShrinkModesAndScopeIsolation(t *testing.T) {
	s := newTransferScheduler()
	s.SetMode("auto")
	var leases []*transferLease
	for i := 0; i < 4; i++ {
		l, e := s.Acquire(context.Background(), transferRequestClass{Scope: "healthy", Direction: "upload"})
		if e != nil {
			t.Fatal(e)
		}
		leases = append(leases, l)
	}
	s.SetResourceCeiling(2)
	if x := s.Snapshot(); x.ActiveBulk != 4 || x.BulkLimit != 2 {
		t.Fatal(x)
	}
	for _, l := range leases {
		l.Release()
	}
	s.SetMode("observe")
	s.SetResourceCeiling(1)
	if x := s.Snapshot(); x.BulkLimit != 8 || x.SuggestedLimit != 1 {
		t.Fatal(x)
	}
	s.SetResourceCeiling(8)
	s.SetMode("auto")
	now := time.Now()
	sample := controllerSample(now, 0)
	sample.Class = transferRequestClass{Scope: "bad", Direction: "upload"}
	sample.Success = false
	sample.Throttled = true
	s.Observe(sample)
	if x := s.Snapshot(); x.BulkLimit != 8 {
		t.Fatal("provider throttle lowered global ceiling", x)
	}
	s.mu.Lock()
	bad := s.scopeLimit(schedulerScope{"bad", "upload"})
	healthy := s.scopeLimit(schedulerScope{"healthy", "upload"})
	s.mu.Unlock()
	if bad != 4 || healthy != 8 {
		t.Fatal(bad, healthy)
	}
	s.ResetScope("bad", "upload")
	s.mu.Lock()
	_, exists := s.learning[schedulerScope{"bad", "upload"}]
	s.mu.Unlock()
	if exists {
		t.Fatal("scope not reset")
	}
	if s.SetMode("invalid") == nil {
		t.Fatal("invalid mode accepted")
	}
}
func TestTransferSchedulerCancelledGrantRace(t *testing.T) {
	for run := 0; run < 100; run++ {
		s := newTransferScheduler()
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l, _ := s.Acquire(ctx, transferRequestClass{TransferID: "same"})
				if l != nil {
					l.Release()
				}
			}()
		}
		cancel()
		wg.Wait()
		if x := s.Snapshot(); x.ActiveBulk != 0 || x.WaitingBulk != 0 {
			t.Fatal(x)
		}
	}
}

func TestTransferSchedulerControlCancellationAndBoundedLearning(t *testing.T) {
	s := newTransferScheduler()
	one, _ := s.Acquire(context.Background(), transferRequestClass{Control: true})
	two, _ := s.Acquire(context.Background(), transferRequestClass{Control: true})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		l, e := s.Acquire(ctx, transferRequestClass{Control: true})
		if l != nil {
			l.Release()
		}
		done <- e
	}()
	cancel()
	if <-done != context.Canceled {
		t.Fatal("queued control did not cancel")
	}
	one.Release()
	two.Release()
	now := time.Unix(100, 0)
	s.now = func() time.Time { return now }
	for i := 0; i < 140; i++ {
		sample := controllerSample(now, 0)
		sample.Class.Scope = string(rune(i + 1))
		sample.Throttled = true
		sample.Success = false
		s.Observe(sample)
	}
	s.mu.Lock()
	count := len(s.learning)
	s.mu.Unlock()
	if count != 128 {
		t.Fatal("unbounded or missing learning", count)
	}
	now = now.Add(31 * time.Minute)
	sample := controllerSample(now, 1)
	sample.Class.Scope = "fresh"
	s.Observe(sample)
	s.mu.Lock()
	count = len(s.learning)
	s.mu.Unlock()
	if count != 1 {
		t.Fatal("idle entries retained", count)
	}
}

func TestTransferSchedulerPopulationInvalidatesProbe(t *testing.T) {
	s := newTransferScheduler()
	s.SetMode("auto")
	s.mu.Lock()
	s.policy.limit = 5
	s.policy.probeFrom = 4
	s.population = "old population"
	s.mu.Unlock()
	sample := controllerSample(time.Now(), 1)
	sample.Class = transferRequestClass{TransferID: "new transfer", Scope: "scope", Direction: "download"}
	s.Observe(sample)
	if x := s.Snapshot(); x.BulkLimit != 4 {
		t.Fatal("workload change retained unconfirmed probe", x)
	}
}

func TestTransferSchedulerHealthyScopeFollowsGlobalProbe(t *testing.T) {
	s := newTransferScheduler()
	s.SetMode("auto")
	now := time.Now()
	sample := controllerSample(now, 1)
	sample.Class = transferRequestClass{Scope: "healthy", Direction: "upload"}
	s.Observe(sample)
	s.mu.Lock()
	s.policy.limit = 5
	s.policy.probeFrom = 4
	if limit := s.scopeLimit(requestScope(sample.Class)); limit != 5 {
		t.Fatal("healthy scope prevents global probe", limit)
	}
	s.mu.Unlock()
	// A recovering provider owns the sole probe; global observations may not
	// start another one concurrently.
	s.mu.Lock()
	p := s.learning[requestScope(sample.Class)]
	p.restricted = true
	p.policy.limit = 3
	p.policy.probeFrom = 2
	s.policy.probeFrom = 0
	s.policy.previous = 1
	s.policy.stable = 1
	s.mu.Unlock()
	sample.Finished = now.Add(time.Second)
	sample.Started = now
	s.Observe(sample)
	s.mu.Lock()
	globalProbe := s.policy.probeFrom
	s.mu.Unlock()
	if globalProbe != 0 {
		t.Fatal("simultaneous global/provider probes")
	}
}

func TestTransferSchedulerIdleProviderDoesNotBlockProbes(t *testing.T) {
	s := newTransferScheduler()
	s.SetMode("auto")
	s.mu.Lock()
	idle := newAdaptiveController()
	idle.limit = 3
	idle.probeFrom = 2
	s.learning[schedulerScope{"idle", "upload"}] = &schedulerLearning{policy: idle, restricted: true, last: time.Now()}
	s.mu.Unlock()
	sample := controllerSample(time.Now(), 1)
	sample.Class = transferRequestClass{Scope: "healthy", Direction: "upload"}
	s.Observe(sample)
	s.mu.Lock()
	defer s.mu.Unlock()
	if idle.probeFrom != 0 || idle.limit != 2 {
		t.Fatal("idle provider probe retained")
	}
}

func TestTransferSchedulerControlAdmissionSurvivesBulkSaturation(t *testing.T) {
	s := newTransferScheduler()
	// Model a fully occupied bulk budget and its maximum backlog. Waiting
	// entries are internal to avoid launching thousands of unrelated goroutines.
	s.mu.Lock()
	s.active = 8
	s.waiting = 4096
	key := schedulerQueueKey{schedulerScope{"bulk", "upload"}, "large"}
	for i := 0; i < 4096; i++ {
		s.queues[key] = append(s.queues[key], &schedulerWaiter{class: transferRequestClass{TransferID: "large", Scope: "bulk", Direction: "upload"}, ready: make(chan struct{})})
	}
	s.order = []schedulerQueueKey{key}
	s.mu.Unlock()
	if l, e := s.Acquire(context.Background(), transferRequestClass{}); e == nil {
		if l != nil {
			l.Release()
		}
		t.Fatal("saturated bulk accepted another waiter")
	}
	one, e := s.Acquire(context.Background(), transferRequestClass{Control: true})
	if e != nil {
		t.Fatal("bulk backlog blocked abort/control", e)
	}
	two, e := s.Acquire(context.Background(), transferRequestClass{Control: true})
	if e != nil {
		t.Fatal("bulk backlog blocked second control", e)
	}
	if x := s.Snapshot(); x.ActiveControl != 2 || x.WaitingBulk != 4096 {
		t.Fatal(x)
	}
	one.Release()
	two.Release()
	// Independently, a saturated control backlog does not reject bulk work.
	other := newTransferScheduler()
	other.mu.Lock()
	other.controls = 2
	for i := 0; i < 128; i++ {
		other.controlQueue = append(other.controlQueue, &schedulerWaiter{class: transferRequestClass{Control: true}, ready: make(chan struct{})})
	}
	other.mu.Unlock()
	if _, e := other.Acquire(context.Background(), transferRequestClass{Control: true}); e == nil {
		t.Fatal("unbounded control backlog")
	}
	bulk, e := other.Acquire(context.Background(), transferRequestClass{})
	if e != nil {
		t.Fatal("control backlog blocked bulk", e)
	}
	bulk.Release()
}
func TestTransferSchedulerSnapshotReflectsProviderCooldown(t *testing.T) {
	s := newTransferScheduler()
	now := time.Unix(100, 0)
	s.now = func() time.Time { return now }
	s.SetMode("auto")
	sample := controllerSample(now, 0)
	sample.Class = transferRequestClass{Scope: "provider", Direction: "upload"}
	sample.Throttled = true
	sample.Success = false
	s.Observe(sample)
	if x := s.Snapshot(); x.ThrottledScopes != 1 || x.Reason != "Storage throttling; reduced activity while recovering" {
		t.Fatal(x)
	}
	now = now.Add(time.Minute)
	if x := s.Snapshot(); x.ThrottledScopes != 0 {
		t.Fatal("inactive expired cooldown still displayed", x)
	}
	s.SetMode("observe")
	sample.Started = now.Add(-time.Second)
	sample.Finished = now
	s.Observe(sample)
	if x := s.Snapshot(); x.ThrottledScopes != 1 || x.BulkLimit != 8 || x.Reason != "Observed storage throttling; fixed request limit retained" {
		t.Fatal(x)
	}
}
