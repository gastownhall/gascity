package dashboardbff

import (
	"os"
	"strconv"
	"strings"
)

// readPlatformMetrics assembles the host and process metrics from procfs. Each
// source is read independently, so a single unreadable file leaves only its
// fields at zero rather than blanking the whole block.
func readPlatformMetrics() platformMetrics {
	l1, l5, l15 := readLoadAvg()
	total, free := readMemInfo()
	return platformMetrics{
		LoadAvg1:      l1,
		LoadAvg5:      l5,
		LoadAvg15:     l15,
		TotalMemBytes: total,
		FreeMemBytes:  free,
		UptimeSec:     readHostUptime(),
		RSSBytes:      readRSSBytes(),
	}
}

// readRSSBytes reads resident set size from /proc/self/statm (field 2, in
// pages) and converts to bytes. Returns 0 when procfs is unavailable.
func readRSSBytes() int64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * int64(os.Getpagesize())
}

// readLoadAvg reads the 1/5/15-minute load averages from /proc/loadavg.
// Missing values degrade to 0.
func readLoadAvg() (float64, float64, float64) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return 0, 0, 0
	}
	return parseFloat(fields[0]), parseFloat(fields[1]), parseFloat(fields[2])
}

// readMemInfo reads MemTotal and MemAvailable from /proc/meminfo and converts
// the kB values to bytes (×1024). MemAvailable maps to free_mem_bytes — it is
// the kernel's best estimate of allocatable memory, the closest analog to
// Node's os.freemem(). Missing values degrade to 0.
func readMemInfo() (total int64, free int64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = parseMemInfoKB(line) * 1024
		case strings.HasPrefix(line, "MemAvailable:"):
			free = parseMemInfoKB(line) * 1024
		}
	}
	return total, free
}

// parseMemInfoKB extracts the kB value from a /proc/meminfo line like
// "MemTotal:       16384000 kB". Returns 0 on any parse failure.
func parseMemInfoKB(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// readHostUptime reads system uptime (seconds, rounded) from /proc/uptime.
// Returns 0 when procfs is unavailable.
func readHostUptime() int64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}
	return int64(parseFloat(fields[0]) + 0.5)
}

// parseFloat parses a base-10 float, returning 0 on any parse failure.
func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}
