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
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// tickTestBead builds an open, session-shaped bead carrying a session_name and
// state. Fixtures are projected to session.Info through the real store edge
// (sessiontest.SeedBead) — the interior never calls the projection codec.
func tickTestBead(id, name, state string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Status: "open",
		Type:   sessionpkg.BeadType,
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"session_name": name,
			"state":        state,
		},
	}
}

// tickSeedInfos projects each bead to its store-edge Info, in order — the
// row-feed the reconciler hands newReconcileTick.
func tickSeedInfos(t *testing.T, beadsIn ...beads.Bead) []sessionpkg.Info {
	t.Helper()
	infos := make([]sessionpkg.Info, len(beadsIn))
	for i, b := range beadsIn {
		infos[i] = sessiontest.SeedBead(t, b)
	}
	return infos
}

// TestNewReconcileTickMatchesProjection pins that the tick snapshot stores the
// tick's ordered Info feed verbatim, keyed by ID, in topo order — the row-based
// constructor holds the rows' already-projected Info rather than re-cracking a
// bead (there is no codec call in the interior).
func TestNewReconcileTickMatchesProjection(t *testing.T) {
	ordered := tickSeedInfos(t,
		tickTestBead("s-1", "alpha", "awake"),
		tickTestBead("s-2", "beta", "asleep"),
		tickTestBead("s-3", "gamma", "creating"),
	)
	tick := newReconcileTick(ordered)

	if len(tick.orderedIDs) != len(ordered) {
		t.Fatalf("orderedIDs len = %d, want %d", len(tick.orderedIDs), len(ordered))
	}
	for i := range ordered {
		if tick.orderedIDs[i] != ordered[i].ID {
			t.Errorf("orderedIDs[%d] = %q, want %q", i, tick.orderedIDs[i], ordered[i].ID)
		}
		if got := tick.infoByID[ordered[i].ID]; !reflect.DeepEqual(got, ordered[i]) {
			t.Errorf("infoByID[%q] = %+v, want %+v", ordered[i].ID, got, ordered[i])
		}
	}
}

// TestReconcileTickApplyMatchesRawFold is the property test for the patch
// mutators: tick.apply / tick.markClosed must fold the snapshot identically to
// applying the same operation directly on the seeded Info, and the stored entry
// must equal the returned Info. This is the coherence guarantee that the front
// door enforces at every fold site (store == snapshot, with the store write
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
		baseInfo := sessiontest.SeedBead(t, base)
		tick := newReconcileTick([]sessionpkg.Info{baseInfo})
		want := baseInfo.ApplyPatch(patch)
		got := tick.apply(baseInfo.ID, patch)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("apply(%v) returned %+v, want %+v", map[string]string(patch), got, want)
		}
		if stored := tick.infoByID[baseInfo.ID]; !reflect.DeepEqual(stored, want) {
			t.Errorf("apply(%v) stored %+v, want %+v", map[string]string(patch), stored, want)
		}
	}

	// markClosed folds identically to a direct MarkClosed on the seeded Info.
	baseInfo := sessiontest.SeedBead(t, base)
	tick := newReconcileTick([]sessionpkg.Info{baseInfo})
	wantClosed := baseInfo.MarkClosed()
	gotClosed := tick.markClosed(baseInfo.ID)
	if !reflect.DeepEqual(gotClosed, wantClosed) {
		t.Errorf("markClosed returned %+v, want %+v", gotClosed, wantClosed)
	}
	if stored := tick.infoByID[baseInfo.ID]; !reflect.DeepEqual(stored, wantClosed) {
		t.Errorf("markClosed stored %+v, want %+v", stored, wantClosed)
	}
}

// TestReconcileTickApplyResultMatchesApplyTo pins that applyResult folds a
// drainAckFinalizeResult identically to calling result.applyTo on the snapshot
// entry.
func TestReconcileTickApplyResultMatchesApplyTo(t *testing.T) {
	base := tickTestBead("s-1", "alpha", "awake")
	res := drainAckFinalizeResult{batch: sessionpkg.MetadataPatch{"state": "asleep"}, closed: true}

	baseInfo := sessiontest.SeedBead(t, base)
	tick := newReconcileTick([]sessionpkg.Info{baseInfo})
	want := res.applyTo(baseInfo)
	got := tick.applyResult(baseInfo.ID, res)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applyResult returned %+v, want %+v", got, want)
	}
	if stored := tick.infoByID[baseInfo.ID]; !reflect.DeepEqual(stored, want) {
		t.Errorf("applyResult stored %+v, want %+v", stored, want)
	}
}

// TestReconcileTickSet pins the set mutator — the front door for the tree's
// write-returns-Info fold shapes, where a write helper already persisted the
// store write and returned the coherent post-write Info as a plain value. set
// records it verbatim and returns it.
func TestReconcileTickSet(t *testing.T) {
	base := tickTestBead("s-1", "alpha", "awake")
	baseInfo := sessiontest.SeedBead(t, base)
	tick := newReconcileTick([]sessionpkg.Info{baseInfo})

	replacement := baseInfo.ApplyPatch(sessionpkg.MetadataPatch{"state": "asleep", "sleep_reason": "idle"})
	got := tick.set(baseInfo.ID, replacement)
	if !reflect.DeepEqual(got, replacement) {
		t.Errorf("set returned %+v, want %+v", got, replacement)
	}
	if stored := tick.infoByID[baseInfo.ID]; !reflect.DeepEqual(stored, replacement) {
		t.Errorf("set stored %+v, want %+v", stored, replacement)
	}
}

// infoByIDBareAssign matches a direct assignment into a bare infoByID map —
// `infoByID[<expr>] = <expr>` (but not `==`) — the open-coded fold the mutators
// replace. Faithful to origin/main's guard regex: it deliberately does NOT match
// the atomic tuple form `infoByID[id], _ = sessFront.ApplyPatchInfo(...)`, whose
// fold is inherent to the call's return and cannot be forgotten.
var infoByIDBareAssign = regexp.MustCompile(`\binfoByID\[[^\]]*\]\s*=[^=]`)

// infoByIDTupleAssign matches the tuple assignment form (`infoByID[id], _ = ...`),
// which the single-assign regex above cannot see (the char after `]` is `,`).
// Anchored to line start AND requiring a single `=` later on the same line: a
// tuple-assignment LHS begins the statement under gofmt and carries its `=` on
// that line, while argument-list READS of infoByID[...] appear either mid-line
// (`f(infoByID[id], x)`) or as a wrapped arg line with no `=`
// (`\tinfoByID[id],`) and must not trip the guard. The atomic store-write+fold shape that used this form now routes
// through tick.applyStore, so ANY tuple write into the bare map is a violation.
var infoByIDTupleAssign = regexp.MustCompile(`^\s*infoByID\[[^\]]*\]\s*,[^=]*=[^=]`)

// TestReconcileTickFoldFrontDoor forbids reintroducing a direct
// `infoByID[...] =` fold in session_reconciler.go: every manual mutation of the
// tick snapshot must route through the reconcileTick front door (apply /
// applyResult / markClosed / set / applyStore / applyOptimistic) so a forgotten
// fold cannot silently desync the cross-session min-floor / awake / drain scans
// from the store. The only place a bare `t.infoByID[...] =` write is allowed is
// reconcile_tick.go itself.
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
		if infoByIDBareAssign.MatchString(code) || infoByIDTupleAssign.MatchString(code) {
			t.Errorf("session_reconciler.go:%d writes infoByID directly (%q); route the fold through the reconcileTick front door (tick.apply / tick.applyResult / tick.markClosed / tick.set / tick.applyStore / tick.applyOptimistic) instead", i+1, strings.TrimSpace(line))
		}
	}
}
