package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func tickTestBead(id, name, state string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Status: "open",
		Type:   "session",
		Metadata: beads.StringMap{
			"session_name": name,
			"state":        state,
		},
	}
}

// TestNewReconcileTickMatchesProjection pins that the tick snapshot is built
// byte-identically to a fresh projection of each bead, in topo order.
func TestNewReconcileTickMatchesProjection(t *testing.T) {
	ordered := []beads.Bead{
		tickTestBead("s-1", "alpha", "awake"),
		tickTestBead("s-2", "beta", "asleep"),
		tickTestBead("s-3", "gamma", "creating"),
	}
	tick := newReconcileTick(ordered)

	if len(tick.orderedIDs) != len(ordered) {
		t.Fatalf("orderedIDs len = %d, want %d", len(tick.orderedIDs), len(ordered))
	}
	for i := range ordered {
		if tick.orderedIDs[i] != ordered[i].ID {
			t.Errorf("orderedIDs[%d] = %q, want %q", i, tick.orderedIDs[i], ordered[i].ID)
		}
		want := sessionpkg.InfoFromPersistedBead(ordered[i])
		if got := tick.infoByID[ordered[i].ID]; !reflect.DeepEqual(got, want) {
			t.Errorf("infoByID[%q] = %+v, want %+v", ordered[i].ID, got, want)
		}
	}
}

// TestReconcileTickApplyMatchesRawFold is the property test for the mutator:
// tick.apply / tick.markClosed must fold the snapshot identically to applying
// the same operation directly on a fresh projection, and the stored entry must
// equal the returned Info. This is the coherence guarantee that the front door
// enforces at every fold site (store == raw == snapshot, with store/raw
// performed by the caller's write helper).
func TestReconcileTickApplyMatchesRawFold(t *testing.T) {
	base := tickTestBead("s-1", "alpha", "creating")
	patches := []sessionpkg.MetadataPatch{
		{"state": "awake"},
		{"state": "asleep", "sleep_reason": "drained"},
		{"pending_create_claim": "", "pending_create_started_at": ""},
		{"session_name": "renamed"},
	}

	for _, patch := range patches {
		tick := newReconcileTick([]beads.Bead{base})
		want := sessionpkg.InfoFromPersistedBead(base).ApplyPatch(patch)
		got := tick.apply(base.ID, patch)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("apply(%v) returned %+v, want %+v", map[string]string(patch), got, want)
		}
		if stored := tick.infoByID[base.ID]; !reflect.DeepEqual(stored, want) {
			t.Errorf("apply(%v) stored %+v, want %+v", map[string]string(patch), stored, want)
		}
	}

	// markClosed folds identically to a direct MarkClosed on the projection.
	tick := newReconcileTick([]beads.Bead{base})
	wantClosed := sessionpkg.InfoFromPersistedBead(base).MarkClosed()
	gotClosed := tick.markClosed(base.ID)
	if !reflect.DeepEqual(gotClosed, wantClosed) {
		t.Errorf("markClosed returned %+v, want %+v", gotClosed, wantClosed)
	}
	if stored := tick.infoByID[base.ID]; !reflect.DeepEqual(stored, wantClosed) {
		t.Errorf("markClosed stored %+v, want %+v", stored, wantClosed)
	}
}

// TestReconcileTickApplyResultMatchesApplyTo pins that applyResult folds a
// drainAckFinalizeResult identically to calling result.applyTo on the snapshot
// entry.
func TestReconcileTickApplyResultMatchesApplyTo(t *testing.T) {
	base := tickTestBead("s-1", "alpha", "awake")
	res := drainAckFinalizeResult{batch: map[string]string{"state": "asleep"}, closed: true}

	tick := newReconcileTick([]beads.Bead{base})
	want := res.applyTo(sessionpkg.InfoFromPersistedBead(base))
	got := tick.applyResult(base.ID, res)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applyResult returned %+v, want %+v", got, want)
	}
	if stored := tick.infoByID[base.ID]; !reflect.DeepEqual(stored, want) {
		t.Errorf("applyResult stored %+v, want %+v", stored, want)
	}
}

// infoByIDBareAssign matches a direct assignment into a bare infoByID map —
// `infoByID[<expr>] = <expr>` (but not `==`) — the open-coded fold the mutator
// replaces.
var infoByIDBareAssign = regexp.MustCompile(`\binfoByID\[[^\]]*\]\s*=[^=]`)

// TestReconcileTickFoldFrontDoor forbids reintroducing a direct
// `infoByID[...] =` fold in session_reconciler.go: every mutation of the tick
// snapshot must route through the reconcileTick front door (apply / applyResult
// / markClosed) so a forgotten fold cannot silently desync the cross-session
// min-floor / awake / drain scans from the store and raw bead. The only place a
// bare `t.infoByID[...] =` write is allowed is reconcile_tick.go itself.
func TestReconcileTickFoldFrontDoor(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(currentFile), "session_reconciler.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		code := line
		if idx := strings.Index(code, "//"); idx >= 0 {
			code = code[:idx] // strip line/inline comment
		}
		if infoByIDBareAssign.MatchString(code) {
			t.Errorf("session_reconciler.go:%d writes infoByID directly (%q); route the fold through the reconcileTick front door (tick.apply / tick.applyResult / tick.markClosed) instead", i+1, strings.TrimSpace(line))
		}
	}
}
