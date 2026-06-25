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
		httpc:  &http.Client{Timeout: statusFetchTimeout},
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
// returns a sampler with an empty (not-sampled-yet) snapshot.
func (m *samplerManager) ensure(name, path string) *citySampler {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs, ok := m.cities[name]
	if !ok {
		cs = &citySampler{name: name, path: path, mgr: m}
		m.cities[name] = cs
	}
	cs.path = path
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
	path string
	mgr  *samplerManager

	started bool

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

func (cs *citySampler) refresh(ctx context.Context) {
	raw, err := cs.mgr.fetchStatus(ctx, cs.name)
	now := time.Now()
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if err != nil {
		cs.statusOK = false
		cs.statusReason = "status_read_failed"
		// Retain last good status, dolt samples, and rig report (degraded, not
		// blank) per the BFF contract.
		return
	}

	cs.statusRaw = raw
	cs.statusAt = now
	cs.statusOK = true

	parsed := parseStatusBody(raw)

	// Dolt size ring (10-min cadence).
	if cs.lastDoltAppend.IsZero() || now.Sub(cs.lastDoltAppend) >= doltAppendInterval {
		if parsed.StoreHealth != nil && parsed.StoreHealth.SizeBytes != nil && *parsed.StoreHealth.SizeBytes >= 0 {
			cs.dolt = append(cs.dolt, doltSample{TS: now.UTC().Format(time.RFC3339Nano), Bytes: *parsed.StoreHealth.SizeBytes})
			if len(cs.dolt) > doltRingSlots {
				cs.dolt = cs.dolt[len(cs.dolt)-doltRingSlots:]
			}
			cs.doltOK = true
			cs.lastDoltAppend = now
		} else {
			cs.doltOK = false
			cs.doltReason = "store_health_absent"
		}
	}

	// Rig probe (5-min cadence; heavy: one bd doctor per rig).
	if cs.lastRig.IsZero() || now.Sub(cs.lastRig) >= rigProbeInterval {
		rigs := make([]rigStoreHealth, 0, len(parsed.RigDetails))
		for _, rd := range parsed.RigDetails {
			rigs = append(rigs, cs.mgr.probeRig(ctx, rd.Name, rd.Path))
		}
		cs.rigs = rigs
		cs.rigAt = now
		cs.rigOK = true
		cs.lastRig = now
	}
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
			Problems: []rigStoreCheck{}, Note: ".beads store not found on disk",
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
		IssueCount: issueCount, Problems: problems, Note: note,
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
		out = append(out, rigStoreCheck{Category: cat, Name: nm, Status: normalizeDoctorStatus(c.Status), Message: c.Message})
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
	path, ok := p.resolveCityPath(name)
	if !ok {
		return nil, false
	}
	return p.samplers.ensure(name, path), true
}
