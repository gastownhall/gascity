package testenv

import (
	"os"
	"path/filepath"
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

// newSyntheticCity builds a t.TempDir() tree with a city.toml marker at its
// root and, unless stateJSON is empty, a .gc/runtime/packs/dolt/dolt-state.json
// containing stateJSON. It returns a directory three levels below the city
// root, so a caller exercises ambientCityDoltPort's upward walk rather than
// just a same-directory check.
func newSyntheticCity(t *testing.T, stateJSON string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "city.toml"), []byte("# synthetic city\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if stateJSON != "" {
		stateDir := filepath.Join(root, ".gc", "runtime", "packs", "dolt")
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			t.Fatalf("mkdir dolt state dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), []byte(stateJSON), 0o644); err != nil {
			t.Fatalf("write dolt-state.json: %v", err)
		}
	}
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested dir: %v", err)
	}
	return nested
}

// TestAmbientCityDoltPort exercises ambientCityDoltPort's upward walk and
// doltStatePort's JSON parsing directly, against synthetic t.TempDir() city
// trees rather than this process's real ambient city — reusing the real
// ambient environment in a unit test would itself be the leak-vector hazard
// this package exists to guard against.
func TestAmbientCityDoltPort(t *testing.T) {
	cases := []struct {
		name     string
		dir      string
		wantPort string
		wantOK   bool
	}{
		{
			name:     "finds port through a city several levels up",
			dir:      newSyntheticCity(t, `{"running":true,"pid":1,"port":19999,"data_dir":"x"}`),
			wantPort: "19999",
			wantOK:   true,
		},
		{
			name:   "city with no dolt-state.json yields no port",
			dir:    newSyntheticCity(t, ""),
			wantOK: false,
		},
		{
			name:   "city with unparsable dolt-state.json yields no port",
			dir:    newSyntheticCity(t, `{not json`),
			wantOK: false,
		},
		{
			name:   "city with zero port yields no port",
			dir:    newSyntheticCity(t, `{"port":0}`),
			wantOK: false,
		},
		{
			name:   "city with negative port yields no port",
			dir:    newSyntheticCity(t, `{"port":-1}`),
			wantOK: false,
		},
		{
			name:   "no city.toml anywhere above dir yields no port",
			dir:    filepath.Join(t.TempDir(), "a", "b"),
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(tc.dir, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", tc.dir, err)
			}
			port, ok := ambientCityDoltPort(tc.dir)
			if ok != tc.wantOK {
				t.Fatalf("ambientCityDoltPort(%q) ok = %v, want %v (port=%q)", tc.dir, ok, tc.wantOK, port)
			}
			if ok && port != tc.wantPort {
				t.Errorf("ambientCityDoltPort(%q) port = %q, want %q", tc.dir, port, tc.wantPort)
			}
		})
	}
}
