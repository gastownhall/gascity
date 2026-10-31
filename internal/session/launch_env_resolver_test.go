package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSendResolvesLaunchEnvWhenStartingReplacement covers the resume half of
// #8184: a start that must launch a replacement runtime consults the
// launch-env resolver and merges its env over the hints env, so the
// replacement carries the generated agent launch layer. The resolver sees the
// persisted Info and a clone of the row's metadata.
func TestSendResolvesLaunchEnvWhenStartingReplacement(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	var gotInfo Info
	var gotMeta map[string]string
	mgr := newTestManager(t, store, sp, WithLaunchEnvResolver(func(info Info, metadata map[string]string) (map[string]string, error) {
		gotInfo = info
		gotMeta = metadata
		return map[string]string{"GC_RIG": "myrig", "GC_RIG_ROOT": "/rigs/myrig"}, nil
	}))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: "/tmp", Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// A stale hints value the launch env must override.
	hints := runtime.Config{WorkDir: "/tmp", Env: map[string]string{"GC_RIG": "stale-caller-rig"}}
	if _, err := mgr.Send(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", "claude --resume "+info.SessionKey, hints); err != nil {
		t.Fatalf("Send: %v", err)
	}

	start := sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil, want a replacement start")
	}
	for key, want := range map[string]string{"GC_RIG": "myrig", "GC_RIG_ROOT": "/rigs/myrig"} {
		if got := start.Env[key]; got != want {
			t.Errorf("start Env[%s] = %q, want %q", key, got, want)
		}
	}
	if gotInfo.ID != info.ID {
		t.Errorf("resolver info.ID = %q, want %q", gotInfo.ID, info.ID)
	}
	if got, want := gotMeta["session_name"], info.SessionName; got != want {
		t.Errorf("resolver metadata[session_name] = %q, want %q", got, want)
	}
	// The metadata map is a clone: writing through it must not reach the row.
	gotMeta["session_name"] = "mutated"
	fresh, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fresh.SessionName != info.SessionName {
		t.Errorf("row session_name = %q, mutated through the resolver's metadata map", fresh.SessionName)
	}
}

// TestSendDoesNotResolveLaunchEnvWhenRuntimeConverges: the resolver is a
// start-time hook. A delivery into a live runtime never consults it.
func TestSendDoesNotResolveLaunchEnvWhenRuntimeConverges(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	calls := 0
	mgr := newTestManager(t, store, sp, WithLaunchEnvResolver(func(Info, map[string]string) (map[string]string, error) {
		calls++
		return map[string]string{"GC_RIG": "myrig"}, nil
	}))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: "/tmp", Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := mgr.Send(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", "claude --resume "+info.SessionKey, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if calls != 0 {
		t.Fatalf("launch-env resolver called %d times, want 0 for a live runtime", calls)
	}
}

// TestSendLaunchEnvErrorRefusesStart: a resolver error refuses the resume
// before anything is killed, minted or started. The error names the session
// and wraps the cause, and the row's incarnation is untouched.
func TestSendLaunchEnvErrorRefusesStart(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cause := errors.New("backend projection failed")
	mgr := newTestManager(t, store, sp, WithLaunchEnvResolver(func(Info, map[string]string) (map[string]string, error) {
		return nil, cause
	}))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: "/tmp", Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	rowBefore, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get before Send: %v", err)
	}

	sp.Calls = nil
	_, err = mgr.Send(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", "claude --resume "+info.SessionKey, runtime.Config{WorkDir: "/tmp"})
	if err == nil {
		t.Fatal("Send = nil error, want the refused start")
	}
	if !strings.Contains(err.Error(), info.ID) {
		t.Errorf("Send err = %q, want it to name session %q", err, info.ID)
	}
	if !errors.Is(err, cause) {
		t.Errorf("Send err = %q, want it to wrap the resolver cause", err)
	}
	for _, call := range sp.Calls {
		if call.Method == "Start" {
			t.Fatalf("calls = %#v, want no Start after a refused resume", sp.Calls)
		}
	}
	rowAfter, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get after Send: %v", err)
	}
	for _, key := range []string{"generation", "instance_token"} {
		if got, want := rowAfter.Metadata[key], rowBefore.Metadata[key]; got != want {
			t.Errorf("row %s = %q after the refused start, want unchanged %q", key, got, want)
		}
	}
}
