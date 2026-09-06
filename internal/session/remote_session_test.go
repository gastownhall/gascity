package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func newRemoteCheckpointManager(t *testing.T) (*Manager, *beads.MemStore, string) {
	t.Helper()
	store := beads.NewMemStore()
	provider := runtime.NewFake()
	mgr := NewManagerWithOptions(store, provider, WithCityPath("test-city"))
	info, err := mgr.CreateSession(t.Context(), CreateOptions{Template: "worker", BeadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(info.ID, "instance_token", "owner-generation-1"); err != nil {
		t.Fatal(err)
	}
	return mgr, store, info.ID
}

func TestRemoteCheckpointRoundTripsInOneSessionMetadataRecord(t *testing.T) {
	mgr, store, id := newRemoteCheckpointManager(t)
	checkpoint := RemoteCheckpoint{
		Ref:           runtime.RemoteSessionRef{SessionID: "opaque-session", RunID: "opaque-run"},
		Phase:         runtime.RemoteSessionRunning,
		EventCursor:   "opaque-event-cursor",
		ReceiptCursor: "opaque-receipt-cursor",
		Handoff: []runtime.RemoteGitHandoff{{
			Repository:  "https://github.com/acme/repo",
			Branch:      "agent/change",
			PullRequest: "https://github.com/acme/repo/pull/1",
		}},
		UpdatedAt: time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC),
	}
	if err := mgr.PersistRemoteCheckpoint(id, "owner-generation-1", checkpoint); err != nil {
		t.Fatalf("PersistRemoteCheckpoint: %v", err)
	}
	got, ok, err := mgr.RemoteCheckpoint(id)
	if err != nil || !ok {
		t.Fatalf("RemoteCheckpoint = %+v, %v, %v", got, ok, err)
	}
	if got.Ref != checkpoint.Ref || got.EventCursor != checkpoint.EventCursor || got.ReceiptCursor != checkpoint.ReceiptCursor {
		t.Fatalf("checkpoint = %+v, want %+v", got, checkpoint)
	}
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata[RemoteCheckpointMetadataKey] == "" {
		t.Fatal("checkpoint was not stored in the canonical metadata record")
	}
}

func TestRemoteCheckpointRejectsStaleSessionOwnerWithoutMutation(t *testing.T) {
	mgr, store, id := newRemoteCheckpointManager(t)
	checkpoint := RemoteCheckpoint{
		Ref:       runtime.RemoteSessionRef{SessionID: "opaque-session"},
		Phase:     runtime.RemoteSessionQueued,
		UpdatedAt: time.Now().UTC(),
	}
	err := mgr.PersistRemoteCheckpoint(id, "stale-owner-generation", checkpoint)
	if !errors.Is(err, ErrRemoteCheckpointFence) {
		t.Fatalf("PersistRemoteCheckpoint error = %v, want ErrRemoteCheckpointFence", err)
	}
	b, getErr := store.Get(id)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if b.Metadata[RemoteCheckpointMetadataKey] != "" {
		t.Fatalf("stale writer mutated checkpoint: %s", b.Metadata[RemoteCheckpointMetadataKey])
	}
}

func TestRemoteCheckpointPersistsFailureClassWithoutProviderMessage(t *testing.T) {
	mgr, store, id := newRemoteCheckpointManager(t)
	checkpoint := RemoteCheckpoint{
		Ref:       runtime.RemoteSessionRef{SessionID: "opaque-session"},
		Phase:     runtime.RemoteSessionFailed,
		Failure:   runtime.RemoteFailureQuota,
		UpdatedAt: time.Now().UTC(),
	}
	if err := mgr.PersistRemoteCheckpoint(id, "owner-generation-1", checkpoint); err != nil {
		t.Fatal(err)
	}
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Metadata[RemoteCheckpointMetadataKey]; got == "" || containsAny(got, "provider_message", "transcript", "prompt", "token") {
		t.Fatalf("checkpoint contains a forbidden payload field: %s", got)
	}
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if value != "" && strings.Contains(text, value) {
			return true
		}
	}
	return false
}
