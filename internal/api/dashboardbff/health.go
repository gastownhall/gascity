package dashboardbff

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

// localToolsTTL is how long a probed LocalToolVersions snapshot is reused
// before the next request re-probes. Tool versions only change at deploy
// cadence, so a short TTL keeps GET /api/health/local-tools from forking three
// subprocesses on every poll.
const localToolsTTL = 45 * time.Second

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
		writeJSON(w, http.StatusOK, p.localToolVersions(r.Context()))
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

// localToolsCache memoizes one Plane's LocalToolVersions snapshot behind a
// mutex and a TTL. The mutex also collapses concurrent refreshes so a burst of
// GETs after expiry forks one set of probes, not one set per request.
type localToolsCache struct {
	mu      sync.Mutex
	val     localToolVersions
	expires time.Time
	primed  bool
}

// localToolVersions returns the memoized tool-version snapshot, re-probing only
// when the cached value is missing or older than localToolsTTL. Repeated GETs
// within the TTL reuse the snapshot instead of forking three subprocesses each.
// The cache lives on the Plane (one per process); its mutex collapses
// concurrent refreshes so a burst of GETs after expiry forks one set of probes.
func (p *Plane) localToolVersions(ctx context.Context) localToolVersions {
	c := p.localTools
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.primed && time.Now().Before(c.expires) {
		return c.val
	}
	c.val = p.probeLocalTools(ctx)
	c.expires = time.Now().Add(localToolsTTL)
	c.primed = true
	return c.val
}

// probeLocalTools probes the dolt, beads, and gc binaries concurrently through
// the shared exec runner (so each probe obeys the concurrency semaphore, clean
// environment, and timeout). Each result is either {status:available,version,
// source} or {status:unavailable,reason}; a probe never fabricates a version.
func (p *Plane) probeLocalTools(ctx context.Context) localToolVersions {
	var (
		dolt, beads, gc localToolVersion
		done            = make(chan struct{}, 3)
	)
	go func() { dolt = p.probeSemverTool(ctx, "dolt", "version"); done <- struct{}{} }()
	go func() { beads = p.probeSemverTool(ctx, "bd", "version"); done <- struct{}{} }()
	go func() { gc = p.probeGCVersion(ctx); done <- struct{}{} }()
	for i := 0; i < 3; i++ {
		<-done
	}
	return localToolVersions{Dolt: dolt, Beads: beads, GC: gc}
}

// probeSemverTool runs "<cmd> <sub>" and extracts a semver token from stdout.
// source is the resolved binary path. A LookPath miss, exec failure, non-zero
// exit, or unrecognizable version surfaces as unavailable with a reason —
// never a fabricated version (probeVersion in version-probe.ts).
func (p *Plane) probeSemverTool(ctx context.Context, cmd, sub string) localToolVersion {
	path, err := exec.LookPath(cmd)
	if err != nil {
		return unavailable(cmd + " not found on PATH")
	}
	stdout, code, err := p.runProbe(ctx, cmd, sub)
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
func (p *Plane) probeGCVersion(ctx context.Context) localToolVersion {
	path, err := exec.LookPath("gc")
	if err != nil {
		return unavailable("gc not found on PATH")
	}
	stdout, code, err := p.runProbe(ctx, "gc", "version", "--json")
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

// runProbe runs a short, shell-free version command through the shared exec
// runner so the probe obeys the maxConcurrent semaphore, the clean environment,
// and a bounded timeout. It returns stdout, the exit code, and a spawn/timeout
// error (a non-zero exit is reported in code, not as an error).
func (p *Plane) runProbe(ctx context.Context, cmd string, args ...string) (stdout string, code int, err error) {
	res, err := p.exec.run(ctx, cmd, args, versionProbeTimeout, maxBytes)
	if err != nil {
		return "", -1, err
	}
	return res.stdout, res.exitCode, nil
}

// unavailable builds an unavailable LocalToolVersion with the given reason. The
// reason forwards subprocess/error text, so it is sanitized before it reaches
// the browser, per the "all subprocess output is sanitized" contract.
func unavailable(reason string) localToolVersion {
	return localToolVersion{Status: "unavailable", Reason: sanitizeTerminalOutput(reason)}
}
