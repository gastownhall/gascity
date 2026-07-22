package beads

import (
	"fmt"
	"sync"
	"testing"
)

// staleDuplicateCloseFixture emulates the documented bd 1.1 duplicate-row state:
// bd show (issues-table-first) can return a stale row that disagrees with the
// canonical wisp row bd query returns and that mutations actually target.
type staleDuplicateCloseFixture struct {
	mu              sync.Mutex
	canonicalStatus string // the wisp / canonical row bd query returns
	staleShowStatus string // the stale issues row bd show returns
	closeCalls      int
}

func (f *staleDuplicateCloseFixture) runner(_ string, name string, args ...string) ([]byte, error) {
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %s %q", name, args)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "show":
		return []byte(fmt.Sprintf(
			`[{"id":"gcw-x","title":"root","status":%q,"issue_type":"molecule","created_at":"2026-07-16T00:00:00Z"}]`,
			f.staleShowStatus,
		)), nil
	case "query":
		return []byte(fmt.Sprintf(
			`[{"id":"gcw-x","title":"root","status":%q,"issue_type":"molecule","created_at":"2026-07-16T00:00:00Z"}]`,
			f.canonicalStatus,
		)), nil
	case "dep":
		return []byte(`[]`), nil
	case "close":
		f.closeCalls++
		f.canonicalStatus = "closed"
		return []byte(`[{"id":"gcw-x","status":"closed"}]`), nil
	default:
		return nil, fmt.Errorf("unexpected bd verb %q", args[0])
	}
}

// TestBdLifecycleTransactionClassifiesFromCanonicalRow proves the bd lifecycle
// transaction (and the close honesty guard) classify from the canonical wisp row
// instead of a stale issues row in the bd 1.1 duplicate-row state.
func TestBdLifecycleTransactionClassifiesFromCanonicalRow(t *testing.T) {
	t.Run("wisp closes while stale issues row stays open", func(t *testing.T) {
		scope := newLifecycleMutationLeaseScope(t)
		fixture := &staleDuplicateCloseFixture{canonicalStatus: "open", staleShowStatus: "open"}
		store := NewBdStore(scope, fixture.runner)

		var result LifecycleCloseResult
		if err := store.WithLifecycleMetadataTransaction("gcw-x", func(tx LifecycleMetadataTransaction) error {
			r, err := CloseWithinLifecycleMetadataTransaction(tx, "molecule autoclose: all step children closed")
			result = r
			return err
		}); err != nil {
			t.Fatalf("lifecycle close: %v", err)
		}
		if !result.AuthoritativeClosed("gcw-x") || !result.Transitioned || !result.CloseSucceeded {
			t.Fatalf("close result = %+v, want acknowledged authoritative transition from the canonical row", result)
		}
		fixture.mu.Lock()
		closeCalls := fixture.closeCalls
		fixture.mu.Unlock()
		if closeCalls != 1 {
			t.Fatalf("bd close calls = %d, want 1", closeCalls)
		}
	})

	t.Run("stale closed issues row does not trigger the already-closed early return", func(t *testing.T) {
		scope := newLifecycleMutationLeaseScope(t)
		// bd show reports the stale issues row as already closed while the
		// canonical wisp is still open. The transaction must classify from the
		// canonical row and actually run the close, not short-circuit on the stale
		// closed snapshot.
		fixture := &staleDuplicateCloseFixture{canonicalStatus: "open", staleShowStatus: "closed"}
		store := NewBdStore(scope, fixture.runner)

		var result LifecycleCloseResult
		if err := store.WithLifecycleMetadataTransaction("gcw-x", func(tx LifecycleMetadataTransaction) error {
			r, err := CloseWithinLifecycleMetadataTransaction(tx, "molecule autoclose: all step children closed")
			result = r
			return err
		}); err != nil {
			t.Fatalf("lifecycle close: %v", err)
		}
		fixture.mu.Lock()
		closeCalls := fixture.closeCalls
		fixture.mu.Unlock()
		if closeCalls != 1 {
			t.Fatalf("bd close calls = %d, want 1 (stale closed issues row wrongly short-circuited the close)", closeCalls)
		}
		if !result.Transitioned || !result.AuthoritativeClosed("gcw-x") {
			t.Fatalf("close result = %+v, want a real transition proven from the canonical row", result)
		}
	})
}
