package targetscope

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// fakeGetter is a minimal BeadGetter that serves the beads it knows and returns
// a caller-chosen error for everything else — so a test can tell a not-found
// ancestor apart from a transport failure, the exact distinction the inherited
// walk now makes.
type fakeGetter struct {
	byID    map[string]beads.Bead
	missErr error
}

func (f fakeGetter) Get(id string) (beads.Bead, error) {
	if b, ok := f.byID[id]; ok {
		return b, nil
	}
	return beads.Bead{}, f.missErr
}

func unscoped(id string, meta map[string]string) beads.Bead {
	return beads.Bead{ID: id, Metadata: meta}
}

// A dangling gc.root_bead_id (the ancestor was cleaned up) must resolve ABSENT,
// not INVALID: an existing unscoped bead with a stale lineage pointer keeps its
// legacy claim/reconcile behavior. This is the regression guard for the flip
// that folding not-found into the transport bucket would cause.
func TestResolveInheritedDanglingRootStaysAbsent(t *testing.T) {
	store := fakeGetter{byID: map[string]beads.Bead{}, missErr: beads.ErrNotFound}
	bead := unscoped("stage", map[string]string{beadmeta.RootBeadIDMetadataKey: "gone-root"})
	if res := ResolveInherited(store, bead); res.State != StateAbsent {
		t.Fatalf("dangling root: state = %s (reason=%v), want absent", res.State, res.Reason)
	}
}

// A dangling ParentID must likewise be skipped, not fail the walk.
func TestResolveInheritedDanglingParentStaysAbsent(t *testing.T) {
	store := fakeGetter{byID: map[string]beads.Bead{}, missErr: beads.ErrNotFound}
	bead := beads.Bead{ID: "stage", ParentID: "gone-parent"}
	if res := ResolveInherited(store, bead); res.State != StateAbsent {
		t.Fatalf("dangling parent: state = %s (reason=%v), want absent", res.State, res.Reason)
	}
}

// A TRANSPORT error mid-walk stays fail-closed (INVALID) — the not-found
// exception must not weaken the guard against a transient store failure
// silently unlocking the cwd stamp.
func TestResolveInheritedTransportErrorIsInvalid(t *testing.T) {
	store := fakeGetter{byID: map[string]beads.Bead{}, missErr: errors.New("dial tcp: i/o timeout")}
	bead := unscoped("stage", map[string]string{beadmeta.RootBeadIDMetadataKey: "unreadable-root"})
	res := ResolveInherited(store, bead)
	if res.State != StateInvalid {
		t.Fatalf("transport error: state = %s, want invalid", res.State)
	}
	if !errors.Is(res.Reason, ErrInvalidScope) {
		t.Fatalf("transport error reason = %v, want wrapped ErrInvalidScope", res.Reason)
	}
}

// A dangling ParentID must be skipped so the walk still reaches a VALID scope on
// the root chain — not-found on one link does not abort the whole walk.
func TestResolveInheritedDanglingParentFallsThroughToRootScope(t *testing.T) {
	rootScope, err := Marshal(Scope{V: SchemaVersion, Branch: "release"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	root := unscoped("root", map[string]string{beadmeta.TargetScopeMetadataKey: rootScope})
	store := fakeGetter{byID: map[string]beads.Bead{"root": root}, missErr: beads.ErrNotFound}
	bead := beads.Bead{ID: "stage", ParentID: "gone-parent", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "root"}}
	res := ResolveInherited(store, bead)
	if res.State != StateValid || res.Scope.Branch != "release" {
		t.Fatalf("dangling parent + scoped root: state=%s branch=%q, want valid/release", res.State, res.Scope.Branch)
	}
}

// Control: a clean unscoped bead with no ancestor references resolves absent.
func TestResolveInheritedCleanUnscopedIsAbsent(t *testing.T) {
	store := fakeGetter{byID: map[string]beads.Bead{}, missErr: beads.ErrNotFound}
	if res := ResolveInherited(store, beads.Bead{ID: "lone"}); res.State != StateAbsent {
		t.Fatalf("clean unscoped: state = %s, want absent", res.State)
	}
}
