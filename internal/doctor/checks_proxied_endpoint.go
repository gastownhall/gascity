package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// BeadsStorePayload is the machine-readable half of the `beads-store` result.
//
// The message stays the operator's line. This is for the readers that have to
// branch on the answer — the acceptance matrix, the perf gate, an operator's
// jq — and that would otherwise be parsing prose. The one thing it must never
// become is a second, disagreeing account of the same run: every field here is
// projected from the same diagnostic and the same endpoint read the message is
// built from.
type BeadsStorePayload struct {
	// Store is the store gc actually opened, e.g. "BdStore" or
	// "NativeDoltStore".
	Store string `json:"store"`
	// PreflightGate and PreflightReason name why, when the store is not the
	// native one.
	PreflightGate   string `json:"preflight_gate,omitempty"`
	PreflightReason string `json:"preflight_reason,omitempty"`
	// Proxied is the STORE OPEN's own account of the proxied-native lane: the
	// generation it pinned, the evidence it pinned it on, the idle policy and the
	// cursors it gated against, the verdict if it refused, and whether the handle
	// has since dropped to the bd leaf.
	//
	// It sits BESIDE Endpoint rather than replacing it, and the two are gathered
	// independently: this one is what gc decided AT OPEN, Endpoint is what the
	// endpoint looks like NOW. An operator debugging a proxied city needs both,
	// because "the proxy changed under us" and "gc read it wrong" produce the same
	// single-account picture and different two-account ones.
	//
	// It is the beads type rather than a projection of it, so the two can never
	// drift into two vocabularies for one fact. Nil on every lane but this one,
	// and `omitempty`, so a flag-off proxied scope serializes byte-identically to
	// what it serializes today.
	Proxied *beads.ProxiedDiagnostic `json:"proxied,omitempty"`
	// Endpoint is present only for a bd-owned proxied scope: it is what gc
	// could establish about bd's proxy without starting, stopping or writing
	// anything.
	Endpoint *ProxiedEndpointPayload `json:"endpoint,omitempty"`
}

// ProxiedEndpointPayload is gc's read-only account of one bd proxy.
type ProxiedEndpointPayload struct {
	// Root is the proxy root gc resolved with bd's own precedence.
	Root string `json:"root"`
	// Host and Port are where bd published the proxy's data listener. Both are
	// zero when there is no valid record.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// PID and ControlPort come from the record as written.
	PID         int `json:"pid,omitempty"`
	ControlPort int `json:"control_port,omitempty"`
	// Generation identifies the proxy process without printing its birth
	// token, which embeds the host's boot id.
	Generation string `json:"generation,omitempty"`
	// Verdict is the typed classification: live, dead, foreign_process,
	// birth_mismatch, no_record, undetermined and the rest.
	Verdict string `json:"verdict"`
	// Evidence is how strong the liveness proof was: "argv+birth", "argv", or
	// "none". A live verdict on "argv" alone is correct and weaker.
	Evidence string `json:"evidence"`
	// BirthEvidence separates "the token matched" from "this host cannot
	// compute one", which the Evidence field alone cannot express.
	BirthEvidence string `json:"birth_evidence"`
	// IdlePolicy is "never", "finite" or "unknown"; IdleTimeout is the window
	// for a finite policy; IdlePolicySource is which evidence decided.
	IdlePolicy       string `json:"idle_policy"`
	IdleTimeout      string `json:"idle_timeout,omitempty"`
	IdlePolicySource string `json:"idle_policy_source"`
	// Probe is the data port's observed state: served, refused,
	// accepted_no_greeting, unknown, or absent when no probe was attempted.
	Probe string `json:"probe,omitempty"`
	// Cursors are the database's two schema-migration cursors, read over the
	// probe session. Absent unless the probe was served.
	Cursors *proxyendpoint.Cursors `json:"cursors,omitempty"`
	// ExpectedCursors are the linked library's. They are reported next to the
	// observed pair because the comparison, not either number, is what decides
	// whether a native open would be safe.
	ExpectedCursors proxyendpoint.Cursors `json:"expected_cursors"`
	// Detail is the failure behind a non-live verdict or an unserved probe.
	Detail string `json:"detail,omitempty"`
}

// ProxiedEndpointIdleDetail is the endpoint detail for a finite-idle scope
// whose proxy is not running: it retired on its idle timeout, and the next bd
// command restarts it.
const ProxiedEndpointIdleDetail = "idle: the proxy retired after its idle timeout; the next bd command restarts it"

// beadsStoreDiagnostic is the subset of a store-open diagnostic the payload
// projects. It keeps this file from depending on the whole beads open result.
type beadsStoreDiagnostic struct {
	Store           string
	PreflightGate   string
	PreflightReason string
	// Proxied is the proxied-native lane's account, nil on every other lane.
	Proxied *beads.ProxiedDiagnostic
}

// proxiedEndpointProcessTable and probeProxiedEndpoint are the two pieces of
// the endpoint account that touch something outside this process. Both are
// injectable so the payload's shape can be driven from a fabricated proxy: a
// test that had to start a real bd proxy to assert a JSON field would be a test
// nobody runs.
var (
	proxiedEndpointProcessTable = proxyendpoint.DefaultProcessTable
	probeProxiedEndpoint        = proxyendpoint.ProbeEndpoint
)

// newBeadsStorePayload builds the structured half of a `beads-store` result.
//
// For a scope that is not a bd-owned proxied one it is just the store selection,
// which is what every other topology has to say. For a proxied scope it adds
// the endpoint account below.
func newBeadsStorePayload(scopeRoot string, target contract.DoltConnectionTarget, diag beadsStoreDiagnostic) *BeadsStorePayload {
	payload := &BeadsStorePayload{
		Store:           diag.Store,
		PreflightGate:   diag.PreflightGate,
		PreflightReason: diag.PreflightReason,
		Proxied:         diag.Proxied,
	}
	if targetIsProviderOwnedProxied(target) || scopeBindingIsProviderOwnedProxied(scopeRoot) {
		payload.Endpoint = inspectProxiedEndpoint(scopeRoot, target.Database)
	}
	return payload
}

// proxiedNativeStoreMessage is the operator's line for a scope gc is serving
// natively over bd's proxy.
//
// It names the GENERATION rather than the port, because a port is not an
// identity: bd allocates a fresh one on most respawns, and a sidecar that pins
// --proxied-server-port hands the same one to a different process over a
// different Dolt child. The generation is what tells an operator whether the
// handle they are looking at is the one they were looking at a minute ago.
//
// It also says where writes go, unprompted. "native" on a line about a bead
// store reads as "gc talks to Dolt", and the whole safety argument of this lane
// is that it does not — every mutation is still bd's.
func proxiedNativeStoreMessage(proxied *beads.ProxiedDiagnostic) string {
	generation := "unknown"
	if proxied != nil && proxied.Endpoint.Generation != "" {
		generation = proxied.Endpoint.Generation
	}
	return fmt.Sprintf("native reads over bd proxy (gen %s, writes via bd CLI)", generation)
}

// rigProxiedStoreMessage is the rig lane's line for a bd-owned proxied scope.
//
// Rigs have no retained store-open diagnostic — NewRigBeadsCheck takes a factory
// returning a bare beads.Store, and unlike the city's (api_state.go's
// CityBeadsDiagnostic) a rig's is discarded after the open. So the lane is read
// off the store gc is actually holding, which is the one piece of evidence this
// check has — through the unwrap seam, so a policy- or cache-wrapped rig store
// still answers instead of reading as the bd front door by default.
func rigProxiedStoreMessage(store beads.Store) string {
	if proxied, ok := beads.ProxiedStoreFrom(store); ok {
		report := proxied.Report()
		if !report.Demoted {
			return proxiedNativeStoreMessage(&beads.ProxiedDiagnostic{
				Endpoint: report.Endpoint,
				Evidence: report.Evidence,
			})
		}
		if verdict := proxied.Verdict(); verdict != nil {
			return proxiedFallbackStoreMessage(&beads.ProxiedDiagnostic{Verdict: verdict.Verdict})
		}
	}
	return proxiedProviderStoreMessage
}

// proxiedDemotedStoreMessage is the line for a handle that WAS serving natively
// and has since stood down.
//
// It is a distinct message from the healthy fallback because the two are
// different facts: a fallback never opened the lane, and a demotion opened it and
// lost it. An operator debugging "why is this city forking again" needs to know
// which, and the verdict names the cause. The status stays OK — the bd front door
// is a supported store for a proxied scope, and the lane is a performance
// property, not an availability one.
func proxiedDemotedStoreMessage(proxied *beads.ProxiedDiagnostic) string {
	verdict := beads.ProxiedVerdictNone
	generation := "unknown"
	if proxied != nil {
		verdict = proxied.Verdict
		if proxied.Endpoint.Generation != "" {
			generation = proxied.Endpoint.Generation
		}
	}
	if verdict == beads.ProxiedVerdictNone {
		return fmt.Sprintf("native reads over bd proxy stood down (gen %s); reads and writes via bd CLI", generation)
	}
	return fmt.Sprintf("native reads over bd proxy stood down (gen %s, verdict=%s); reads and writes via bd CLI",
		generation, verdict)
}

// proxiedFallbackStoreMessage is the healthy-fallback line: the message a
// proxied scope has today, plus the verdict when the proxied-native lane
// produced one.
//
// The base message is UNCHANGED on purpose. A fallback to the bd front door is
// the designed outcome for a proxied scope, not a degradation, and doctor's
// matcher plus the topology matrix both key on it. The verdict is appended
// rather than substituted so an operator learns WHY the lane declined without
// anything that reads the message losing its anchor.
func proxiedFallbackStoreMessage(proxied *beads.ProxiedDiagnostic) string {
	if proxied == nil || proxied.Verdict == beads.ProxiedVerdictNone {
		return proxiedProviderStoreMessage
	}
	message := fmt.Sprintf("%s; verdict=%s", proxiedProviderStoreMessage, proxied.Verdict)
	if proxied.Verdict == beads.ProxiedVerdictSchemaSkew && proxied.Cursors != (proxyendpoint.Cursors{}) {
		// The one verdict whose qualifier an operator cannot infer: which lane
		// drifted and in which direction is the difference between "the database
		// would be migrated on open" and "this binary would issue old-shape SQL".
		message += fmt.Sprintf(" (database %s, this binary expects %s)", proxied.Cursors, expectedCursors())
	}
	return message
}

// inspectProxiedEndpoint reads and classifies a proxied scope's endpoint, and
// probes its data port when — and only when — the record proves a live proxy.
//
// The order is what keeps this read-only and cheap. Reading the record and
// checking the process table costs two small files and no connection at all; a
// probe costs bd's Dolt child one session, so it is spent only on an endpoint
// gc has already established is bd's live proxy for this root. Dialing a port
// named by a dead, foreign or unparseable record would be talking to whatever
// happens to be listening there.
//
// Nothing here starts, stops, adopts or writes anything. The cursors are read
// with two read-only point queries; the library store is never opened, which is
// the whole reason this can run against a proxied scope at all — opening it
// would run initSchema against a database bd owns.
func inspectProxiedEndpoint(scopeRoot, database string) *ProxiedEndpointPayload {
	root, err := proxyendpoint.ProviderRoot(scopeRoot)
	if err != nil {
		return &ProxiedEndpointPayload{
			Verdict:          proxyendpoint.VerdictUndetermined.String(),
			Evidence:         proxyendpoint.EvidenceNone.String(),
			BirthEvidence:    proxyendpoint.BirthUnchecked.String(),
			IdlePolicy:       proxyendpoint.IdleUnknown.String(),
			IdlePolicySource: proxyendpoint.IdleSourceNone.String(),
			ExpectedCursors:  expectedCursors(),
			Detail:           fmt.Sprintf("resolve proxy root: %v", err),
		}
	}

	ep := proxyendpoint.Inspect(root, proxiedEndpointProcessTable())
	sidecar, sidecarErr := proxyendpoint.ReadSidecar(scopeBeadsDir(scopeRoot))
	idle := proxyendpoint.ResolveIdlePolicy(sidecar, ep.Liveness.IdlePolicy)

	payload := &ProxiedEndpointPayload{
		Root:             root,
		Port:             ep.Record.Port,
		PID:              ep.Record.PID,
		ControlPort:      ep.Record.ControlPort,
		Verdict:          ep.Verdict.String(),
		Evidence:         ep.Liveness.Evidence.String(),
		BirthEvidence:    ep.Liveness.Birth.String(),
		IdlePolicy:       idle.Kind.String(),
		IdlePolicySource: idle.Source.String(),
		ExpectedCursors:  expectedCursors(),
	}
	if idle.Kind == proxyendpoint.IdleFinite {
		payload.IdleTimeout = idle.Timeout.String()
	}
	if ep.Record.Port > 0 {
		payload.Host = proxyendpoint.Host
	}
	if ep.Record.Birth != "" {
		payload.Generation = proxyendpoint.NewPoolKey(ep.Record, database).Generation()
	}
	switch {
	case ep.Verdict == proxyendpoint.VerdictNoRecord && sidecarErr == nil && idle.Kind == proxyendpoint.IdleFinite:
		// A finite scope with no record is idle, not a gap: bd removed the
		// record when the proxy retired on its idle timeout.
		payload.Detail = ProxiedEndpointIdleDetail
	case ep.Err != nil:
		payload.Detail = ep.Err.Error()
	case sidecarErr != nil:
		payload.Detail = sidecarErr.Error()
	}
	if !ep.Verdict.Live() {
		return payload
	}

	probe := probeProxiedEndpoint(context.Background(), ep, database)
	payload.Probe = probe.Outcome.String()
	if probe.Outcome == proxyendpoint.ProbeServed {
		cursors := probe.Cursors
		payload.Cursors = &cursors
		return payload
	}
	if probe.Err != nil {
		payload.Detail = probe.Err.Error()
	}
	return payload
}

// expectedCursors is the linked library's schema pair.
func expectedCursors() proxyendpoint.Cursors {
	main, ignored := beads.PinnedSchemaCursors()
	return proxyendpoint.Cursors{Main: main, Ignored: ignored}
}

// scopeBeadsDir is a scope's .beads directory, normalized the way every other
// scope-file read in this package normalizes it.
func scopeBeadsDir(scopeRoot string) string {
	return filepath.Join(pathutil.NormalizePathForCompare(scopeRoot), ".beads")
}

// ProxiedIdleTimeoutCheck compares, for each gc-owned proxied scope, the idle
// timeout gc is configured to give it ([beads] proxied_idle_timeout, the rig's
// beads_proxied_idle_timeout, or GC_BEADS_PROXIED_IDLE_TIMEOUT) with what bd
// persisted in the scope's sidecar, and with what the running proxy was
// started with.
//
// Drift is advisory, never a failure: the scope works either way. bd offers no
// verb that changes an initialized scope's idle timeout, and gc must not edit
// the sidecar, which is bd's file — so the configured value applies to scopes
// gc creates (gc init, gc rig add, gc beads city migrate-proxied), and an older
// scope keeps the value it was created with until bd can change it.
type ProxiedIdleTimeoutCheck struct {
	cityPath   string
	cfg        *config.City
	scopeRoots []string
}

// NewProxiedIdleTimeoutCheckForConfig returns the check for a city with at
// least one gc-owned proxied scope, and nil for a city with none.
//
// "gc-owned" is the ownership journal, not the proxied binding. A proxied
// workspace gc merely found — an operator's, a clone's — is nobody's to hold an
// opinion about here, and a doctor line telling an operator their own scope is
// misconfigured would be gc asserting a preference as a defect.
func NewProxiedIdleTimeoutCheckForConfig(cityPath string, cfg *config.City, cfgErr error) *ProxiedIdleTimeoutCheck {
	var roots []string
	for _, scopeRoot := range managedDoltScopeRootsForConfig(cityPath, cfg, cfgErr) {
		if !scopeBindingIsProviderOwnedProxied(scopeRoot) || !scopeJournaledToProvider(cityPath, scopeRoot) {
			continue
		}
		roots = append(roots, scopeRoot)
	}
	if len(roots) == 0 {
		return nil
	}
	if cfgErr != nil {
		cfg = nil
	}
	return &ProxiedIdleTimeoutCheck{cityPath: cityPath, cfg: cfg, scopeRoots: roots}
}

// Name returns the check identifier.
func (c *ProxiedIdleTimeoutCheck) Name() string { return "proxied-idle-timeout" }

// Run compares each settled gc-owned proxied scope's configured, persisted and
// running idle timeouts.
//
// A scope the journal records as still initializing is not compared: bd writes
// metadata.json and the sidecar in one batch, so a crashed `bd init` leaves a
// scope with no sidecar, and every other proxied check names that state as
// pending initialisation (see BeadsStoreCheck.Run's pendingScopeInitResult).
func (c *ProxiedIdleTimeoutCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}

	var drift, restart, notes, unreadable, pending []string
	settled := 0
	for _, scopeRoot := range c.scopeRoots {
		label := proxiedScopeLabel(c.cityPath, scopeRoot)
		if scopeInitializationPending(c.cityPath, scopeRoot) {
			pending = append(pending, fmt.Sprintf("%s %s", label, pendingScopeDetailSuffix))
			continue
		}
		settled++
		rig := c.rigForScope(scopeRoot)
		shares := rig != nil && proxyendpoint.SharesCityRoot(c.cityPath, scopeRoot)
		want, ignored, err := config.ProxiedIdleTimeoutForScope(c.cfg, rig, shares)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		if ignored {
			notes = append(notes, fmt.Sprintf("%s: beads_proxied_idle_timeout is ignored because the rig shares the city's proxy root; it uses the city's value", label))
		}
		sidecar, err := proxyendpoint.ReadSidecar(scopeBeadsDir(scopeRoot))
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		if !sidecar.IdleMatches(want.Duration) {
			drift = append(drift, fmt.Sprintf("%s: configured %s (%s), scope has %s", label, want, want.Source, sidecar.IdlePolicy()))
			continue
		}
		if argv, live := liveProxyIdlePolicy(scopeRoot); live && !sameIdlePolicy(argv, sidecar.IdlePolicy()) {
			restart = append(restart, fmt.Sprintf("%s: running proxy has %s, scope has %s", label, argv, sidecar.IdlePolicy()))
		}
	}
	envNote := ""
	if env := strings.TrimSpace(os.Getenv(config.ProxiedIdleTimeoutEnv)); env != "" {
		envNote = fmt.Sprintf("%s=%s overrides the configured idle timeout for every scope gc initializes from this environment", config.ProxiedIdleTimeoutEnv, env)
	}

	if len(drift) == 0 && len(restart) == 0 && len(notes) == 0 && len(unreadable) == 0 {
		if settled == 0 {
			// Nothing to have an opinion about yet: every scope this check
			// covers is still being initialized.
			r.Status = StatusWarning
			r.Message = pendingScopeInitMessage
			r.FixHint = "run `gc start` to finish provider-owned beads initialisation"
			r.Details = pending
			return r
		}
		r.Status = StatusOK
		r.Message = fmt.Sprintf("%d gc-owned proxied scope(s) carry the configured idle timeout", settled)
		if len(pending) > 0 {
			r.Message += fmt.Sprintf("; %d still initializing", len(pending))
		}
		if envNote != "" {
			// Informational: the scopes match what this environment resolves.
			r.Message += "; " + config.ProxiedIdleTimeoutEnv + " is set"
			r.Details = append(r.Details, envNote)
		}
		r.Details = append(r.Details, pending...)
		return r
	}
	if envNote != "" {
		notes = append(notes, envNote)
	}

	r.Status = StatusWarning
	switch {
	case len(drift) > 0:
		r.Message = fmt.Sprintf("%d gc-owned proxied scope(s) do not carry the configured idle timeout", len(drift))
		r.FixHint = "bd cannot yet change an initialized scope's idle timeout, and gc will not edit the sidecar, which is bd's file: the configured value applies to scopes gc creates (gc init, gc rig add, gc beads city migrate-proxied) and takes effect on existing scopes once bd supports `bd dolt set idle-timeout`. To keep these scopes as they are and silence this, set the value they carry, e.g. `[beads] proxied_idle_timeout = \"0\"` for scopes that never idle"
	case len(unreadable) > 0:
		r.Message = fmt.Sprintf("could not compare the idle timeout of %d gc-owned proxied scope(s)", len(unreadable))
		r.FixHint = "inspect <scope>/.beads/proxied_server_client_info.json and the city's idle-timeout config"
	case len(restart) > 0:
		r.Message = fmt.Sprintf("%d running proxy(ies) still use an older idle timeout", len(restart))
		r.FixHint = "the scope's value takes effect at the proxy's next start: after its idle exit, or after `gc stop` and `gc start`"
	default:
		r.Message = "the proxied idle timeout carries a note"
		r.FixHint = "no action needed unless the note is unexpected"
	}
	r.Details = append(append(append(append(drift, unreadable...), restart...), notes...), pending...) //nolint:gocritic // one detail list, drift first
	return r
}

// rigForScope returns the configured rig whose path is scopeRoot, or nil for
// the city and for a scope no rig names.
func (c *ProxiedIdleTimeoutCheck) rigForScope(scopeRoot string) *config.Rig {
	if c.cfg == nil || pathutil.SamePath(c.cityPath, scopeRoot) {
		return nil
	}
	for i := range c.cfg.Rigs {
		path := c.cfg.Rigs[i].Path
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.cityPath, path)
		}
		if pathutil.SamePath(path, scopeRoot) {
			return &c.cfg.Rigs[i]
		}
	}
	return nil
}

// liveProxyIdlePolicy reports the idle policy a scope's running proxy was
// started with, read from its argv. live is false when no proxy runs or its
// argv did not answer.
func liveProxyIdlePolicy(scopeRoot string) (proxyendpoint.IdlePolicy, bool) {
	root, err := proxyendpoint.ProviderRoot(scopeRoot)
	if err != nil {
		return proxyendpoint.IdlePolicy{}, false
	}
	ep := proxyendpoint.Inspect(root, proxiedEndpointProcessTable())
	if !ep.Verdict.Live() || !ep.Liveness.IdlePolicy.Known() {
		return proxyendpoint.IdlePolicy{}, false
	}
	return ep.Liveness.IdlePolicy, true
}

// sameIdlePolicy compares two policies by behavior, ignoring their sources.
func sameIdlePolicy(a, b proxyendpoint.IdlePolicy) bool {
	if a.Kind != b.Kind {
		return false
	}
	return a.Kind != proxyendpoint.IdleFinite || a.Timeout == b.Timeout
}

// CanFix returns false: the sidecar is bd's file, and the repair is a bd init
// gc drives through its own front door rather than an edit gc makes here.
func (c *ProxiedIdleTimeoutCheck) CanFix() bool { return false }

// Fix is a no-op. See CanFix.
func (c *ProxiedIdleTimeoutCheck) Fix(_ *CheckContext) error { return nil }
