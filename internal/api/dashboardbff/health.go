package dashboardbff

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
)

// processStart is captured once at package init and used to derive the admin
// process uptime, the Go equivalent of Node's process.uptime().
var processStart = time.Now()

// adminHealth is the dashboard backend process state, matching the admin block
// of shared/src/dashboard-health.ts SystemHealth. node_version carries the Go
// runtime version (this backend is the Go port of the former Node BFF).
type adminHealth struct {
	Pid           int                 `json:"pid"`
	UptimeSec     int64               `json:"uptime_sec"`
	RSS           healthMetric[int64] `json:"rss"`
	HeapUsedBytes int64               `json:"heap_used_bytes"`
	NodeVersion   string              `json:"node_version"`
}

// hostHealth is the machine-level state, matching the host block of
// shared/src/dashboard-health.ts SystemHealth.
type hostHealth struct {
	Load     healthMetric[hostLoadAverages] `json:"load"`
	Memory   healthMetric[hostMemory]       `json:"memory"`
	CPUCount int                            `json:"cpu_count"`
	Uptime   healthMetric[int64]            `json:"uptime"`
}

type hostLoadAverages struct {
	LoadAvg1  float64 `json:"load_avg_1"`
	LoadAvg5  float64 `json:"load_avg_5"`
	LoadAvg15 float64 `json:"load_avg_15"`
}

type hostMemory struct {
	TotalMemBytes int64 `json:"total_mem_bytes"`
	FreeMemBytes  int64 `json:"free_mem_bytes"`
}

type healthMetricStatus string

const (
	healthMetricAvailable   healthMetricStatus = "available"
	healthMetricUnavailable healthMetricStatus = "unavailable"
)

type healthMetricUnavailableReason string

const (
	healthMetricSampleFailed  healthMetricUnavailableReason = "sample_failed"
	healthMetricInvalidSample healthMetricUnavailableReason = "invalid_sample"
	healthMetricValueOverflow healthMetricUnavailableReason = "value_overflow"
)

// healthMetric is an independently sampled host measurement. Available values
// and unavailable reasons are mutually exclusive on the wire, so clients
// cannot mistake a sampler failure for a numeric zero.
type healthMetric[T any] struct {
	Status healthMetricStatus            `json:"status"`
	Value  *T                            `json:"value,omitempty"`
	Reason healthMetricUnavailableReason `json:"reason,omitempty"`
}

func availableHealthMetric[T any](value T) healthMetric[T] {
	return healthMetric[T]{Status: healthMetricAvailable, Value: &value}
}

func unavailableHealthMetric[T any](reason healthMetricUnavailableReason) healthMetric[T] {
	return healthMetric[T]{Status: healthMetricUnavailable, Reason: reason}
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
	p.mux.HandleFunc("GET /api/health/system", func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := p.healthSnapshot(r.Context())
		if err != nil {
			log.Printf("dashboard system-health sample failed: %v", err)
			writeError(w, http.StatusServiceUnavailable, "system health unavailable")
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	})
	p.mux.HandleFunc("GET /api/health/local-tools", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.localToolVersions(r.Context()))
	})
}

type healthSamplers struct {
	loadAverage   func() (*load.AvgStat, error)
	virtualMemory func(context.Context) (*mem.VirtualMemoryStat, error)
	hostUptime    func(context.Context) (uint64, error)
	processRSS    func(context.Context, int32) (uint64, error)
}

// currentSystemHealth assembles the admin and host health blocks using
// cross-platform OS samplers. Each fallible metric carries its own availability
// state so one failed sampler cannot hide independent successful measurements.
func currentSystemHealth(ctx context.Context) (systemHealth, error) {
	// On Windows, the first gopsutil Avg call starts a process-global sampler
	// under the supplied context. A request context would stop that singleton
	// permanently when the request ends, so always give it process lifetime.
	return currentSystemHealthWithSamplers(ctx, healthSamplers{
		loadAverage:   load.Avg,
		virtualMemory: mem.VirtualMemoryWithContext,
		hostUptime:    host.UptimeWithContext,
		processRSS: func(ctx context.Context, pid int32) (uint64, error) {
			self, err := process.NewProcessWithContext(ctx, pid)
			if err != nil {
				return 0, fmt.Errorf("opening dashboard process metrics: %w", err)
			}
			processMemory, err := self.MemoryInfoWithContext(ctx)
			if err != nil {
				return 0, fmt.Errorf("reading dashboard process memory: %w", err)
			}
			return processMemory.RSS, nil
		},
	})
}

func currentSystemHealthWithSamplers(
	ctx context.Context,
	samplers healthSamplers,
) (systemHealth, error) {
	var runtimeMem runtime.MemStats
	runtime.ReadMemStats(&runtimeMem)
	pid := os.Getpid()
	const maxProcessPID = 1<<31 - 1
	if pid < 0 || pid > maxProcessPID {
		return systemHealth{}, fmt.Errorf("opening dashboard process metrics: invalid PID %d", pid)
	}
	cpuCount := runtime.NumCPU()
	if cpuCount <= 0 {
		return systemHealth{}, fmt.Errorf("reading host CPU count: invalid value %d", cpuCount)
	}

	loadMetric := sampleLoadAverage(samplers.loadAverage)
	memoryMetric := sampleHostMemory(ctx, samplers.virtualMemory)
	uptimeMetric := samplePositiveInt64Metric(ctx, "host uptime", samplers.hostUptime)
	rssMetric := sampleProcessRSS(ctx, int32(pid), samplers.processRSS)

	return systemHealth{
		Admin: adminHealth{
			Pid:           pid,
			UptimeSec:     int64(time.Since(processStart).Round(time.Second).Seconds()),
			RSS:           rssMetric,
			HeapUsedBytes: int64(runtimeMem.HeapAlloc),
			NodeVersion:   runtime.Version(),
		},
		Host: hostHealth{
			Load:     loadMetric,
			Memory:   memoryMetric,
			CPUCount: cpuCount,
			Uptime:   uptimeMetric,
		},
	}, nil
}

func sampleLoadAverage(sample func() (*load.AvgStat, error)) healthMetric[hostLoadAverages] {
	avg, err := sample()
	if err != nil {
		return failedHealthMetric[hostLoadAverages]("host load average", healthMetricSampleFailed, err)
	}
	if avg == nil || !validLoadAverage(avg.Load1) || !validLoadAverage(avg.Load5) || !validLoadAverage(avg.Load15) {
		return failedHealthMetric[hostLoadAverages]("host load average", healthMetricInvalidSample, fmt.Errorf("invalid load average values"))
	}
	return availableHealthMetric(hostLoadAverages{
		LoadAvg1: avg.Load1, LoadAvg5: avg.Load5, LoadAvg15: avg.Load15,
	})
}

func sampleHostMemory(
	ctx context.Context,
	sample func(context.Context) (*mem.VirtualMemoryStat, error),
) healthMetric[hostMemory] {
	virtualMemory, err := sample(ctx)
	if err != nil {
		return failedHealthMetric[hostMemory]("host memory", healthMetricSampleFailed, err)
	}
	if virtualMemory == nil || virtualMemory.Total == 0 || virtualMemory.Available > virtualMemory.Total {
		return failedHealthMetric[hostMemory]("host memory", healthMetricInvalidSample, fmt.Errorf("invalid total/available values"))
	}
	if uint64OverflowsInt64(virtualMemory.Total) || uint64OverflowsInt64(virtualMemory.Available) {
		return failedHealthMetric[hostMemory]("host memory", healthMetricValueOverflow, fmt.Errorf("total/available value overflows int64"))
	}
	return availableHealthMetric(hostMemory{
		TotalMemBytes: int64(virtualMemory.Total),
		FreeMemBytes:  int64(virtualMemory.Available),
	})
}

func samplePositiveInt64Metric(
	ctx context.Context,
	name string,
	sample func(context.Context) (uint64, error),
) healthMetric[int64] {
	value, err := sample(ctx)
	if err != nil {
		return failedHealthMetric[int64](name, healthMetricSampleFailed, err)
	}
	return positiveInt64HealthMetric(name, value)
}

func sampleProcessRSS(
	ctx context.Context,
	pid int32,
	sample func(context.Context, int32) (uint64, error),
) healthMetric[int64] {
	value, err := sample(ctx, pid)
	if err != nil {
		return failedHealthMetric[int64]("dashboard process RSS", healthMetricSampleFailed, err)
	}
	return positiveInt64HealthMetric("dashboard process RSS", value)
}

func positiveInt64HealthMetric(name string, value uint64) healthMetric[int64] {
	if value == 0 {
		return failedHealthMetric[int64](name, healthMetricInvalidSample, fmt.Errorf("invalid zero value"))
	}
	if uint64OverflowsInt64(value) {
		return failedHealthMetric[int64](name, healthMetricValueOverflow, fmt.Errorf("value %d overflows int64", value))
	}
	return availableHealthMetric(int64(value))
}

func failedHealthMetric[T any](
	name string,
	reason healthMetricUnavailableReason,
	err error,
) healthMetric[T] {
	log.Printf("dashboard system-health metric %s unavailable (%s): %v", name, reason, err)
	return unavailableHealthMetric[T](reason)
}

func validLoadAverage(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func uint64OverflowsInt64(value uint64) bool {
	const maxInt64 = uint64(1<<63 - 1)
	return value > maxInt64
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
