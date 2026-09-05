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

	"gopkg.in/yaml.v3"
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
// the parse, and the per-rig provider probe (plus direct-mode TCP probe)
// fan-out — on local
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

	// 4. Probe the rigs (5-min cadence; heavy: one provider ping per rig and a
	// TCP dial only for direct/server stores) into locals. No lock is held
	// across the fan-out.
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

var benignCheckCategories = map[string]bool{"Git Integration": true, "Integrations": true}

const doltConnectionCheck = "Dolt Connection"

// pingConnectivityCheck is the name of the single connectivity check
// synthesized from `bd ping`. It is the ping-mode counterpart of
// doltConnectionCheck, and doltConnectedFromChecks matches both so a
// server-backed store still reports its connectivity when the probe ran
// through ping.
const pingConnectivityCheck = "Database Connectivity"

// maxProbeErrorRunes caps subprocess error text folded into a check message or
// note, so a chatty provider cannot bloat the snapshot the dashboard polls.
const maxProbeErrorRunes = 512

func (m *samplerManager) probeRig(ctx context.Context, rigName, rigPath string) rigStoreHealth {
	beadsPath := filepath.Join(rigPath, ".beads")
	if !isDir(beadsPath) {
		return rigStoreHealth{
			Rig: rigName, BeadsPath: beadsPath, Rollup: "down", Reachable: false,
			Problems: []rigStoreCheck{}, Note: sanitizeTerminalOutput(".beads store not found on disk"),
		}
	}

	var doltEndpoint *string
	// A proxied-server store owns transport selection inside Beads. In
	// particular, proxied-local may leave a stale dolt-server.port artifact
	// behind; never infer proxy health by dialing that port. Direct/server
	// stores retain the legacy endpoint and TCP probe for parity.
	port := 0
	mode, modeSafe := readDoltMode(beadsPath)
	if modeSafe && mode != "proxied-server" {
		port = readDoltServerPort(beadsPath)
	}
	if port > 0 {
		ep := "127.0.0.1:" + strconv.Itoa(port)
		doltEndpoint = &ep
	}

	var checks []rigStoreCheck
	var note string
	if res, err := m.exec.execBdPing(ctx, beadsPath); err != nil {
		note = "bd ping probe failed: " + err.Error()
	} else if parsed, ok := parsePingCheck(res); ok {
		checks = parsed
	} else if res.exitCode != 0 {
		// ping failed before it could emit JSON — the store could not be
		// opened or the provider refused the connection, and the only
		// actionable detail is the plain-text error on stderr. Synthesize the
		// connectivity check ping would have emitted so the failure rolls up
		// as "down" with a listed problem instead of an empty "warn".
		msg := strings.TrimSpace(res.stderr)
		if msg == "" {
			msg = "database connectivity failed"
		}
		checks = []rigStoreCheck{{
			Category: "Beads",
			Name:     pingConnectivityCheck,
			Status:   "error",
			Message:  sanitizeTerminalOutput(truncateRunes(msg, maxProbeErrorRunes)),
		}}
	} else {
		note = "bd ping returned no valid JSON (store unreachable or provider refused the probe)"
		if stderr := strings.TrimSpace(res.stderr); stderr != "" {
			note += ": " + truncateRunes(stderr, maxProbeErrorRunes)
		}
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
		// Note carries subprocess/error text (bd ping failure reason); sanitize
		// it before it reaches the browser, per the "all subprocess output is
		// sanitized" contract.
		IssueCount: issueCount, Problems: problems, Note: sanitizeTerminalOutput(note),
	}
}

// parsePingCheck normalizes bd ping's provider-neutral JSON result into the
// existing dashboard check shape. ping is intentionally a single connectivity
// check: ping is available for embedded, direct-server, and
// proxied-server stores, so the dashboard does not need transport-specific
// branches. A non-zero exit is still represented as a check when ping emitted
// valid JSON, preserving the actionable provider error for the dashboard.
func parsePingCheck(res *execResult) ([]rigStoreCheck, bool) {
	var payload struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	trimmed := strings.TrimSpace(res.stdout)
	if trimmed == "" || trimmed[0] != '{' || json.Unmarshal([]byte(trimmed), &payload) != nil || payload.Status == "" {
		return nil, false
	}
	status := "error"
	if strings.EqualFold(payload.Status, "ok") && res.exitCode == 0 {
		status = "ok"
	}
	message := payload.Error
	if message == "" {
		if status == "ok" {
			message = "database connectivity ok"
		} else {
			message = "database connectivity failed"
		}
	}
	return []rigStoreCheck{{
		Category: "Beads",
		Name:     pingConnectivityCheck,
		Status:   status,
		Message:  sanitizeTerminalOutput(message),
	}}, true
}

func storeProblems(checks []rigStoreCheck) []rigStoreCheck {
	out := []rigStoreCheck{}
	for _, c := range checks {
		if c.Status != "ok" && !benignCheckCategories[c.Category] {
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
		if c.Name == doltConnectionCheck || c.Name == pingConnectivityCheck {
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

// readDoltMode returns the persisted Beads storage mode and whether it is safe
// to trust a direct-mode port artifact. Only the explicit server mode,
// or absent markers for legacy direct stores, permit a TCP probe. Malformed,
// unreadable, embedded, and unknown markers fail closed.
func readDoltMode(beadsPath string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(beadsPath, "metadata.json"))
	metadataPresent := err == nil
	if err == nil {
		var metadata struct {
			DoltMode string `json:"dolt_mode"`
		}
		if json.Unmarshal(raw, &metadata) == nil {
			if mode := strings.ToLower(strings.TrimSpace(metadata.DoltMode)); mode != "" {
				return recognizedDoltMode(mode)
			}
		} else {
			metadataPresent = true // malformed metadata is fail-closed
		}
	} else if !os.IsNotExist(err) {
		return "", false
	}
	// config.yaml is the canonical marker emitted by Gas City. Parse it rather
	// than scanning lines so malformed YAML without dolt.mode cannot be mistaken
	// for an unmarked legacy direct store.
	configRaw, configErr := os.ReadFile(filepath.Join(beadsPath, "config.yaml"))
	if configErr == nil {
		var config struct {
			DoltMode string `yaml:"dolt.mode"`
		}
		if err := yaml.Unmarshal(configRaw, &config); err != nil {
			return "", false
		}
		if mode := strings.ToLower(strings.TrimSpace(config.DoltMode)); mode != "" {
			return recognizedDoltMode(mode)
		}
	} else if !os.IsNotExist(configErr) {
		return "", false
	}
	if metadataPresent {
		// Metadata existed but omitted/failed to decode its mode. Do not infer a
		// direct endpoint from a potentially stale port artifact.
		return "", false
	}
	return "", true
}

func recognizedDoltMode(mode string) (string, bool) {
	switch mode {
	case "proxied-server", "server":
		return mode, true
	default:
		return "", false
	}
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
