package session

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestRequestFreshRestart_ClearsStaleSpawnMetadata verifies that
// Manager.RequestFreshRestart (the engine behind `gc session reset`) clears
// the spawn metadata fields that the reconciler treats as authoritative for
// command construction. Without this, an agent.toml change between resets
// (e.g. switching wake_mode = "resume" to wake_mode = "fresh", or changing
// the spawn command) is not honored on the next wake because the bead still
// carries the previous command / resume_flag / session_key / continuation_epoch.
// Clearing the fields lets the materialization loop in cmd/gc/session_beads.go
// (see "refresh command and resume fields" comment) refill them from the
// freshly resolved TemplateParams on the next reconcile tick.
func TestRequestFreshRestart_ClearsStaleSpawnMetadata(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "my chat", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate stale spawn metadata that an earlier wake stamped onto the
	// bead. These are the exact fields zk-city saw as drift after an
	// agent.toml change: `gc session reset` cleared the conversation but
	// left these intact, so the next wake resumed the prior conversation
	// instead of starting fresh under the new spawn command.
	stale := map[string]string{
		"command":             "claude --dangerously-skip-permissions --effort max --resume 536a5a4b-286b-4a8f-ad9e-b8d2e8d7953e",
		"resume_flag":         "--resume",
		"resume_style":        "flag",
		"resume_command":      "claude --resume",
		"session_key":         "536a5a4b-286b-4a8f-ad9e-b8d2e8d7953e",
		"continuation_epoch":  "17",
		"started_config_hash": "deadbeefcafef00d",
		"started_live_hash":   "feedfacecafe1234",
		"live_hash":           "abc123",
	}
	if err := store.SetMetadataBatch(info.ID, stale); err != nil {
		t.Fatalf("SetMetadataBatch(stale): %v", err)
	}

	if err := mgr.RequestFreshRestart(info.ID); err != nil {
		t.Fatalf("RequestFreshRestart: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}

	// Restart flags must be set so the reconciler picks the bead up.
	if got := b.Metadata["restart_requested"]; got != "true" {
		t.Errorf("restart_requested = %q, want %q", got, "true")
	}
	if got := b.Metadata["continuation_reset_pending"]; got != "true" {
		t.Errorf("continuation_reset_pending = %q, want %q", got, "true")
	}

	// Provider-identity spawn metadata must be cleared so the next
	// reconcile tick refills from the current agent.toml.
	clearedFields := []string{
		"command",
		"resume_flag",
		"resume_style",
		"resume_command",
		"continuation_epoch",
	}
	for _, field := range clearedFields {
		if got := b.Metadata[field]; got != "" {
			t.Errorf("after RequestFreshRestart, %s = %q, want cleared", field, got)
		}
	}

	// session_key and the runtime hash fields are deliberately PRESERVED
	// here so the controller-side reconciler can decide on the next tick
	// whether the prior conversation is still resumable (see the comment
	// on RequestFreshRestart and TestResetConfiguredNamedSessionForConfigDrift_PreservesSessionKeyOnContinuationReset).
	preservedFields := map[string]string{
		"session_key":         "536a5a4b-286b-4a8f-ad9e-b8d2e8d7953e",
		"started_config_hash": "deadbeefcafef00d",
		"started_live_hash":   "feedfacecafe1234",
		"live_hash":           "abc123",
	}
	for field, want := range preservedFields {
		if got := b.Metadata[field]; got != want {
			t.Errorf("after RequestFreshRestart, %s = %q, want preserved %q", field, got, want)
		}
	}
}
