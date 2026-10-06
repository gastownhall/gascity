package doctor

import (
	"context"
	"fmt"
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

// ProxiedIdleTimeoutCheck reports a gc-owned proxied scope whose sidecar does
// not state, in writing, that its proxy has no idle timeout.
//
// bd tags the sidecar's idle_timeout `omitempty`, so an absent key is what a
// scope initialized without the flag looks like — and bd's provider substitutes
// a 30s window for it. On such a scope bd retires the proxy AND its Dolt child
// after every quiet period, and the next gc command pays a cold start: about a
// second of proxy adoption plus the Dolt child's own startup, per scope, on a
// city that may have a dozen.
//
// gc's own provider script passes `--proxied-server-idle-timeout 0` at init,
// which bd maps to IdleTimeoutNever before persisting, so a scope gc created
// carries `-1`. An absent or non-negative value on a gc-owned scope therefore
// means something rewrote the sidecar, or the scope was initialized by
// something other than gc's front door — which is an ordinary thing for an
// operator to have done and NOT a broken city. That is why it warns rather than
// fails: the scope works, it is just slower than gc's topology intends, and gc
// must not repair it by writing to a file bd owns.
type ProxiedIdleTimeoutCheck struct {
	cityPath   string
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
	return &ProxiedIdleTimeoutCheck{cityPath: cityPath, scopeRoots: roots}
}

// Name returns the check identifier.
func (c *ProxiedIdleTimeoutCheck) Name() string { return "proxied-idle-timeout" }

// Run reads each settled gc-owned proxied scope's sidecar and reports the ones
// that do not pin their proxy resident.
//
// A scope the journal records as still initializing is not an offender, and this
// is the one check that could have said otherwise. bd writes metadata.json and
// the sidecar in one batch, so the state that persists after a crashed `bd init`
// is a scope with NO sidecar — which reads here as "absent idle_timeout", the
// operator-misconfiguration message, with a hint to re-initialize a scope that is
// mid-initialization. Every other proxied check names that state as pending
// initialisation (see BeadsStoreCheck.Run's pendingScopeInitResult), and the lens
// has to agree across checks or the operator is told to fix the wrong thing.
func (c *ProxiedIdleTimeoutCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}

	var offenders, unreadable, pending []string
	settled := 0
	for _, scopeRoot := range c.scopeRoots {
		label := proxiedScopeLabel(c.cityPath, scopeRoot)
		if scopeInitializationPending(c.cityPath, scopeRoot) {
			pending = append(pending, fmt.Sprintf("%s %s", label, pendingScopeDetailSuffix))
			continue
		}
		settled++
		sidecar, err := proxyendpoint.ReadSidecar(scopeBeadsDir(scopeRoot))
		switch {
		case err != nil:
			unreadable = append(unreadable, fmt.Sprintf("%s: %v", label, err))
		case sidecar.ExplicitIdleNever():
			continue
		default:
			offenders = append(offenders, fmt.Sprintf("%s (%s)", label, sidecar.IdlePolicy()))
		}
	}

	if len(offenders) == 0 && len(unreadable) == 0 {
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
		r.Message = fmt.Sprintf("%d gc-owned proxied scope(s) pin their proxy resident (idle_timeout < 0)", settled)
		if len(pending) > 0 {
			r.Message += fmt.Sprintf("; %d still initializing", len(pending))
			r.Details = pending
		}
		return r
	}

	r.Status = StatusWarning
	switch {
	case len(offenders) > 0:
		r.Message = fmt.Sprintf(
			"%d gc-owned proxied scope(s) do not pin their proxy resident: %s — bd retires the proxy and its Dolt child after each quiet period, so every later command pays a cold start",
			len(offenders), strings.Join(offenders, ", "))
		r.FixHint = "re-initialize the scope through gc (`gc rig add` / `gc init`), which passes --proxied-server-idle-timeout 0 and makes bd persist idle_timeout -1; gc will not edit the sidecar, which is bd's file"
	default:
		r.Message = fmt.Sprintf("could not read the proxied sidecar of %d gc-owned scope(s)", len(unreadable))
		r.FixHint = "inspect <scope>/.beads/proxied_server_client_info.json; bd rewrites it on the next `bd init` for that scope"
	}
	r.Details = append(append(offenders, unreadable...), pending...) //nolint:gocritic // one detail list, offenders first
	return r
}

// CanFix returns false: the sidecar is bd's file, and the repair is a bd init
// gc drives through its own front door rather than an edit gc makes here.
func (c *ProxiedIdleTimeoutCheck) CanFix() bool { return false }

// Fix is a no-op. See CanFix.
func (c *ProxiedIdleTimeoutCheck) Fix(_ *CheckContext) error { return nil }
