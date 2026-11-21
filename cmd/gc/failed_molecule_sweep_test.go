package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// failFirstActivationStore loses the first fenced-graph activation update of a
// sequential instantiation (the Update that clears gc.instantiating) to a Dolt
// serialization conflict, as the #6177 incident did. Embedding beads.Store
// alone hides MemStore's graph-apply capability, so instantiation takes the
// sequential path.
type failFirstActivationStore struct {
	beads.Store
	mu     sync.Mutex
	failed bool
}

func (s *failFirstActivationStore) Update(id string, opts beads.UpdateOpts) error {
	s.mu.Lock()
	if v, ok := opts.Metadata[beadmeta.InstantiatingMetadataKey]; ok && v == "" && !s.failed {
		s.failed = true
		s.mu.Unlock()
		return errors.New("sql commit (regular): Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction")
	}
	s.mu.Unlock()
	return s.Store.Update(id, opts)
}

// closeAllRecorder records every CloseAll batch it forwards.
type closeAllRecorder struct {
	beads.Store
	mu      sync.Mutex
	batches [][]string
}

func (s *closeAllRecorder) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.mu.Lock()
	s.batches = append(s.batches, append([]string(nil), ids...))
	s.mu.Unlock()
	return s.Store.CloseAll(ids, metadata)
}

// TestOrderDispatchFailedInstantiationSweptByWatchdog pins #6177: an order
// dispatch mints with no idempotency key, so when its sequential
// instantiation fails on the first fenced activation the molecule_failed
// chain it leaves has no same-key caller to close it. The failed-molecule
// watchdog closes it once the grace window has passed.
func TestOrderDispatchFailedInstantiationSweptByWatchdog(t *testing.T) {
	cityPath, cfg, a := newGraphOrderFixture(t)
	base := beads.NewMemStore()
	store := &failFirstActivationStore{Store: base}

	var rec memRecorder
	dispatchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &memoryOrderDispatcher{
		aa:                   []orders.Order{a},
		storeFn:              func(execStoreTarget) (beads.Store, error) { return store, nil },
		cfg:                  cfg,
		cityName:             "test-city",
		cityPath:             cityPath,
		rec:                  &rec,
		stderr:               io.Discard,
		maxDispatchesPerTick: 1,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       cancel,
	}
	dispatchGraphOrder(t, m, cityPath)

	if !store.failed {
		t.Fatal("fault injection never fired: no fenced activation update reached the store")
	}
	if !rec.hasType(events.OrderFailed) {
		t.Fatalf("events = %+v, want order.failed for the failed mint", rec.events)
	}
	var chain []beads.Bead
	var sawWispFailed bool
	for _, b := range allBeads(t, base) {
		if b.Metadata[beadmeta.MoleculeFailedMetadataKey] == "true" {
			chain = append(chain, b)
		}
		for _, l := range b.Labels {
			if l == "wisp-failed" {
				sawWispFailed = true
			}
		}
	}
	if !sawWispFailed {
		t.Fatalf("no tracking bead records the wisp-failed outcome; beads = %+v", allBeads(t, base))
	}
	if len(chain) < 2 {
		t.Fatalf("molecule_failed beads = %d, want the root and its step(s); beads = %+v", len(chain), allBeads(t, base))
	}
	for _, b := range chain {
		if b.Status == "closed" {
			t.Fatalf("bead %s closed before the sweep ran; the fixture no longer reproduces the dead chain", b.ID)
		}
		for _, l := range b.Labels {
			if strings.HasPrefix(l, "order-run:") {
				t.Fatalf("bead %s carries %q; a failed mint is stamped on success only", b.ID, l)
			}
		}
	}

	var stderr strings.Builder
	sweepStore := &closeAllRecorder{Store: base}
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test-city",
		cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
		standaloneCityStore: sweepStore,
		stdout:              io.Discard,
		stderr:              &stderr,
		logPrefix:           "gc test",
	}
	cr.runFailedMoleculeSweepWatchdog(cr.cfg, time.Now().Add(failedMoleculeSweepGrace+time.Minute))

	if len(sweepStore.batches) != 1 || len(sweepStore.batches[0]) != len(chain) {
		t.Fatalf("CloseAll batches = %v, want the %d-bead chain closed in one batch", sweepStore.batches, len(chain))
	}
	for _, want := range chain {
		got, err := base.Get(want.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", want.ID, err)
		}
		if got.Status != "closed" {
			t.Errorf("bead %s status = %q after the sweep, want closed", got.ID, got.Status)
		}
		if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped {
			t.Errorf("bead %s gc.outcome = %q, want %q", got.ID, got.Metadata[beadmeta.OutcomeMetadataKey], beadmeta.OutcomeSkipped)
		}
		if got.Metadata["close_reason"] != sourceworkflow.FailedMoleculeSweepCloseReason {
			t.Errorf("bead %s close_reason = %q, want the sweep's", got.ID, got.Metadata["close_reason"])
		}
		if got.Metadata[beadmeta.MoleculeFailedMetadataKey] != "true" {
			t.Errorf("bead %s lost molecule_failed on close", got.ID)
		}
	}
	if !strings.Contains(stderr.String(), "failed-molecule sweep: closed failed chain") {
		t.Errorf("stderr = %q, want a closed-chain line", stderr.String())
	}
}

// sweepReadCounter counts the failed-molecule sweep's discovery reads.
type sweepReadCounter struct {
	beads.Store
	mu    sync.Mutex
	reads int
}

func (s *sweepReadCounter) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Metadata[beadmeta.MoleculeFailedMetadataKey] == "true" && !q.UpdatedBefore.IsZero() {
		s.mu.Lock()
		s.reads++
		s.mu.Unlock()
	}
	return s.Store.List(q)
}

func (s *sweepReadCounter) sweepReads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// seedFailedChain creates a failed workflow root with one failed step in
// store and returns their IDs.
func seedFailedChain(t *testing.T, store beads.Store, title string) []string {
	t.Helper()
	root, err := store.Create(beads.Bead{Title: title, Metadata: map[string]string{
		beadmeta.KindMetadataKey:           "workflow",
		beadmeta.MoleculeFailedMetadataKey: "true",
	}})
	if err != nil {
		t.Fatalf("Create(%s root): %v", title, err)
	}
	step, err := store.Create(beads.Bead{Title: title + " step", ParentID: root.ID, Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:     root.ID,
		beadmeta.MoleculeFailedMetadataKey: "true",
	}})
	if err != nil {
		t.Fatalf("Create(%s step): %v", title, err)
	}
	return []string{root.ID, step.ID}
}

func assertBeadStatus(t *testing.T, store beads.Store, want string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != want {
			t.Errorf("bead %s (%q) status = %q, want %s", id, b.Title, b.Status, want)
		}
	}
}

// AC-5: in a default-config city (no orders, no wisp GC settings) the
// watchdog sweeps the graph-class store, which is the city work store, and
// every rig work store that is not suspended, each once per pass, and a
// second pass inside the watchdog interval issues no sweep read.
func TestFailedMoleculeSweepWatchdogSweepsEveryStoreOnce(t *testing.T) {
	city := &sweepReadCounter{Store: beads.NewMemStore()}
	rig := &sweepReadCounter{Store: beads.NewMemStore()}
	parked := &sweepReadCounter{Store: beads.NewMemStore()}
	cityChain := seedFailedChain(t, city, "city chain")
	rigChain := seedFailedChain(t, rig, "rig chain")
	parkedChain := seedFailedChain(t, parked, "suspended rig chain")

	var stderr strings.Builder
	cr := &CityRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Rigs: []config.Rig{
				{Name: "r1", Path: t.TempDir()},
				{Name: "r2", Path: t.TempDir(), Suspended: true},
			},
		},
		standaloneCityStore: city,
		standaloneRigStores: map[string]beads.Store{"r1": rig, "r2": parked},
		stdout:              io.Discard,
		stderr:              &stderr,
		logPrefix:           "gc test",
	}
	now := time.Now().Add(failedMoleculeSweepGrace + time.Minute)
	cr.runFailedMoleculeSweepWatchdog(cr.cfg, now)

	assertBeadStatus(t, city, "closed", cityChain...)
	assertBeadStatus(t, rig, "closed", rigChain...)
	assertBeadStatus(t, parked, "open", parkedChain...)
	if got := city.sweepReads(); got != 1 {
		t.Errorf("city store sweep reads = %d, want 1 (graph and work are one store)", got)
	}
	if got := rig.sweepReads(); got != 1 {
		t.Errorf("rig store sweep reads = %d, want 1", got)
	}
	if got := parked.sweepReads(); got != 0 {
		t.Errorf("suspended rig store sweep reads = %d, want 0", got)
	}
	if want := fmt.Sprintf(`closed failed chain %s (2 bead(s)) in rig "r1" work store`, rigChain[0]); !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want a closed-chain line %q", stderr.String(), want)
	}

	cr.runFailedMoleculeSweepWatchdog(cr.cfg, now.Add(time.Minute))
	if got := city.sweepReads() + rig.sweepReads() + parked.sweepReads(); got != 2 {
		t.Errorf("sweep reads after a pass inside the interval = %d, want still 2", got)
	}
}

// faultyCloseStore refuses any CloseAll batch that closes a bead in refuse.
type faultyCloseStore struct {
	beads.Store
	mu     sync.Mutex
	refuse map[string]bool
}

func (s *faultyCloseStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if s.refuse[id] {
			return 0, errors.New("injected close fault")
		}
	}
	return s.Store.CloseAll(ids, metadata)
}

func (s *faultyCloseStore) clearFaults() {
	s.mu.Lock()
	s.refuse = nil
	s.mu.Unlock()
}

// AC-8: a close that fails for one chain is logged with that chain's owner
// ID, the other chains in that store and in the rig store still close, and
// the first pass after the fault clears closes the failed chain.
func TestFailedMoleculeSweepWatchdogRetriesAChainWhoseCloseFailed(t *testing.T) {
	city := &faultyCloseStore{Store: beads.NewMemStore()}
	rig := beads.NewMemStore()
	stuck := seedFailedChain(t, city, "stuck chain")
	healthy := seedFailedChain(t, city, "healthy chain")
	rigChain := seedFailedChain(t, rig, "rig chain")
	city.refuse = map[string]bool{stuck[0]: true}

	var stderr strings.Builder
	cr := &CityRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Rigs:      []config.Rig{{Name: "r1", Path: t.TempDir()}},
		},
		standaloneCityStore: city,
		standaloneRigStores: map[string]beads.Store{"r1": rig},
		stdout:              io.Discard,
		stderr:              &stderr,
		logPrefix:           "gc test",
	}
	now := time.Now().Add(failedMoleculeSweepGrace + time.Minute)
	cr.runFailedMoleculeSweepWatchdog(cr.cfg, now)

	assertBeadStatus(t, city, "open", stuck...)
	assertBeadStatus(t, city, "closed", healthy...)
	assertBeadStatus(t, rig, "closed", rigChain...)
	wantErr := "failed-molecule sweep: graph store: closing failed molecule " + stuck[0] + ": "
	if !strings.Contains(stderr.String(), wantErr) || !strings.Contains(stderr.String(), "injected close fault") {
		t.Errorf("stderr = %q, want an error line %q naming the injected fault", stderr.String(), wantErr)
	}
	if pass := cr.takeFailedMoleculeSweepPass(); pass == nil || pass.errors != 1 || pass.closedChains != 2 {
		t.Errorf("pass = %+v, want 2 chains closed and 1 error", pass)
	}

	city.clearFaults()
	stderr.Reset()
	cr.runFailedMoleculeSweepWatchdog(cr.cfg, now.Add(failedMoleculeSweepWatchdogInterval))
	assertBeadStatus(t, city, "closed", stuck...)
	if want := "closed failed chain " + stuck[0] + " (2 bead(s)) in graph store"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}
