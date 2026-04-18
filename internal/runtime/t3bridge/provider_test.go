package t3bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gorilla/websocket"
)

func TestResolveProviderModel_PrefersCurrentConfigOverStoredEnvelope(t *testing.T) {
	cfg := runtime.Config{
		Command: "codex --dangerously-bypass-approvals-and-sandbox",
		Env: map[string]string{
			"GC_MODEL": "gpt-5.4-mini",
		},
	}
	envelope := StartupEnvelope{
		Runtime: RuntimeSection{
			Provider: "claudeAgent",
			Model:    "claude-sonnet-4-6",
		},
	}

	provider, model := resolveProviderModel(cfg, envelope)
	if provider != "codex" {
		t.Fatalf("provider = %q, want codex", provider)
	}
	if model != "gpt-5.4-mini" {
		t.Fatalf("model = %q, want gpt-5.4-mini", model)
	}
}

func TestResolveProviderModel_NormalizesClaudeProviderName(t *testing.T) {
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_PROVIDER": "claude",
			"GC_MODEL":    "claude-sonnet-4-6",
		},
	}

	provider, model := resolveProviderModel(cfg, StartupEnvelope{})
	if provider != "claudeAgent" {
		t.Fatalf("provider = %q, want claudeAgent", provider)
	}
	if model != "claude-sonnet-4-6" {
		t.Fatalf("model = %q, want claude-sonnet-4-6", model)
	}
}

func TestResolveProviderModel_InfersCodexFromGptModelWhenProviderMissing(t *testing.T) {
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_MODEL": "gpt-5.4-mini",
		},
	}

	provider, model := resolveProviderModel(cfg, StartupEnvelope{})
	if provider != "codex" {
		t.Fatalf("provider = %q, want codex", provider)
	}
	if model != "gpt-5.4-mini" {
		t.Fatalf("model = %q, want gpt-5.4-mini", model)
	}
}

func TestResolveProviderModel_DefaultsCodexToGPT54WhenModelMissing(t *testing.T) {
	cfg := runtime.Config{
		Command: "codex --dangerously-bypass-approvals-and-sandbox",
	}

	provider, model := resolveProviderModel(cfg, StartupEnvelope{})
	if provider != "codex" {
		t.Fatalf("provider = %q, want codex", provider)
	}
	if model != defaultCodexModel {
		t.Fatalf("model = %q, want %s", model, defaultCodexModel)
	}
}

func TestProcessAlive_ReadyCountsAsAlive(t *testing.T) {
	server := newT3BridgeTestServer(t, map[string]interface{}{
		"threads": []interface{}{
			map[string]interface{}{
				"id":        "thread-1",
				"projectId": "project-1",
				"customMetadata": map[string]interface{}{
					"gc.agent":       "mayor",
					"gc.sessionName": "mayor",
				},
				"session": map[string]interface{}{
					"status": "ready",
				},
			},
		},
	})
	defer server.Close()
	t.Setenv("T3_WS_URL", server.wsURL())

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}

	if !p.ProcessAlive("mayor", nil) {
		t.Fatal("ProcessAlive(ready) = false, want true")
	}
}

func TestStart_ReusedThreadDoesNotInjectStartupTurns(t *testing.T) {
	server := newT3BridgeTestServer(t, map[string]interface{}{
		"projects": []interface{}{
			map[string]interface{}{
				"id":            "project-1",
				"workspaceRoot": "/tmp/mayor",
			},
		},
		"threads": []interface{}{
			map[string]interface{}{
				"id": "thread-1",
				"session": map[string]interface{}{
					"status": "ready",
				},
			},
		},
	})
	defer server.Close()

	t.Setenv("T3_WS_URL", server.wsURL())

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}
	cfg := runtime.Config{
		WorkDir:      "/tmp/mayor",
		Command:      "codex",
		PromptSuffix: "gc prime --hook",
		Nudge:        "Check mail and hook status, then act accordingly.",
		Env: map[string]string{
			"GC_CITY_PATH": "/tmp/gc",
			"GC_ALIAS":     "mayor",
			"GC_TEMPLATE":  "mayor",
			"GC_PROVIDER":  "codex",
			"GC_MODEL":     "gpt-5.4",
		},
	}
	server.snapshot["threads"] = []interface{}{
		map[string]interface{}{
			"id":        "thread-1",
			"projectId": "project-1",
			"title":     "mayor · mayor",
			"model":     "gpt-5.4",
			"customMetadata": map[string]interface{}{
				"gc.agent":           "mayor",
				"gc.sessionName":     "mayor",
				"gc.startupTemplate": "mayor",
				"gc.startupWorkDir":  cfg.WorkDir,
				"gc.runtimeProvider": "codex",
				"gc.startupModel":    "gpt-5.4",
			},
			"session": map[string]interface{}{
				"status": "ready",
			},
		},
	}

	if err := p.Start(context.Background(), "mayor", cfg); err != nil {
		t.Fatalf("Start(reuse): %v", err)
	}

	for _, typ := range server.commandTypes() {
		if typ == "thread.turn.start" {
			t.Fatalf("reused thread received startup turn: commands=%v", server.commandTypes())
		}
	}
}

func TestBuildThreadEnv_DropsStartupEnvelope(t *testing.T) {
	env := buildThreadEnv(map[string]string{
		"GC_STARTUP_ENVELOPE":      `{"runtime":{"provider":"claudeAgent","model":"claude-sonnet-4-6"}}`,
		"GC_MODEL":                 "gpt-5.4-mini",
		"GC_SESSION_NAME":          "gc--mayor",
		"GC_DOLT_HOST":             "127.0.0.1",
		"GC_DOLT_PORT":             "35819",
		"BEADS_DOLT_SHARED_SERVER": "1",
		"NOT_GC":                   "ignore",
	})

	if _, ok := env["GC_STARTUP_ENVELOPE"]; ok {
		t.Fatal("GC_STARTUP_ENVELOPE should not persist into thread env")
	}
	if env["GC_MODEL"] != "gpt-5.4-mini" {
		t.Fatalf("GC_MODEL = %q, want gpt-5.4-mini", env["GC_MODEL"])
	}
	if env["GC_SESSION_NAME"] != "gc--mayor" {
		t.Fatalf("GC_SESSION_NAME = %q, want gc--mayor", env["GC_SESSION_NAME"])
	}
	if env["BEADS_DOLT_SERVER_HOST"] != "127.0.0.1" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want 127.0.0.1", env["BEADS_DOLT_SERVER_HOST"])
	}
	if env["BEADS_DOLT_SERVER_PORT"] != "35819" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want 35819", env["BEADS_DOLT_SERVER_PORT"])
	}
	if env["BEADS_DOLT_SERVER_MODE"] != "1" {
		t.Fatalf("BEADS_DOLT_SERVER_MODE = %q, want 1", env["BEADS_DOLT_SERVER_MODE"])
	}
	if _, ok := env["BEADS_DOLT_SHARED_SERVER"]; ok {
		t.Fatal("BEADS_DOLT_SHARED_SERVER should not persist into thread env")
	}
	if _, ok := env["NOT_GC"]; ok {
		t.Fatal("non-GC key should not persist into thread env")
	}
}

func TestBuildGCMetadata_UsesFirstClassT3BridgeProviderName(t *testing.T) {
	meta := buildGCMetadata(StartupEnvelope{}, "codex", "active", nil)
	if got := meta["gc.provider"]; got != "t3bridge" {
		t.Fatalf("gc.provider = %v, want t3bridge", got)
	}
}

func TestDeriveProjectWorkspaceRoot_UsesCityRootForCityAgents(t *testing.T) {
	root := deriveProjectWorkspaceRoot("/data/projects/gc/.gc/agents/deacon", StartupEnvelope{
		GC: GCSection{
			CityPath: "/data/projects/gc",
			Agent:    "deacon",
		},
	})

	if root != "/data/projects/gc" {
		t.Fatalf("root = %q, want /data/projects/gc", root)
	}
}

func TestDeriveProjectWorkspaceRoot_UsesRigRootForRigAgents(t *testing.T) {
	root := deriveProjectWorkspaceRoot("/data/projects/gc/.gc/agents/t3code/witness", StartupEnvelope{
		GC: GCSection{
			CityPath: "/data/projects/gc",
			RigPath:  "/data/projects/t3code",
			RigName:  "t3code",
			Agent:    "t3code/witness",
		},
	})

	if root != "/data/projects/t3code" {
		t.Fatalf("root = %q, want /data/projects/t3code", root)
	}
}

func TestDeriveProjectTitle_UsesWorkspaceRootInsteadOfAgentCwd(t *testing.T) {
	title := deriveProjectTitle("deacon", "/data/projects/gc", StartupEnvelope{
		GC: GCSection{
			Agent: "deacon",
		},
	})

	if title != "gc" {
		t.Fatalf("title = %q, want gc", title)
	}
}

func TestWaitForThreadGCMetadata_RecognizesProjectedSessionEnv(t *testing.T) {
	server := newT3BridgeTestServer(t, map[string]interface{}{
		"threads": []interface{}{
			map[string]interface{}{
				"id":        "thread-1",
				"projectId": "project-1",
				"customMetadata": map[string]interface{}{
					"gc.agent":       "t3code/crew",
					"gc.sessionName": "t3code--crew",
					"gc.rig":         "t3code",
					"gc.sessionEnv":  `{"GC_SESSION_NAME":"t3code--crew","GC_AGENT":"t3code/crew","GC_ALIAS":"t3code/crew","GC_CITY":"gc","GC_CITY_PATH":"/data/projects/gc","GC_TEMPLATE":"t3code/crew","GC_RIG":"t3code","GC_RIG_ROOT":"/data/projects/t3code"}`,
				},
			},
		},
	})
	defer server.Close()
	t.Setenv("T3_WS_URL", server.wsURL())

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}

	if err := p.waitForThreadGCMetadata("thread-1", 500*time.Millisecond); err != nil {
		t.Fatalf("waitForThreadGCMetadata: %v", err)
	}
}

func TestResolveBindingProviderModel_DefaultsCodexToGPT54WhenModelMissing(t *testing.T) {
	provider, model := resolveBindingProviderModel(threadBinding{
		Provider: "codex",
	}, nil)

	if provider != "codex" {
		t.Fatalf("provider = %q, want codex", provider)
	}
	if model != defaultCodexModel {
		t.Fatalf("model = %q, want %s", model, defaultCodexModel)
	}
}

type t3BridgeTestServer struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	commands []string
	snapshot map[string]interface{}
}

func newT3BridgeTestServer(t *testing.T, snapshot map[string]interface{}) *t3BridgeTestServer {
	t.Helper()
	ts := &t3BridgeTestServer{t: t, snapshot: snapshot}
	upgrader := websocket.Upgrader{}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()

		var req struct {
			ID      string          `json:"id"`
			Tag     string          `json:"tag"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := conn.ReadJSON(&req); err != nil {
			t.Errorf("read websocket request: %v", err)
			return
		}

		value := map[string]interface{}{}
		switch req.Tag {
		case "orchestration.getSnapshot":
			value = ts.snapshot
		case "orchestration.dispatchCommand":
			var payload map[string]interface{}
			if err := json.Unmarshal(req.Payload, &payload); err != nil {
				t.Errorf("decode dispatch payload: %v", err)
				return
			}
			ts.recordCommand(commandType(payload))
		}

		resp := map[string]interface{}{
			"_tag":      "Exit",
			"requestId": req.ID,
			"exit": map[string]interface{}{
				"_tag":  "Success",
				"value": value,
			},
		}
		if err := conn.WriteJSON(resp); err != nil {
			t.Errorf("write websocket response: %v", err)
		}
	}))
	return ts
}

func (ts *t3BridgeTestServer) Close() {
	ts.server.Close()
}

func (ts *t3BridgeTestServer) wsURL() string {
	return "ws" + strings.TrimPrefix(ts.server.URL, "http")
}

func (ts *t3BridgeTestServer) recordCommand(typ string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.commands = append(ts.commands, typ)
}

func (ts *t3BridgeTestServer) commandTypes() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.commands...)
}

func commandType(payload map[string]interface{}) string {
	if typ, _ := payload["type"].(string); typ != "" {
		return typ
	}
	if nested, _ := payload["command"].(map[string]interface{}); nested != nil {
		typ, _ := nested["type"].(string)
		return typ
	}
	return ""
}

func TestResolveBindingProviderModel_FallsBackToThreadEnvModel(t *testing.T) {
	provider, model := resolveBindingProviderModel(threadBinding{
		Provider: "",
		Model:    "",
	}, map[string]string{
		"GC_PROVIDER": "codex",
		"GC_MODEL":    "gpt-5.4-mini",
	})
	if provider != "codex" {
		t.Fatalf("provider = %q, want codex", provider)
	}
	if model != "gpt-5.4-mini" {
		t.Fatalf("model = %q, want gpt-5.4-mini", model)
	}
}

func TestResolveBindingProviderModel_InfersCodexFromStoredGptModel(t *testing.T) {
	provider, model := resolveBindingProviderModel(threadBinding{
		Provider: "",
		Model:    "gpt-5.4-mini",
	}, nil)
	if provider != "codex" {
		t.Fatalf("provider = %q, want codex", provider)
	}
	if model != "gpt-5.4-mini" {
		t.Fatalf("model = %q, want gpt-5.4-mini", model)
	}
}

func TestResolveConfigProviderModel_PrefersStoredEnvelopeIntent(t *testing.T) {
	rawEnvelope, err := json.Marshal(StartupEnvelope{
		Runtime: RuntimeSection{
			Provider: "codex",
			Model:    "gpt-5.4-mini",
			WorkDir:  "/data/projects/gc/.gc/worktrees/t3code/refinery",
		},
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	provider, model, ok := resolveConfigProviderModel(&execStartConfig{
		Command:         "claude --print",
		Env:             map[string]string{},
		StartupEnvelope: rawEnvelope,
	})
	if !ok {
		t.Fatal("expected config provider/model to resolve")
	}
	if provider != "codex" {
		t.Fatalf("provider = %q, want codex", provider)
	}
	if model != "gpt-5.4-mini" {
		t.Fatalf("model = %q, want gpt-5.4-mini", model)
	}
}
