package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// projectionStore is a backing store whose ready-projection enrichment fails a
// chosen way, so the cache's reaction to each verdict can be tested without a bd
// subprocess.
type projectionStore struct {
	Store
	err   error
	calls int
}

func (p *projectionStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	p.calls++
	return items, p.err
}

func primedProjectionCache(t *testing.T, err error) (*CachingStore, string) {
	t.Helper()
	backing := NewMemStore()
	ready, createErr := backing.Create(Bead{Type: "task", Status: "open", Title: "ready work"})
	if createErr != nil {
		t.Fatalf("Create: %v", createErr)
	}
	cache := NewCachingStoreForTest(&projectionStore{Store: backing, err: err}, nil)
	if primeErr := cache.Prime(context.Background()); primeErr != nil {
		t.Fatalf("Prime: %v", primeErr)
	}
	return cache, ready.ID
}

// TestPrimeDegradesRatherThanGoingPartialOnAnUnsupportedProjection is the
// operator-visible half of the maintainer-city defect. The enrichment failure
// folded into primePartialErr, which is never cleared except by a clean prime,
// so every cache-only read declined with "bead cache unavailable" for the life
// of the process and fell back to a live 5-6s bd subprocess.
//
// A projection the backing store CANNOT serve is not an incomplete snapshot:
// IsBlocked==nil is the documented fallback and cachedBeadReady derives
// readiness from dependencies. So the cache degrades to a named state — the
// cause lands on the problem log — and keeps serving.
func TestPrimeDegradesRatherThanGoingPartialOnAnUnsupportedProjection(t *testing.T) {
	cause := fmt.Errorf("bd sql ready projection: %w: exit status 1: Error: 'bd sql' is not yet supported in embedded mode", ErrReadyProjectionUnsupported)
	cache, readyID := primedProjectionCache(t, cause)

	cache.mu.RLock()
	partial := cache.primePartialErr
	cache.mu.RUnlock()
	if partial != nil {
		t.Fatalf("primePartialErr = %v, want nil: an unsupported projection must not make the cache permanently unavailable", partial)
	}

	rows, err := cache.ReadyContext(context.Background())
	if err != nil {
		t.Fatalf("ReadyContext after the degrade: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != readyID {
		t.Fatalf("ReadyContext rows = %+v, want the cached ready bead %s", rows, readyID)
	}

	stats := cache.Stats()
	if !strings.Contains(stats.LastProblem, "not yet supported in embedded mode") {
		t.Errorf("LastProblem = %q, want the degrade to name its cause", stats.LastProblem)
	}
	if !strings.Contains(stats.LastProblem, "ready projection") {
		t.Errorf("LastProblem = %q, want the degrade to name the projection", stats.LastProblem)
	}
}

// TestPrimeStaysPartialOnATransientProjectionFailure is the control: only the
// structural verdict degrades. A projection that merely failed this cycle still
// marks the snapshot partial, because the rows really are missing an answer the
// store can give.
func TestPrimeStaysPartialOnATransientProjectionFailure(t *testing.T) {
	cache, _ := primedProjectionCache(t, errors.New("bd sql ready projection: exit status 1: dial tcp: connection refused"))

	cache.mu.RLock()
	partial := cache.primePartialErr
	cache.mu.RUnlock()
	if partial == nil {
		t.Fatal("primePartialErr = nil, want a transient projection failure to keep marking the snapshot partial")
	}
}
