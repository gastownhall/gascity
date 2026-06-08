package extmsg

import (
	"context"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// makeSessionBead creates an open session bead carrying the given stable
// session name, mirroring how the session lifecycle records identity. It
// returns the volatile bead ID, which changes across respawn.
func makeSessionBead(t *testing.T, store beads.Store, name string) string {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    "session " + name,
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"session_name": name},
	})
	if err != nil {
		t.Fatalf("create session bead %q: %v", name, err)
	}
	return b.ID
}

// respawn closes the session bead at oldID and mints a fresh one under the
// same name, modeling a crash-and-restart that changes the bead ID.
func respawn(t *testing.T, store beads.Store, oldID, name string) string {
	t.Helper()
	if err := store.Close(oldID); err != nil {
		t.Fatalf("close session bead %s: %v", oldID, err)
	}
	return makeSessionBead(t, store, name)
}

func TestBindStoresStableSessionName(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Bindings
	ref := testConversationRef()

	sessionID := makeSessionBead(t, store, "gc-pl")
	binding, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    sessionID,
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if binding.SessionName != "gc-pl" {
		t.Fatalf("SessionName = %q, want %q", binding.SessionName, "gc-pl")
	}
	bead, err := store.Get(binding.ID)
	if err != nil {
		t.Fatalf("get binding bead: %v", err)
	}
	if bead.Metadata["session_name"] != "gc-pl" {
		t.Fatalf("metadata session_name = %q, want %q", bead.Metadata["session_name"], "gc-pl")
	}
	if !slices.Contains(bead.Labels, bindingSessionNameLabel("gc-pl")) {
		t.Fatalf("binding labels %v missing session-name label %q", bead.Labels, bindingSessionNameLabel("gc-pl"))
	}
}

func TestResolveByConversationOverlaysLiveSessionAfterRespawn(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Bindings
	ref := testConversationRef()

	oldID := makeSessionBead(t, store, "gc-pl")
	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    oldID,
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	newID := respawn(t, store, oldID, "gc-pl")

	got, err := svc.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if got == nil {
		t.Fatal("ResolveByConversation returned nil after respawn")
	}
	if got.SessionID != newID {
		t.Fatalf("SessionID = %q, want live respawned bead %q (overlay failed)", got.SessionID, newID)
	}
}

func TestResolveByConversationLeavesStaleIDWhenSessionGone(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Bindings
	ref := testConversationRef()

	oldID := makeSessionBead(t, store, "gc-pl")
	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    oldID,
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	// No respawn — the session is just gone.
	if err := store.Close(oldID); err != nil {
		t.Fatalf("close session: %v", err)
	}

	got, err := svc.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if got == nil || got.SessionID != oldID {
		t.Fatalf("expected binding to retain stale id %q for the reaper to clear, got %+v", oldID, got)
	}
}

func TestReapStaleBindingsReassignsToRespawnedSession(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Bindings
	ref := testConversationRef()

	oldID := makeSessionBead(t, store, "gc-pl")
	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    oldID,
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	newID := respawn(t, store, oldID, "gc-pl")

	stats, err := ReapStaleBindings(context.Background(), store, testNow())
	if err != nil {
		t.Fatalf("ReapStaleBindings: %v", err)
	}
	if stats.Scanned != 1 || stats.Reassigned != 1 || stats.Cleared != 0 {
		t.Fatalf("stats = %+v, want Scanned=1 Reassigned=1 Cleared=0", stats)
	}

	// The persisted binding now points at the live bead even without the
	// read-time overlay: re-resolving by the dead name resolver would fail,
	// so prove the stored session_id was healed.
	got, err := svc.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if got == nil || got.SessionID != newID {
		t.Fatalf("after reap, SessionID = %v, want %q", got, newID)
	}
	bead, err := store.Get(got.ID)
	if err != nil {
		t.Fatalf("get binding bead: %v", err)
	}
	if bead.Metadata["session_id"] != newID {
		t.Fatalf("stored session_id = %q, want healed %q", bead.Metadata["session_id"], newID)
	}
}

func TestReapStaleBindingsClearsDeadNamedSession(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Bindings
	ref := testConversationRef()

	oldID := makeSessionBead(t, store, "gc-pl")
	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    oldID,
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	// Session retired with no replacement: the name has no live owner.
	if err := store.Close(oldID); err != nil {
		t.Fatalf("close session: %v", err)
	}

	stats, err := ReapStaleBindings(context.Background(), store, testNow())
	if err != nil {
		t.Fatalf("ReapStaleBindings: %v", err)
	}
	if stats.Scanned != 1 || stats.Reassigned != 0 || stats.Cleared != 1 {
		t.Fatalf("stats = %+v, want Scanned=1 Reassigned=0 Cleared=1", stats)
	}
	got, err := svc.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if got != nil {
		t.Fatalf("expected binding cleared, got %+v", got)
	}
}

func TestReapStaleBindingsClearsLegacyBindingWithClosedBead(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Bindings
	ref := testConversationRef()

	// Legacy binding: target session bead carries no stable session_name.
	legacy, err := store.Create(beads.Bead{
		Title:  "session legacy",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})
	if err != nil {
		t.Fatalf("create legacy session bead: %v", err)
	}
	binding, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    legacy.ID,
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if binding.SessionName != "" {
		t.Fatalf("legacy binding unexpectedly recorded a name: %q", binding.SessionName)
	}

	// While the bead is open the reaper must leave the legacy binding alone.
	stats, err := ReapStaleBindings(context.Background(), store, testNow())
	if err != nil {
		t.Fatalf("ReapStaleBindings (open): %v", err)
	}
	if stats.Cleared != 0 {
		t.Fatalf("reaper cleared a live legacy binding: %+v", stats)
	}

	if err := store.Close(legacy.ID); err != nil {
		t.Fatalf("close legacy session: %v", err)
	}
	stats, err = ReapStaleBindings(context.Background(), store, testNow())
	if err != nil {
		t.Fatalf("ReapStaleBindings (closed): %v", err)
	}
	if stats.Cleared != 1 {
		t.Fatalf("stats = %+v, want Cleared=1 after legacy session closed", stats)
	}
}

// TestBindBackfillsSessionNameOnLegacyBinding verifies that calling Bind on a
// conversation whose active binding has no session_name backfills the name from
// the session bead, turning a legacy binding into a respawn-survivable one.
// The scenario: binding was created before the session bead carried a stable
// session_name; a later Bind call on the same conversation+session triggers the
// backfill when the session bead has since been updated with a name.
func TestBindBackfillsSessionNameOnLegacyBinding(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Bindings
	ref := testConversationRef()

	// Create a session bead without session_name (pre-fix format).
	legacyBead, err := store.Create(beads.Bead{
		Title:  "session gc-pl",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})
	if err != nil {
		t.Fatalf("create legacy session bead: %v", err)
	}

	// Initial Bind: session bead has no session_name, so SessionName is empty.
	binding, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    legacyBead.ID,
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("initial Bind: %v", err)
	}
	if binding.SessionName != "" {
		t.Fatalf("legacy binding should have empty SessionName, got %q", binding.SessionName)
	}

	// Upgrade the session bead in-place to carry the stable session_name,
	// simulating a system that back-patches existing session beads.
	if err := store.Update(legacyBead.ID, beads.UpdateOpts{
		Metadata: map[string]string{"session_name": "gc-pl"},
	}); err != nil {
		t.Fatalf("upgrade session bead: %v", err)
	}

	// Re-Bind: same conversation + same session bead (allowed — not a conflict).
	// Now sessionNameForSelector finds "gc-pl" and the backfill path fires.
	upgraded, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    legacyBead.ID,
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("rebind for backfill: %v", err)
	}
	if upgraded.SessionName != "gc-pl" {
		t.Fatalf("backfilled SessionName = %q, want %q", upgraded.SessionName, "gc-pl")
	}

	// Verify the persisted binding bead now carries the session_name metadata.
	bead, err := store.Get(binding.ID)
	if err != nil {
		t.Fatalf("get binding bead: %v", err)
	}
	if bead.Metadata["session_name"] != "gc-pl" {
		t.Fatalf("stored session_name = %q, want %q", bead.Metadata["session_name"], "gc-pl")
	}
	if !slices.Contains(bead.Labels, bindingSessionNameLabel("gc-pl")) {
		t.Fatalf("backfilled binding labels %v missing session-name label %q", bead.Labels, bindingSessionNameLabel("gc-pl"))
	}
}
