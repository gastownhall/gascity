package herdr

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// liveHerdrSkipReason reports why the live tier should be skipped, or "" when it
// should run. Split out from requireLiveHerdr so the gating decision itself is
// testable without a t.Skip that unwinds the calling goroutine.
func liveHerdrSkipReason(short, herdrInstalled bool, fastUnit, liveTests string) string {
	if short {
		return "skipping live herdr test in -short mode"
	}
	if !herdrInstalled {
		return "herdr not installed"
	}
	if strings.TrimSpace(liveTests) == "1" {
		return ""
	}
	if strings.TrimSpace(fastUnit) == "0" {
		return ""
	}
	return "skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`"
}

// requireLiveHerdr gates the package's live journeys, which drive a real herdr
// server: they place panes, force agent-status reports, bounce the server, and
// assert on the wire event stream.
//
// Presence of the binary is not the precondition these tests actually need.
// What they need is a herdr whose behavior matches the contract they assert,
// and that is not something a guard can probe cheaply: herdr 0.8.0 made the
// agent registry detection-based, so a plain shell pane is never registered and
// agent lookups correctly report not-found. Gating on the binary alone made the
// result depend on which herdr happened to be installed, and since CI has no
// herdr, a version bump turns every local `make test` red while CI stays green.
//
// So this tier is opt-in. `make test` runs ./... with GC_FAST_UNIT=1 and no
// -short, and TESTING.md places live journeys in explicit profile lanes rather
// than the fast unit sweep. `make test-herdr-live` is the lane that runs them;
// scripts/test-integration-shard also sets GC_FAST_UNIT=0.
func requireLiveHerdr(t *testing.T) {
	t.Helper()
	_, err := exec.LookPath("herdr")
	if reason := liveHerdrSkipReason(
		testing.Short(),
		err == nil,
		os.Getenv("GC_FAST_UNIT"),
		os.Getenv("GC_HERDR_LIVE_TESTS"),
	); reason != "" {
		t.Skip(reason)
	}
}

func TestLiveHerdrSkipReason(t *testing.T) {
	for _, tc := range []struct {
		name      string
		short     bool
		installed bool
		fastUnit  string
		liveTests string
		wantRun   bool
	}{
		{name: "short mode skips even when opted in", short: true, installed: true, fastUnit: "0", liveTests: "1"},
		{name: "missing binary skips even when opted in", installed: false, fastUnit: "0", liveTests: "1"},
		{name: "unit lane skips", installed: true, fastUnit: "1"},
		{name: "unset skips", installed: true},
		{name: "GC_FAST_UNIT=0 runs", installed: true, fastUnit: "0", wantRun: true},
		{name: "GC_HERDR_LIVE_TESTS=1 runs", installed: true, fastUnit: "1", liveTests: "1", wantRun: true},
		{name: "opt-in is whitespace tolerant", installed: true, fastUnit: "1", liveTests: " 1 ", wantRun: true},
		{name: "GC_HERDR_LIVE_TESTS=0 does not opt in", installed: true, fastUnit: "1", liveTests: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := liveHerdrSkipReason(tc.short, tc.installed, tc.fastUnit, tc.liveTests)
			if gotRun := reason == ""; gotRun != tc.wantRun {
				t.Fatalf("short=%v installed=%v GC_FAST_UNIT=%q GC_HERDR_LIVE_TESTS=%q: run=%v want run=%v (reason %q)",
					tc.short, tc.installed, tc.fastUnit, tc.liveTests, gotRun, tc.wantRun, reason)
			}
		})
	}
}

// herdrRegistryIsDetectionBased reports whether the installed herdr derives its
// agent registry from pane detection (0.8.0 and later) rather than registering
// every pane the provider places (0.7.x). Parsing failures report false, so an
// unrecognized version runs the test rather than silently skipping it.
func herdrRegistryIsDetectionBased(version string) bool {
	fields := strings.Fields(version)
	if len(fields) < 2 {
		return false
	}
	parts := strings.SplitN(fields[1], ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return major > 0 || minor >= 8
}

// skipOnDetectionBasedRegistry waives the known herdr 0.8.0 incompatibility
// tracked as gastownhall/gascity#5808: 0.8.0 registers an agent only when it
// detects a supported interactive agent in the pane, so the plain shell panes
// these journeys place are never registered and the registry assertions fail.
// The waiver is version-scoped, so it lifts on its own against 0.7.x and once
// #5808 re-points the test at the contract 0.8 actually provides.
//
// The version probe goes through the provider's own client rather than a fresh
// exec.Command so the live tier adds no new subprocess call to the untagged
// test-source census (internal/testpolicy/resourcecensus).
func skipOnDetectionBasedRegistry(t *testing.T, p *Provider) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := p.c.runRaw(ctx, "--version")
	if err != nil {
		return
	}
	if herdrRegistryIsDetectionBased(out) {
		t.Skipf("herdr %q uses a detection-based agent registry; tracked as #5808",
			strings.TrimSpace(out))
	}
}

func TestHerdrRegistryIsDetectionBased(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{in: "herdr 0.8.0", want: true},
		{in: "herdr 0.8.1\n", want: true},
		{in: "herdr 0.9.0", want: true},
		{in: "herdr 1.0.0", want: true},
		{in: "herdr 0.7.5", want: false},
		{in: "herdr 0.7.4", want: false},
		{in: "herdr", want: false},
		{in: "herdr v0.8", want: false},
		{in: "", want: false},
	} {
		if got := herdrRegistryIsDetectionBased(tc.in); got != tc.want {
			t.Errorf("herdrRegistryIsDetectionBased(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
