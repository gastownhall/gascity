//go:build acceptance_a

// Flock concurrency integration test for quota state.
//
// Verifies that two concurrent goroutines using withQuotaLock produce
// a consistent quota state file with no corruption. PRD refs: G3
// (flock-protected), Simultaneous rotations edge case.
package main

import (
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// TestQuotaFlockConcurrency verifies that two goroutines attempting
// withQuotaLock simultaneously result in a consistent quota.json.
// Both goroutines modify the state under flock; the final file must
// contain both modifications (sequential execution enforced by flock).
func TestQuotaFlockConcurrency(t *testing.T) {
	dir := t.TempDir()
	quotaPath := dir + "/quota.json"

	// Seed the file with an initial empty state.
	initial := &config.QuotaState{
		Accounts: make(map[string]config.QuotaAccountState),
	}
	if err := saveQuotaState(quotaPath, initial); err != nil {
		t.Fatalf("seeding quota state: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)

	// Goroutine 1: mark "work1" as limited under flock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := withQuotaLock(quotaPath, 5*time.Second, func(state *config.QuotaState) error {
			state.Accounts["work1"] = config.QuotaAccountState{
				Status:    config.QuotaStatusLimited,
				LimitedAt: "2026-04-07T10:00:00Z",
			}
			// Small delay to increase window for race.
			time.Sleep(50 * time.Millisecond)
			return nil
		})
		if err != nil {
			errs <- err
		}
	}()

	// Goroutine 2: mark "work2" as limited under flock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := withQuotaLock(quotaPath, 5*time.Second, func(state *config.QuotaState) error {
			state.Accounts["work2"] = config.QuotaAccountState{
				Status:    config.QuotaStatusLimited,
				LimitedAt: "2026-04-07T10:01:00Z",
			}
			// Small delay to increase window for race.
			time.Sleep(50 * time.Millisecond)
			return nil
		})
		if err != nil {
			errs <- err
		}
	}()

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("withQuotaLock returned error: %v", err)
	}

	// Read final state and verify both modifications are present.
	final, err := loadQuotaState(quotaPath)
	if err != nil {
		t.Fatalf("loading final state: %v", err)
	}

	if len(final.Accounts) != 2 {
		t.Fatalf("expected 2 accounts in final state, got %d: %+v", len(final.Accounts), final.Accounts)
	}

	w1, ok := final.Accounts["work1"]
	if !ok {
		t.Fatal("work1 not found in final state")
	}
	if w1.Status != config.QuotaStatusLimited {
		t.Errorf("work1 status = %q, want %q", w1.Status, config.QuotaStatusLimited)
	}
	if w1.LimitedAt != "2026-04-07T10:00:00Z" {
		t.Errorf("work1 limited_at = %q, want %q", w1.LimitedAt, "2026-04-07T10:00:00Z")
	}

	w2, ok := final.Accounts["work2"]
	if !ok {
		t.Fatal("work2 not found in final state")
	}
	if w2.Status != config.QuotaStatusLimited {
		t.Errorf("work2 status = %q, want %q", w2.Status, config.QuotaStatusLimited)
	}
	if w2.LimitedAt != "2026-04-07T10:01:00Z" {
		t.Errorf("work2 limited_at = %q, want %q", w2.LimitedAt, "2026-04-07T10:01:00Z")
	}

	// Verify no file corruption: re-load should produce the same state.
	reloaded, err := loadQuotaState(quotaPath)
	if err != nil {
		t.Fatalf("re-loading final state (corruption check): %v", err)
	}
	if len(reloaded.Accounts) != 2 {
		t.Fatalf("corruption detected: re-loaded state has %d accounts, expected 2", len(reloaded.Accounts))
	}
}
