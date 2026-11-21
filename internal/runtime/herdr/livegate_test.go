package herdr

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/herdr/herdrtest"
)

// requireLiveHerdr gates this package's live journeys. The decision itself,
// and the reasoning behind making the tier opt-in, live in
// internal/runtime/herdr/herdrtest: the controller's own live journeys under
// cmd/gc need the same gate, and one predicate cannot drift from itself.
func requireLiveHerdr(t *testing.T) {
	t.Helper()
	herdrtest.RequireLive(t)
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
