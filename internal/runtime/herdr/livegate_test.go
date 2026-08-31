package herdr

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

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
// -short (Makefile), and TESTING.md places live journeys in scheduled or
// explicit profile lanes rather than the fast unit sweep.
func requireLiveHerdr(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping live herdr test in -short mode")
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}
	if strings.TrimSpace(os.Getenv("GC_HERDR_LIVE_TESTS")) == "1" {
		return
	}
	if strings.TrimSpace(os.Getenv("GC_FAST_UNIT")) == "0" {
		return
	}
	t.Skip("skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1")
}
