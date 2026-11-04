// Package runtimecontract is the golden conformance suite for Runtime
// Provider Protocol (RPP) executables. It is the runtime-level sibling of
// internal/worker/workertest: a requirement-coded catalog plus a wire
// runner that drives an arbitrary executable and a structured report.
//
// The promise: an executable that passes every required catalog entry is
// guaranteed to behave like a gascity runtime, because the catalog mirrors
// the in-tree provider contract (internal/runtime/runtimetest.RunProviderTests).
// Two invariants enforce that the promise cannot rot:
//
//   - Completeness — [Run] emits exactly one [Result] per catalog
//     [Requirement] (TestRunCoversEveryCatalogRequirement).
//   - Lockstep — internal/runtime/runtimetest binds the catalog to
//     RunProviderTests: the same golden reference executable passes both
//     suites, and a coverage map fails CI if a contract behavior gains a
//     RunProviderTests case without a catalog entry.
//
// Two exceptions to the RunProviderTests mirror are wire-only groups — there
// is no runtime.Provider method to contract-test yet, so they are validated by
// the runtimecontract probe (and the runtimecapability env runner) rather than
// by RunProviderTests, and are exempt from the catalog↔RunProviderTests backing
// check (TestEveryCatalogCodeBacksAContractCase):
//
//   - connection (the exec primitive) — re-binds to RunProviderTests when the
//     Go-side connection method (Place.Exec) lands.
//   - provision (the box-without-agent op of the runtime/transport un-weld) —
//     an optional RPP op that creates a reachable box WITHOUT launching the
//     agent, so the controller can relaunch the agent in a warm box over exec
//     instead of re-provisioning. In-repo Provision is still the welded Start,
//     so there is no distinct RunProviderTests case to mirror yet.
//   - observe (observe.v1, PAR-EXEC), but for the requirements a provider
//     method backs (see wireOnlyCodes): the `observe`, `inventory` and
//     `kill-object` wire shapes, the attach answer, the fault and city-scope
//     tokens and the probe latency have no runtime.Provider method of their
//     own to contract-test.
//
// Unlike rppcheck (the lighter `gc runtime check` smoke test), this suite
// also proves each requirement is *gated*: a reference script that violates
// one behavior fails exactly that requirement's check (the negative-gating
// tests). "Probed" is not "guaranteed"; gating is.
//
// The catalog grows toward full RunProviderTests parity group by group;
// every group that lands is fully gated. Remaining groups are tracked in
// internal/runtime/REQUIREMENTS.md.
package runtimecontract

// Code is a stable RPP conformance requirement identifier. Codes are
// referenced by the ledger (internal/runtime/REQUIREMENTS.md) and by the
// runtimetest lockstep map, so they must never be renumbered once shipped.
type Code string

// Group buckets requirements by the provider behavior they exercise. The
// groups mirror RunProviderTests' test groups.
type Group string

// Requirement groups.
const (
	GroupProtocol   Group = "protocol"
	GroupLifecycle  Group = "lifecycle"
	GroupConnection Group = "connection"
	GroupProvision  Group = "provision"
	GroupObserve    Group = "observe"
)

// Requirement is one behavior an RPP executable must satisfy to be a
// gascity-compatible runtime. Each maps to a RunProviderTests case (see the
// runtimetest lockstep map).
type Requirement struct {
	// Code is the stable identifier (e.g. "RPP-LIFECYCLE-001").
	Code Code
	// Group is the behavior bucket.
	Group Group
	// Title is a one-line human description of the required behavior.
	Title string
	// Optional marks a behavior gated behind a declared capability or an
	// optional op the executable need not implement: absent reads as SKIP,
	// present must conform. Required behaviors (Optional=false) must PASS.
	Optional bool
}

// Requirement codes. Stable — never renumber a shipped code.
const (
	ReqProtocolHandshake Code = "RPP-PROTOCOL-001"

	ReqLifecycleStartRunning      Code = "RPP-LIFECYCLE-001"
	ReqLifecycleDuplicateErr      Code = "RPP-LIFECYCLE-002"
	ReqLifecycleStopNotRunning    Code = "RPP-LIFECYCLE-003"
	ReqLifecycleStopIdempotent    Code = "RPP-LIFECYCLE-004"
	ReqLifecycleUnknownNotRunning Code = "RPP-LIFECYCLE-005"

	// ReqConnectionExec is the slim connection primitive: a carrier drives the
	// box THROUGH exec instead of via dedicated driving ops. The six legacy
	// driving ops (nudge / send-keys / interrupt / clear-scrollback / peek /
	// watch-startup) are deliberately NOT catalog requirements — they are
	// reproducible carrier-side over exec, so a runtime author is never
	// contractually forced to implement them. It is Optional for now (absent =
	// SKIP): gc still delivers input/observation via the driving-op methods, so
	// requiring exec before that carrier rewrite would let an exec-only runtime
	// pass conformance while gc silently no-ops its nudge/peek calls. It flips
	// to required in the slice that moves gc's own driving over exec. See
	// REQUIREMENTS.md (RUNTIME-RPP-013).
	ReqConnectionExec Code = "RPP-CONN-001"

	// ReqProvisionBoxWithoutAgent is the runtime/transport un-weld's RPP wire
	// half: an optional `provision` op (advertised via the proc.provision
	// capability) that creates a reachable box but does NOT launch the agent, so
	// the controller can launch — and later relaunch — the agent in a warm box
	// over exec instead of paying a full re-provision on every config change. The
	// defining behavior: after `provision`, is-running reports false (no agent),
	// yet the box is exec-able (the controller's launch transport). Optional
	// (absent = SKIP): a welded `start` pack that launches the agent in one shot
	// need not implement it. See REQUIREMENTS.md (RUNTIME-RPP-014).
	ReqProvisionBoxWithoutAgent Code = "RPP-PROVISION-001"

	// The observe group is the observe.v1 profile (PAR-EXEC): what a script
	// declaring the observe.v1 capability promises reconciler v2. Every
	// requirement is Optional (SKIP when observe.v1 is not declared); a
	// script that declares it must pass every one, except the
	// token-gated ones it does not declare (conformance-faults,
	// city-scoped, report-attachment, report-activity). See
	// REQUIREMENTS.md (RUNTIME-RPP-018..025).
	ReqObserveDeclaredOpsRespond   Code = "RPP-OBSERVE-001"
	ReqObserveAbsentNeverStarted   Code = "RPP-OBSERVE-002"
	ReqObserveRunningWithIdentity  Code = "RPP-OBSERVE-003"
	ReqObserveAbsentAfterStop      Code = "RPP-OBSERVE-004"
	ReqObserveObjectStable         Code = "RPP-OBSERVE-005"
	ReqObserveAgentLiveness        Code = "RPP-OBSERVE-006"
	ReqObserveInventoryComplete    Code = "RPP-OBSERVE-007"
	ReqObserveMetaIsIdentity       Code = "RPP-OBSERVE-008"
	ReqObserveFreshOnlyRefuses     Code = "RPP-OBSERVE-009"
	ReqObserveKillObjectExact      Code = "RPP-OBSERVE-010"
	ReqObserveAttachedIsStrict     Code = "RPP-OBSERVE-011"
	ReqObserveFaultsReadUnknown    Code = "RPP-OBSERVE-012"
	ReqObserveWithinBound          Code = "RPP-OBSERVE-013"
	ReqObserveCityScoped           Code = "RPP-OBSERVE-014"
	ReqObserveInventoryHasActivity Code = "RPP-OBSERVE-015"
	ReqObservePeekNoScreen         Code = "RPP-OBSERVE-016"
)

// catalog is the authoritative, ordered requirement list. Run walks it in
// order. Append-only: new requirements get new codes at the end of their
// group.
var catalog = []Requirement{
	{ReqProtocolHandshake, GroupProtocol, "protocol op returns a well-formed handshake (version >= 0, parseable capabilities) or is absent (exit 2 = v0, no caps)", false},

	{ReqLifecycleStartRunning, GroupLifecycle, "start creates a session that is-running reports true", false},
	{ReqLifecycleDuplicateErr, GroupLifecycle, "start on an already-running session fails (exit 1), never silently succeeds", false},
	{ReqLifecycleStopNotRunning, GroupLifecycle, "stop makes is-running report false", false},
	{ReqLifecycleStopIdempotent, GroupLifecycle, "stop on a missing session succeeds (idempotent)", false},
	{ReqLifecycleUnknownNotRunning, GroupLifecycle, "is-running on a never-started session reports false", false},

	{ReqConnectionExec, GroupConnection, "exec runs a command in the box: command on stdin, combined output on stdout, op exit == command exit (absent = SKIP; becomes required when gc drives its own input/observation over exec)", true},

	{ReqProvisionBoxWithoutAgent, GroupProvision, "provision creates a reachable box WITHOUT launching the agent: after provision, is-running reports false yet the box is exec-able, so the controller launches the agent over exec (absent = SKIP; a welded start pack launches in one shot)", true},

	{ReqObserveDeclaredOpsRespond, GroupObserve, "observe, inventory and kill-object answer (never exit 2) when observe.v1 is declared", true},
	{ReqObserveAbsentNeverStarted, GroupObserve, "observe on a never-started name answers absent", true},
	{ReqObserveRunningWithIdentity, GroupObserve, "after start, the first observe answers running with its object and the identity the start env carried (identity seeded before the box is visible)", true},
	{ReqObserveAbsentAfterStop, GroupObserve, "after stop, observe answers absent and is-running false", true},
	{ReqObserveObjectStable, GroupObserve, "a box's object is the same across observes, and new for a new box under the name", true},
	{ReqObserveAgentLiveness, GroupObserve, "observe answers agent alive for the box's real process name, and dead or unknown for one that is not running", true},
	{ReqObserveInventoryComplete, GroupObserve, "inventory is complete: it lists every started name with observe's object and identity, and omits stopped ones", true},
	{ReqObserveMetaIsIdentity, GroupObserve, "get-meta answers observe's identity, and a set-meta of an identity key is reflected by the next observe", true},
	{ReqObserveFreshOnlyRefuses, GroupObserve, "a fresh_only start over a held name exits 73 (or the collision phrase) and leaves the box untouched", true},
	{ReqObserveKillObjectExact, GroupObserve, "kill-object kills only the exact object, after its re-check: a stale object is gone, corpse on a live box is live, zombie on a live agent is changed, a dead agent's box is killed", true},
	{ReqObserveAttachedIsStrict, GroupObserve, "is-attached answers exactly true or false (required with report-attachment)", true},
	{ReqObserveFaultsReadUnknown, GroupObserve, "under GC_RPP_FAULT=unavailable, observe, inventory and is-attached answer unknown or fail, never absent or false (declared conformance-faults only)", true},
	{ReqObserveWithinBound, GroupObserve, "observe answers within 4s, v2's probe bound", true},
	{ReqObserveCityScoped, GroupObserve, "a name started under one GC_EXEC_CITY_PATH is not listed under another (declared city-scoped only)", true},
	{ReqObserveInventoryHasActivity, GroupObserve, "inventory carries last_activity for every running session when report-activity is declared", true},
	{ReqObservePeekNoScreen, GroupObserve, "peek reads a running box's screen (exit 0), and answers a name no box holds with exit 66, no screen: never an empty success", true},
}

// Catalog returns the authoritative requirement list in run order. The
// returned slice is a copy; callers may not mutate the catalog.
func Catalog() []Requirement {
	out := make([]Requirement, len(catalog))
	copy(out, catalog)
	return out
}
