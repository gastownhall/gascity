package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestCountBeadStoreRowsBdStoreCounterBeatsHydratingTimeout is the
// production-shaped regression for the store-health half of
// gastownhall/gascity#7546: a proxied BdStore whose closed-inclusive
// hydrating List cannot finish inside the per-store read budget (measured
// 2026-10-27 on designwins-orchestrator: 5.2-8.0s against a 1s budget over
// 25.6k rows) must still yield a correct retained-row count through the
// bd count fast path, with no timeout error and no hydration.
func TestCountBeadStoreRowsBdStoreCounterBeatsHydratingTimeout(t *testing.T) {
	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 150 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	listStarted := make(chan struct{}, 1)
	runner := func(_, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "count":
			return []byte(`{"count": 25617, "schema_version": 1}`), nil
		case "list":
			// Model the real hydrating scan: it answers eventually, but far
			// past the read budget the caller must never spend waiting.
			select {
			case listStarted <- struct{}{}:
			default:
			}
			<-release
			return []byte("[]"), nil
		}
		return nil, fmt.Errorf("unexpected bd command: %v", args)
	}
	store := beads.NewCachingStoreForTest(beads.NewBdStore("/city", runner), nil)

	start := time.Now()
	got, err := countBeadStoreRows(context.Background(), newFakeState(t), store)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("countBeadStoreRows: %v", err)
	}
	if got != 25617 {
		t.Fatalf("countBeadStoreRows = %d, want 25617 from the bd count fast path", got)
	}
	if elapsed >= statusStoreReadTimeout {
		t.Fatalf("count took %s, at or past the %s budget; the counter fast path must not wait on hydration", elapsed, statusStoreReadTimeout)
	}
	select {
	case <-listStarted:
		t.Fatal("hydrating List started; the counter fast path must never reach it")
	default:
	}
}

// TestCountBeadStoreRowsBdStoreTimeoutStaysBoundedOnListOnlyLedgers is the
// negative companion: a BdStore whose count command fails (a bd old enough to
// lack the verb) keeps the prior behavior — the hydrating fallback runs, is
// bounded by the read budget, and surfaces the branch-named timeout error
// rather than a fabricated count.
func TestCountBeadStoreRowsBdStoreTimeoutStaysBoundedOnListOnlyLedgers(t *testing.T) {
	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 100 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	runner := func(_, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "count":
			return nil, fmt.Errorf("bd: unknown command %q for %q", "count", "bd")
		case "list":
			<-release
			return []byte("[]"), nil
		}
		return nil, fmt.Errorf("unexpected bd command: %v", args)
	}
	store := beads.NewCachingStoreForTest(beads.NewBdStore("/city", runner), nil)

	got, err := countBeadStoreRows(context.Background(), newFakeState(t), store)
	if got != 0 {
		t.Fatalf("countBeadStoreRows = %d, want zero value on timeout", got)
	}
	if err == nil {
		t.Fatal("countBeadStoreRows error = nil, want the bounded hydrating-fallback timeout error")
	}
	// The branch name in the error is what makes the failure attributable
	// after the fact (see countBeadStoreRows doc comment).
	for _, want := range []string{"hydrating list fallback", "timed out"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("countBeadStoreRows error = %v, want it to contain %q", err, want)
		}
	}
}
