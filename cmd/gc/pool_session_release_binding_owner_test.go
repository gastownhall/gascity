package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The maintainer-city shape behind ga-b0o6a: a rig-routed graph-class step whose
// row lives in the relocated class binding, not in the rig work ledger its
// gc.routed_to prefix names.
const (
	bindingOrphanSeat     = "test-city--gc.implementation-worker-1"
	bindingOrphanTemplate = "beads/gc.implementation-worker"
)

func bindingOrphanConfig(t *testing.T) *config.City {
	t.Helper()
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "beads", Path: filepath.Join(t.TempDir(), "beads")}},
		Agents: []config.Agent{{
			Name:              "gc.implementation-worker",
			Dir:               "beads",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		}},
	}
}

// bindingOrphanClaim is the claim row: a graph-class step, in_progress under the
// dead seat's identity, routed to the rig-scoped pool agent.
func bindingOrphanClaim() beads.Bead {
	return beads.Bead{
		ID:       "gcg-1",
		Title:    "graph step held by a dead seat",
		Type:     "task",
		Status:   "in_progress",
		Assignee: bindingOrphanSeat,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: bindingOrphanTemplate,
			"gc.root_store_ref":          "rig:beads",
		},
	}
}

func bindingOrphanSessionInfo() sessionpkg.Info {
	return seedSessionInfo(beads.Bead{
		ID:     "gcs-1",
		Status: "open",
		Metadata: map[string]string{
			"template":     bindingOrphanTemplate,
			"session_name": bindingOrphanSeat,
			"state":        "asleep",
			"pool_managed": "true",
		},
	})
}

// TestReleaseConfirmedOrphanSessionWork_ReleasesBindingResidentClaimThroughAlignedStore
// is the repro. The orphan-close tie-break resolved each candidate's owner store
// from its gc.routed_to prefix, which names a WORK ledger. On a split city the
// claim is a graph-class row that lives only in the class binding, so the rig
// ledger answers "no such bead", the release is skipped, and the close guard
// keeps refusing — the ga-jrnou wedge, reopened for every binding-resident row.
//
// The census already read the row through the binding. That leg is the row's
// owner, and passing it index-aligned is the whole fix.
func TestReleaseConfirmedOrphanSessionWork_ReleasesBindingResidentClaimThroughAlignedStore(t *testing.T) {
	cfg := bindingOrphanConfig(t)
	work := bindingOrphanClaim()
	binding := beads.NewMemStoreFrom(1, []beads.Bead{work}, nil)
	rigStores := map[string]beads.Store{"beads": beads.NewMemStore()}

	released := releaseConfirmedOrphanSessionWork(
		cfg, binding, rigStores, []beads.Bead{work}, []beads.Store{binding}, bindingOrphanSessionInfo(),
	)

	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %#v, want the binding-resident claim %q released through its aligned leg", released, work.ID)
	}
	got, err := binding.Get(work.ID)
	if err != nil {
		t.Fatalf("binding.Get(%s): %v", work.ID, err)
	}
	if got.Assignee != "" || got.Status != "open" {
		t.Fatalf("claim after release: status=%q assignee=%q, want open/unassigned so pool demand can re-route it", got.Status, got.Assignee)
	}
}

// TestReleaseConfirmedOrphanSessionWork_NilAlignedStoresKeepsRoutedFallback is
// the mutation probe for the test above AND the compatibility contract: with no
// aligned slice the resolver falls back to the gc.routed_to prefix exactly as it
// always has. That fallback is what strands the row here — which is why the
// aligned slice, not some new probe, is the thing under test.
func TestReleaseConfirmedOrphanSessionWork_NilAlignedStoresKeepsRoutedFallback(t *testing.T) {
	cfg := bindingOrphanConfig(t)
	work := bindingOrphanClaim()
	binding := beads.NewMemStoreFrom(1, []beads.Bead{work}, nil)
	rigStores := map[string]beads.Store{"beads": beads.NewMemStore()}

	released := releaseConfirmedOrphanSessionWork(
		cfg, binding, rigStores, []beads.Bead{work}, nil, bindingOrphanSessionInfo(),
	)

	if len(released) != 0 {
		t.Fatalf("released = %#v, want none — with no aligned leg the routed prefix names the rig ledger, which does not hold this row", released)
	}
	got, err := binding.Get(work.ID)
	if err != nil {
		t.Fatalf("binding.Get(%s): %v", work.ID, err)
	}
	if got.Status != "in_progress" || got.Assignee != bindingOrphanSeat {
		t.Fatalf("claim = status %q assignee %q, want the untouched in_progress claim — the fallback must not write through a store that never held the row", got.Status, got.Assignee)
	}
}

// TestReleaseConfirmedOrphanSessionWork_LegacyRigStoreUnchanged keeps the
// pre-existing lane intact: a row that really does live in the rig ledger its
// route names is still released when no aligned slice is supplied. Every caller
// that passes nothing behaves exactly as it did.
func TestReleaseConfirmedOrphanSessionWork_LegacyRigStoreUnchanged(t *testing.T) {
	cfg := bindingOrphanConfig(t)
	work := bindingOrphanClaim()
	rigStore := beads.NewMemStoreFrom(1, []beads.Bead{work}, nil)
	rigStores := map[string]beads.Store{"beads": rigStore}

	released := releaseConfirmedOrphanSessionWork(
		cfg, beads.NewMemStore(), rigStores, []beads.Bead{work}, nil, bindingOrphanSessionInfo(),
	)

	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %#v, want the rig-resident claim %q released through the routed fallback", released, work.ID)
	}
	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("rigStore.Get(%s): %v", work.ID, err)
	}
	if got.Assignee != "" || got.Status != "open" {
		t.Fatalf("claim after release: status=%q assignee=%q, want open/unassigned", got.Status, got.Assignee)
	}
}

// TestReleaseConfirmedOrphanSessionWork_MisalignedStoresSkipTheUnalignedBead
// pins the alignment invariant at the function boundary: a short slice is never
// indexed past its end and never re-used for a bead it does not describe. The
// bead with no aligned leg is skipped, not guessed at.
func TestReleaseConfirmedOrphanSessionWork_MisalignedStoresSkipTheUnalignedBead(t *testing.T) {
	cfg := bindingOrphanConfig(t)
	first := bindingOrphanClaim()
	second := bindingOrphanClaim()
	second.ID = "gcg-2"
	binding := beads.NewMemStoreFrom(2, []beads.Bead{first, second}, nil)
	rigStores := map[string]beads.Store{"beads": beads.NewMemStore()}

	released := releaseConfirmedOrphanSessionWork(
		cfg, binding, rigStores, []beads.Bead{first, second}, []beads.Store{binding}, bindingOrphanSessionInfo(),
	)

	if len(released) != 1 || released[0].ID != first.ID {
		t.Fatalf("released = %#v, want only %q — a bead past the end of the aligned slice has no known owner and must be skipped", released, first.ID)
	}
	got, err := binding.Get(second.ID)
	if err != nil {
		t.Fatalf("binding.Get(%s): %v", second.ID, err)
	}
	if got.Status != "in_progress" || got.Assignee != bindingOrphanSeat {
		t.Fatalf("claim %s = status %q assignee %q, want untouched", second.ID, got.Status, got.Assignee)
	}
}
