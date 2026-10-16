//go:build darwin || linux

package main

import (
	"golang.org/x/sys/unix"
	"time"
)

func samplePlatformTuningResources(cachePath string, r *tuningResources) (time.Duration, bool) {
	sampleTuningMemory(r)
	if cachePath != "" {
		var fs unix.Statfs_t
		if unix.Statfs(cachePath, &fs) == nil && fs.Bsize > 0 {
			blockSize := uint64(fs.Bsize)
			if fs.Bavail <= ^uint64(0)/blockSize {
				r.DiskKnown, r.DiskAvailableBytes = true, fs.Bavail*blockSize
			}
		}
	}
	var usage unix.Rusage
	if unix.Getrusage(unix.RUSAGE_SELF, &usage) != nil {
		return 0, false
	}
	return time.Duration(usage.Utime.Sec+usage.Stime.Sec)*time.Second + time.Duration(usage.Utime.Usec+usage.Stime.Usec)*time.Microsecond, true
}
