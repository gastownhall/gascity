package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The Counter capability for the bd-CLI store is the store-health half of
// gastownhall/gascity#7546. countBeadStoreRows prefers a hydration-free Count
// precisely because the closed-inclusive hydrating List cannot finish inside
// statusStoreReadTimeout on a long-lived proxied city (measured 2026-10-27 on
// designwins-orchestrator: bd list --all --include-infra --include-gates
// hydrated 25.6k rows in 5.2-8.0s against a 1s budget; bd count --include-infra
// answered the same cardinality in 0.03s). BdStore never grew the capability,
// so every proxied city permanently took the timeout path.
//
// bd's own contract backs the parity claim: `bd count --include-infra` is
// documented as matching `bd list --include-infra --all` cardinality, and that
// parity was re-measured row-for-row before this capability shipped.

func TestBdStoreCountAnswersWholeLedgerScan(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"count": 25617, "schema_version": 1}`), nil
	}
	s := NewBdStore("/city", runner)

	n, err := s.Count(context.Background(), ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 25617 {
		t.Fatalf("Count = %d, want 25617", n)
	}
	want := []string{"count", "--json", "--include-infra"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("bd args = %v, want %v", gotArgs, want)
	}
}

func TestBdStoreCountAcceptsLiveReadFlag(t *testing.T) {
	// CachingStore.Count delegates with Live=true (liveListQuery); Live is a
	// read-mode flag, not a filter, and must not cost the query the fast path.
	runner := func(_, _ string, args ...string) ([]byte, error) {
		return []byte(`{"count": 3}`), nil
	}
	s := NewBdStore("/city", runner)

	n, err := s.Count(context.Background(), ListQuery{AllowScan: true, IncludeClosed: true, Live: true})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Fatalf("Count = %d, want 3", n)
	}
}

// TestBdStoreCountDeclinesNarrowedShapes pins the conservative scope: Count
// must return exactly what List would, minus excludeTypes. bd count's status
// filter does NOT include the infra/wisp rows BdStore.List always adds
// --include-infra --include-gates for (measured 2026-10-27: bd count --status
// open = 25 against bd list --status open --include-infra --include-gates =
// 27), so every narrowed shape reports ErrCountUnsupported and callers fall
// back to List exactly as the Counter contract specifies.
func TestBdStoreCountDeclinesNarrowedShapes(t *testing.T) {
	cases := []struct {
		name  string
		query ListQuery
	}{
		{"status filter", ListQuery{AllowScan: true, IncludeClosed: true, Status: "open"}},
		{"status closed filter", ListQuery{AllowScan: true, Status: "closed"}},
		{"type filter", ListQuery{AllowScan: true, IncludeClosed: true, Type: "task"}},
		{"assignee filter", ListQuery{AllowScan: true, IncludeClosed: true, Assignee: "a"}},
		{"label filter", ListQuery{AllowScan: true, IncludeClosed: true, Label: "l"}},
		{"parent filter", ListQuery{AllowScan: true, IncludeClosed: true, ParentID: "p"}},
		{"active-only scan", ListQuery{AllowScan: true}},
		{"tier both", ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth}},
		{"tier wisps", ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierWisps}},
		{"limited", ListQuery{AllowScan: true, IncludeClosed: true, Limit: 10}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := func(_, _ string, args ...string) ([]byte, error) {
				t.Fatalf("runner called with %v; narrowed shapes must decline before spending a subprocess", args)
				return nil, nil
			}
			s := NewBdStore("/city", runner)
			_, err := s.Count(context.Background(), tc.query)
			if !errors.Is(err, ErrCountUnsupported) {
				t.Fatalf("Count error = %v, want ErrCountUnsupported", err)
			}
		})
	}
}

func TestBdStoreCountDeclinesExcludedTypes(t *testing.T) {
	runner := func(_, _ string, args ...string) ([]byte, error) {
		t.Fatalf("runner called with %v; excludeTypes cannot be expressed by bd count", args)
		return nil, nil
	}
	s := NewBdStore("/city", runner)
	_, err := s.Count(context.Background(), ListQuery{AllowScan: true, IncludeClosed: true}, "message")
	if !errors.Is(err, ErrCountUnsupported) {
		t.Fatalf("Count error = %v, want ErrCountUnsupported", err)
	}
}

func TestBdStoreCountRequiresScan(t *testing.T) {
	s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) { return nil, nil })
	_, err := s.Count(context.Background(), ListQuery{})
	if !errors.Is(err, ErrQueryRequiresScan) {
		t.Fatalf("Count error = %v, want ErrQueryRequiresScan", err)
	}
}

func TestBdStoreCountHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
		t.Fatal("runner called after context cancellation")
		return nil, nil
	})
	_, err := s.Count(ctx, ListQuery{AllowScan: true, IncludeClosed: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Count error = %v, want context.Canceled", err)
	}
}

func TestBdStoreCountPropagatesRunnerError(t *testing.T) {
	s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("bd: connection refused")
	})
	_, err := s.Count(context.Background(), ListQuery{AllowScan: true, IncludeClosed: true})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("Count error = %v, want wrapped runner error", err)
	}
}

// TestBdStoreCountMapsCapabilityAbsentRefusalToUnsupported: a bd build without
// the count verb must signal ErrCountUnsupported so callers take the List
// fallback, not report a store-health failure on a healthy ledger. Mirrors
// isBdBlockedUnsupported's treatment of the blocked door.
func TestBdStoreCountMapsCapabilityAbsentRefusalToUnsupported(t *testing.T) {
	s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf(`bd: unknown command "count" for "bd"`)
	})
	_, err := s.Count(context.Background(), ListQuery{AllowScan: true, IncludeClosed: true})
	if !errors.Is(err, ErrCountUnsupported) {
		t.Fatalf("Count error = %v, want ErrCountUnsupported for a capability-absent refusal", err)
	}
}

func TestBdStoreCountParsesAroundNonJSONPrefixes(t *testing.T) {
	// bd occasionally prefixes warnings on stdout; the list path tolerates that
	// via extractJSON and Count must too.
	s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
		return []byte("warning: something benign\n{\"count\": 7}\n"), nil
	})
	n, err := s.Count(context.Background(), ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 7 {
		t.Fatalf("Count = %d, want 7", n)
	}
}

// TestCachingStoreCountDelegatesClosedInclusiveToBdCounter proves the seam the
// status store-health read depends on: a closed-inclusive Count over a
// CachingStore wrapping a BdStore must reach bd count instead of answering
// ErrCountUnsupported and forcing the hydrating list fallback.
func TestCachingStoreCountDelegatesClosedInclusiveToBdCounter(t *testing.T) {
	runner := func(_, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "count" {
			return []byte(`{"count": 11}`), nil
		}
		return nil, fmt.Errorf("unexpected bd command: %v", args)
	}
	cache := NewCachingStoreForTest(NewBdStore("/city", runner), nil)

	n, err := cache.Count(context.Background(), ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("CachingStore.Count: %v", err)
	}
	if n != 11 {
		t.Fatalf("CachingStore.Count = %d, want 11", n)
	}
}
