package main

import (
	"golang.org/x/sys/windows"
	"time"
	"unsafe"
)

var tuningGlobalMemoryStatus = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

type tuningMemoryStatus struct {
	Length               uint32
	Load                 uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func samplePlatformTuningResources(cachePath string, r *tuningResources) (time.Duration, bool) {
	status := tuningMemoryStatus{}
	status.Length = uint32(unsafe.Sizeof(status))
	if tuningGlobalMemoryStatus.Find() == nil {
		ok, _, _ := tuningGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&status)))
		if ok != 0 && status.TotalPhys > 0 && status.AvailPhys <= status.TotalPhys {
			r.MemoryKnown, r.MemoryTotalBytes, r.MemoryAvailableBytes = true, status.TotalPhys, status.AvailPhys
		}
	}
	if cachePath != "" {
		if path, err := windows.UTF16PtrFromString(cachePath); err == nil {
			var total, free uint64
			if windows.GetDiskFreeSpaceEx(path, &r.DiskAvailableBytes, &total, &free) == nil {
				r.DiskKnown = true
			}
		}
	}
	process, err := windows.GetCurrentProcess()
	if err != nil {
		return 0, false
	}
	var creation, exit, kernel, user windows.Filetime
	if windows.GetProcessTimes(process, &creation, &exit, &kernel, &user) != nil {
		return 0, false
	}
	// FILETIME is a count of 100ns ticks; these values are CPU durations.
	ticks := (uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)) + (uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime))
	if ticks > uint64((1<<63-1)/100) {
		return 0, false
	}
	return time.Duration(ticks * 100), true
}
