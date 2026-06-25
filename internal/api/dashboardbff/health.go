package dashboardbff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// processStart is captured once at package init and used to derive the admin
// process uptime, the Go equivalent of Node's process.uptime().
var processStart = time.Now()

// adminHealth is the dashboard backend process state, matching the admin block
// of shared/src/dashboard-health.ts SystemHealth. node_version carries the Go
// runtime version (this backend is the Go port of the former Node BFF).
type adminHealth struct {
	Pid           int    `json:"pid"`
	UptimeSec     int64  `json:"uptime_sec"`
	RssBytes      int64  `json:"rss_bytes"`
	HeapUsedBytes int64  `json:"heap_used_bytes"`
	NodeVersion   string `json:"node_version"`
}

// hostHealth is the machine-level state, matching the host block of
// shared/src/dashboard-health.ts SystemHealth. Values are sourced from /proc;
// any unreadable metric degrades to 0 rather than failing the request.
type hostHealth struct {
	LoadAvg1      float64 `json:"load_avg_1"`
	LoadAvg5      float64 `json:"load_avg_5"`
	LoadAvg15     float64 `json:"load_avg_15"`
	TotalMemBytes int64   `json:"total_mem_bytes"`
	FreeMemBytes  int64   `json:"free_mem_bytes"`
	CPUCount      int     `json:"cpu_count"`
	UptimeSec     int64   `json:"uptime_sec"`
}

// systemHealth is the GET /api/health/system response, matching
// shared/src/dashboard-health.ts SystemHealth.
type systemHealth struct {
	Admin adminHealth `json:"admin"`
	Host  hostHealth  `json:"host"`
}

// localToolVersion is one probed tool's status, matching the union in
// shared/src/dashboard-health.ts LocalToolVersion. On success only
// {status,version,source} is emitted; on failure only {status,reason}. The
// unused arm's fields are omitted so the wire shape matches the TS union exactly.
type localToolVersion struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// localToolVersions is the GET /api/health/local-tools response, matching
// shared/src/dashboard-health.ts LocalToolVersions.
type localToolVersions struct {
	Dolt  localToolVersion `json:"dolt"`
	Beads localToolVersion `json:"beads"`
	GC    localToolVersion `json:"gc"`
}

// versionProbeTimeout bounds each local tool version probe.
const versionProbeTimeout = 5 * time.Second

// semverRE extracts a dotted semver token from version output (SEMVER_RE in
// version-probe.ts).
var semverRE = regexp.MustCompile(`(\d+\.\d+\.\d+)`)

// registerHealth wires GET /api/health/system and GET /api/health/local-tools
// onto the plane mux.
func (p *Plane) registerHealth() {
	p.mux.HandleFunc("GET /api/health/system", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, currentSystemHealth())
	})
	p.mux.HandleFunc("GET /api/health/local-tools", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, probeLocalTools(r.Context()))
	})
}

// currentSystemHealth assembles the admin and host health blocks. Host metrics
// come from /proc; an unreadable metric degrades to 0 so the endpoint never
// errors on a platform without procfs.
func currentSystemHealth() systemHealth {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	l1, l5, l15 := readLoadAvg()
	total, free := readMemInfo()

	return systemHealth{
		Admin: adminHealth{
			Pid:           os.Getpid(),
			UptimeSec:     int64(time.Since(processStart).Round(time.Second).Seconds()),
			RssBytes:      readRSSBytes(),
			HeapUsedBytes: int64(mem.HeapAlloc),
			NodeVersion:   runtime.Version(),
		},
		Host: hostHealth{
			LoadAvg1:      l1,
			LoadAvg5:      l5,
			LoadAvg15:     l15,
			TotalMemBytes: total,
			FreeMemBytes:  free,
			CPUCount:      runtime.NumCPU(),
			UptimeSec:     readHostUptime(),
		},
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

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// probeLocalTools probes the dolt, beads, and gc binaries concurrently. Each
// result is either {status:available,version,source} or {status:unavailable,
// reason}; a probe never fabricates a version.
func probeLocalTools(ctx context.Context) localToolVersions {
	var (
		dolt, beads, gc localToolVersion
		done            = make(chan struct{}, 3)
	)
	go func() { dolt = probeSemverTool(ctx, "dolt", "version"); done <- struct{}{} }()
	go func() { beads = probeSemverTool(ctx, "bd", "version"); done <- struct{}{} }()
	go func() { gc = probeGCVersion(ctx); done <- struct{}{} }()
	for i := 0; i < 3; i++ {
		<-done
	}
	return localToolVersions{Dolt: dolt, Beads: beads, GC: gc}
}

// probeSemverTool runs "<cmd> <sub>" and extracts a semver token from stdout.
// source is the resolved binary path. A LookPath miss, exec failure, non-zero
// exit, or unrecognizable version surfaces as unavailable with a reason —
// never a fabricated version (probeVersion in version-probe.ts).
func probeSemverTool(ctx context.Context, cmd, sub string) localToolVersion {
	path, err := exec.LookPath(cmd)
	if err != nil {
		return unavailable(cmd + " not found on PATH")
	}
	stdout, code, err := runProbe(ctx, cmd, sub)
	if err != nil {
		return unavailable(cmd + " " + sub + " probe failed: " + err.Error())
	}
	if code != 0 {
		return unavailable(cmd + " " + sub + " exited " + strconv.Itoa(code))
	}
	m := semverRE.FindStringSubmatch(stdout)
	if m == nil {
		return unavailable(cmd + " " + sub + " output had no recognizable version")
	}
	return localToolVersion{Status: "available", Version: m[1], Source: path}
}

// gcVersionJSON is the shape of `gc version --json` output we read from.
type gcVersionJSON struct {
	Version string `json:"version"`
}

// probeGCVersion runs `gc version --json` and reads the version field verbatim
// so a local `dev` build surfaces as "dev" rather than collapsing to "no
// recognizable version" (probeGcVersionJson in version-probe.ts).
func probeGCVersion(ctx context.Context) localToolVersion {
	path, err := exec.LookPath("gc")
	if err != nil {
		return unavailable("gc not found on PATH")
	}
	stdout, code, err := runProbe(ctx, "gc", "version", "--json")
	if err != nil {
		return unavailable("gc version probe failed: " + err.Error())
	}
	if code != 0 {
		return unavailable("gc version exited " + strconv.Itoa(code))
	}
	var parsed gcVersionJSON
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &parsed); err != nil || parsed.Version == "" {
		return unavailable("gc version --json output had no version field")
	}
	return localToolVersion{Status: "available", Version: parsed.Version, Source: path}
}

// runProbe runs a short, shell-free version command under the plane's clean
// environment, returning stdout, the exit code, and a spawn/timeout error.
func runProbe(ctx context.Context, cmd string, args ...string) (stdout string, code int, err error) {
	cctx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()
	c := exec.CommandContext(cctx, cmd, args...)
	c.Env = cleanEnv()
	var out bytes.Buffer
	c.Stdout = &out
	runErr := c.Run()
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return out.String(), ee.ExitCode(), nil
		}
		return out.String(), -1, runErr
	}
	return out.String(), 0, nil
}

// unavailable builds an unavailable LocalToolVersion with the given reason.
func unavailable(reason string) localToolVersion {
	return localToolVersion{Status: "unavailable", Reason: reason}
}
