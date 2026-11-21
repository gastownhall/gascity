package main

import (
	"context"
	"errors"
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
