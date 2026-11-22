package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A due key that no open session lists is counted orphaned once per key per
// pass, however many items share it, and logged with its resolution; a padded
// key the claim's trimmed match gives to an open session is not an orphan, and
// not-matched still counts only the open session with nothing it matches.
func TestDispatchAllQueuedNudgesCountsOrphanedKeys(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_DEBUG", "1")

	dir := t.TempDir()
	due := time.Now().Add(-time.Minute)
	for _, item := range []queuedNudge{
		newQueuedNudge("gc-gone", "first", due),
		newQueuedNudge("gc-gone", "second", due),
		newQueuedNudge("gc-gone-2", "third", due),
		newQueuedNudge(" helper ", "padded", due),
	} {
		if err := enqueueQueuedNudge(dir, item); err != nil {
			t.Fatalf("enqueueQueuedNudge(%q): %v", item.Agent, err)
		}
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{{
		ID:     "helper-session",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "helper-session",
			"agent_name":   "helper",
			"template":     "helper",
		},
	}})

	var debugOut strings.Builder
	if _, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, runtime.NewFake(), snapshot, &debugOut); err != nil {
		t.Fatalf("dispatch pass 1: %v", err)
	}
	state, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := state.DispatchSkips["orphaned"]; got != 2 {
		t.Fatalf("pass 1: DispatchSkips[orphaned] = %d, want 2 (gc-gone, gc-gone-2); full map %#v", got, state.DispatchSkips)
	}
	if got := state.DispatchSkips["not-matched"]; got != 1 {
		t.Fatalf("pass 1: DispatchSkips[not-matched] = %d, want 1 (the helper session's exact match misses the padded key); full map %#v", got, state.DispatchSkips)
	}
	out := debugOut.String()
	for _, want := range []string{"route=skip reason=orphaned", "agent=gc-gone ", "agent=gc-gone-2 ", "resolution=unfenced", "items=2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "reason=orphaned agent= helper") || strings.Contains(out, "reason=orphaned agent=helper") {
		t.Fatalf("the padded helper key was reported orphaned:\n%s", out)
	}

	if _, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, runtime.NewFake(), snapshot, nil); err != nil {
		t.Fatalf("dispatch pass 2: %v", err)
	}
	state, err = nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := state.DispatchSkips["orphaned"]; got != 4 {
		t.Fatalf("pass 2: DispatchSkips[orphaned] = %d, want 4; full map %#v", got, state.DispatchSkips)
	}
	if len(state.Pending) != 4 {
		t.Fatalf("pending = %d, want all 4 items still queued", len(state.Pending))
	}
}
