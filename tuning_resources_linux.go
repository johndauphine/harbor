package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

func sampleTuningMemory(r *tuningResources) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer file.Close()
	var total, available uint64
	var totalKnown, availableKnown bool
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || fields[2] != "kB" {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || value > ^uint64(0)/1024 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, totalKnown = value*1024, true
		case "MemAvailable:":
			available, availableKnown = value*1024, true
		}
	}
	if scanner.Err() == nil && totalKnown && total > 0 {
		r.MemoryTotalBytes = total
		if availableKnown && available <= total {
			r.MemoryKnown, r.MemoryAvailableBytes = true, available
		}
	}
}
