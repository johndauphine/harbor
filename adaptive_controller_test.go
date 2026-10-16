package main

import (
	"fmt"
	"testing"
	"time"
)

func controllerSample(at time.Time, bytes int64) transferSample {
	return transferSample{Started: at.Add(-time.Second), Finished: at, Bytes: bytes, Success: true}
}
func controllerWindow(c *adaptiveController, at *time.Time, bytes int64) {
	if c.start.IsZero() {
		*at = at.Add(time.Second)
		c.observe(controllerSample(*at, bytes), true)
	}
	count := 3 * c.limit
	for i := 0; i < count; i++ {
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

func TestAdaptiveControllerSlowDemandRetainsMeasurements(t *testing.T) {
	c := newAdaptiveController()
	c.limit = 4
	now := time.Unix(100, 0)
	c.observe(controllerSample(now, 1<<20), true)
	// A saturated slow connection can take longer than the idle interval to
	// finish each part. Those completions must still form learning windows.
	for i := 0; i < 24; i++ {
		started := now
		now = now.Add(35 * time.Second)
		c.observe(transferSample{Started: started, Finished: now, Bytes: 1 << 20, Success: true}, true)
	}
	if c.limit != 5 || c.probeFrom != 4 {
		t.Fatalf("sustained slow demand never probed recovery: %+v", c)
	}
	for i := 0; i < 16; i++ {
		started := now
		now = now.Add(35 * time.Second)
		c.observe(transferSample{Started: started, Finished: now, Bytes: 2 << 20, Success: true}, true)
	}
	if c.limit != 5 || c.probeFrom != 0 || c.baseline <= 0 {
		t.Fatalf("slow connection did not accept measured recovery gain: %+v", c)
	}
	// Even saturated demand must discard comparisons after a real no-work gap.
	c.probeFrom, c.limit = 5, 6
	started := now.Add(31 * time.Second)
	c.observe(transferSample{Started: started, Finished: started.Add(time.Second), Bytes: 1 << 20, Success: true}, true)
	if c.limit != 5 || c.probeFrom != 0 || c.previous != 0 {
		t.Fatalf("genuine idle gap retained old probe: %+v", c)
	}
}

// Parallel parts commonly finish in aligned cohorts. A partial old cohort must
// not reject a newly admitted stream before its useful bytes can be measured.
func TestAdaptiveControllerParallelCohortProbe(t *testing.T) {
	for _, base := range []int{4, 7} {
		for _, gain := range []bool{false, true} {
			for _, phase := range []time.Duration{-200 * time.Millisecond, -100 * time.Millisecond, 0, 100 * time.Millisecond, 200 * time.Millisecond} {
				name := fmt.Sprintf("%d_to_%d/gain_%t/phase_%s", base, base+1, gain, phase)
				t.Run(name, func(t *testing.T) {
					c := newAdaptiveController()
					c.limit = base
					origin := time.Unix(100, 0)
					service := 8 * time.Second
					// Two comparable baseline windows, with batches finishing close together.
					for batch := 0; batch < 10 && c.probeFrom == 0; batch++ {
						start := origin.Add(time.Duration(batch) * service)
						for worker := 0; worker < base; worker++ {
							finish := start.Add(service + time.Duration(worker)*20*time.Millisecond)
							c.observe(transferSample{Started: start, Finished: finish, Bytes: 16 << 20, Success: true}, true)
							if c.probeFrom > 0 {
								break
							}
						}
					}
					if c.probeFrom != base || c.limit != base+1 {
						t.Fatalf("baseline did not probe: %+v", c)
					}
					entered := c.eligibleAfter
					// Several old attempts, including old queued SDK operations, finish late.
					// Their bytes and their completion times cannot enter the probe window.
					for i := 0; i < base; i++ {
						c.observe(transferSample{Started: entered.Add(-time.Second), Finished: entered.Add(time.Duration(i+1) * time.Millisecond), Bytes: 64 << 20, Success: true}, true)
					}
					if c.probeFrom != base || !c.start.IsZero() || c.bytes != 0 {
						t.Fatal("old cohort contaminated probe", c)
					}
					duration := service
					if !gain {
						duration = service * time.Duration(base+1) / time.Duration(base)
					}
					for batch := 0; batch < 6 && c.probeFrom > 0; batch++ {
						start := entered.Add(time.Second + time.Duration(batch)*duration)
						jitter := phase
						if batch%2 == 0 {
							jitter = -phase
						}
						for worker := 0; worker < base+1; worker++ {
							finish := start.Add(duration + jitter + time.Duration(worker)*20*time.Millisecond)
							c.observe(transferSample{Started: start, Finished: finish, Bytes: 16 << 20, Success: true}, true)
							if c.probeFrom == 0 {
								break
							}
						}
					}
					want := base
					if gain {
						want++
					}
					if c.probeFrom != 0 || c.limit != want {
						t.Fatalf("cohort comparison gain=%t: limit=%d want%d probe=%d baseline%f", gain, c.limit, want, c.probeFrom, c.baseline)
					}
				})
			}
		}
	}
}

func TestAdaptiveControllerLateOldCompletionResetsFreshWindow(t *testing.T) {
	c := newAdaptiveController()
	entered := time.Unix(100, 0)
	c.limit = 5
	c.probeFrom = 4
	c.eligibleAfter = entered
	c.baseline = 8 << 20
	fresh := transferSample{Started: entered.Add(time.Second), Finished: entered.Add(9 * time.Second), Bytes: 16 << 20, Success: true}
	c.observe(fresh, true)
	fresh.Started = entered.Add(2 * time.Second)
	fresh.Finished = entered.Add(10 * time.Second)
	c.observe(fresh, true)
	if c.start.IsZero() || c.bytes == 0 || c.successes != 1 {
		t.Fatal("fresh measurement setup failed", c)
	}
	c.observe(transferSample{Started: entered.Add(-time.Second), Finished: entered.Add(11 * time.Second), Bytes: 64 << 20, Success: true}, true)
	if !c.start.IsZero() || c.bytes != 0 || c.successes != 0 || c.probeFrom != 4 || c.baseline != 8<<20 {
		t.Fatal("late old cohort did not discard comparison window", c)
	}
	fresh.Started = entered.Add(3 * time.Second)
	fresh.Finished = entered.Add(12 * time.Second)
	c.observe(fresh, true)
	if c.start != fresh.Finished || c.bytes != 0 || c.successes != 0 || c.probeFrom != 4 {
		t.Fatal("fresh operation did not rewarm after old cohort", c)
	}
}
