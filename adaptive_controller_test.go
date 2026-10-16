package main

import (
	"testing"
	"time"
)

func controllerSample(at time.Time, bytes int64) transferSample {
	return transferSample{Started: at.Add(-time.Second), Finished: at, Bytes: bytes, Success: true}
}
func controllerWindow(c *adaptiveController, at *time.Time, bytes int64) {
	for i := 0; i < 4; i++ {
		*at = at.Add(time.Second)
		c.observe(controllerSample(*at, bytes), true)
	}
}
func TestAdaptiveControllerProbeAndRollback(t *testing.T) {
	c := newAdaptiveController()
	c.limit = 4 // Establish a reduced baseline to exercise upward recovery.
	now := time.Unix(100, 0)
	c.observe(controllerSample(now, 1<<20), true)
	controllerWindow(c, &now, 1<<20)
	if c.limit != 4 {
		t.Fatal(c.limit)
	}
	controllerWindow(c, &now, 1<<20)
	if c.limit != 5 {
		t.Fatal("no conservative probe", c.limit)
	}
	controllerWindow(c, &now, 1<<20)
	if c.limit != 4 || c.cooldown.IsZero() {
		t.Fatal("plateau did not roll back", c)
	}
}
func TestAdaptiveControllerUsefulAggregateAndGain(t *testing.T) {
	c := newAdaptiveController()
	c.limit = 4 // Establish a reduced baseline to exercise upward recovery.
	now := time.Unix(100, 0)
	c.observe(controllerSample(now, 1<<20), true)
	controllerWindow(c, &now, 1<<20)
	controllerWindow(c, &now, 1<<20)
	controllerWindow(c, &now, 2<<20)
	if c.limit != 5 || c.probeFrom != 0 {
		t.Fatal("gain was not accepted", c)
	}
	if c.baseline != 2<<20 {
		t.Fatalf("aggregate wall-time rate %v", c.baseline)
	}
}
func TestAdaptiveControllerNoiseDemandAndFailures(t *testing.T) {
	c := newAdaptiveController()
	c.limit = 4 // Establish a reduced baseline to exercise upward recovery.
	now := time.Unix(100, 0)
	c.observe(controllerSample(now, 1), true)
	controllerWindow(c, &now, 1<<20)
	controllerWindow(c, &now, 2<<20)
	if c.limit != 4 {
		t.Fatal("noisy windows caused probe")
	}
	c.observe(controllerSample(now.Add(time.Second), 1), false)
	if c.previous != 0 || c.start != (time.Time{}) {
		t.Fatal("demand change retained comparison")
	}
	cancelled := controllerSample(now.Add(2*time.Second), 0)
	cancelled.Cancelled = true
	cancelled.Throttled = true
	c.observe(cancelled, true)
	if c.limit != 4 {
		t.Fatal("cancel counted as congestion")
	}
	throttle := controllerSample(now.Add(3*time.Second), 0)
	throttle.Success = false
	throttle.Throttled = true
	c.observe(throttle, true)
	if c.limit != 2 {
		t.Fatal("throttle not reduced")
	}
	retry := controllerSample(now.Add(4*time.Second), 0)
	retry.Success = false
	retry.TransportError = true
	c.observe(retry, true)
	if c.limit != 2 || c.cooldown.Before(retry.Finished) {
		t.Fatal("retry did not suspend probing")
	}
	c.ceiling(1)
	if c.limit != 1 {
		t.Fatal("resource ceiling ignored")
	}
}
func TestAdaptiveControllerIdleAndClockOrder(t *testing.T) {
	c := newAdaptiveController()
	c.limit = 4 // Establish a reduced baseline to exercise upward recovery.
	now := time.Unix(100, 0)
	c.observe(controllerSample(now, 1), true)
	controllerWindow(c, &now, 1<<20)
	controllerWindow(c, &now, 1<<20)
	if c.probeFrom == 0 {
		t.Fatal("setup")
	}
	c.observe(controllerSample(now.Add(31*time.Second), 1), true)
	if c.limit != 4 || c.probeFrom != 0 || c.previous != 0 {
		t.Fatal("idle probe comparison retained")
	}
	last := c.last
	c.observe(controllerSample(now, 1), true)
	if c.last != last {
		t.Fatal("out of order sample changed clock")
	}
}

func TestAdaptiveControllerThrottleDuringProbe(t *testing.T) {
	c := newAdaptiveController()
	c.limit = 5
	c.probeFrom = 4
	s := controllerSample(time.Unix(100, 0), 0)
	s.Throttled = true
	s.Success = false
	c.observe(s, true)
	if c.limit != 2 || c.probeFrom != 0 {
		t.Fatal("probe rollback undid throttle reduction", c)
	}
	// Concurrent callbacks can arrive out of finish order. Safety feedback must
	// still take effect, while an old success cannot distort a later window.
	s.Finished = s.Finished.Add(-time.Second)
	s.Started = s.Started.Add(-time.Second)
	c.observe(s, true)
	if c.limit != 1 {
		t.Fatal("late throttle was discarded")
	}
}

func TestAdaptiveControllerStartsAtProvenDefault(t *testing.T) {
	c := newAdaptiveController()
	if c.limit != 8 || c.resource != 8 {
		t.Fatal("cold start regresses proven concurrency", c)
	}
	now := time.Unix(100, 0)
	c.observe(controllerSample(now, 1), true)
	controllerWindow(c, &now, 1<<20)
	controllerWindow(c, &now, 1<<20)
	if c.limit != 8 || c.probeFrom != 0 {
		t.Fatal("healthy default changed unnecessarily", c)
	}
}
func TestAdaptiveControllerResourceReliefRecoversCautiously(t *testing.T) {
	c := newAdaptiveController()
	c.ceiling(2)
	if c.limit != 2 {
		t.Fatal("pressure did not clamp", c)
	}
	c.ceiling(8)
	if c.limit != 2 {
		t.Fatal("resource relief jumped without measuring", c)
	}
	now := time.Unix(100, 0)
	c.observe(controllerSample(now, 1), true)
	controllerWindow(c, &now, 2<<20)
	controllerWindow(c, &now, 2<<20)
	if c.limit != 3 || c.probeFrom != 2 {
		t.Fatal("no gradual recovery probe", c)
	}
	controllerWindow(c, &now, 4<<20)
	if c.limit != 3 || c.probeFrom != 0 {
		t.Fatal("recovery gain not retained", c)
	}
	c.limit = 5
	c.probeFrom = 4
	c.ceiling(2)
	c.resetLearning()
	if c.limit != 2 || c.probeFrom != 0 {
		t.Fatal("probe rollback exceeded resource ceiling", c)
	}
}
