package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// proveAbsentCity builds the plain file-store city the proof rows run against:
// a city scope store and one rig scope store, both real, no relocated classes —
// the topology every candidate the proof enumerates is drawn from.
func proveAbsentCity(t *testing.T) (cityDir, rigDir string, cfg *config.City) {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	cityDir = t.TempDir()
	rigDir = filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating the rig dir: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("laying out the city store: %v", err)
	}
	writeProviderAwareTestCity(t, cityDir, `[workspace]
name = "prove-absent"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "FE"
`)
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("persisting the city store: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("persisting the rig store: %v", err)
	}
	loaded, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("loading the city config: %v", err)
	}
	return cityDir, rigDir, loaded
}

// TestHookCityClaimOpsCarriesProveAbsent pins the wiring: the claim ops a hook
// invocation builds carry the absence proof, so a claim-time purge verdict has
// evidence to authorize it. The seam spans the invocation's city — here an
// empty city store and an empty rig store, so an id no store holds is proven
// absent, and the answer comes from the real topology, not a test fake.
func TestHookCityClaimOpsCarriesProveAbsent(t *testing.T) {
	cityDir, rigDir, cfg := proveAbsentCity(t)
	ops := newHookCity(cityDir, cfg).claimOps()
	if ops.ProveAbsent == nil {
		t.Fatal("hookCity.claimOps() left ProveAbsent nil: a claim-time purge verdict has no evidence to authorize it")
	}
	proven, err := ops.ProveAbsent(context.Background(), rigDir, []string{"gc-never-created"})
	if err != nil {
		t.Fatalf("proving gc-never-created absent: %v", err)
	}
	if !proven {
		t.Fatal("absence proof over an empty city and rig store = not proven, want proven")
	}
}

// faultableGetStore records every Get against its own id so a row can assert
// which stores the proof actually read.
type faultableGetStore struct {
	beads.Store
	id      string
	gets    int
	present bool
	readErr error
}

func (s *faultableGetStore) Get(id string) (beads.Bead, error) {
	if id == s.id {
		s.gets++
		if s.readErr != nil {
			return beads.Bead{}, s.readErr
		}
		if s.present {
			return beads.Bead{ID: id}, nil
		}
	}
	return s.Store.Get(id)
}

// injectProveAbsentOpener swaps the opener for one answering from a per-dir
// table and restores it at the end of the test.
func injectProveAbsentOpener(t *testing.T, open func(dir string) (beads.Store, error)) {
	t.Helper()
	prev := hookProveAbsentOpener
	hookProveAbsentOpener = func(dir, _ string, _ *config.City) (absentProbeStore, error) {
		return open(dir)
	}
	t.Cleanup(func() { hookProveAbsentOpener = prev })
}

// proveAbsentLogicCity is the hand-built config rows with injected openers
// run against: a city and one rig, no by-id furniture beyond a plain
// city.toml, so the rows exercise the proof's own loop and nothing else.
func proveAbsentLogicCity(t *testing.T) (cityDir, rigDir string, cfg *config.City) {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	cityDir = t.TempDir()
	rigDir = filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating the rig dir: %v", err)
	}
	// The rig scope must carry the file-store contract (.gc/beads.json) even
	// though the injected openers never read it: without the marker
	// convoyStoreCandidates collapses the city to legacyCityOnly, the rig
	// scope is not a candidate, and the rows would never probe it.
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("marking the rig scope as file-backed: %v", err)
	}
	writeProviderAwareTestCity(t, cityDir, `[workspace]
name = "prove-absent"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "FE"
`)
	cfg = &config.City{Rigs: []config.Rig{{Name: "frontend", Path: "frontend", Prefix: "FE"}}}
	return cityDir, rigDir, cfg
}

// TestProveAbsentDisprovenByAHitInAnyStore pins the rule the verdict rests
// on: one live row anywhere — the rig's ledger, the city's, or the claim's
// own scope, enumerated or not — keeps the workflow's input alive.
func TestProveAbsentDisprovenByAHitInAnyStore(t *testing.T) {
	cityDir, rigDir, cfg := proveAbsentLogicCity(t)

	// A hit in the rig's store.
	injectProveAbsentOpener(t, func(dir string) (beads.Store, error) {
		store := beads.NewMemStore()
		if dir == rigDir {
			return &faultableGetStore{Store: store, id: "gc-sought", present: true}, nil
		}
		return store, nil
	})
	proven, err := hookProveAbsentAt(cityDir, cfg)(context.Background(), rigDir, []string{"gc-sought"})
	if err != nil {
		t.Fatalf("proving a rig-held id: %v", err)
	}
	if proven {
		t.Fatal("a rig-held id was proven absent; one hit anywhere must disprove")
	}

	// A hit in the claim's own scope, which the candidate enumeration alone
	// does not cover.
	claimDir := filepath.Join(cityDir, "unlisted-scope")
	if err := os.MkdirAll(claimDir, 0o755); err != nil {
		t.Fatalf("creating the claim dir: %v", err)
	}
	injectProveAbsentOpener(t, func(dir string) (beads.Store, error) {
		store := beads.NewMemStore()
		if dir == claimDir {
			return &faultableGetStore{Store: store, id: "gc-sought", present: true}, nil
		}
		return store, nil
	})
	proven, err = hookProveAbsentAt(cityDir, cfg)(context.Background(), claimDir, []string{"gc-sought"})
	if err != nil {
		t.Fatalf("proving an id held only in the claim's own scope: %v", err)
	}
	if proven {
		t.Fatal("an id held in the claim's own scope was proven absent; the claim's store must be probed beside the candidates")
	}
}

// TestProveAbsentTreatsQuiescentAndFaultedCoverageAsNotProven pins the two
// ways a store does not answer: an intentionally unread scope (a suspended
// rig's nil handle) is not-proven with no error and no read, while a store
// that will not open or read is an operational error carrying id and store
// context — either way the verdict stays pending, never purged.
func TestProveAbsentTreatsQuiescentAndFaultedCoverageAsNotProven(t *testing.T) {
	cityDir, rigDir, cfg := proveAbsentLogicCity(t)

	// A suspended scope's nil handle: quiescence, not a fault.
	injectProveAbsentOpener(t, func(dir string) (beads.Store, error) {
		if dir == rigDir {
			return nil, nil
		}
		return &faultableGetStore{Store: beads.NewMemStore(), id: "gc-sought"}, nil
	})
	proven, err := hookProveAbsentAt(cityDir, cfg)(context.Background(), "", []string{"gc-sought"})
	if err != nil {
		t.Fatalf("proving over a suspended rig: %v", err)
	}
	if proven {
		t.Fatal("an unread suspended scope was treated as proven absent; incomplete coverage must read as not proven")
	}

	// A store that will not open.
	openFault := errors.New("store offline")
	injectProveAbsentOpener(t, func(dir string) (beads.Store, error) {
		if dir == rigDir {
			return nil, openFault
		}
		return beads.NewMemStore(), nil
	})
	proven, err = hookProveAbsentAt(cityDir, cfg)(context.Background(), "", []string{"gc-sought"})
	if err == nil || !strings.Contains(err.Error(), "gc-sought") || !strings.Contains(err.Error(), rigDir) {
		t.Fatalf("proving over a store that will not open = (%v, %v); want an error naming the id and the store dir", proven, err)
	}
	if proven {
		t.Fatal("a faulted open still answered proven; an operational error must never authorize the purge verdict")
	}

	// A store whose read faults.
	readFault := errors.New("disk on fire")
	injectProveAbsentOpener(t, func(dir string) (beads.Store, error) {
		if dir == rigDir {
			return &faultableGetStore{Store: beads.NewMemStore(), id: "gc-sought", readErr: readFault}, nil
		}
		return beads.NewMemStore(), nil
	})
	proven, err = hookProveAbsentAt(cityDir, cfg)(context.Background(), "", []string{"gc-sought"})
	if err == nil || !strings.Contains(err.Error(), "gc-sought") || !strings.Contains(err.Error(), rigDir) {
		t.Fatalf("proving over a faulted read = (%v, %v); want an error naming the id and the store dir", proven, err)
	}
	if proven {
		t.Fatal("a faulted read still answered proven; an operational error must never authorize the purge verdict")
	}
}

// TestProveAbsentRejectsTheProofOnceOneIdSurvives pins the all-of semantics:
// the ids are one proof, so the first id still held — or unprovable — ends
// the run without probing the rest.
func TestProveAbsentRejectsTheProofOnceOneIdSurvives(t *testing.T) {
	cityDir, rigDir, cfg := proveAbsentLogicCity(t)

	second := &faultableGetStore{Store: beads.NewMemStore(), id: "gc-second", present: true}
	injectProveAbsentOpener(t, func(dir string) (beads.Store, error) {
		if dir == rigDir {
			return second, nil
		}
		return &faultableGetStore{Store: beads.NewMemStore(), id: "gc-first"}, nil
	})
	proven, err := hookProveAbsentAt(cityDir, cfg)(context.Background(), rigDir, []string{"gc-first", "gc-second"})
	if err != nil {
		t.Fatalf("proving a partially-purged convoy: %v", err)
	}
	if proven {
		t.Fatal("a convoy with one surviving member was proven fully absent; one survivor must end the proof")
	}
	if second.gets != 1 {
		t.Fatalf("the surviving member was read %d times, want the run to stop at the first survivor", second.gets)
	}
}
