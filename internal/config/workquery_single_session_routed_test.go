package config

import (
	"strings"
	"testing"
)

// Single-session config-agent graph steps are delivered by gc.routed_to alone:
// route time stamps the alias and leaves Assignee empty, and a concrete session
// binds Assignee on claim (#6020). That makes the routed (pool-demand) tier of
// this agent's own generated work query the ONLY delivery leg, and the leg spans
// three packages with nothing pinning it end to end:
//
//   - the probe target baked in as $1 (poolDemandTarget()) must stay equal to
//     the value the router stamps in gc.routed_to (agentutil.RoutedToIdentity(),
//     which is QualifiedName() for a non-pool agent — pinned from the stamping
//     side by TestGraphRouteBindingForAgent_SingleSessionRoutesToQualifiedName),
//     and
//   - the origin gate must fall through for a plain config agent, which carries
//     no GC_SESSION_ORIGIN.
//
// Rename either identity, or start stamping an origin on plain config agents,
// and these beads become unclaimable again — the exact outage #6020 fixes — with
// the rest of the suite green. The tests below pin both halves.

// singleSessionConfigAgent is the shape the route change fires for:
// max_active_sessions = 1 with no min_active_sessions, no scale_check and no
// namepool. SupportsInstanceExpansion() is false for exactly this shape, which
// is what selects the non-MetadataOnly (single-session) binding in
// graphroute.ApplyGraphRouteBinding and dispatch.applyAttemptStepRoute.
func singleSessionConfigAgent() Agent {
	return Agent{Name: "run-operator", Dir: "gascity", MaxActiveSessions: ptrInt(1)}
}

// TestSingleSessionConfigAgentProbesItsOwnQualifiedName pins the identity
// coincidence the routed delivery rests on: this agent's pool-demand probe
// target is its bare QualifiedName(), the same value the router stamps in
// gc.routed_to. A slot suffix or any other divergence here routes the seat's
// probe at an identity nothing stamps.
func TestSingleSessionConfigAgentProbesItsOwnQualifiedName(t *testing.T) {
	a := singleSessionConfigAgent()

	if a.SupportsInstanceExpansion() {
		t.Fatalf("SupportsInstanceExpansion() = true; this fixture must stay the single-session (non-pool) shape the route change targets")
	}
	if !a.UsesCanonicalSingletonPoolIdentity() {
		t.Fatalf("UsesCanonicalSingletonPoolIdentity() = false; the singleton must route under its canonical identity, not a {name}-1 slot")
	}
	if got, want := a.poolDemandTarget(), a.QualifiedName(); got != want {
		t.Fatalf("poolDemandTarget() = %q, want %q (the gc.routed_to identity the router stamps)", got, want)
	}
}

// TestEffectiveWorkQueryForSingleSessionConfigAgentServesRoutedUnassignedWork is
// the primary pin for the delivery half: the generated work query must probe
// `gc.routed_to=<QualifiedName>` and must NOT be short-circuited when
// GC_SESSION_ORIGIN is empty.
//
// The fake bd serves the bead only to a read keyed on this seat's own identity,
// so dropping the route predicate reddens this test; the seat carries a session
// identity but no alias and no origin, so the `ephemeral|""` fall-through arm of
// the origin gate is the only thing that can admit the routed tier (a
// self-target admit cannot mask its removal), so narrowing that arm reddens it
// too.
//
// The fake deliberately does NOT re-spell the serve flags that read carries as a
// shell glob: which flags bdReadyPoolDemandShell renders, and that it renders
// them immediately after the route predicate, are pinned against the production
// rendering by TestWorkQueryGolden, TestPoolDemandPredicateSharedWithWorkQuery
// and TestBdReadyPoolDemandShellExcludesDispatchHoldLabels. Keying this fake on
// the flag order too would only couple it to an incidental property of
// PoolDemandServeRules.ShellArgs().
func TestEffectiveWorkQueryForSingleSessionConfigAgentServesRoutedUnassignedWork(t *testing.T) {
	a := singleSessionConfigAgent()

	out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), map[string]string{
		// A live single-session seat: concrete session identity, no alias,
		// and — being a plain config agent, not a [[named_session]] — no
		// GC_SESSION_ORIGIN.
		"GC_SESSION_ID":   "gcg-session-6020",
		"GC_SESSION_NAME": "gascity--run-operator",
	}, fakeBDSelfRoutedFrontier(a.QualifiedName(), "gcg-routed-step"))

	if !strings.Contains(out, "gcg-routed-step") {
		t.Fatalf("single-session config agent did not surface its own routed unassigned step.\nEffectiveWorkQueryFor output = %q\nwant candidate gcg-routed-step served by the routed tier", out)
	}
}

// TestEffectiveWorkQueryForSingleSessionConfigAgentSkipsForeignRoutedWork is the
// over-claim control: the routed tier keys on this seat's own identity, so work
// routed elsewhere stays invisible even though the seat now discovers unassigned
// work. The pin above proves the seat's own work arrives; this one proves that
// is the ONLY work it can reach.
//
// Its fake inverts the pin above: it refuses any read naming this seat's route
// and serves foreign work to every other unassigned read, so a query that
// stopped keying on the seat's identity surfaces gcg-foreign-step here. Keying
// this fake on the foreign route instead would be vacuous — this seat's probe
// target is always its own QualifiedName(), so no such read can exist and the
// assertion could never fail.
func TestEffectiveWorkQueryForSingleSessionConfigAgentSkipsForeignRoutedWork(t *testing.T) {
	a := singleSessionConfigAgent()

	out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), map[string]string{
		"GC_SESSION_ID":   "gcg-session-6020",
		"GC_SESSION_NAME": "gascity--run-operator",
	}, fakeBDRoutedFrontier("gascity/other-operator", "gcg-foreign-step", routedReadGlob(a.QualifiedName()), `*"--unassigned"*`))

	if strings.Contains(out, "gcg-foreign-step") {
		t.Fatalf("single-session config agent served work routed to another identity: %q", out)
	}
}
