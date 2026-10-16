package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type transferRequestClass struct {
	TransferID, Scope, Direction string
	Control                      bool
	ExpectedBytes                int64
}
type transferSample struct {
	Class                                         transferRequestClass
	Started, Finished                             time.Time
	QueueDelay                                    time.Duration
	Bytes                                         int64
	Success, Throttled, TransportError, Cancelled bool
}
type tuningSchedulerSnapshot struct {
	Mode                                                                            string
	BulkLimit, ActiveBulk, WaitingBulk, ActiveControl, ControlLimit, SuggestedLimit int
	ThrottledScopes                                                                 int
	Reason                                                                          string
	CompletedRequests, ThrottledRequests                                            uint64
}
type schedulerScope struct{ scope, direction string }
type schedulerQueueKey struct {
	scope    schedulerScope
	transfer string
}
type schedulerWaiter struct {
	class   transferRequestClass
	ready   chan struct{}
	granted bool
}
type schedulerLearning struct {
	policy     *adaptiveController
	last       time.Time
	population string
	restricted bool
}
type transferScheduler struct {
	mu                        sync.Mutex
	mode                      string
	resource                  int
	active, controls, waiting int
	queues                    map[schedulerQueueKey][]*schedulerWaiter
	order                     []schedulerQueueKey
	controlQueue              []*schedulerWaiter
	scopes                    map[schedulerScope]int
	learning                  map[schedulerScope]*schedulerLearning
	policy                    *adaptiveController
	population                string
	activeTransfers           map[schedulerQueueKey]int
	completed, throttled      uint64
	now                       func() time.Time
}
type transferLease struct {
	scheduler *transferScheduler
	class     transferRequestClass
	once      sync.Once
}

func newTransferScheduler() *transferScheduler {
	return &transferScheduler{mode: "observe", resource: 8, activeTransfers: map[schedulerQueueKey]int{}, queues: map[schedulerQueueKey][]*schedulerWaiter{}, scopes: map[schedulerScope]int{}, learning: map[schedulerScope]*schedulerLearning{}, policy: newAdaptiveController(), now: time.Now}
}
func requestScope(c transferRequestClass) schedulerScope { return schedulerScope{c.Scope, c.Direction} }
func (s *transferScheduler) limit() int {
	if s.mode == "auto" {
		return min(s.resource, s.policy.limit)
	}
	return 8
}
func (s *transferScheduler) scopeLimit(k schedulerScope) int {
	if s.mode == "auto" {
		if p := s.learning[k]; p != nil && p.restricted {
			return min(s.limit(), p.policy.limit)
		}
	}
	return s.limit()
}
func (s *transferScheduler) Acquire(ctx context.Context, c transferRequestClass) (*transferLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w := &schedulerWaiter{class: c, ready: make(chan struct{})}
	s.mu.Lock()
	// Control admission has its own bounded backlog: saturated bulk work must
	// not consume the slots needed to abort or finish existing transfers.
	if c.Control && len(s.controlQueue) >= 128 {
		s.mu.Unlock()
		return nil, errors.New("Storage control request queue is full")
	}
	if !c.Control && s.waiting >= 4096 {
		s.mu.Unlock()
		return nil, errors.New("Transfer request queue is full")
	}
	if c.Control {
		s.controlQueue = append(s.controlQueue, w)
	} else {
		k := schedulerQueueKey{requestScope(c), c.TransferID}
		if len(s.queues[k]) == 0 {
			s.order = append(s.order, k)
		}
		s.queues[k] = append(s.queues[k], w)
		s.waiting++
	}
	s.dispatch()
	s.mu.Unlock()
	select {
	case <-w.ready:
		lease := &transferLease{scheduler: s, class: c}
		if err := ctx.Err(); err != nil {
			lease.Release()
			return nil, err
		}
		return lease, nil
	case <-ctx.Done():
		s.mu.Lock()
		if w.granted {
			s.release(c)
		} else {
			s.remove(w)
		}
		s.dispatch()
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}
func (s *transferScheduler) remove(w *schedulerWaiter) {
	if w.class.Control {
		for i, v := range s.controlQueue {
			if v == w {
				s.controlQueue = append(s.controlQueue[:i], s.controlQueue[i+1:]...)
				return
			}
		}
		return
	}
	k := schedulerQueueKey{requestScope(w.class), w.class.TransferID}
	q := s.queues[k]
	for i, v := range q {
		if v == w {
			s.queues[k] = append(q[:i], q[i+1:]...)
			s.waiting--
			break
		}
	}
	if len(s.queues[k]) == 0 {
		s.dropQueue(k)
	}
}
func (s *transferScheduler) dropQueue(k schedulerQueueKey) {
	delete(s.queues, k)
	for i, v := range s.order {
		if v == k {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}
func (s *transferScheduler) dispatch() {
	for s.controls < 2 && len(s.controlQueue) > 0 {
		w := s.controlQueue[0]
		s.controlQueue = s.controlQueue[1:]
		s.controls++
		w.granted = true
		close(w.ready)
	}
	for s.active < s.limit() && len(s.order) > 0 {
		found := false
		for scan := len(s.order); scan > 0; scan-- {
			k := s.order[0]
			s.order = s.order[1:]
			q := s.queues[k]
			if s.scopes[k.scope] >= s.scopeLimit(k.scope) {
				s.order = append(s.order, k)
				continue
			}
			w := q[0]
			q = q[1:]
			if len(q) > 0 {
				s.queues[k] = q
				s.order = append(s.order, k)
			} else {
				delete(s.queues, k)
			}
			s.waiting--
			s.active++
			s.scopes[k.scope]++
			s.activeTransfers[k]++
			w.granted = true
			close(w.ready)
			found = true
			break
		}
		if !found {
			return
		}
	}
}
func (s *transferScheduler) release(c transferRequestClass) {
	if c.Control {
		s.controls--
	} else {
		s.active--
		k := requestScope(c)
		s.scopes[k]--
		tk := schedulerQueueKey{k, c.TransferID}
		s.activeTransfers[tk]--
		if s.activeTransfers[tk] == 0 {
			delete(s.activeTransfers, tk)
		}
		if s.scopes[k] == 0 {
			delete(s.scopes, k)
		}
	}
}
func (l *transferLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() { s := l.scheduler; s.mu.Lock(); s.release(l.class); s.dispatch(); s.mu.Unlock() })
}
func (s *transferScheduler) SetMode(mode string) error {
	if mode != "fixed" && mode != "observe" && mode != "auto" {
		return errors.New("Unknown transfer tuning mode")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if mode != s.mode {
		s.mode = mode
		s.policy = newAdaptiveController()
		s.policy.ceiling(s.resource)
		s.learning = map[schedulerScope]*schedulerLearning{}
	}
	s.dispatch()
	return nil
}
func (s *transferScheduler) SetResourceCeiling(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resource = max(1, min(8, n))
	s.policy.ceiling(s.resource)
	for _, p := range s.learning {
		p.policy.ceiling(s.resource)
	}
	s.dispatch()
}
func (s *transferScheduler) Observe(sample transferSample) {
	if sample.Class.Control {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, p := range s.learning {
		if now.Sub(p.last) > 30*time.Minute && s.scopes[k] == 0 {
			delete(s.learning, k)
		}
	}
	// Capture the ready/active population, including the just-finished request.
	// Changed transfer sets invalidate comparisons between concurrency levels.
	k := requestScope(sample.Class)
	queuedScopes := map[schedulerScope]bool{}
	for key := range s.queues {
		queuedScopes[key.scope] = true
	}
	for key, entry := range s.learning {
		if key != k && s.scopes[key] == 0 && !queuedScopes[key] {
			entry.policy.resetLearning()
		}
	}
	globalPopulation := s.populationKey(sample.Class, false)
	if globalPopulation != s.population {
		s.policy.resetLearning()
		s.population = globalPopulation
	}
	beforeGlobalLimit := s.policy.limit
	scopeProbe := false
	for _, entry := range s.learning {
		if entry.restricted && entry.policy.probeFrom > 0 {
			scopeProbe = true
			break
		}
	}
	demand := s.active+1 >= s.limit()
	if sample.Throttled || sample.TransportError || !sample.Success {
		if !sample.Cancelled {
			s.policy.resetLearning()
			s.policy.cooldown = sample.Finished.Add(15 * time.Second)
			s.policy.reason = "Waiting for stable transfer measurements"
		}
	} else if !scopeProbe {
		s.policy.observe(sample, demand)
	}
	if s.policy.limit != beforeGlobalLimit {
		for _, entry := range s.learning {
			entry.policy.resetLearning()
		}
	}
	p := s.learning[k]
	if p == nil && len(s.learning) >= 128 {
		// Bounded LRU eviction only of inactive scopes. The live scope
		// count is bounded by the request ceiling, so admission remains protected.
		var oldest schedulerScope
		var stamp time.Time
		for key, v := range s.learning {
			if s.scopes[key] == 0 && (stamp.IsZero() || v.last.Before(stamp)) {
				oldest = key
				stamp = v.last
			}
		}
		if !stamp.IsZero() {
			delete(s.learning, oldest)
		}
	}
	if p == nil && len(s.learning) < 128 {
		p = &schedulerLearning{policy: newAdaptiveController()}
		p.policy.ceiling(s.resource)
		s.learning[k] = p
	}
	if p != nil {
		population := s.populationKey(sample.Class, true)
		if population != p.population {
			p.policy.resetLearning()
			p.population = population
		}
		p.last = now
		if sample.Throttled && !sample.Cancelled && !p.restricted {
			p.policy.limit = s.limit()
			p.restricted = true
		}
		if sample.Throttled || sample.TransportError || !sample.Success || (p.restricted && s.policy.probeFrom == 0 && !s.otherScopeProbe(k)) {
			before := p.policy.limit
			p.policy.observe(sample, s.scopes[k]+1 >= min(s.limit(), p.policy.limit))
			if p.policy.limit != before {
				s.policy.resetLearning()
				for key, entry := range s.learning {
					if key != k {
						entry.policy.resetLearning()
					}
				}
			}
		} else if p.restricted {
			p.policy.resetLearning()
		}
		if p.restricted && !sample.Throttled && !sample.TransportError && sample.Success && p.policy.probeFrom == 0 && p.policy.limit >= s.limit() {
			p.restricted = false
		}
	}

	if sample.Success && !sample.Cancelled {
		s.completed++
	}
	if sample.Throttled && !sample.Cancelled {
		s.throttled++
	}
	s.dispatch()
}
func (s *transferScheduler) Snapshot() tuningSchedulerSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	throttled := 0
	for key, p := range s.learning {
		if p.restricted && (s.scopes[key] > 0 || s.scopeWaiting(key) > 0 || now.Before(p.policy.cooldown)) {
			throttled++
		}
	}
	reason := s.policy.reason
	if throttled > 0 {
		if s.mode == "auto" {
			reason = "Storage throttling; reduced activity while recovering"
		} else {
			reason = "Observed storage throttling; fixed request limit retained"
		}
	}
	return tuningSchedulerSnapshot{ThrottledScopes: throttled, Mode: s.mode, BulkLimit: s.limit(), ActiveBulk: s.active, WaitingBulk: s.waiting, ActiveControl: s.controls, ControlLimit: 2, SuggestedLimit: min(s.resource, s.policy.limit), Reason: reason, CompletedRequests: s.completed, ThrottledRequests: s.throttled}
}

func (s *transferScheduler) scopeWaiting(k schedulerScope) int {
	n := 0
	for key, q := range s.queues {
		if key.scope == k {
			n += len(q)
		}
	}
	return n
}
func (s *transferScheduler) populationKey(c transferRequestClass, scoped bool) string {
	keys := map[schedulerQueueKey]bool{{requestScope(c), c.TransferID}: true}
	for k := range s.activeTransfers {
		if !scoped || k.scope == requestScope(c) {
			keys[k] = true
		}
	}
	for k := range s.queues {
		if !scoped || k.scope == requestScope(c) {
			keys[k] = true
		}
	}
	values := make([]string, 0, len(keys))
	for k := range keys {
		values = append(values, fmt.Sprintf("%q/%q/%q", k.scope.scope, k.scope.direction, k.transfer))
	}
	sort.Strings(values)
	return strings.Join(values, ";")
}

// ResetScope invalidates learned comparisons after identity, source or local
// durability failures. It does not discard transfer checkpoints or kill work.
func (s *transferScheduler) ResetScope(scope, direction string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := schedulerScope{scope, direction}
	delete(s.learning, k)
	s.policy.resetLearning()
	s.dispatch()
}

// Probes are serialized across the global and connection policies so a gain
// cannot be attributed to two simultaneous admission changes.
func (s *transferScheduler) otherScopeProbe(k schedulerScope) bool {
	for key, p := range s.learning {
		if key != k && p.restricted && p.policy.probeFrom > 0 {
			return true
		}
	}
	return false
}
