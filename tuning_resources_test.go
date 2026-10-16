package main

import (
	"math"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestTuningResourceCeiling(t *testing.T) {
	const mib = uint64(1 << 20)
	tests := []struct {
		name      string
		resources tuningResources
		want      int
	}{
		{"unknown is not exhausted", tuningResources{}, 8},
		{"large machine does not raise maximum", tuningResources{LogicalCPUs: 128, MemoryKnown: true, MemoryTotalBytes: 256 << 30, MemoryAvailableBytes: 128 << 30}, 8},
		{"known zero memory", tuningResources{MemoryKnown: true}, 1},
		{"critical memory", tuningResources{MemoryKnown: true, MemoryAvailableBytes: 127 * mib}, 1},
		{"low memory", tuningResources{MemoryKnown: true, MemoryAvailableBytes: 511 * mib}, 2},
		{"limited memory", tuningResources{MemoryKnown: true, MemoryAvailableBytes: 1023 * mib}, 4},
		{"healthy memory", tuningResources{MemoryKnown: true, MemoryAvailableBytes: 1024 * mib}, 8},
		{"relative pressure", tuningResources{MemoryKnown: true, MemoryTotalBytes: 64 << 30, MemoryAvailableBytes: 1 << 30}, 2},
		{"heap high", tuningResources{MemoryTotalBytes: 2 << 30, HeapBytes: 1 << 30}, 2},
		{"heap critical", tuningResources{MemoryTotalBytes: 2 << 30, HeapBytes: 1536 * mib}, 1},
		{"CPU unknown ignored", tuningResources{CPUFraction: 1}, 8},
		{"CPU elevated", tuningResources{CPUKnown: true, CPUFraction: .75}, 4},
		{"CPU high", tuningResources{CPUKnown: true, CPUFraction: .9}, 2},
		{"disk unknown ignored", tuningResources{DiskAvailableBytes: 0}, 8},
		{"disk critical", tuningResources{DiskKnown: true, DiskAvailableBytes: 0}, 1},
		{"disk low", tuningResources{DiskKnown: true, DiskAvailableBytes: 512 * mib}, 2},
		{"most restrictive wins", tuningResources{MemoryKnown: true, MemoryAvailableBytes: 100 * mib, CPUKnown: true, CPUFraction: 1}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := tuningResourceCeiling(tt.resources)
			if got != tt.want || reason == "" {
				t.Fatalf("got (%d, %q), want limit %d and reason", got, reason, tt.want)
			}
		})
	}
}

func TestSampleTuningResources(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("native resource readings are not supported on this platform")
	}
	directory := t.TempDir()
	first, previous := sampleTuningResources(directory, nil)
	if first.LogicalCPUs < 1 || first.SampledAt.IsZero() || previous == nil {
		t.Fatal("missing runtime resource readings")
	}
	if first.CPUKnown {
		t.Fatal("first sample cannot have a CPU rate")
	}
	if !first.DiskKnown {
		t.Fatal("existing temporary directory must have a disk reading on supported platforms")
	}
	if first.MemoryKnown && first.MemoryAvailableBytes > first.MemoryTotalBytes {
		t.Fatal("available memory exceeds total")
	}
	second, _ := sampleTuningResources(directory, previous)
	if !second.CPUKnown {
		t.Fatal("second sample must have process CPU rate on supported platforms")
	}
	if second.CPUFraction < 0 || second.CPUFraction > 1 || math.IsNaN(second.CPUFraction) {
		t.Fatal("invalid CPU fraction")
	}
	missing, _ := sampleTuningResources(filepath.Join(directory, "does-not-exist"), previous)
	if missing.DiskKnown {
		t.Fatal("nonexistent path must not produce a disk reading")
	}
	unknown, _ := sampleTuningResources("", &tuningResourceReading{at: time.Now().Add(time.Hour), cpuKnown: true})
	if unknown.CPUKnown {
		t.Fatal("invalid elapsed interval must not produce CPU rate")
	}
}
