package beads

import (
	"context"
	"errors"
	"testing"
)

// TestEmptyLedgerReadyContextServesFromCache is the pilot-rig half of
// gastownhall/gascity#7546 on the proxied bd provider.
//
// A proxied scope whose ledger is EMPTY can never latch the inline dependency
// witness (nothing short of a listed row carrying blocking edges proves the
// projection is live — bdstore_inline_deps.go), so
// CachingStore.fetchDepsForBeads answered depsComplete=false on every prime
// and every reconcile, and cachedReadyCompleteOnly — which gates on
// depsComplete — declined every ReadyContext read forever:
//
//	"work ready: reading complete ready projection from cache: bead cache
//	 unavailable"
//
// on every status response, for the life of the process, on a scope whose
// correct ready projection is provably the empty set. An empty snapshot has no
// edges to know: dependency coverage over zero beads is vacuously complete.
func TestEmptyLedgerReadyContextServesFromCache(t *testing.T) {
	ledger := &fakeBdLedger{}
	cache, _ := primeBdCacheOverScope(t, ledger, "/city/empty")

	rows, err := cache.ReadyContext(context.Background())
	if err != nil {
		t.Fatalf("ReadyContext error = %v, want the empty projection served from cache", err)
	}
	if len(rows) != 0 {
		t.Fatalf("ReadyContext = %d rows, want 0", len(rows))
	}

	cache.mu.RLock()
	complete := cache.depsComplete
	cache.mu.RUnlock()
	if !complete {
		t.Fatal("depsComplete = false after priming an empty ledger; an empty snapshot has no edges to know")
	}
}

// TestEmptyLedgerReadyContextSurvivesReconcile pins the same verdict after a
// reconcile pass, the state a long-lived supervisor cache actually spends its
// life in: the reconcile merge must not demote the vacuously-complete deps
// coverage back to false and re-decline readiness.
func TestEmptyLedgerReadyContextSurvivesReconcile(t *testing.T) {
	ledger := &fakeBdLedger{}
	cache, _ := primeBdCacheOverScope(t, ledger, "/city/empty")

	cache.mu.RLock()
	startSeq := cache.mutationSeq
	cache.mu.RUnlock()
	cache.runReconciliation()

	cache.mu.RLock()
	complete := cache.depsComplete
	state := cache.state
	cache.mu.RUnlock()
	if state != cacheLive {
		t.Fatalf("cache state = %v, want cacheLive after reconcile", state)
	}
	if !complete {
		t.Fatal("depsComplete demoted to false by reconcile over an empty ledger")
	}
	if cache.mutationSeq != startSeq {
		t.Fatalf("mutationSeq moved from %d to %d on an empty reconcile", startSeq, cache.mutationSeq)
	}

	rows, err := cache.ReadyContext(context.Background())
	if err != nil {
		t.Fatalf("ReadyContext after reconcile error = %v, want served from cache", err)
	}
	if len(rows) != 0 {
		t.Fatalf("ReadyContext after reconcile = %d rows, want 0", len(rows))
	}
}

// TestEmptyLedgerDepsStillRequireWitnessForNonEmptyRows guards the fix against
// the opposite regression: vacuous completeness is a claim about the EMPTY
// snapshot only. A ledger with rows but no witnessed inline projection must
// keep depsComplete=false and keep declining ReadyContext, because "no rows
// carried edges" is exactly the shape the witness cannot tell from "bd dropped
// the column".
func TestEmptyLedgerDepsStillRequireWitnessForNonEmptyRows(t *testing.T) {
	ledger := &fakeBdLedger{rows: []bdLedgerRow{{id: "bd-lonely"}}}
	cache, _ := primeBdCacheOverScope(t, ledger, "/city/lonely")

	cache.mu.RLock()
	complete := cache.depsComplete
	cache.mu.RUnlock()
	if complete {
		t.Fatal("depsComplete = true over a non-empty unwitnessed ledger; the witness must stay authoritative there")
	}

	_, err := cache.ReadyContext(context.Background())
	if !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("ReadyContext error = %v, want ErrCacheUnavailable over an unwitnessed non-empty ledger", err)
	}
}
