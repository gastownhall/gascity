package tmux

import (
	"testing"
	"time"
)

// TestSessionRoster locks in the batched-but-correctness-preserving contract:
// Attached folds into the single list-sessions -F exec (safe -- same
// underlying tmux state as the per-call IsAttached path), but LastActivity
// is filled via the existing GetSessionActivity per session (in sorted name
// order, for deterministic call sequencing) rather than trusting that same
// exec's raw #{session_activity} field, which is documented-stale for
// detached sessions (see rawSessionActivity).
func TestSessionRoster(t *testing.T) {
	exec := &fakeExecutor{
		outs: []string{
			"sess-b|0\nsess-a|1", // list-sessions -F, deliberately unsorted
			"1500",               // list-windows for sess-a (sorted first)
			"1000\n2000",         // list-windows for sess-b (sorted second)
		},
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: exec}

	roster, err := tm.SessionRoster()
	if err != nil {
		t.Fatalf("SessionRoster: %v", err)
	}
	if len(roster) != 2 {
		t.Fatalf("len(roster) = %d, want 2: %+v", len(roster), roster)
	}

	a, ok := roster["sess-a"]
	if !ok {
		t.Fatal("sess-a missing from roster")
	}
	if !a.Attached {
		t.Error("sess-a.Attached = false, want true")
	}
	if !a.LastActivity.Equal(time.Unix(1500, 0)) {
		t.Errorf("sess-a.LastActivity = %v, want %v", a.LastActivity, time.Unix(1500, 0))
	}

	b, ok := roster["sess-b"]
	if !ok {
		t.Fatal("sess-b missing from roster")
	}
	if b.Attached {
		t.Error("sess-b.Attached = true, want false")
	}
	if !b.LastActivity.Equal(time.Unix(2000, 0)) {
		t.Errorf("sess-b.LastActivity = %v, want %v", b.LastActivity, time.Unix(2000, 0))
	}

	wantCalls := [][]string{
		{"-u", "-L", "x", "list-sessions", "-F", "#{session_name}|#{session_attached}"},
		{"-u", "-L", "x", "list-windows", "-t", "sess-a", "-F", "#{window_activity}"},
		{"-u", "-L", "x", "list-windows", "-t", "sess-b", "-F", "#{window_activity}"},
	}
	if len(exec.calls) != len(wantCalls) {
		t.Fatalf("len(calls) = %d, want %d: %v", len(exec.calls), len(wantCalls), exec.calls)
	}
	for i, want := range wantCalls {
		got := exec.calls[i]
		if len(got) != len(want) {
			t.Fatalf("calls[%d] = %v, want %v", i, got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("calls[%d][%d] = %q, want %q", i, j, got[j], want[j])
			}
		}
	}
}

// TestSessionRoster_NoServerReturnsEmpty mirrors GetSessionSet's absorption
// of ErrNoServer into an empty result: no tmux server means no sessions, not
// an error the caller must special-case.
func TestSessionRoster_NoServerReturnsEmpty(t *testing.T) {
	exec := &fakeExecutor{errs: []error{ErrNoServer}}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: exec}

	roster, err := tm.SessionRoster()
	if err != nil {
		t.Fatalf("SessionRoster: %v", err)
	}
	if len(roster) != 0 {
		t.Fatalf("len(roster) = %d, want 0: %+v", len(roster), roster)
	}
}

// TestSessionRoster_AbsentNameIsNotRunning locks in the documented contract
// on [runtime.SessionRosterProvider]: a name absent from the map is not
// running.
func TestSessionRoster_AbsentNameIsNotRunning(t *testing.T) {
	exec := &fakeExecutor{
		outs: []string{"sess-a|0", "1000"},
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: exec}

	roster, err := tm.SessionRoster()
	if err != nil {
		t.Fatalf("SessionRoster: %v", err)
	}
	if _, ok := roster["sess-nonexistent"]; ok {
		t.Fatal("roster contains a session that was never listed")
	}
}
