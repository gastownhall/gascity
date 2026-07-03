package runproj

import "testing"

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
