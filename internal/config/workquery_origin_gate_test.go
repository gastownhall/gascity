package config

import (
	"strings"
	"testing"
)

// This file pins the routed-tier origin gate against the controller's
// routed-demand wake contract (#4644, ga-jl73y2).
//
// The gate exists so a named or manual session cannot steal routed demand
// that a pool standby will serve. For a canonical singleton template
// (max_active_sessions = 1, no namepool) the controller never mints that
// standby: it wakes the named holder instead (NamedSessionRoutedDemand,
// "routed-demand" wake reason) and assumes "routed metadata is consumed by
// the agent-side gc hook path". If the hook then gates the holder out, the
// controller wakes a session that by construction cannot see the bead it was
// woken for — an infinite woke -> no_work -> idle_killed loop while the bead
// strands. (#5884)

// routedSingletonFakeBD serves one routed bead for the template's route and
// nothing else, so the only way the query returns it is through the routed
// tier past the origin gate.
const routedSingletonFakeBD = `#!/bin/sh
set -eu
case "$*" in
  *"--metadata-field gc.routed_to=hello-world/archivist"*"--unassigned"*)
    printf '[{"id":"routed-bead","status":"open","issue_type":"task","metadata":{"gc.routed_to":"hello-world/archivist"}}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`

func canonicalSingletonAgent() Agent {
	return Agent{
		Name:              "archivist",
		Dir:               "hello-world",
		MinActiveSessions: ptrInt(0),
		MaxActiveSessions: ptrInt(1),
	}
}

func multiInstancePoolAgent() Agent {
	return Agent{
		Name:              "archivist",
		Dir:               "hello-world",
		MinActiveSessions: ptrInt(0),
		MaxActiveSessions: ptrInt(3),
	}
}

// TestRoutedTierOriginGateByAgentShape is the full contract table: which
// origins each template shape admits to the routed tier.
func TestRoutedTierOriginGateByAgentShape(t *testing.T) {
	cases := []struct {
		name   string
		agent  Agent
		origin string
		served bool
	}{
		// Ephemeral pool members and controller probes (empty origin) are the
		// baseline consumers on every shape.
		{"singleton/ephemeral", canonicalSingletonAgent(), "ephemeral", true},
		{"singleton/probe", canonicalSingletonAgent(), "", true},
		{"multi/ephemeral", multiInstancePoolAgent(), "ephemeral", true},
		{"multi/probe", multiInstancePoolAgent(), "", true},

		// The fix: a canonical singleton's named holder IS the session the
		// controller woke for this demand, and no standby will ever exist.
		{"singleton/named", canonicalSingletonAgent(), "named", true},

		// Unchanged: a multi-instance pool serves routed demand with a
		// standby, so its named session must not steal it.
		{"multi/named", multiInstancePoolAgent(), "named", false},

		// Unchanged: manual sessions never consume routed demand, on any shape.
		{"singleton/manual", canonicalSingletonAgent(), "manual", false},
		{"multi/manual", multiInstancePoolAgent(), "manual", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, query := range map[string]string{
				"work":   tc.agent.EffectiveWorkQuery(),
				"routed": tc.agent.EffectiveRoutedPoolQuery(),
			} {
				out := strings.TrimSpace(runShellWithFakeBd(t, query, map[string]string{
					"GC_SESSION_ID":     "sess-1",
					"GC_SESSION_NAME":   "hello-world--archivist",
					"GC_ALIAS":          "hello-world/archivist",
					"GC_SESSION_ORIGIN": tc.origin,
				}, routedSingletonFakeBD))
				got := strings.Contains(out, "routed-bead")
				if got != tc.served {
					t.Errorf("%s query, origin=%q: routed bead served=%v, want %v\noutput: %s",
						tc.name, tc.origin, got, tc.served, out)
				}
			}
		})
	}
}

// TestRoutedTierOriginGateScriptShape pins the generated shell for both
// shapes so the golden fixtures and this reasoning cannot drift apart: the
// singleton admits named, the multi-instance pool does not, and neither
// admits manual.
func TestRoutedTierOriginGateScriptShape(t *testing.T) {
	singleton := canonicalSingletonAgent()
	multi := multiInstancePoolAgent()

	if got := singleton.EffectiveWorkQuery(); !strings.Contains(got, `ephemeral|named|"") ;;`) {
		t.Errorf("canonical singleton work query does not admit the named holder to the routed tier:\n%s", got)
	}
	if got := multi.EffectiveWorkQuery(); !strings.Contains(got, `ephemeral|"") ;;`) || strings.Contains(got, `|named|`) {
		t.Errorf("multi-instance pool work query must keep the named origin gated:\n%s", got)
	}
	for name, q := range map[string]string{"singleton": singleton.EffectiveWorkQuery(), "multi": multi.EffectiveWorkQuery()} {
		if strings.Contains(q, "manual") {
			t.Errorf("%s work query admits manual sessions to the routed tier:\n%s", name, q)
		}
	}
}

// TestRoutedTierOriginGatePlainNamedSingletonAlsoAdmitted: the plain
// named-session flavor (max=1 with no min/scale_check, the shape lint calls
// a "named singleton") is a canonical singleton too. The controller's demand
// path keys on SupportsGenericEphemeralSessions (max != 0) and the
// routed-demand wake on UsesCanonicalSingletonPoolIdentity (max == 1), so an
// on_demand named session of this shape is woken for routed demand exactly
// like the explicit-min pool flavor — and would be exactly as blind without
// the same exemption. The gate keys on UsesCanonicalSingletonPoolIdentity,
// not on SupportsInstanceExpansion, for that reason.
func TestRoutedTierOriginGatePlainNamedSingletonAlsoAdmitted(t *testing.T) {
	a := Agent{Name: "archivist", Dir: "hello-world", MaxActiveSessions: ptrInt(1)}
	if a.SupportsInstanceExpansion() {
		t.Fatalf("fixture unexpectedly pool-shaped")
	}
	if !a.UsesCanonicalSingletonPoolIdentity() {
		t.Fatalf("fixture is not a canonical singleton")
	}
	out := runShellWithFakeBd(t, a.EffectiveWorkQuery(), map[string]string{
		"GC_SESSION_ID":     "sess-1",
		"GC_SESSION_NAME":   "hello-world--archivist",
		"GC_ALIAS":          "hello-world/archivist",
		"GC_SESSION_ORIGIN": "named",
	}, routedSingletonFakeBD)
	if !strings.Contains(out, "routed-bead") {
		t.Errorf("plain max=1 named singleton under origin=named was not served its routed bead:\n%s", out)
	}
}

// TestRoutedTierOriginGateNamepoolStaysGated: a namepool template with max=1
// is NOT a canonical singleton (it expands to named members and the
// controller does mint standbys for it), so its named origin stays gated.
func TestRoutedTierOriginGateNamepoolStaysGated(t *testing.T) {
	a := Agent{Name: "archivist", Dir: "hello-world", MaxActiveSessions: ptrInt(1), NamepoolNames: []string{"alpha", "beta"}}
	if a.UsesCanonicalSingletonPoolIdentity() {
		t.Fatalf("fixture unexpectedly a canonical singleton")
	}
	if got := a.EffectiveWorkQuery(); !strings.Contains(got, `ephemeral|"") ;;`) || strings.Contains(got, `|named|`) {
		t.Errorf("namepool template must keep the stock origin gate:\n%s", got)
	}
}
