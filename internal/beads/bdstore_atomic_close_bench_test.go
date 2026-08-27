//go:build integration

package beads_test

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// terminalCloseMicroBenchN is the bounded sample size the ga-f7v2ft.78.6
// adjudication set for the perf criterion: the fenced terminal close is not on
// the session-start hot path, so a 100-sample comparison decides whether the
// full acceptance cohorts are warranted. Only if the close path turns out to be
// hot does the 3x30 acceptance_c run become necessary.
const terminalCloseMicroBenchN = 100

// TestTerminalCloseMicroBenchmark compares the fused guarded terminal close
// against the historical split sequence over a real bd store. It is opt-in
// (GC_RUN_TERMINAL_CLOSE_MICROBENCH=1): 200 real bd closes take minutes, which
// does not belong in the default integration sweep.
func TestTerminalCloseMicroBenchmark(t *testing.T) {
	if os.Getenv("GC_RUN_TERMINAL_CLOSE_MICROBENCH") != "1" {
		t.Skip("set GC_RUN_TERMINAL_CLOSE_MICROBENCH=1 to run the bounded close-path comparison")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skipf("bd not on PATH: %v", err)
	}
	store, _ := newConditionalIntegrationBdStore(t)
	if _, ok := beads.AtomicConditionalCloserFor(store); !ok {
		t.Skip("installed bd lacks `bd update --if-status`")
	}

	terminal := map[string]string{
		"state":        "drained",
		"close_reason": "session drained: pool slot retired by reconciler",
		"closed_at":    "2026-08-08T00:00:00Z",
	}

	split := make([]beads.Bead, terminalCloseMicroBenchN)
	fused := make([]beads.Bead, terminalCloseMicroBenchN)
	for i := range terminalCloseMicroBenchN {
		split[i] = mustCreate(t, store, "microbench split close")
		fused[i] = mustCreate(t, store, "microbench fused close")
	}

	splitStart := time.Now()
	for _, b := range split {
		// The historical arm, exactly as session.Store.Close ran it before the
		// fix: SetMetadataBatch(ClosePatch) then Close.
		if err := store.SetMetadataBatch(b.ID, terminal); err != nil {
			t.Fatalf("split arm SetMetadataBatch: %v", err)
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("split arm Close: %v", err)
		}
	}
	splitElapsed := time.Since(splitStart)

	fusedStart := time.Now()
	for _, b := range fused {
		current, err := store.Get(b.ID)
		if err != nil {
			t.Fatalf("fused arm Get: %v", err)
		}
		if _, err := store.CloseWithMetadataIfMatch(b.ID, current.Revision, terminal); err != nil {
			t.Fatalf("fused arm CloseWithMetadataIfMatch: %v", err)
		}
	}
	fusedElapsed := time.Since(fusedStart)

	t.Logf("terminal close, N=%d per arm against real bd:", terminalCloseMicroBenchN)
	t.Logf("  split (SetMetadataBatch + Close): total %s, mean %s/close",
		splitElapsed.Round(time.Millisecond), (splitElapsed / terminalCloseMicroBenchN).Round(time.Microsecond))
	t.Logf("  fused (Get + guarded update + verify): total %s, mean %s/close",
		fusedElapsed.Round(time.Millisecond), (fusedElapsed / terminalCloseMicroBenchN).Round(time.Microsecond))
	t.Logf("  delta: %+.1f%%", 100*(fusedElapsed.Seconds()-splitElapsed.Seconds())/splitElapsed.Seconds())
}
