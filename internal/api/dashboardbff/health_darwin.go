package dashboardbff

import (
	"encoding/binary"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// loadavgFscaleDefault is the classic BSD load-average fixed-point scale
// (LSCALE). The Darwin struct loadavg carries its own fscale, but this is the
// fallback when the sysctl payload is truncated to just the three averages.
const loadavgFscaleDefault = 2048

// readPlatformMetrics assembles the host and process metrics from Darwin sysctls
// and getrusage. Each source is read independently, so one failing syscall
// leaves only its fields at zero rather than blanking the whole block.
//
// Two values differ in meaning from their Linux counterparts and are documented
// at their readers: FreeMemBytes is strictly free pages (not an "available"
// estimate — macOS spends most RAM on caches), and RSSBytes is the process peak
// resident size (ru_maxrss), not the instantaneous RSS that /proc/self/statm
// reports.
func readPlatformMetrics() platformMetrics {
	l1, l5, l15 := readLoadAvg()
	return platformMetrics{
		LoadAvg1:      l1,
		LoadAvg5:      l5,
		LoadAvg15:     l15,
		TotalMemBytes: readTotalMem(),
		FreeMemBytes:  readFreeMem(),
		UptimeSec:     readHostUptime(),
		RSSBytes:      readRSSBytes(),
	}
}

// readLoadAvg reads the 1/5/15-minute load averages from the vm.loadavg sysctl,
// which returns a C `struct loadavg { fixpt_t ldavg[3]; long fscale; }`. The
// three fixed-point averages are divided by fscale to recover the float values.
// Missing or malformed data degrades to 0.
func readLoadAvg() (float64, float64, float64) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 12 {
		return 0, 0, 0
	}
	// fixpt_t is uint32; on 64-bit Darwin the `long fscale` sits after 4 bytes
	// of alignment padding, i.e. at offset 16. Fall back to LSCALE if the
	// payload is truncated or reports a zero scale.
	fscale := uint64(loadavgFscaleDefault)
	if len(raw) >= 24 {
		if s := binary.NativeEndian.Uint64(raw[16:24]); s != 0 {
			fscale = s
		}
	}
	scaled := func(off int) float64 {
		return float64(binary.NativeEndian.Uint32(raw[off:off+4])) / float64(fscale)
	}
	return scaled(0), scaled(4), scaled(8)
}

// readTotalMem reads total physical RAM in bytes from the hw.memsize sysctl.
// Returns 0 when the sysctl is unavailable.
func readTotalMem() int64 {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(v)
}

// readFreeMem estimates free memory as vm.page_free_count × page size. Unlike
// Linux's MemAvailable this counts only wholly free pages — macOS holds most
// RAM in reclaimable caches, so this reads far lower than "available" memory.
// Returns 0 when the sysctl is unavailable.
func readFreeMem() int64 {
	pages, err := unix.SysctlUint32("vm.page_free_count")
	if err != nil {
		return 0
	}
	return int64(pages) * int64(os.Getpagesize())
}

// readHostUptime derives system uptime (seconds) from now minus the
// kern.boottime sysctl timeval. Returns 0 when the sysctl is unavailable or the
// clock has not advanced past boot.
func readHostUptime() int64 {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil || tv == nil {
		return 0
	}
	up := time.Now().Unix() - tv.Sec
	if up < 0 {
		return 0
	}
	return up
}

// readRSSBytes reports the process peak resident set size in bytes via
// getrusage(RUSAGE_SELF).ru_maxrss, which Darwin already reports in bytes
// (Linux reports kilobytes). This is the high-water mark, not the instantaneous
// RSS the Linux /proc/self/statm reader returns. Returns 0 on syscall failure.
func readRSSBytes() int64 {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if ru.Maxrss < 0 {
		return 0
	}
	return ru.Maxrss
}
