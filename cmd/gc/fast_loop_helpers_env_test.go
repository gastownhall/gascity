package main

import (
	"strings"
	"testing"
)

// Regression for gastownhall/gascity#938:
// beads_provider_lifecycle_test.go test cases that run the real
// gc-beads-bd shell script did `append(os.Environ(), ...)` to build
// their subprocess env. That inherited any GC_*/BEADS_* vars the user
// had in their shell — crucially GC_CITY_RUNTIME_DIR, which makes the
// dolt runtime layout resolver write dolt-provider-state.json into the
// *real* user city rather than the test's t.TempDir(). Confirmed in a
// dev city: `.gc/runtime/packs/dolt/dolt-provider-state.json.corrupt-bak`
// contained a data_dir pointing at a deleted test tempdir.
//
// sanitizedBaseEnv is the escape hatch: it strips every GC_*/BEADS_*
// entry from os.Environ() before appending the test's own overrides, so
// even a shell where the user had been `gc session`'d into a real city
// can't leak into a lifecycle-test run.

func TestSanitizedBaseEnv_StripsGCPrefixed(t *testing.T) {
	t.Setenv("GC_CITY_RUNTIME_DIR", "/pretend/real/city/.gc/runtime")
	t.Setenv("GC_DOLT_STATE_FILE", "/pretend/real/city/.gc/runtime/packs/dolt/dolt-provider-state.json")
	t.Setenv("GC_PACK_STATE_DIR", "/pretend/real/city/.gc/runtime/packs/dolt")

	env := sanitizedBaseEnv()

	for _, kv := range env {
		if strings.HasPrefix(kv, "GC_") {
			t.Errorf("sanitizedBaseEnv leaked GC_* var: %q", kv)
		}
	}
}

func TestSanitizedBaseEnv_StripsBEADSPrefixed(t *testing.T) {
	// BEADS_DIR / BEADS_DOLT_* get set by gc when it spawns agent shells.
	// A test running from inside such a shell would inherit them.
	t.Setenv("BEADS_DIR", "/pretend/real/city/.beads")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "28231")

	env := sanitizedBaseEnv()

	for _, kv := range env {
		if strings.HasPrefix(kv, "BEADS_") {
			t.Errorf("sanitizedBaseEnv leaked BEADS_* var: %q", kv)
		}
	}
}

func TestSanitizedBaseEnv_PreservesUnrelatedVars(t *testing.T) {
	// The child process typically still needs HOME, PATH, USER, etc.
	// We only filter the namespaces that drive gc path resolution.
	t.Setenv("MCDCLIENT_GASCITY_MARKER_FOR_TEST", "keepme")

	env := sanitizedBaseEnv()

	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "MCDCLIENT_GASCITY_MARKER_FOR_TEST=") {
			if kv != "MCDCLIENT_GASCITY_MARKER_FOR_TEST=keepme" {
				t.Errorf("unrelated var corrupted: %q", kv)
			}
			found = true
			break
		}
	}
	if !found {
		t.Error("sanitizedBaseEnv stripped an unrelated var; expected to keep MCDCLIENT_GASCITY_MARKER_FOR_TEST")
	}
}

func TestSanitizedBaseEnv_AppendsExtras(t *testing.T) {
	// Also: extras that happen to start with GC_/BEADS_ are allowed — this
	// is the mechanism the caller uses to set GC_CITY_PATH=<tempdir>.
	env := sanitizedBaseEnv(
		"GC_CITY_PATH=/tmp/explicit/city",
		"PATH=/opt/bin:/usr/bin",
	)

	gotCity := ""
	gotPath := ""
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "GC_CITY_PATH="):
			gotCity = kv
		case strings.HasPrefix(kv, "PATH="):
			gotPath = kv
		}
	}
	if gotCity != "GC_CITY_PATH=/tmp/explicit/city" {
		t.Errorf("GC_CITY_PATH extra not appended: got %q", gotCity)
	}
	if gotPath != "PATH=/opt/bin:/usr/bin" {
		t.Errorf("PATH extra not appended: got %q", gotPath)
	}
}

func TestSanitizedBaseEnv_ExtrasOverrideInheritedLastWins(t *testing.T) {
	// If the helper ever regresses to NOT filter a var and the extras
	// also set it, the caller still hands exec.Cmd an env slice whose last
	// entry is the intended override. Otherwise the leak reappears silently.
	t.Setenv("SOMETHING_UNFILTERED", "inherited")
	env := sanitizedBaseEnv("SOMETHING_UNFILTERED=override")

	// Find the last occurrence — this test only verifies the helper's
	// slice ordering, not child-process environment lookup semantics.
	last := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "SOMETHING_UNFILTERED=") {
			last = kv
		}
	}
	if last != "SOMETHING_UNFILTERED=override" {
		t.Errorf("override ordering violated; got final %q, want SOMETHING_UNFILTERED=override", last)
	}
}

func TestSanitizedBaseEnv_AllowsExplicitEmptyFilteredOverride(t *testing.T) {
	t.Setenv("GC_BIN", "/pretend/inherited/gc")

	env := sanitizedBaseEnv("GC_BIN=")

	got := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "GC_BIN=") {
			got = kv
		}
	}
	if got != "GC_BIN=" {
		t.Fatalf("explicit empty GC_BIN override missing: got %q", got)
	}
}

// Sanity: the filter matches the prefixes we actually care about and
// nothing else. Guards against a future refactor that tightens the
// matcher too far.
func TestSanitizedBaseEnv_MatchesExactlyGCAndBEADSPrefixes(t *testing.T) {
	for _, kv := range []string{
		"GC_=empty_key",
		"GC_FOO=1",
		"BEADS_=empty_key",
		"BEADS_BAR=2",
	} {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed test env entry %q", kv)
		}
		t.Setenv(key, val)
	}

	env := sanitizedBaseEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "GC_") || strings.HasPrefix(kv, "BEADS_") {
			t.Errorf("sanitizedBaseEnv kept %q; both GC_* and BEADS_* must be filtered", kv)
		}
	}

	// Defensive: a var that merely contains "GC_" mid-name must pass through.
	t.Setenv("HMM_GC_LIKE", "mid-token-not-a-prefix")
	env = sanitizedBaseEnv()
	found := false
	for _, kv := range env {
		if kv == "HMM_GC_LIKE=mid-token-not-a-prefix" {
			found = true
			break
		}
	}
	if !found {
		t.Error("sanitizedBaseEnv stripped a var whose key merely contained GC_ as a substring")
	}
}
