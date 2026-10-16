package main

import (
	"runtime"
	"time"
)

// MemoryKnown means available memory was measured, not merely installed RAM.
// CPUFraction is process CPU time divided by elapsed time and logical CPUs.
type tuningResources struct {
	LogicalCPUs          int
	MemoryTotalBytes     uint64
	MemoryAvailableBytes uint64
	MemoryKnown          bool
	HeapBytes            uint64
	CPUFraction          float64
	CPUKnown             bool
	DiskAvailableBytes   uint64
	DiskKnown            bool
	SampledAt            time.Time
}

type tuningResourceReading struct {
	at       time.Time
	cpu      time.Duration
	cpuKnown bool
}

func sampleTuningResources(cachePath string, previous *tuningResourceReading) (tuningResources, *tuningResourceReading) {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	r := tuningResources{LogicalCPUs: runtime.NumCPU(), HeapBytes: memory.HeapAlloc, SampledAt: time.Now()}
	cpu, known := samplePlatformTuningResources(cachePath, &r)
	reading := &tuningResourceReading{at: r.SampledAt, cpu: cpu, cpuKnown: known}
	if previous != nil && known && previous.cpuKnown && cpu >= previous.cpu && r.LogicalCPUs > 0 {
		elapsed := r.SampledAt.Sub(previous.at)
		if elapsed > 0 {
			r.CPUKnown = true
			r.CPUFraction = float64(cpu-previous.cpu) / float64(elapsed) / float64(r.LogicalCPUs)
			if r.CPUFraction > 1 {
				r.CPUFraction = 1
			}
		}
	}
	return r, reading
}

// Resource measurements only reduce the request budget. Throughput observations,
// rather than installed memory or processor count, decide whether to grow it.
func tuningResourceCeiling(r tuningResources) (limit int, reason string) {
	limit, reason = 8, "within resource limits"
	capAt := func(n int, why string) {
		if n < limit {
			limit, reason = n, why
		}
	}
	const mib = uint64(1 << 20)
	if r.MemoryKnown {
		switch {
		case r.MemoryAvailableBytes < 128*mib:
			capAt(1, "available memory is critically low")
		case r.MemoryAvailableBytes < 512*mib:
			capAt(2, "available memory is low")
		case r.MemoryAvailableBytes < 1024*mib:
			capAt(4, "available memory is limited")
		}
		if r.MemoryTotalBytes > 0 && float64(r.MemoryAvailableBytes)/float64(r.MemoryTotalBytes) < .03 {
			capAt(2, "system memory pressure is high")
		}
	}
	if r.MemoryTotalBytes > 0 {
		fraction := float64(r.HeapBytes) / float64(r.MemoryTotalBytes)
		if fraction >= .75 {
			capAt(1, "process memory pressure is high")
		} else if fraction >= .5 {
			capAt(2, "process memory use is high")
		}
	}
	if r.CPUKnown {
		if r.CPUFraction >= .9 {
			capAt(2, "process CPU use is high")
		} else if r.CPUFraction >= .75 {
			capAt(4, "process CPU use is elevated")
		}
	}
	if r.DiskKnown {
		if r.DiskAvailableBytes < 128*mib {
			capAt(1, "cache disk space is critically low")
		} else if r.DiskAvailableBytes < 1024*mib {
			capAt(2, "cache disk space is low")
		}
	}
	return
}
