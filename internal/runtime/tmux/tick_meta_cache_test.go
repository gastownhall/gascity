package tmux

import (
	"testing"
)

// TestGetMetaMemoizesWithinTick verifies that once ResetTickCache is called,
// repeated GetMeta calls for the same session (from the reconcile tick's many
// independent call sites) share ONE `tmux show-environment` fork instead of
// one fork per call — the fork storm this fix targets (sys-yre7dj).
func TestGetMetaMemoizesWithinTick(t *testing.T) {
	fe := &fakeExecutor{out: "GC_DRAIN_ACK=1\nGC_RESTART_REQUESTED=\nGC_INSTANCE_TOKEN=abc123"}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}
	p := &Provider{tm: tm}

	p.ResetTickCache()

	got, err := p.GetMeta("mayor", "GC_DRAIN_ACK")
	if err != nil {
		t.Fatalf("GetMeta GC_DRAIN_ACK: %v", err)
	}
	if got != "1" {
		t.Errorf("GC_DRAIN_ACK = %q, want %q", got, "1")
	}

	got, err = p.GetMeta("mayor", "GC_INSTANCE_TOKEN")
	if err != nil {
		t.Fatalf("GetMeta GC_INSTANCE_TOKEN: %v", err)
	}
	if got != "abc123" {
		t.Errorf("GC_INSTANCE_TOKEN = %q, want %q", got, "abc123")
	}

	// A third, distinct call site querying a key absent from the session's
	// environment must see "key not set" (empty, nil error), not a fetch.
	got, err = p.GetMeta("mayor", "GC_DRIFT_RESTART")
	if err != nil {
		t.Fatalf("GetMeta GC_DRIFT_RESTART: %v", err)
	}
	if got != "" {
		t.Errorf("GC_DRIFT_RESTART = %q, want empty", got)
	}

	if len(fe.calls) != 1 {
		t.Fatalf("tmux forks = %d, want 1 (three GetMeta calls for one session in one tick): %v", len(fe.calls), fe.calls)
	}
	want := []string{"-u", "show-environment", "-t", "mayor"}
	if len(fe.calls[0]) != len(want) {
		t.Fatalf("fork args = %v, want %v", fe.calls[0], want)
	}
	for i := range want {
		if fe.calls[0][i] != want[i] {
			t.Errorf("fork args[%d] = %q, want %q", i, fe.calls[0][i], want[i])
		}
	}
}

// TestGetMetaMemoIsPerSession verifies the memo does not conflate two
// different sessions queried in the same tick.
func TestGetMetaMemoIsPerSession(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{"GC_DRAIN_ACK=1", "GC_DRAIN_ACK="},
	}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}
	p := &Provider{tm: tm}
	p.ResetTickCache()

	got1, err := p.GetMeta("witness", "GC_DRAIN_ACK")
	if err != nil {
		t.Fatalf("GetMeta witness: %v", err)
	}
	got2, err := p.GetMeta("refinery", "GC_DRAIN_ACK")
	if err != nil {
		t.Fatalf("GetMeta refinery: %v", err)
	}
	if got1 != "1" || got2 != "" {
		t.Fatalf("got1=%q got2=%q, want 1 and empty", got1, got2)
	}
	if len(fe.calls) != 2 {
		t.Fatalf("tmux forks = %d, want 2 (one per distinct session)", len(fe.calls))
	}
}

// TestResetTickCacheStartsNewWindow verifies a second ResetTickCache call
// (the next reconcile tick) discards the previous tick's memoized reads —
// the correctness requirement the bead's blast-radius note calls out: a memo
// that outlives its tick would serve stale drain-ack/restart-request state.
func TestResetTickCacheStartsNewWindow(t *testing.T) {
	fe := &fakeExecutor{outs: []string{"GC_DRAIN_ACK=1", "GC_DRAIN_ACK=0"}}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}
	p := &Provider{tm: tm}

	p.ResetTickCache()
	first, err := p.GetMeta("mayor", "GC_DRAIN_ACK")
	if err != nil {
		t.Fatalf("GetMeta (tick 1): %v", err)
	}
	if first != "1" {
		t.Fatalf("first = %q, want %q", first, "1")
	}

	p.ResetTickCache()
	second, err := p.GetMeta("mayor", "GC_DRAIN_ACK")
	if err != nil {
		t.Fatalf("GetMeta (tick 2): %v", err)
	}
	if second != "0" {
		t.Fatalf("second = %q, want %q (stale memo carried across ResetTickCache)", second, "0")
	}
	if len(fe.calls) != 2 {
		t.Fatalf("tmux forks = %d, want 2 (one per tick)", len(fe.calls))
	}
}

// TestSetMetaInvalidatesTickCache verifies a same-tick SetMeta is never
// masked by a stale cached GetMeta read for that session.
func TestSetMetaInvalidatesTickCache(t *testing.T) {
	// outs[0]: the tick's first GetMeta fetch (GetAllEnvironment). outs[1]: the
	// SetMeta call itself (SetEnvironment; output unused). outs[2]: the
	// post-invalidate refetch on the next GetMeta.
	fe := &fakeExecutor{outs: []string{"GC_DRAIN_ACK=", "", "GC_DRAIN_ACK=1"}}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}
	p := &Provider{tm: tm}
	p.ResetTickCache()

	before, err := p.GetMeta("mayor", "GC_DRAIN_ACK")
	if err != nil {
		t.Fatalf("GetMeta (before): %v", err)
	}
	if before != "" {
		t.Fatalf("before = %q, want empty", before)
	}

	if err := p.SetMeta("mayor", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	after, err := p.GetMeta("mayor", "GC_DRAIN_ACK")
	if err != nil {
		t.Fatalf("GetMeta (after): %v", err)
	}
	if after != "1" {
		t.Fatalf("after = %q, want %q (SetMeta masked by stale tick-cache read)", after, "1")
	}
}

// TestRemoveMetaInvalidatesTickCache mirrors TestSetMetaInvalidatesTickCache
// for RemoveMeta.
func TestRemoveMetaInvalidatesTickCache(t *testing.T) {
	// outs[0]: the tick's first GetMeta fetch. outs[1]: the RemoveMeta call
	// itself (RemoveEnvironment; output unused). outs[2]: the post-invalidate
	// refetch on the next GetMeta, proving it re-fetched rather than reused
	// the pre-removal cached copy.
	fe := &fakeExecutor{outs: []string{"GC_DRAIN_ACK=1", "", "OTHER_KEY=1"}}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}
	p := &Provider{tm: tm}
	p.ResetTickCache()

	before, err := p.GetMeta("mayor", "GC_DRAIN_ACK")
	if err != nil {
		t.Fatalf("GetMeta (before): %v", err)
	}
	if before != "1" {
		t.Fatalf("before = %q, want %q", before, "1")
	}

	if err := p.RemoveMeta("mayor", "GC_DRAIN_ACK"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}

	after, err := p.GetMeta("mayor", "GC_DRAIN_ACK")
	if err != nil {
		t.Fatalf("GetMeta (after): %v", err)
	}
	if after != "" {
		t.Fatalf("after = %q, want empty (RemoveMeta masked by stale tick-cache read)", after)
	}
}

// TestGetMetaTickCachePropagatesSessionGoneError verifies the cached path
// preserves GetMeta's existing error-classification contract: a
// session-not-found/no-server error propagates, everything else collapses to
// ("", nil).
func TestGetMetaTickCachePropagatesSessionGoneError(t *testing.T) {
	fe := &fakeExecutor{err: ErrSessionNotFound}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}
	p := &Provider{tm: tm}
	p.ResetTickCache()

	_, err := p.GetMeta("gone", "GC_DRAIN_ACK")
	if err != ErrSessionNotFound {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}
