package main

import (
	"context"
	"io"
	"sync"
	"time"
)

const controlRequestTimeout = 30 * time.Second
const transferInactivityTimeout = 60 * time.Second

// Each HTTP attempt owns its watchdog. Cancelling the SDK's parent context here
// would prevent the SDK from retrying a stalled connection.
type networkAttempt struct {
	parent            context.Context
	ctx               context.Context
	cancel            context.CancelFunc
	mu                sync.Mutex
	timer             *time.Timer
	deadline          time.Time
	interval          time.Duration
	inactivity        bool
	finished, expired bool
}

type networkTimeoutError struct{}

func (networkTimeoutError) Error() string {
	return "Storage request timed out waiting for network progress"
}
func (networkTimeoutError) Timeout() bool   { return true }
func (networkTimeoutError) Temporary() bool { return true }

func newNetworkAttempt(parent context.Context, interval time.Duration, inactivity bool) *networkAttempt {
	ctx, cancel := context.WithCancel(parent)
	a := &networkAttempt{parent: parent, ctx: ctx, cancel: cancel, interval: interval, inactivity: inactivity, deadline: time.Now().Add(interval)}
	a.mu.Lock()
	a.timer = time.AfterFunc(interval, a.expire)
	a.mu.Unlock()
	return a
}
func (a *networkAttempt) expire() {
	a.mu.Lock()
	if a.finished || a.expired {
		a.mu.Unlock()
		return
	}
	// A callback already queued before progress reset the timer must honor the
	// new deadline. Reset and expiry are serialized with progress under mu.
	if remaining := time.Until(a.deadline); remaining > 0 {
		a.timer.Reset(remaining)
		a.mu.Unlock()
		return
	}
	a.expired = true
	a.mu.Unlock()
	a.cancel()
}
func (a *networkAttempt) progress(n int) {
	if n <= 0 || !a.inactivity {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished || a.expired {
		return
	}
	a.deadline = time.Now().Add(a.interval)
	a.timer.Reset(a.interval)
}
func (a *networkAttempt) err(err error) error {
	if parentErr := a.parent.Err(); parentErr != nil {
		return parentErr
	}
	a.mu.Lock()
	expired := a.expired
	a.mu.Unlock()
	if expired {
		return networkTimeoutError{}
	}
	return err
}
func (a *networkAttempt) stop() {
	a.mu.Lock()
	a.finished = true
	a.timer.Stop()
	a.mu.Unlock()
	a.cancel()
}

type networkProgressBody struct {
	io.ReadCloser
	attempt *networkAttempt
}

func (b *networkProgressBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.attempt.progress(n)
	return n, b.attempt.err(err)
}
