package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// routedBridgeStore answers the routed bridge's per-identity listings from
// fixed candidates. Each listing announces itself on arrived and then waits
// for release, so a test can see how many are in flight at once.
type routedBridgeStore struct {
	beads.Store
	candidates map[string][]beads.Bead
	failing    map[string]bool
	arrived    chan string
	release    chan struct{}
}

func (s *routedBridgeStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	identity, routed := q.Metadata[beadmeta.RoutedToMetadataKey]
	if !routed {
		return s.Store.List(q)
	}
	s.arrived <- identity
	<-s.release
	if s.failing[identity] {
		return nil, errors.New("routed scan failed")
	}
	return s.candidates[identity], nil
}

func newRoutedBridgeStore(t *testing.T, identities int) (*routedBridgeStore, *beads.MemStore) {
	t.Helper()
	graph := beads.NewMemStore()
	return &routedBridgeStore{
		Store:      beads.NewMemStore(),
		candidates: map[string][]beads.Bead{},
		failing:    map[string]bool{},
		arrived:    make(chan string, identities),
		release:    make(chan struct{}),
	}, graph
}

// TestRoutedBridgeListsEveryIdentityAtOnce pins the concurrency the step
// reminder relies on: each identity's gc.routed_to listing is a scan of the
// rig's open beads (about 0.37 s on maintainer-city), and they are independent,
// so all of them are in flight together rather than one after another.
func TestRoutedBridgeListsEveryIdentityAtOnce(t *testing.T) {
	identities := []string{"alias-1", "name-1", "gcs-1"}
	work, graph := newRoutedBridgeStore(t, len(identities))
	r := stepResolution{work: work, graph: graph, logf: t.Logf}
	done := make(chan *beads.Bead, 1)
	go func() { done <- r.rootViaRoutedBridge(identities) }()
	deadline := time.After(10 * time.Second)
	seen := map[string]bool{}
	for len(seen) < len(identities) {
		select {
		case identity := <-work.arrived:
			seen[identity] = true
		case <-deadline:
			close(work.release)
			<-done
			t.Fatalf("only %d of %d routed listings were in flight together (%v); they must be asked at once", len(seen), len(identities), seen)
		}
	}
	close(work.release)
	if root := <-done; root != nil {
		t.Fatalf("rootViaRoutedBridge = %s, want none: no identity has a routed bridge", root.ID)
	}
}

// TestRoutedBridgeTakesRootsInIdentityOrder pins that asking at once changes no
// answer: the first identity, in the given order, whose candidates resolve to a
// root wins whatever order the listings finish in, and an identity whose
// listing fails is skipped as before.
func TestRoutedBridgeTakesRootsInIdentityOrder(t *testing.T) {
	identities := []string{"failing-alias", "nothing-name", "second", "third"}
	work, graph := newRoutedBridgeStore(t, len(identities))
	rootSecond, err := graph.Create(beads.Bead{Title: "second root", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	rootThird, err := graph.Create(beads.Bead{Title: "third root", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	work.failing["failing-alias"] = true
	work.candidates["failing-alias"] = []beads.Bead{{ID: "src-0", Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: rootThird.ID}}}
	work.candidates["second"] = []beads.Bead{
		{ID: "src-1"},
		{ID: "src-2", Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: rootSecond.ID}},
	}
	work.candidates["third"] = []beads.Bead{{ID: "src-3", Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: rootThird.ID}}}
	close(work.release)
	r := stepResolution{work: work, graph: graph, logf: t.Logf}
	root := r.rootViaRoutedBridge(identities)
	if root == nil || root.ID != rootSecond.ID {
		t.Fatalf("rootViaRoutedBridge = %v, want %s, the root of the first identity in order that has one", root, rootSecond.ID)
	}
}
