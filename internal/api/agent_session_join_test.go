package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestAgentSessionIdentitiesPrefersAgentNameThenAlias(t *testing.T) {
	keys := agentSessionIdentities("devcity", "", session.Info{
		Template:    "myrig/pack.archivist",
		AgentName:   "myrig/pack.archivist-2",
		Alias:       "myrig/pack.archivist-1",
		SessionName: "pack__archivist-de-o44",
	})

	if len(keys) == 0 || keys[0] != "myrig/pack.archivist-2" {
		t.Fatalf("keys = %v, want AgentName first", keys)
	}
	if len(keys) < 2 || keys[1] != "myrig/pack.archivist-1" {
		t.Fatalf("keys = %v, want Alias second", keys)
	}
}

// The template names the pool, not a slot. Attributing a session to it would
// let a single worker masquerade as the whole pool in the roster.
func TestAgentSessionIdentitiesSkipsTemplate(t *testing.T) {
	keys := agentSessionIdentities("devcity", "", session.Info{
		Template:    "myrig/pack.archivist",
		AgentName:   "myrig/pack.archivist",
		Alias:       "myrig/pack.archivist",
		SessionName: "myrig--pack__archivist",
	})

	for _, key := range keys {
		if key == "myrig/pack.archivist" {
			t.Fatalf("keys = %v, must not contain the bare template", keys)
		}
	}
}

func TestAgentSessionIdentitiesFallsBackToSessionName(t *testing.T) {
	keys := agentSessionIdentities("devcity", "", session.Info{
		Template:    "myrig/pack.archivist",
		SessionName: "myrig--pack__archivist-1",
	})

	if len(keys) != 1 || keys[0] != "myrig/pack.archivist-1" {
		t.Fatalf("keys = %v, want the unsanitized session name", keys)
	}
}

type stubRunningProvider struct {
	running map[string]bool
}

func (s stubRunningProvider) IsRunning(name string) bool { return s.running[name] }

func TestResolveAgentRuntimeDeterministicHitSkipsIndex(t *testing.T) {
	sp := stubRunningProvider{running: map[string]bool{"mayor": true}}
	lookupCalls := 0
	lookup := func() map[string]liveAgentSession {
		lookupCalls++
		return nil
	}

	name, running := resolveAgentRuntime(sp, "mayor", "mayor", lookup)

	if !running || name != "mayor" {
		t.Fatalf("got (%q, %v), want (\"mayor\", true)", name, running)
	}
	if lookupCalls != 0 {
		t.Errorf("lookup called %d times on the deterministic hit path, want 0", lookupCalls)
	}
}

func TestResolveAgentRuntimeAdoptsLiveSessionName(t *testing.T) {
	sp := stubRunningProvider{running: map[string]bool{"pack__archivist-de-o44": true}}
	lookup := func() map[string]liveAgentSession {
		return map[string]liveAgentSession{
			"myrig/pack.archivist-1": {sessionName: "pack__archivist-de-o44", state: session.StateActive},
		}
	}

	name, running := resolveAgentRuntime(
		sp, "myrig--pack__archivist-1", "myrig/pack.archivist-1", lookup)

	if !running {
		t.Error("running = false, want true — the slot's session is alive under a different name")
	}
	if name != "pack__archivist-de-o44" {
		t.Errorf("sessionName = %q, want the live runtime name", name)
	}
}

// When the runtime probe cannot see the session (a socket mismatch, a partial
// runtime), the persisted state is the better answer for the roster.
func TestResolveAgentRuntimeFallsBackToPersistedState(t *testing.T) {
	sp := stubRunningProvider{running: map[string]bool{}}
	lookup := func() map[string]liveAgentSession {
		return map[string]liveAgentSession{
			"myrig/pack.archivist-1": {sessionName: "pack__archivist-de-o44", state: session.StateActive},
		}
	}

	_, running := resolveAgentRuntime(
		sp, "myrig--pack__archivist-1", "myrig/pack.archivist-1", lookup)

	if !running {
		t.Error("running = false, want true from the persisted active state")
	}
}

func TestResolveAgentRuntimeAsleepSessionIsNotRunning(t *testing.T) {
	sp := stubRunningProvider{running: map[string]bool{}}
	lookup := func() map[string]liveAgentSession {
		return map[string]liveAgentSession{
			"myrig/pack.archivist-1": {sessionName: "pack__archivist-de-o44", state: session.StateAsleep},
		}
	}

	name, running := resolveAgentRuntime(
		sp, "myrig--pack__archivist-1", "myrig/pack.archivist-1", lookup)

	if running {
		t.Error("running = true for an asleep session, want false")
	}
	if name != "pack__archivist-de-o44" {
		t.Errorf("sessionName = %q, want the live runtime name even when not running", name)
	}
}

func TestResolveAgentRuntimeNoMatchKeepsDeterministicName(t *testing.T) {
	sp := stubRunningProvider{running: map[string]bool{}}
	lookup := func() map[string]liveAgentSession { return nil }

	name, running := resolveAgentRuntime(
		sp, "myrig--pack__archivist-1", "myrig/pack.archivist-1", lookup)

	if running {
		t.Error("running = true with no matching session, want false")
	}
	if name != "myrig--pack__archivist-1" {
		t.Errorf("sessionName = %q, want the deterministic name preserved", name)
	}
}

// End-to-end regression for #4703: a bounded pool whose live session is named
// after its session id (not its slot ordinal) must still report running.
func TestAgentListReportsBoundedPoolSlotRunningFromAliasedSession(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.cfg.Agents = []config.Agent{
		{
			Name:              "archivist",
			Dir:               "myrig",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		},
	}

	mgr := session.NewManagerWithOptions(state.cityBeadStore, state.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template:  "myrig/archivist",
		Alias:     "myrig/archivist-1",
		Title:     "myrig/archivist-1",
		Command:   "echo test",
		WorkDir:   "/tmp",
		Provider:  "test",
		Hints:     runtime.Config{},
		ExtraMeta: map[string]string{"session_origin": "ephemeral"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Guard the premise: the slot's deterministic name must NOT be what the
	// runtime actually started, else this test would pass without the fix.
	deterministic := agentSessionName(state.CityName(), "myrig/archivist-1", "")
	if info.SessionName == deterministic {
		t.Fatalf("runtime session name %q equals the deterministic slot name; "+
			"the bug this test covers cannot occur", info.SessionName)
	}
	if !state.sp.IsRunning(info.SessionName) {
		t.Fatalf("session %q not running in the fake provider", info.SessionName)
	}

	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)
	req := httptest.NewRequest("GET", cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []agentResponse `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var slot *agentResponse
	for i := range resp.Items {
		if resp.Items[i].Name == "myrig/archivist-1" {
			slot = &resp.Items[i]
			break
		}
	}
	if slot == nil {
		t.Fatalf("slot myrig/archivist-1 absent from roster: %+v", resp.Items)
	}
	if !slot.Running {
		t.Errorf("slot Running = false, want true (live session %q)", info.SessionName)
	}
	if slot.State == "stopped" {
		t.Errorf("slot State = %q, want a live state", slot.State)
	}
	if slot.Session == nil || slot.Session.Name != info.SessionName {
		t.Errorf("slot Session = %+v, want the live runtime name %q", slot.Session, info.SessionName)
	}
}
