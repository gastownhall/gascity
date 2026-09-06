package tmux

import (
	"testing"
	"time"
)

// TestGetMetaMemoizedWithoutResetTickCache verifies that GetMeta is served
// from the per-session environment memo even when no reconcile tick has
// called ResetTickCache — the supervisor's API handlers (/rigs, /sessions)
// query GetMeta for every configured session slot on every request, outside
// any tick, and each of those was an uncached `tmux show-environment` fork
// (sys-4za3nm).
func TestGetMetaMemoizedWithoutResetTickCache(t *testing.T) {
	fe := &fakeExecutor{out: "GC_DRAIN_ACK=1"}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}
	p := &Provider{tm: tm}

	for i := 0; i < 3; i++ {
		if _, err := p.GetMeta("mayor", "GC_DRAIN_ACK"); err != nil {
			t.Fatalf("GetMeta #%d: %v", i, err)
		}
	}
	if len(fe.calls) != 1 {
		t.Fatalf("tmux forks = %d, want 1 (GetMeta memoized outside a tick)", len(fe.calls))
	}
}

// TestGetMetaMemoExpiresAfterTTL verifies the memo is bounded: an entry
// older than metaCacheTTL is re-fetched, so a reader outside a tick never
// sees session metadata staler than the TTL.
func TestGetMetaMemoExpiresAfterTTL(t *testing.T) {
	fe := &fakeExecutor{outs: []string{"GC_DRAIN_ACK=", "GC_DRAIN_ACK=1"}}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}
	p := &Provider{tm: tm}

	now := time.Unix(1_700_000_000, 0)
	p.ResetTickCache()
	p.activeTickCache().now = func() time.Time { return now }

	first, _ := p.GetMeta("mayor", "GC_DRAIN_ACK")
	if first != "" {
		t.Fatalf("first = %q, want empty", first)
	}
	// Within the TTL: still served from the memo.
	now = now.Add(metaCacheTTL / 2)
	if v, _ := p.GetMeta("mayor", "GC_DRAIN_ACK"); v != "" {
		t.Fatalf("within TTL = %q, want empty (memo hit)", v)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("tmux forks = %d, want 1 within TTL", len(fe.calls))
	}
	// Past the TTL: re-fetched, and the new value is visible.
	now = now.Add(metaCacheTTL)
	if v, _ := p.GetMeta("mayor", "GC_DRAIN_ACK"); v != "1" {
		t.Fatalf("after TTL = %q, want %q (stale memo served past TTL)", v, "1")
	}
	if len(fe.calls) != 2 {
		t.Fatalf("tmux forks = %d, want 2 after TTL", len(fe.calls))
	}
}
