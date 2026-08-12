package dashboardbff

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The three Health-view samplers (supervisor-status, dolt-noms trend, per-rig
// store health) all derive from one slow read: the supervisor's
// GET /v0/city/{name}/status. That read turns slow on a bloated store and would
// trip an interactive timeout, so each city runs a background sampler that
// refreshes the snapshot off the request path; the endpoints serve the cached
// snapshot (with availability + freshness metadata) and never block on the
// probe. Samplers are started lazily on first request for a city (mirroring the
// BFF's lazy per-city runtime) so cities nobody views cost nothing.
const (
	statusSampleInterval = 60 * time.Second
	doltAppendInterval   = 10 * time.Minute
	rigProbeInterval     = 5 * time.Minute
	doltRingSlots        = 144 // 24h at 10-min cadence
	statusFetchTimeout   = 40 * time.Second
	tcpProbeTimeout      = 2 * time.Second

	doltSource = "status.store_health.size_bytes"
)

// ── Wire shapes (must match shared/src/*.ts exactly) ──────────────────────

type supervisorStatusReport struct {
	Available bool            `json:"available"`
	SampledAt string          `json:"sampledAt,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Status    json.RawMessage `json:"status"`
}

type doltSample struct {
	TS    string `json:"ts"`
	Bytes int64  `json:"bytes"`
}

type doltTrendReport struct {
	Available bool         `json:"available"`
	Samples   []doltSample `json:"samples"`
	Source    string       `json:"source,omitempty"`
	Reason    string       `json:"reason,omitempty"`
}

type rigStoreCheck struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Message  string `json:"message"`
}

type rigStoreHealth struct {
	Rig           string          `json:"rig"`
	BeadsPath     string          `json:"beadsPath"`
	Rollup        string          `json:"rollup"`
	Reachable     bool            `json:"reachable"`
	DoltEndpoint  *string         `json:"doltEndpoint"`
	DoltConnected *bool           `json:"doltConnected"`
	IssueCount    *int64          `json:"issueCount"`
	Problems      []rigStoreCheck `json:"problems"`
	Note          string          `json:"note,omitempty"`
}

type rigStoreHealthReport struct {
	Available bool             `json:"available"`
	SampledAt string           `json:"sampledAt,omitempty"`
	Reason    string           `json:"reason,omitempty"`
	Rigs      []rigStoreHealth `json:"rigs"`
}

// statusBodyParsed extracts only the fields the samplers need from the raw
// supervisor StatusBody.
type statusBodyParsed struct {
	StoreHealth *struct {
		SizeBytes *int64 `json:"size_bytes"`
	} `json:"store_health"`
	RigDetails []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"rig_details"`
}

// ── Sampler manager ───────────────────────────────────────────────────────

type samplerManager struct {
	deps  Deps
	exec  *execRunner
	httpc *http.Client

	mu      sync.Mutex
	cities  map[string]*citySampler
	ctx     context.Context
	wg      *sync.WaitGroup
	enabled bool
}

func newSamplerManager(deps Deps, exec *execRunner) *samplerManager {
	return &samplerManager{
		deps:   deps,
		exec:   exec,
		httpc:  &http.Client{Timeout: statusFetchTimeout, Transport: deps.SelfReadTransport},
		cities: make(map[string]*citySampler),
	}
}

// enable records the lifecycle context and waitgroup so lazily-started city
// samplers stop cleanly on shutdown.
func (m *samplerManager) enable(ctx context.Context, wg *sync.WaitGroup) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ctx = ctx
	m.wg = wg
	m.enabled = true
}

// ensure returns the sampler for a city, starting its background loop on first
// use when the manager has been enabled (Start called). Before Start, it
// returns a sampler with an empty (not-sampled-yet) snapshot. The city's
// on-disk path is not stored: rig paths come from the supervisor status body,
// and the sampler keys everything else off cs.name.
func (m *samplerManager) ensure(name string) *citySampler {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs, ok := m.cities[name]
	if !ok {
		cs = &citySampler{name: name, mgr: m}
		m.cities[name] = cs
	}
	if m.enabled && m.ctx != nil && !cs.started {
		cs.started = true
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			cs.loop(m.ctx)
		}()
	}
	return cs
}

// ── Per-city sampler ──────────────────────────────────────────────────────

type citySampler struct {
	name string
	mgr  *samplerManager

	started bool

	// beforeProbe, when set, runs once per rig-probe pass while no lock is held.
	// It exists only as a test seam to prove refresh() does its blocking probe
	// work off the lock; production never sets it.
	beforeProbe func()

	mu sync.RWMutex
	// status
	statusRaw    json.RawMessage
	statusAt     time.Time
	statusOK     bool
	statusReason string // SupervisorStatusUnavailableReason when !statusOK
	// dolt trend
	dolt           []doltSample
	lastDoltAppend time.Time
	doltOK         bool
	doltReason     string // DoltNomsUnavailableReason
	// rig store health
	rigs      []rigStoreHealth
	rigAt     time.Time
	rigOK     bool
	rigReason string // RigStoreHealthUnavailableReason
	lastRig   time.Time
}

func (cs *citySampler) loop(ctx context.Context) {
	cs.refresh(ctx)
	t := time.NewTicker(statusSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cs.refresh(ctx)
		}
	}
}

// refresh recomputes the cached snapshot off the request path. It is the
// module's hot loop, so it does ALL blocking/expensive work — the status read,
// the parse, and the per-rig bd-doctor + TCP probe fan-out — on local
// variables with NO lock held, then takes cs.mu.Lock() exactly once at the end
// to publish the computed fields (microseconds). The contract is that a reader
// (supervisorStatus/doltTrend/rigStoreHealth) never blocks behind a probe, so
// the write lock must never be held across exec/TCP/HTTP.
//
// refresh runs only from the single loop() goroutine per sampler, so reading
// the cadence gates and dolt ring under a brief RLock up front and writing the
// computed ring back at the end cannot race another refresh.
func (cs *citySampler) refresh(ctx context.Context) {
	// 1. Blocking status read — already lock-free.
	raw, err := cs.mgr.fetchStatus(ctx, cs.name)
	now := time.Now()

	if err != nil {
		// Degrade, don't blank: keep the last-good status, dolt samples, and rig
		// report; only flip the status availability + reason.
		cs.mu.Lock()
		cs.statusOK = false
		cs.statusReason = "status_read_failed"
		cs.mu.Unlock()
		return
	}

	// 2. Snapshot the cadence gates and the current dolt ring under a brief
	// RLock so the heavy work below sees a consistent starting point.
	cs.mu.RLock()
	lastDoltAppend := cs.lastDoltAppend
	lastRig := cs.lastRig
	prevDolt := cs.dolt
	cs.mu.RUnlock()

	parsed := parseStatusBody(raw)

	// 3. Compute the dolt ring (10-min cadence) into locals. doltChanged tracks
	// whether the gate fired so we only advance lastDoltAppend / publish a new
	// ring when it did.
	var (
		newDolt        []doltSample
		doltChanged    bool
		appendDoltRing bool
	)
	if lastDoltAppend.IsZero() || now.Sub(lastDoltAppend) >= doltAppendInterval {
		doltChanged = true
		if parsed.StoreHealth != nil && parsed.StoreHealth.SizeBytes != nil && *parsed.StoreHealth.SizeBytes >= 0 {
			appendDoltRing = true
			ring := make([]doltSample, len(prevDolt), len(prevDolt)+1)
			copy(ring, prevDolt)
			ring = append(ring, doltSample{TS: now.UTC().Format(time.RFC3339Nano), Bytes: *parsed.StoreHealth.SizeBytes})
			if len(ring) > doltRingSlots {
				ring = ring[len(ring)-doltRingSlots:]
			}
			newDolt = ring
		}
	}

	// 4. Probe the rigs (5-min cadence; heavy: one bd doctor + TCP dial per
	// rig) into locals. No lock is held across the fan-out.
	var (
		newRigs    []rigStoreHealth
		rigChanged bool
	)
	if lastRig.IsZero() || now.Sub(lastRig) >= rigProbeInterval {
		rigChanged = true
		if cs.beforeProbe != nil {
			cs.beforeProbe()
		}
		rigs := make([]rigStoreHealth, 0, len(parsed.RigDetails))
		for _, rd := range parsed.RigDetails {
			rigs = append(rigs, cs.mgr.probeRig(ctx, rd.Name, rd.Path))
		}
		newRigs = rigs
	}

	// 5. Publish: one short critical section, assignments only.
	cs.mu.Lock()
	cs.statusRaw = raw
	cs.statusAt = now
	cs.statusOK = true
	if doltChanged {
		if appendDoltRing {
			cs.dolt = newDolt
			cs.doltOK = true
			cs.lastDoltAppend = now
		} else {
			// store_health absent: report unavailable but keep the last-good ring
			// and do not advance lastDoltAppend, so the next tick retries.
			cs.doltOK = false
			cs.doltReason = "store_health_absent"
		}
	}
	if rigChanged {
		cs.rigs = newRigs
		cs.rigAt = now
		cs.rigOK = true
		cs.lastRig = now
	}
	cs.mu.Unlock()
}

func (cs *citySampler) supervisorStatus() supervisorStatusReport {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.statusOK && !cs.statusAt.IsZero() && cs.statusRaw != nil {
		return supervisorStatusReport{
			Available: true,
			SampledAt: cs.statusAt.UTC().Format(time.RFC3339Nano),
			Status:    cs.statusRaw,
		}
	}
	reason := cs.statusReason
	if reason == "" {
		reason = "not_sampled_yet"
	}
	status := json.RawMessage("null")
	if cs.statusRaw != nil {
		status = cs.statusRaw
	}
	return supervisorStatusReport{Available: false, Reason: reason, Status: status}
}

func (cs *citySampler) doltTrend() doltTrendReport {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	samples := make([]doltSample, len(cs.dolt))
	copy(samples, cs.dolt)
	if cs.doltOK {
		return doltTrendReport{Available: true, Samples: samples, Source: doltSource}
	}
	reason := cs.doltReason
	if reason == "" {
		reason = "store_health_absent"
	}
	return doltTrendReport{Available: false, Samples: samples, Reason: reason}
}

func (cs *citySampler) rigStoreHealth() rigStoreHealthReport {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	rigs := make([]rigStoreHealth, len(cs.rigs))
	copy(rigs, cs.rigs)
	if cs.rigOK && !cs.rigAt.IsZero() {
		return rigStoreHealthReport{Available: true, SampledAt: cs.rigAt.UTC().Format(time.RFC3339Nano), Rigs: rigs}
	}
	reason := cs.rigReason
	if reason == "" {
		reason = "not_sampled_yet"
	}
	return rigStoreHealthReport{Available: false, Reason: reason, Rigs: rigs}
}

// fetchStatus reads GET {base}/v0/city/{name}/status over loopback. An empty
// base, non-2xx, or transport error returns an error so the sampler records the
// degraded reason.
func (m *samplerManager) fetchStatus(ctx context.Context, name string) (json.RawMessage, error) {
	base := strings.TrimRight(m.deps.SupervisorBaseURL, "/")
	if base == "" {
		return nil, fmt.Errorf("dashboardbff: supervisor base URL not configured")
	}
	url := base + "/v0/city/" + name + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status read: HTTP %d", resp.StatusCode)
	}
	return json.RawMessage(body), nil
}

func parseStatusBody(raw json.RawMessage) statusBodyParsed {
	var p statusBodyParsed
	_ = json.Unmarshal(raw, &p)
	return p
}

// ── Per-rig store probe (ported from routes/rig-store-health.ts) ───────────

var benignDoctorCategories = map[string]bool{"Git Integration": true, "Integrations": true}

const doltConnectionCheck = "Dolt Connection"

func (m *samplerManager) probeRig(ctx context.Context, rigName, rigPath string) rigStoreHealth {
	beadsPath := filepath.Join(rigPath, ".beads")
	if !isDir(beadsPath) {
		return rigStoreHealth{
			Rig: rigName, BeadsPath: beadsPath, Rollup: "down", Reachable: false,
			Problems: []rigStoreCheck{}, Note: sanitizeTerminalOutput(".beads store not found on disk"),
		}
	}

	var doltEndpoint *string
	port := readDoltServerPort(beadsPath)
	if port > 0 {
		ep := "127.0.0.1:" + strconv.Itoa(port)
		doltEndpoint = &ep
	}

	var checks []rigStoreCheck
	var note string
	if res, err := m.exec.execBdDoctor(ctx, beadsPath); err != nil {
		note = "bd doctor probe failed: " + err.Error()
	} else if parsed, ok := parseDoctorChecks(res.stdout); ok {
		checks = parsed
	} else {
		note = "bd doctor returned no JSON (embedded mode or dolt server unreachable)"
	}

	var doltConnected *bool
	if port > 0 {
		ok := tcpProbe(port)
		doltConnected = &ok
	} else if checks != nil {
		doltConnected = doltConnectedFromChecks(checks)
	}

	problems := storeProblems(checks)
	issueCount := issueCountFromChecks(checks)
	rollup := rollupFor(true, doltConnected, problems, note != "")

	return rigStoreHealth{
		Rig: rigName, BeadsPath: beadsPath, Rollup: rollup, Reachable: true,
		DoltEndpoint: doltEndpoint, DoltConnected: doltConnected,
		// Note carries subprocess/error text (bd doctor failure reason); sanitize
		// it before it reaches the browser, per the "all subprocess output is
		// sanitized" contract.
		IssueCount: issueCount, Problems: problems, Note: sanitizeTerminalOutput(note),
	}
}

func parseDoctorChecks(stdout string) ([]rigStoreCheck, bool) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" || trimmed[0] != '{' {
		return nil, false
	}
	var parsed struct {
		Checks []struct {
			Category string `json:"category"`
			Name     string `json:"name"`
			Status   string `json:"status"`
			Message  string `json:"message"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, false
	}
	if parsed.Checks == nil {
		return nil, false
	}
	out := make([]rigStoreCheck, 0, len(parsed.Checks))
	for _, c := range parsed.Checks {
		cat := c.Category
		if cat == "" {
			cat = "unknown"
		}
		nm := c.Name
		if nm == "" {
			nm = "unknown"
		}
		// Category, Name, and Message all come from bd doctor's JSON output;
		// sanitize each before it reaches the browser, per the "all subprocess
		// output is sanitized" contract. Status is normalized to a fixed enum.
		out = append(out, rigStoreCheck{
			Category: sanitizeTerminalOutput(cat),
			Name:     sanitizeTerminalOutput(nm),
			Status:   normalizeDoctorStatus(c.Status),
			Message:  sanitizeTerminalOutput(c.Message),
		})
	}
	return out, true
}

func normalizeDoctorStatus(s string) string {
	switch strings.ToLower(s) {
	case "ok", "pass", "passed":
		return "ok"
	case "warning", "warn":
		return "warning"
	case "error", "fail", "failed", "critical":
		return "error"
	default:
		return "warning"
	}
}

func storeProblems(checks []rigStoreCheck) []rigStoreCheck {
	out := []rigStoreCheck{}
	for _, c := range checks {
		if c.Status != "ok" && !benignDoctorCategories[c.Category] {
			out = append(out, c)
		}
	}
	return out
}

var issueCountRE = regexp.MustCompile(`(\d[\d,]*)`)

func issueCountFromChecks(checks []rigStoreCheck) *int64 {
	for _, c := range checks {
		if strings.Contains(c.Name, "Issue Count") {
			m := issueCountRE.FindStringSubmatch(c.Message)
			if m == nil {
				return nil
			}
			n, err := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
			if err != nil {
				return nil
			}
			return &n
		}
	}
	return nil
}

func doltConnectedFromChecks(checks []rigStoreCheck) *bool {
	for _, c := range checks {
		if c.Name == doltConnectionCheck {
			ok := c.Status == "ok"
			return &ok
		}
	}
	return nil
}

func rollupFor(reachable bool, doltConnected *bool, problems []rigStoreCheck, incomplete bool) string {
	if !reachable {
		return "down"
	}
	if doltConnected != nil && !*doltConnected {
		return "down"
	}
	for _, p := range problems {
		if p.Status == "error" {
			return "down"
		}
	}
	for _, p := range problems {
		if p.Status == "warning" {
			return "warn"
		}
	}
	if incomplete {
		return "warn"
	}
	return "ok"
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func readDoltServerPort(beadsPath string) int {
	raw, err := os.ReadFile(filepath.Join(beadsPath, "dolt-server.port"))
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

func tcpProbe(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), tcpProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ── Routes ────────────────────────────────────────────────────────────────

func (p *Plane) registerSamplers() {
	p.mux.HandleFunc("GET /api/city/{cityName}/supervisor-status", func(w http.ResponseWriter, r *http.Request) {
		cs, ok := p.citySampler(r.PathValue("cityName"))
		if !ok {
			writeError(w, http.StatusNotFound, "unknown city")
			return
		}
		writeJSON(w, http.StatusOK, cs.supervisorStatus())
	})
	p.mux.HandleFunc("GET /api/city/{cityName}/dolt-noms/trend", func(w http.ResponseWriter, r *http.Request) {
		cs, ok := p.citySampler(r.PathValue("cityName"))
		if !ok {
			writeError(w, http.StatusNotFound, "unknown city")
			return
		}
		writeJSON(w, http.StatusOK, cs.doltTrend())
	})
	p.mux.HandleFunc("GET /api/city/{cityName}/rig-store-health", func(w http.ResponseWriter, r *http.Request) {
		cs, ok := p.citySampler(r.PathValue("cityName"))
		if !ok {
			writeError(w, http.StatusNotFound, "unknown city")
			return
		}
		writeJSON(w, http.StatusOK, cs.rigStoreHealth())
	})
}

// citySampler resolves the city to its sampler, returning false for an unknown
// city (so the handler can 404). Starting the background loop is lazy.
func (p *Plane) citySampler(name string) (*citySampler, bool) {
	if _, ok := p.resolveCityPath(name); !ok {
		return nil, false
	}
	return p.samplers.ensure(name), true
}
