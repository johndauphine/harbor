package main

import "golang.org/x/sys/unix"

func sampleTuningMemory(r *tuningResources) {
	r.MemoryTotalBytes, _ = unix.SysctlUint64("hw.memsize")
	// Some kernels do not expose these VM counters through sysctl. Do not
	// substitute physical RAM for available RAM in that case.
	free, freeErr := unix.SysctlUint32("vm.page_free_count")
	inactive, inactiveErr := unix.SysctlUint32("vm.page_inactive_count")
	if freeErr == nil && inactiveErr == nil && r.MemoryTotalBytes > 0 {
		available := (uint64(free) + uint64(inactive)) * uint64(unix.Getpagesize())
		if available <= r.MemoryTotalBytes {
			r.MemoryKnown, r.MemoryAvailableBytes = true, available
		}
	}
}
