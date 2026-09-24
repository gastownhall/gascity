package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestGCBdRefusesMistypedMetadataPairs pins that `gc bd update` refuses a bare
// key=value in bd's positional issue-id slot.
//
// `--set-metadata` is a repeatable stringArray taking ONE pair per flag, so in
// `--set-metadata a=1 b=2 c=3` only `a=1` is the flag's value. bd reads `b=2` and
// `c=3` as issue ids, fails to resolve them, prints the failures to stderr — and
// still prints its success line and EXITS 0. Measured on bd 1.1.0 and unchanged
// in 1.1.2: six pairs in, one pair written, exit 0, so no caller can tell.
//
// gc bd writes exec raw bd rather than the routed fastpath, so the classifier's
// guard does not cover this path; it needs its own. The refusal must come before
// any store work, so nothing is written and the exit code is honest.
func TestGCBdRefusesMistypedMetadataPairs(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"trailing pairs", []string{"update", "bd-1", "--set-metadata", "a=1", "b=2", "c=3"}},
		{"one trailing pair", []string{"update", "bd-1", "--set-metadata", "a=1", "b=2"}},
		{"inline flag form", []string{"update", "bd-1", "--set-metadata=a=1", "b=2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := doBd(tc.args, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("doBd(%v) = 0, want non-zero; stderr=%q", tc.args, stderr.String())
			}
			msg := stderr.String()
			if !strings.Contains(msg, "--set-metadata") {
				t.Errorf("stderr %q does not name --set-metadata", msg)
			}
			// The message must name the offending token, so the caller can see
			// which pair bd would have dropped.
			if !strings.Contains(msg, "b=2") {
				t.Errorf("stderr %q does not name the dropped pair b=2", msg)
			}
		})
	}
}

// TestGCBdRefusesMistypedMetadataPairsBehindGlobalFlags pins the exit-code
// contract through the path that used to disarm the guard.
//
// The guard is keyed off the bd subcommand, and bd's global value-flags sit
// BEFORE the subcommand. Locating the verb as the first non-dash token read
// `bob` out of `bd --actor bob update <id> …`, so the verb was not "update", the
// refusal did not fire, and raw bd performed exactly the silent 1-of-N write
// plus exit 0 the guard exists to prevent.
//
// The exit code is the whole contract: this defect survives precisely because
// everything downstream trusts an exit code that lies. So this asserts the code,
// not just the message — and the guard runs before resolveBdCity, so a refusal
// here is also proof that no store was opened and nothing was written.
func TestGCBdRefusesMistypedMetadataPairsBehindGlobalFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"--actor", []string{"--actor", "bob", "update", "bd-1", "--set-metadata", "a=1", "b=2"}},
		{"-C dir", []string{"-C", "/some/dir", "update", "bd-1", "--set-metadata", "a=1", "b=2"}},
		{"--db path", []string{"--db", "/x/y.db", "update", "bd-1", "--set-metadata", "a=1", "b=2"}},
		{"--directory", []string{"--directory", "/d", "update", "bd-1", "--set-metadata", "a=1", "b=2"}},
		{"--dolt-auto-commit", []string{"--dolt-auto-commit", "off", "update", "bd-1", "--set-metadata", "a=1", "b=2"}},
		{"stacked globals", []string{"--actor", "bob", "--json", "-C", "/d", "update", "bd-1", "--set-metadata", "a=1", "b=2"}},
		{"inline global", []string{"--actor=bob", "update", "bd-1", "--set-metadata", "a=1", "b=2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := doBd(tc.args, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("doBd(%v) = 0, want non-zero; stderr=%q", tc.args, stderr.String())
			}
			msg := stderr.String()
			if !strings.Contains(msg, "would be dropped") {
				t.Fatalf("doBd(%v) exited %d but not via the mistyped-pair guard; stderr=%q", tc.args, code, msg)
			}
			if !strings.Contains(msg, "b=2") {
				t.Errorf("stderr %q does not name the dropped pair b=2", msg)
			}
		})
	}
}

// TestGCBdAllowsCorrectMetadataForms pins that the guard does not fire on the
// shapes that actually work: one --set-metadata per pair, a plain id, and
// several ids sharing one pair (bd applies the update to every id). These must
// get past the guard and continue into normal resolution — the guard is not
// allowed to become a reason a working invocation stops working.
func TestGCBdAllowsCorrectMetadataForms(t *testing.T) {
	// These forms deliberately run PAST the guard, so pin an explicit non-city
	// temp dir rather than leaning on ambient state: doBd then fails
	// deterministically at resolveBdCity, before any store is opened or bd is
	// exec'd. TestMain's env scrub and the test-binary refusal of ambient
	// upward discovery cover this today; pinning it here keeps the isolation
	// local to the test instead of a property of that scrub list, and matches
	// the idiom every sibling test in cmd_bd_test.go uses.
	t.Setenv("GC_CITY_PATH", t.TempDir())

	cases := []struct {
		name string
		args []string
	}{
		{"repeated flag", []string{"update", "bd-1", "--set-metadata", "a=1", "--set-metadata", "b=2"}},
		{"single pair", []string{"update", "bd-1", "--set-metadata", "a=1"}},
		{"multi-id one pair", []string{"update", "bd-1", "bd-2", "--set-metadata", "a=1"}},
		{"flag value before id", []string{"update", "--set-metadata", "a=1", "bd-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			_ = doBd(tc.args, &stdout, &stderr)
			// The call fails later for want of a city/store in this environment;
			// what matters is that it was NOT stopped by the mistyped-pair guard.
			if strings.Contains(stderr.String(), "would be dropped") {
				t.Fatalf("guard fired on a valid form %v: %s", tc.args, stderr.String())
			}
		})
	}
}
