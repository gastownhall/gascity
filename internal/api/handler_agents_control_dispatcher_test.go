package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// controlDispatcherAgent is the SDK's control dispatcher as the core pack
// declares it: a start_command that runs `gc convoy control --serve`, and no
// provider.
func controlDispatcherAgent(t *testing.T, dir string) config.Agent {
	t.Helper()
	a := config.Agent{
		Name:              config.ControlDispatcherAgentName,
		Dir:               dir,
		StartCommand:      config.ControlDispatcherStartCommand,
		MaxActiveSessions: intPtr(1),
	}
	if !config.IsDeterministicControlDispatcher(&a) {
		t.Fatalf("fixture %+v is not the deterministic control dispatcher", a)
	}
	return a
}

// lookPathNotFound is a LookPathFunc for a host with no provider binary
// installed.
func lookPathNotFound(string) (string, error) {
	return "", errors.New("executable file not found in $PATH")
}

// TestAgentControlDispatcherHasNoProvider pins that the control dispatcher,
// providerless by design, does not inherit the workspace provider: the agent
// list and the agent read report no provider for it, and a provider binary
// missing from PATH does not mark it unavailable. An ordinary agent without a
// provider still inherits the workspace default, and its missing binary still
// marks it unavailable, which also proves the PATH check ran.
func TestAgentControlDispatcherHasNoProvider(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Workspace.Provider = "claude"
	state.cfg.Agents = []config.Agent{
		controlDispatcherAgent(t, "myrig"),
		{Name: "coder", Dir: "myrig", MaxActiveSessions: intPtr(1)},
	}
	srv := New(state)
	srv.LookPathFunc = lookPathNotFound
	h := newTestCityHandlerWith(t, state, srv)

	list := func(t *testing.T) map[string]agentResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/agents"), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /agents status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var resp struct {
			Items []agentResponse `json:"items"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode /agents: %v", err)
		}
		rows := make(map[string]agentResponse, len(resp.Items))
		for _, item := range resp.Items {
			rows[item.Name] = item
		}
		return rows
	}
	get := func(t *testing.T, name string) agentResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/agent/"+name), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /agent/%s status = %d, want %d: %s", name, rec.Code, http.StatusOK, rec.Body.String())
		}
		var resp agentResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode /agent/%s: %v", name, err)
		}
		return resp
	}

	dispatcher := "myrig/" + config.ControlDispatcherAgentName
	rows := list(t)
	for _, tt := range []struct {
		endpoint string
		row      agentResponse
	}{
		{endpoint: "GET /agents", row: rows[dispatcher]},
		{endpoint: "GET /agent/" + dispatcher, row: get(t, dispatcher)},
	} {
		if tt.row.Name != dispatcher {
			t.Fatalf("%s: no row for %s (rows=%+v)", tt.endpoint, dispatcher, rows)
		}
		if tt.row.Provider != "" || tt.row.DisplayName != "" {
			t.Errorf("%s: dispatcher provider = %q, display_name = %q; want both empty, it runs no provider",
				tt.endpoint, tt.row.Provider, tt.row.DisplayName)
		}
		if !tt.row.Available || tt.row.UnavailableReason != "" {
			t.Errorf("%s: dispatcher available = %v (%q), want true: no provider binary gates it",
				tt.endpoint, tt.row.Available, tt.row.UnavailableReason)
		}
	}

	for _, tt := range []struct {
		endpoint string
		row      agentResponse
	}{
		{endpoint: "GET /agents", row: rows["myrig/coder"]},
		{endpoint: "GET /agent/myrig/coder", row: get(t, "myrig/coder")},
	} {
		if tt.row.Provider != "claude" || tt.row.DisplayName != "Claude Code" {
			t.Errorf("%s: coder provider = %q, display_name = %q; want the workspace default claude / Claude Code",
				tt.endpoint, tt.row.Provider, tt.row.DisplayName)
		}
		if want := "provider 'claude' not found in PATH"; tt.row.Available || tt.row.UnavailableReason != want {
			t.Errorf("%s: coder available = %v (%q), want false (%q)", tt.endpoint, tt.row.Available, tt.row.UnavailableReason, want)
		}
	}
}

// TestSessionToResponseControlDispatcherHasNoProvider pins the session side:
// a control-dispatcher session records no provider because it has none, so
// its response must not fill in the workspace default. A session of an
// ordinary agent that recorded no provider still falls back to it.
func TestSessionToResponseControlDispatcherHasNoProvider(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "claude"},
		Agents: []config.Agent{
			controlDispatcherAgent(t, ""),
			{Name: "coder", MaxActiveSessions: intPtr(1)},
		},
	}

	got := sessionToResponse(session.Info{ID: "gc-1", Template: config.ControlDispatcherAgentName}, cfg)
	if got.Provider != "" || got.DisplayName != "" {
		t.Errorf("control-dispatcher session provider = %q, display_name = %q; want both empty", got.Provider, got.DisplayName)
	}

	got = sessionToResponse(session.Info{ID: "gc-2", Template: "coder"}, cfg)
	if got.Provider != "claude" || got.DisplayName != "Claude Code" {
		t.Errorf("coder session provider = %q, display_name = %q; want the workspace default claude / Claude Code", got.Provider, got.DisplayName)
	}
}

// TestSessionModelContextSkipsControlDispatcher pins that a control-dispatcher
// session reports no model or context: it runs no model, so a transcript in
// its work dir belongs to another session, here a Claude agent sharing that
// directory. The same transcript does enrich a session of an agent inheriting
// the workspace provider, which proves the fixture is discoverable.
func TestSessionModelContextSkipsControlDispatcher(t *testing.T) {
	const model = "claude-opus-4-5-20251101"
	state := newFakeState(t)
	state.cfg.Workspace.Provider = "claude"
	state.cfg.Agents = []config.Agent{
		controlDispatcherAgent(t, ""),
		{Name: "coder", MaxActiveSessions: intPtr(1)},
	}
	workDir := t.TempDir()
	searchDir := t.TempDir()
	slugDir := filepath.Join(searchDir, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"assistant","message":{"role":"assistant","model":"` + model + `","usage":{"input_tokens":10000}}}` + "\n"
	if err := os.WriteFile(filepath.Join(slugDir, "coder-session.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := New(state)
	srv.sessionLogSearchPaths = []string{searchDir}

	for _, tt := range []struct {
		name        string
		template    string
		sessionName string
		wantModel   string
	}{
		{name: "control dispatcher", template: config.ControlDispatcherAgentName, sessionName: "control-dispatcher-gc-1", wantModel: ""},
		{name: "agent inheriting the workspace provider", template: "coder", sessionName: "coder", wantModel: model},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := state.sp.Start(context.Background(), tt.sessionName, runtime.Config{}); err != nil {
				t.Fatalf("start runtime session %s: %v", tt.sessionName, err)
			}
			info := session.Info{
				ID:          "gc-" + tt.sessionName,
				Template:    tt.template,
				State:       session.StateActive,
				SessionName: tt.sessionName,
				WorkDir:     workDir,
			}
			resp := sessionToResponse(info, state.cfg)
			srv.enrichSessionResponse(&resp, info, state.cfg, sessionResponseCapabilityHandle{}, false, false, true, 0)
			if !resp.Running {
				t.Fatalf("running = false, want true: runtime session %s is live", tt.sessionName)
			}
			if resp.Model != tt.wantModel {
				t.Errorf("model = %q, want %q", resp.Model, tt.wantModel)
			}
		})
	}
}

// TestCanAttributeSessionIgnoresControlDispatcher pins that the control
// dispatcher, which writes no transcript, does not make transcript
// attribution ambiguous for a Claude agent sharing its work dir, as a second
// Claude agent there does.
func TestCanAttributeSessionIgnoresControlDispatcher(t *testing.T) {
	cityPath := t.TempDir()
	coder := config.Agent{Name: "coder", Dir: "myrig", MaxActiveSessions: intPtr(1)}
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "claude"},
		Agents:    []config.Agent{controlDispatcherAgent(t, "myrig"), coder},
		Rigs:      []config.Rig{{Name: "myrig", Path: filepath.Join(cityPath, "myrig")}},
	}
	if !canAttributeSession(coder, "myrig/coder", cfg, cityPath) {
		t.Errorf("canAttributeSession(myrig/coder) = false beside the control dispatcher, want true")
	}

	cfg.Agents = append(cfg.Agents, config.Agent{Name: "reviewer", Dir: "myrig", MaxActiveSessions: intPtr(1)})
	if canAttributeSession(coder, "myrig/coder", cfg, cityPath) {
		t.Errorf("canAttributeSession(myrig/coder) = true beside a second Claude agent in the same work dir, want false")
	}
}
