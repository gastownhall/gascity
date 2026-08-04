package runproj

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// TestRunSessionLinkForNormalization ports session-link.test.ts: regression
// coverage for the rig-store / polecat "invalid session id" bug. A run records
// its session as a pool-qualified NAME (polecat-gc-333573) whose real supervisor
// id is the gc-suffix; the link must normalize to the supervisor id or degrade,
// never leak an unvalidated handle into the session route.
func TestRunSessionLinkForNormalization(t *testing.T) {
	var emptyCtx runSessionLinkContext

	t.Run("normalizes a pool-qualified session name in metadata to the supervisor id", func(t *testing.T) {
		bead := runSnapshotBead{
			assignee: "polecat-gc-333573",
			metadata: map[string]string{"session_id": "polecat-gc-333573"},
		}
		link, ok := runSessionLinkFor(bead, "done", emptyCtx)
		if !ok {
			t.Fatalf("expected a link")
		}
		if link.SessionID != "gc-333573" {
			t.Errorf("sessionID = %q, want %q", link.SessionID, "gc-333573")
		}
	})

	t.Run("leaves a clean gc-prefixed session id unchanged", func(t *testing.T) {
		bead := runSnapshotBead{metadata: map[string]string{"session_id": "gc-333573"}}
		link, ok := runSessionLinkFor(bead, "done", emptyCtx)
		if !ok {
			t.Fatalf("expected a link")
		}
		if link.SessionID != "gc-333573" {
			t.Errorf("sessionID = %q, want %q", link.SessionID, "gc-333573")
		}
	})

	t.Run("derives the id from a pool-qualified assignee when no metadata id is present", func(t *testing.T) {
		bead := runSnapshotBead{assignee: "polecat-gc-333573"}
		link, ok := runSessionLinkFor(bead, "done", emptyCtx)
		if !ok {
			t.Fatalf("expected a link")
		}
		if link.SessionID != "gc-333573" {
			t.Errorf("sessionID = %q, want %q", link.SessionID, "gc-333573")
		}
	})

	t.Run("degrades to no link when an unresolvable value carries no supervisor id", func(t *testing.T) {
		bead := runSnapshotBead{metadata: map[string]string{"session_id": "mystery-handle"}}
		if _, ok := runSessionLinkFor(bead, "done", emptyCtx); ok {
			t.Errorf("expected no link for an unresolvable handle")
		}
	})

	t.Run("degrades a runtime-derived bare assignee that cannot yield a supervisor id", func(t *testing.T) {
		bead := runSnapshotBead{assignee: "polecat"}
		if _, ok := runSessionLinkFor(bead, "done", emptyCtx); ok {
			t.Errorf("expected no link for a bare worker name")
		}
	})

	t.Run("returns no link for pending/ready nodes (no session yet)", func(t *testing.T) {
		bead := runSnapshotBead{assignee: "polecat-gc-333573"}
		if _, ok := runSessionLinkFor(bead, "pending", emptyCtx); ok {
			t.Errorf("expected no link for a pending node")
		}
		if _, ok := runSessionLinkFor(bead, "ready", emptyCtx); ok {
			t.Errorf("expected no link for a ready node")
		}
	})
}

// TestRunSessionLinkForTrustsDurableStampOverRecycledSlotName is the red-team P1
// regression: the run-detail session index is ACTIVE-ONLY, so once the session
// that ran a step CLOSES it leaves the index. If resolution then falls through to
// byName on the deterministic, REUSED pool slot name, a NEW live session occupying
// the same slot would be mis-attributed — a WRONG transcript/diff link, worse than
// no link. The durable gc.session_id stamp must win over the recycled-slot name.
func TestRunSessionLinkForTrustsDurableStampOverRecycledSlotName(t *testing.T) {
	// Active-only index: S1 (mc-s1) has CLOSED and is absent; a NEW live session S2
	// (mc-s2) now occupies the SAME recycled pool slot name gc__worker-1.
	idx := buildRunSessionIndex([]DashboardSession{
		{ID: "mc-s2", SessionName: "gc__worker-1", State: "active", Running: true},
	})
	ctx := runSessionLinkContext{sessionIndex: &idx}

	// Step B ran on S1 and closed; its Assignee is cleared, so only the durable
	// gc.session_id / gc.session_name stamp survives.
	bead := runSnapshotBead{metadata: map[string]string{
		beadmeta.SessionIDMetadataKey:   "mc-s1",
		beadmeta.SessionNameMetadataKey: "gc__worker-1",
	}}
	link, ok := runSessionLinkFor(bead, "done", ctx)
	if !ok {
		t.Fatalf("expected a link from the durable stamp")
	}
	if link.SessionID != "mc-s1" {
		t.Fatalf("sessionID = %q, want mc-s1 (durable stamp); MUST NOT be the recycled-slot session mc-s2", link.SessionID)
	}
}

// TestRunSessionLinkForResolvesActiveDurableStamp: an active step stamped with a
// live session's durable id resolves to that session (short store prefixes that
// sessionIDRe rejects are accepted for the provenance-trusted durable id).
func TestRunSessionLinkForResolvesActiveDurableStamp(t *testing.T) {
	idx := buildRunSessionIndex([]DashboardSession{
		{ID: "mc-s2", SessionName: "gc__worker-1", State: "active", Running: true},
	})
	ctx := runSessionLinkContext{sessionIndex: &idx}

	bead := runSnapshotBead{
		assignee: "gc__worker-1",
		metadata: map[string]string{
			beadmeta.SessionIDMetadataKey:   "mc-s2",
			beadmeta.SessionNameMetadataKey: "gc__worker-1",
		},
	}
	link, ok := runSessionLinkFor(bead, "working", ctx)
	if !ok || link.SessionID != "mc-s2" {
		t.Fatalf("link = {%q ok:%v}, want mc-s2 (live durable stamp)", link.SessionID, ok)
	}
}

// TestRunSessionLinkForLegacyNameFallbackStaysGated covers the legacy/direct path
// (no durable stamp): a step carrying only gc.session_name still resolves via the
// index byName fallback, but ONLY when the resolved id passes the strict
// sessionIDRe gate — and a byName hit onto a short-prefix (recycled-prone) session
// is rejected rather than mis-attributed.
func TestRunSessionLinkForLegacyNameFallbackStaysGated(t *testing.T) {
	t.Run("gate-passing byName hit resolves", func(t *testing.T) {
		idx := buildRunSessionIndex([]DashboardSession{
			{ID: "gc-legacy", SessionName: "gc__solo", State: "active", Running: true},
		})
		ctx := runSessionLinkContext{sessionIndex: &idx}
		bead := runSnapshotBead{metadata: map[string]string{beadmeta.SessionNameMetadataKey: "gc__solo"}}
		link, ok := runSessionLinkFor(bead, "done", ctx)
		if !ok || link.SessionID != "gc-legacy" {
			t.Fatalf("link = {%q ok:%v}, want gc-legacy via gated byName", link.SessionID, ok)
		}
	})

	t.Run("byName hit onto a short-prefix session is gated out (no wrong link)", func(t *testing.T) {
		idx := buildRunSessionIndex([]DashboardSession{
			{ID: "mc-live", SessionName: "gc__solo", State: "active", Running: true},
		})
		ctx := runSessionLinkContext{sessionIndex: &idx}
		bead := runSnapshotBead{metadata: map[string]string{beadmeta.SessionNameMetadataKey: "gc__solo"}}
		if _, ok := runSessionLinkFor(bead, "done", ctx); ok {
			t.Fatalf("expected no link: a legacy byName hit onto a short-prefix session must stay gated")
		}
	})
}

// TestRunSessionLinkForRejectsGarbageDurableStamp: a malformed gc.session_id
// (whitespace/uppercase/prefixless) is rejected by the durable validator and never
// leaks a link, even though the durable path is index-independent.
func TestRunSessionLinkForRejectsGarbageDurableStamp(t *testing.T) {
	for _, garbage := range []string{"garbage value", "BOGUS", "nostoreprefix", "mystery-handle"} {
		bead := runSnapshotBead{metadata: map[string]string{beadmeta.SessionIDMetadataKey: garbage}}
		if _, ok := runSessionLinkFor(bead, "done", runSessionLinkContext{}); ok {
			t.Errorf("expected no link for garbage durable session id %q", garbage)
		}
	}
}
