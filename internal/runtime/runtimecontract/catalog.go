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
	GroupProtocol  Group = "protocol"
	GroupLifecycle Group = "lifecycle"
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
}

// Catalog returns the authoritative requirement list in run order. The
// returned slice is a copy; callers may not mutate the catalog.
func Catalog() []Requirement {
	out := make([]Requirement, len(catalog))
	copy(out, catalog)
	return out
}
