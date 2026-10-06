package storebinding

// The generic beads adapter answers the graph contract over whatever canonical
// store it was handed, and some of those stores cannot serve every method. The
// decision it makes in that case has to be observable: an unobserved
// capability veto is the failure mode this program has produced repeatedly —
// a caller asks for readiness, gets an error that looks like a store fault,
// and nothing in the composition ever said the class was incomplete.
//
// These tests pin both halves. Through a store without the capability the
// adapter returns a typed ErrBeadsAdapterCapability rather than a silent empty
// answer; through a store that has it, the adapter delegates. The deployed
// SQLite Graph front door takes the third route — it implements the missing
// methods rather than vetoing them — and that is pinned next to the front door
// itself, in internal/storebinding/sqlite.

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func graphAdapterOver(t *testing.T, store beads.Store) GraphStore {
	t.Helper()
	adapters, err := NewBeadsAdapters(store, BeadsAdapterIdentity{OpenerID: "test", ComponentID: "graph", PhysicalID: "graph-leg"})
	if err != nil {
		t.Fatalf("NewBeadsAdapters: %v", err)
	}
	return adapters.Graph
}

// narrowStore is a canonical store with no optional capabilities: embedding
// the beads.Store INTERFACE (rather than a concrete store) promotes exactly
// the canonical method set, so ReadyContext, WaitForParentProjection, and
// Claim are genuinely absent — the shape a backend without them really has.
type narrowStore struct{ beads.Store }

// TestBeadsGraphAdapterReportsMissingCapabilities observes every degradation
// the adapter can take. Each of these was previously reachable by a caller and
// asserted by nothing.
func TestBeadsGraphAdapterReportsMissingCapabilities(t *testing.T) {
	graph := graphAdapterOver(t, narrowStore{Store: beads.NewMemStore()})
	created, err := graph.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := graph.ReadyContext(context.Background()); !errors.Is(err, ErrBeadsAdapterCapability) {
		t.Fatalf("ReadyContext over a store without it = %v, want ErrBeadsAdapterCapability", err)
	}
	if err := graph.WaitForParentProjection(context.Background(), created.ID, "", "gc-1"); !errors.Is(err, ErrBeadsAdapterCapability) {
		t.Fatalf("WaitForParentProjection over a store without it = %v, want ErrBeadsAdapterCapability", err)
	}
	claimed, ok, err := graph.Claim(created.ID, "worker")
	if !errors.Is(err, ErrBeadsAdapterCapability) {
		t.Fatalf("Claim over a store without it = %v, want ErrBeadsAdapterCapability", err)
	}
	if ok {
		t.Fatalf("a vetoed Claim reported success: %+v", claimed)
	}
	metadata, ok, err := graph.DepMetadata(created.ID, "gc-1")
	if !errors.Is(err, ErrBeadsAdapterCapability) {
		t.Fatalf("DepMetadata over a store without it = %v, want ErrBeadsAdapterCapability", err)
	}
	if ok || metadata != "" {
		t.Fatalf("a vetoed DepMetadata reported (%q, %v), want no payload", metadata, ok)
	}

	// A veto must not look like a legitimate empty answer. Every one of these
	// would otherwise be indistinguishable from "nothing is ready", "the
	// projection converged", and "somebody else holds it".
	if err := graph.WaitForParentProjection(context.Background(), created.ID, "", "gc-1"); err == nil {
		t.Fatal("WaitForParentProjection reported convergence it never checked")
	}
}

// claimingMemStore is a canonical store that DOES implement the two-argument
// claim, proving the capability probe finds one rather than always vetoing.
type claimingMemStore struct {
	beads.Store
	lastID       string
	lastAssignee string
}

func (s *claimingMemStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	s.lastID, s.lastAssignee = id, assignee
	bead, err := s.Get(id)
	if err != nil {
		return beads.Bead{}, false, err
	}
	return bead, true, nil
}

func TestBeadsGraphAdapterDelegatesClaimWhenAvailable(t *testing.T) {
	backing := &claimingMemStore{Store: beads.NewMemStore()}
	graph := graphAdapterOver(t, backing)
	created, err := graph.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	claimed, ok, err := graph.Claim(created.ID, "worker")
	if err != nil || !ok {
		t.Fatalf("Claim = (%+v, %v, %v), want a delegated success", claimed, ok, err)
	}
	if backing.lastID != created.ID || backing.lastAssignee != "worker" {
		t.Fatalf("adapter passed (%q, %q) to the store, want (%q, %q)", backing.lastID, backing.lastAssignee, created.ID, "worker")
	}
}
