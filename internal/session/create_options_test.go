package session

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestCreateOptionsDefaultSessionOrigin(t *testing.T) {
	if got := (CreateOptions{}).defaultSessionOrigin(); got != "manual" {
		t.Errorf("started defaultSessionOrigin = %q, want %q", got, "manual")
	}
	if got := (CreateOptions{BeadOnly: true}).defaultSessionOrigin(); got != "ephemeral" {
		t.Errorf("bead-only defaultSessionOrigin = %q, want %q", got, "ephemeral")
	}
}

func TestCreateSessionStartedDefaultsToManualOrigin(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "helper",
		Title:    "my chat",
		Command:  "claude",
		WorkDir:  "/tmp",
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if info.State != StateActive {
		t.Errorf("State = %q, want %q", info.State, StateActive)
	}
	if !sp.IsRunning(info.SessionName) {
		t.Error("runtime session not started")
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Metadata["session_origin"] != "manual" {
		t.Errorf("session_origin = %q, want %q", b.Metadata["session_origin"], "manual")
	}
}

func TestCreateSessionBeadOnlyDefaultsToEphemeralOrigin(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		BeadOnly: true,
		Template: "helper",
		Title:    "queued",
		Command:  "claude",
		WorkDir:  "/tmp",
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession(bead-only): %v", err)
	}
	if info.State != StateStartPending {
		t.Errorf("State = %q, want %q", info.State, StateStartPending)
	}
	if sp.IsRunning(info.SessionName) {
		t.Error("bead-only create must not start a runtime session")
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Metadata["session_origin"] != "ephemeral" {
		t.Errorf("session_origin = %q, want %q", b.Metadata["session_origin"], "ephemeral")
	}
}

func TestCreateSessionExtraMetaOverridesOriginDefault(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template:  "helper",
		Command:   "claude",
		WorkDir:   "/tmp",
		Provider:  "claude",
		ExtraMeta: map[string]string{"session_origin": "named"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Metadata["session_origin"] != "named" {
		t.Errorf("session_origin = %q, want explicit %q", b.Metadata["session_origin"], "named")
	}
}

// TestCreateSessionFieldNamedSpecMapsCorrectly guards against argument
// transposition: alias, explicit name, and transport land on their own fields.
func TestCreateSessionFieldNamedSpecMapsCorrectly(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Alias:        "sky",
		ExplicitName: "myrig--worker",
		Template:     "helper",
		Title:        "Sky",
		Command:      "claude",
		WorkDir:      "/tmp",
		Provider:     "claude",
		Transport:    "acp",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Metadata["alias"] != "sky" {
		t.Errorf("alias = %q, want %q", b.Metadata["alias"], "sky")
	}
	if b.Metadata["session_name"] != "myrig--worker" {
		t.Errorf("session_name = %q, want %q", b.Metadata["session_name"], "myrig--worker")
	}
	if b.Metadata["transport"] != "acp" {
		t.Errorf("transport = %q, want %q", b.Metadata["transport"], "acp")
	}
	if b.Metadata["template"] != "helper" {
		t.Errorf("template = %q, want %q", b.Metadata["template"], "helper")
	}
}

// TestCreateSessionMatchesLegacyWrapper proves the collapsed CreateSession
// default coincides with the legacy started-wrapper's hardcoded
// session_origin=manual. The retired Create/CreateNamed* wrappers stamped
// "manual" literally; the collapsed path instead relies on
// defaultSessionOrigin(). Locking those two together means a future change to
// the default that diverged from the historical hardcoded value would fail
// here, rather than silently altering started-session provenance.
func TestCreateSessionMatchesLegacyWrapper(t *testing.T) {
	viaDefault := createOriginMetadata(t, func(mgr *Manager) (Info, error) {
		// No session_origin in ExtraMeta: exercise the collapsed default path.
		return mgr.CreateSession(context.Background(), CreateOptions{
			Template: "helper", Title: "chat", Command: "claude", WorkDir: "/tmp", Provider: "claude",
		})
	})
	viaLegacyExplicit := createOriginMetadata(t, func(mgr *Manager) (Info, error) {
		// The value the retired started wrappers hardcoded.
		return mgr.CreateSession(context.Background(), CreateOptions{
			Template: "helper", Title: "chat", Command: "claude", WorkDir: "/tmp", Provider: "claude",
			ExtraMeta: map[string]string{"session_origin": "manual"},
		})
	})
	if viaDefault != viaLegacyExplicit {
		t.Errorf("session_origin parity mismatch: default=%q legacy-explicit=%q", viaDefault, viaLegacyExplicit)
	}
	if viaDefault != "manual" {
		t.Errorf("collapsed default session_origin = %q, want legacy %q", viaDefault, "manual")
	}
}

func createOriginMetadata(t *testing.T, create func(*Manager) (Info, error)) string {
	t.Helper()
	store := beads.NewMemStore()
	mgr := NewManagerWithOptions(store, runtime.NewFake())
	info, err := create(mgr)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	return b.Metadata["session_origin"]
}
