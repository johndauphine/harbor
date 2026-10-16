//go:build !darwin && !linux && !windows

package main

import "time"

func samplePlatformTuningResources(_ string, _ *tuningResources) (time.Duration, bool) {
	return 0, false
}
