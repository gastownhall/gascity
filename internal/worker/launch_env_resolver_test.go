package worker

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestFactoryResolveLaunchEnvFeedsReplacementStart covers the worker boundary
// half of #8184: a factory with ResolveLaunchEnv wires it into the session
// manager, so a start that launches a missing runtime merges the resolver's
// env into the replacement. The resolver receives the row's
// real_world_app_session_kind, derived the same way the runtime resolver's
// is.
func TestFactoryResolveLaunchEnvFeedsReplacementStart(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()))

	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{BeadOnly: true, Template: "worker", Title: "Probe", Command: "", WorkDir: t.TempDir(), Provider: "stub", Resume: sessionpkg.ProviderResume{SessionIDFlag: "--session-id"}})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	if err := store.SetMetadata(info.ID, "real_world_app_session_kind", "agent"); err != nil {
		t.Fatalf("SetMetadata(real_world_app_session_kind): %v", err)
	}

	var gotSessionKind string
	factory, err := NewFactory(FactoryConfig{
		CityPath: t.TempDir(),
		Store:    store,
		Provider: sp,
		ResolveSessionRuntime: func(_ sessionpkg.Info, _ string, _ map[string]string) (*ResolvedRuntime, error) {
			return &ResolvedRuntime{
				Command:  "/bin/echo",
				WorkDir:  t.TempDir(),
				Provider: "stub",
				Resume:   sessionpkg.ProviderResume{SessionIDFlag: "--session-id"},
				Hints:    runtime.Config{Env: map[string]string{"HINTS_ENV": "hints"}},
			}, nil
		},
		ResolveLaunchEnv: func(_ sessionpkg.Info, sessionKind string, _ map[string]string) (map[string]string, error) {
			gotSessionKind = sessionKind
			return map[string]string{"GC_RIG": "myrig", "HINTS_ENV": "launch"}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	handle, err := factory.SessionByID(info.ID)
	if err != nil {
		t.Fatalf("SessionByID(%q): %v", info.ID, err)
	}
	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if gotSessionKind != "agent" {
		t.Fatalf("sessionKind = %q, want agent", gotSessionKind)
	}
	start := sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil, want a replacement start")
	}
	if got := start.Env["GC_RIG"]; got != "myrig" {
		t.Errorf("start Env[GC_RIG] = %q, want myrig", got)
	}
	// The launch env wins over the hints env for the keys it sets.
	if got := start.Env["HINTS_ENV"]; got != "launch" {
		t.Errorf("start Env[HINTS_ENV] = %q, want launch (launch env overrides hints)", got)
	}
}

// TestFactoryWithoutResolveLaunchEnvKeepsHintsEnv: a nil ResolveLaunchEnv
// leaves the option unset, so the hints env alone starts the replacement —
// the pre-#8184 behavior, preserved for factories that configure no
// resolver (the API factory, session-logs factories).
func TestFactoryWithoutResolveLaunchEnvKeepsHintsEnv(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()))

	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{BeadOnly: true, Template: "worker", Title: "Probe", Command: "", WorkDir: t.TempDir(), Provider: "stub", Resume: sessionpkg.ProviderResume{SessionIDFlag: "--session-id"}})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}

	factory, err := NewFactory(FactoryConfig{
		CityPath: t.TempDir(),
		Store:    store,
		Provider: sp,
		ResolveSessionRuntime: func(_ sessionpkg.Info, _ string, _ map[string]string) (*ResolvedRuntime, error) {
			return &ResolvedRuntime{
				Command:  "/bin/echo",
				WorkDir:  t.TempDir(),
				Provider: "stub",
				Resume:   sessionpkg.ProviderResume{SessionIDFlag: "--session-id"},
				Hints:    runtime.Config{Env: map[string]string{"HINTS_ENV": "hints"}},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	handle, err := factory.SessionByID(info.ID)
	if err != nil {
		t.Fatalf("SessionByID(%q): %v", info.ID, err)
	}
	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	start := sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil, want a start")
	}
	if got := start.Env["HINTS_ENV"]; got != "hints" {
		t.Errorf("start Env[HINTS_ENV] = %q, want hints (no resolver configured)", got)
	}
	if _, ok := start.Env["GC_RIG"]; ok {
		t.Errorf("start Env[GC_RIG] = %q, want absent with no resolver", start.Env["GC_RIG"])
	}
}
