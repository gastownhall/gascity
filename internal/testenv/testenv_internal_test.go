package testenv

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// localDoltHostCases classifies every isLocalDoltHost branch: empty values,
// localhost with case/whitespace noise, IPv4/IPv6 loopback, bracketed IPv6
// literals, unspecified addresses, and external values that must not be
// treated as local.
var localDoltHostCases = []struct {
	host string
	want bool
}{
	{"", true},
	{"   ", true},
	{"localhost", true},
	{" LOCALHOST ", true},
	{"127.0.0.1", true},
	{" 127.0.0.1 ", true},
	{"::1", true},
	{"[::1]", true},
	{"0.0.0.0", true},
	{"::", true},
	{"[::]", true},
	{"city-db.example.com", false},
	{"192.0.2.10", false},
	{"2001:db8::10", false},
	{"[2001:db8::10]", false},
}

// TestIsLocalDoltHost exercises every isLocalDoltHost branch directly,
// including the bracketed IPv6 forms that originally bypassed the
// prod-Dolt-port guard.
func TestIsLocalDoltHost(t *testing.T) {
	for _, tc := range localDoltHostCases {
		if got := isLocalDoltHost(tc.host); got != tc.want {
			t.Errorf("isLocalDoltHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// TestIsLocalDoltHostMatchesCanonicalClassifier pins isLocalDoltHost to the
// canonical contract.DoltHostIsLocal semantics. isLocalDoltHost is a
// stdlib-only copy — this package is blank-imported by every test binary and
// must not link domain packages — and a host form the canonical classifier
// calls local but the copy does not is exactly the divergence that let
// "[::1]":3307 reach the production Dolt server.
func TestIsLocalDoltHostMatchesCanonicalClassifier(t *testing.T) {
	for _, tc := range localDoltHostCases {
		if got, want := isLocalDoltHost(tc.host), contract.DoltHostIsLocal(tc.host); got != want {
			t.Errorf("isLocalDoltHost(%q) = %v, but contract.DoltHostIsLocal(%q) = %v; keep the testenv copy aligned with the canonical classifier", tc.host, got, tc.host, want)
		}
	}
}

// TestDoltPortVarsAreLeakVectors enforces the load-bearing coupling between
// the prod-Dolt-port guard and the scrub: refuseProdDoltPort models
// post-scrub survival via the passthrough list, which is only exact when
// every var it guards is also scrubbed. A doltPortVars entry missing from
// LeakVectorVars fails silently in the dangerous direction — skipped by the
// guard (not passthrough-listed, so survives reports false) yet kept by the
// scrub.
func TestDoltPortVarsAreLeakVectors(t *testing.T) {
	leak := make(map[string]bool, len(LeakVectorVars))
	for _, name := range LeakVectorVars {
		leak[name] = true
	}
	for portVar, hostVar := range doltPortVars {
		if !leak[portVar] {
			t.Errorf("doltPortVars key %q is missing from LeakVectorVars; every guarded Dolt port var must also be scrubbed", portVar)
		}
		if hostVar != "" && !leak[hostVar] {
			t.Errorf("doltPortVars[%q] host var %q is missing from LeakVectorVars; every guarded Dolt host var must also be scrubbed", portVar, hostVar)
		}
	}
}
