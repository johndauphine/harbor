package main

import "time"

// adaptiveController is policy only: timestamps and demand are supplied by the
// caller, so evaluation never requires timers or background goroutines.
type adaptiveController struct {
	limit, resource       int
	reason                string
	start, last, cooldown time.Time
	eligibleAfter         time.Time
	bytes                 int64
	successes             int
	baseline, previous    float64
	stable                int
	probeFrom             int
}

// Begin at the measured fixed default. Adaptation protects that throughput
// unless resource pressure or provider feedback requires a reduction.
func newAdaptiveController() *adaptiveController {
	return &adaptiveController{limit: 8, resource: 8, reason: "Learning transfer conditions"}
}
func (c *adaptiveController) ceiling(n int) {
	n = max(1, min(8, n))
	if n != c.resource {
		c.resetLearning()
		c.resource = n
	}
	if c.limit > c.resource {
		c.limit = c.resource
		c.reason = "Limited by available resources"
	}
}
func (c *adaptiveController) resetWindow() { c.start = time.Time{}; c.bytes = 0; c.successes = 0 }
func (c *adaptiveController) resetLearning() {
	if c.probeFrom > 0 {
		c.limit = min(c.resource, c.probeFrom)
	}
	c.resetWindow()
	c.baseline = 0
	c.previous = 0
	c.stable = 0
	c.probeFrom = 0
	c.eligibleAfter = time.Time{}
}
func (c *adaptiveController) observe(s transferSample, demand bool) {
	if s.Cancelled || s.Finished.IsZero() || !s.Finished.After(s.Started) {
		return
	}
	now := s.Finished
	if !c.last.IsZero() && now.Before(c.last) {
		if !s.Throttled && !s.TransportError {
			return
		}
		now = c.last
	}
	// A long-running request is active work, even if no part has finished
	// recently. Only a gap before the next request started is idle.
	if !c.last.IsZero() && s.Started.Sub(c.last) >= 30*time.Second {
		c.resetLearning()
		c.reason = "Relearning after idle time"
	}
	c.last = now
	if s.Throttled {
		c.resetLearning()
		c.limit = max(1, c.limit/2)
		c.cooldown = now.Add(30 * time.Second)
		c.reason = "Storage throttling; reducing activity"
		return
	}
	if s.TransportError || !s.Success {
		c.resetLearning()
		c.cooldown = now.Add(15 * time.Second)
		c.reason = "Waiting for a stable connection"
		return
	}
	if !demand {
		c.resetLearning()
		c.reason = "Keeping capacity for current demand"
		return
	}
	if now.Before(c.cooldown) {
		c.resetWindow()
		return
	}
	// Samples may complete in bursts or have waited in the SDK queue. Old
	// operations cannot measure the new admission setting. A fresh operation
	// establishes a warm-up boundary; only later completions enter its window.
	if !c.eligibleAfter.IsZero() && s.Started.Before(c.eligibleAfter) {
		c.resetWindow()
		return
	}
	if c.start.IsZero() {
		c.start = now
		return // warm-up: subsequent completions share one wall-clock window
	}
	c.bytes += max(0, s.Bytes)
	c.successes++
	elapsed := now.Sub(c.start)
	if elapsed < 3*time.Second || c.successes < 3*c.limit || c.bytes == 0 {
		return
	}
	// Three cohorts scale the observation interval with actual completion
	// cadence and avoid judging a probe from a partial simultaneous batch.
	rate := float64(c.bytes) / elapsed.Seconds()
	c.resetWindow()
	c.start = now
	if c.probeFrom > 0 {
		if rate < c.baseline*1.08 {
			c.limit = min(c.resource, c.probeFrom)
			c.reason = "Additional requests did not improve speed"
			c.cooldown = now.Add(30 * time.Second)
		} else {
			c.baseline = rate
			c.reason = "Higher concurrency improved speed"
			c.cooldown = now.Add(3 * time.Second)
		}
		c.probeFrom = 0
		c.eligibleAfter = time.Time{}
		c.previous = 0
		c.stable = 0
		return
	}
	// Two comparable windows are required before a probe. Noisy conditions hold
	// the current limit instead of chasing a single fast observation.
	if c.previous > 0 && rate >= c.previous*.85 && rate <= c.previous*1.15 {
		c.stable++
	} else {
		c.stable = 0
	}
	c.previous = rate
	if c.stable < 1 {
		c.reason = "Measuring sustained transfer speed"
		return
	}
	c.baseline = rate
	c.stable = 0
	if c.limit < c.resource {
		c.probeFrom = c.limit
		c.limit++
		c.eligibleAfter = now
		c.resetWindow()
		c.reason = "Testing one additional request"
	} else {
		c.reason = "Within the current request ceiling"
	}
}
