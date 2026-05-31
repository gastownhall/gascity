package t3bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestDecodeIssuedBearerSessionToken(t *testing.T) {
	token, err := decodeIssuedBearerSessionToken([]byte(`{"sessionId":"session-1","token":"test-bearer","role":"owner"}`))
	if err != nil {
		t.Fatalf("decodeIssuedBearerSessionToken: %v", err)
	}
	if token != "test-bearer" {
		t.Fatalf("token = %q, want test-bearer", token)
	}
}

func TestDecodeIssuedBearerSessionToken_EmptyToken(t *testing.T) {
	_, err := decodeIssuedBearerSessionToken([]byte(`{"sessionId":"session-1","token":"","role":"owner"}`))
	if err == nil || !strings.Contains(err.Error(), "empty token") {
		t.Fatalf("err = %v, want empty token", err)
	}
}

func TestResolveWsURLCandidates_PrefersRuntimeStateOverStaleWSURL(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("T3_HOME", filepath.Join(tempHome, ".t3"))
	t.Setenv("T3_WS_URL", "")
	if err := os.MkdirAll(filepath.Join(tempHome, ".t3", "dev"), 0o755); err != nil {
		t.Fatalf("mkdir dev dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempHome, ".t3", "ws-url"), []byte("ws://127.0.0.1:3773/ws"), 0o644); err != nil {
		t.Fatalf("write ws-url: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(tempHome, ".t3", "dev", "server-runtime.json"),
		[]byte(`{"origin":"http://127.0.0.1:3774"}`),
		0o644,
	); err != nil {
		t.Fatalf("write server-runtime.json: %v", err)
	}

	candidates := resolveWsURLCandidates()
	if len(candidates) == 0 {
		t.Fatal("resolveWsURLCandidates returned no candidates")
	}
	if candidates[0] != "ws://127.0.0.1:3774/ws" {
		t.Fatalf("first candidate = %q, want ws://127.0.0.1:3774/ws", candidates[0])
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
	t.Setenv("T3_BEARER_TOKEN", "")
	t.Setenv("T3_WS_URL", server.wsURL())
	t.Setenv("GC_T3BRIDGE_STATE_DIR", t.TempDir())

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}

	if !p.ProcessAlive("mayor", nil) {
		t.Fatal("ProcessAlive(ready) = false, want true")
	}
}

func TestProcessAlive_ReadyCountsAsAlive_WithResultWrappedSnapshot(t *testing.T) {
	server := newT3BridgeTestServer(t, map[string]interface{}{
		"result": map[string]interface{}{
			"threads": []interface{}{
				map[string]interface{}{
					"id":        "thread-1",
					"projectId": "project-1",
					"customMetadata": map[string]interface{}{
						"gc.agent":       "gascity/gastown.polecat",
						"gc.sessionName": "gastown__polecat-gc-qghp",
					},
					"session": map[string]interface{}{
						"status": "ready",
					},
				},
			},
		},
	})
	defer server.Close()
	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
	t.Setenv("T3_WS_URL", server.wsURL())

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}

	if !p.ProcessAlive("gastown__polecat-gc-qghp", nil) {
		t.Fatal("ProcessAlive(result-wrapped ready) = false, want true")
	}
}

func TestIsRunning_UsesCachedSnapshotWithinTTL(t *testing.T) {
	resetBridgeAuthCacheForTest(t)
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = nil
	t.Cleanup(func() {
		defaultWSURLCandidates = oldDefaults
	})

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
	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
	t.Setenv("T3_WS_URL", server.wsURL())

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}

	if !p.IsRunning("mayor") {
		t.Fatal("IsRunning(first) = false, want true")
	}
	if !p.IsRunning("mayor") {
		t.Fatal("IsRunning(second) = false, want true")
	}
	if calls := server.wsCalls(); calls != 1 {
		t.Fatalf("ws calls = %d, want 1", calls)
	}
}

func TestAuthenticatedWsURL_UsesBearerTokenForNonLoopback(t *testing.T) {
	resetBridgeAuthCacheForTest(t)
	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
	t.Setenv("GC_T3BRIDGE_STATE_DIR", t.TempDir())
	wsURL, headers, err := authenticatedWsURL("wss://remote.example/ws")
	if err != nil {
		t.Fatalf("authenticatedWsURL: %v", err)
	}
	if wsURL != "wss://remote.example/ws" {
		t.Fatalf("wsURL = %q, want wss://remote.example/ws", wsURL)
	}
	if got := headers.Get("Authorization"); got != "Bearer test-bearer" {
		t.Fatalf("authorization = %q, want Bearer test-bearer", got)
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

	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
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
	meta := buildGCMetadata(StartupEnvelope{}, "codex", nil)
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

func TestResolveActiveProjectID_UsesResultWrappedSnapshot(t *testing.T) {
	snapshot := map[string]interface{}{
		"result": map[string]interface{}{
			"projects": []interface{}{
				map[string]interface{}{
					"id":            "project-1",
					"workspaceRoot": "/data/projects/gascity",
				},
			},
		},
	}

	if got := resolveActiveProjectID(snapshot, "/data/projects/gascity"); got != "project-1" {
		t.Fatalf("resolveActiveProjectID(result-wrapped) = %q, want project-1", got)
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
	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
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

func TestSetMetaGetMetaRemoveMeta_UsesNativeStateStore(t *testing.T) {
	server := newT3BridgeTestServer(t, map[string]interface{}{
		"threads": []interface{}{
			map[string]interface{}{
				"id":        "thread-1",
				"projectId": "project-1",
				"customMetadata": map[string]interface{}{
					"gc.agent":       "t3code/crew",
					"gc.sessionName": "t3code--crew",
				},
			},
		},
	})
	defer server.Close()
	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
	t.Setenv("T3_WS_URL", server.wsURL())
	t.Setenv("GC_T3BRIDGE_STATE_DIR", t.TempDir())

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}

	if err := p.SetMeta("t3code--crew", "GC_DRAIN", "123"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	got, err := p.GetMeta("t3code--crew", "GC_DRAIN")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if got != "123" {
		t.Fatalf("GC_DRAIN = %q, want 123", got)
	}
	if err := p.RemoveMeta("t3code--crew", "GC_DRAIN"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
	got, err = p.GetMeta("t3code--crew", "GC_DRAIN")
	if err != nil {
		t.Fatalf("GetMeta(after remove): %v", err)
	}
	if got != "" {
		t.Fatalf("GC_DRAIN after remove = %q, want empty", got)
	}
}

func TestMetaFilePath_SanitizesNameAndKey(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("GC_T3BRIDGE_STATE_DIR", stateDir)

	path := metaFilePath("../crew/name", "../../GC/DRAIN")
	rel, err := filepath.Rel(stateDir, path)
	if err != nil {
		t.Fatalf("rel meta path: %v", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatalf("meta path escaped state dir: %q", path)
	}
	if got, want := filepath.Base(path), ".._crew_name.meta..._.._GC_DRAIN"; got != want {
		t.Fatalf("meta filename = %q, want %q", got, want)
	}
}

func TestCopyTo_UsesThreadWorkDir(t *testing.T) {
	workDir := t.TempDir()
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "note.txt")
	if err := os.WriteFile(srcFile, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write src file: %v", err)
	}

	server := newT3BridgeTestServer(t, map[string]interface{}{
		"threads": []interface{}{
			map[string]interface{}{
				"id":        "thread-1",
				"projectId": "project-1",
				"customMetadata": map[string]interface{}{
					"gc.agent":          "t3code/crew",
					"gc.sessionName":    "t3code--crew",
					"gc.startupWorkDir": workDir,
				},
			},
		},
	})
	defer server.Close()
	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
	t.Setenv("T3_WS_URL", server.wsURL())

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}

	if err := p.CopyTo("t3code--crew", srcFile, "nested/copied.txt"); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "nested", "copied.txt"))
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("copied file = %q, want hello", string(data))
	}
}

func TestCopyTo_RejectsRelDstEscapingWorkDir(t *testing.T) {
	parent := t.TempDir()
	workDir := filepath.Join(parent, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}
	srcFile := filepath.Join(parent, "note.txt")
	if err := os.WriteFile(srcFile, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write src file: %v", err)
	}

	server := newT3BridgeTestServer(t, map[string]interface{}{
		"threads": []interface{}{
			map[string]interface{}{
				"id":        "thread-1",
				"projectId": "project-1",
				"customMetadata": map[string]interface{}{
					"gc.agent":          "t3code/crew",
					"gc.sessionName":    "t3code--crew",
					"gc.startupWorkDir": workDir,
				},
			},
		},
	})
	defer server.Close()
	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
	t.Setenv("T3_WS_URL", server.wsURL())

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}

	if err := p.CopyTo("t3code--crew", srcFile, "../outside.txt"); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "outside.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file stat err = %v, want not exist", err)
	}
}

type t3BridgeTestServer struct {
	t                 *testing.T
	server            *httptest.Server
	mu                sync.Mutex
	commands          []string
	snapshot          map[string]interface{}
	authFailures      int
	authFailureStatus int
	authFailureBody   string
	authRequestCount  int
	wsAuthorization   []string
	wsRequestCount    int
}

func newT3BridgeTestServer(t *testing.T, snapshot map[string]interface{}) *t3BridgeTestServer {
	t.Helper()
	ts := &t3BridgeTestServer{t: t, snapshot: snapshot}
	upgrader := websocket.Upgrader{}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/ws-token" || r.URL.Path == "/api/auth/bridge-ws-token" {
			ts.mu.Lock()
			ts.authRequestCount++
			failuresRemaining := ts.authFailures
			if ts.authFailures > 0 {
				ts.authFailures--
			}
			failureStatus := ts.authFailureStatus
			failureBody := ts.authFailureBody
			ts.mu.Unlock()
			if failuresRemaining > 0 {
				if failureStatus == 0 {
					failureStatus = http.StatusServiceUnavailable
				}
				if failureBody == "" {
					failureBody = "bridge warming up"
				}
				http.Error(w, failureBody, failureStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-ws-token"})
			return
		}
		ts.mu.Lock()
		ts.wsRequestCount++
		ts.wsAuthorization = append(ts.wsAuthorization, r.Header.Get("Authorization"))
		ts.mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

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

func (ts *t3BridgeTestServer) setAuthFailures(count, status int, body string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.authFailures = count
	ts.authFailureStatus = status
	ts.authFailureBody = body
}

func (ts *t3BridgeTestServer) authCalls() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.authRequestCount
}

func (ts *t3BridgeTestServer) wsCalls() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.wsRequestCount
}

func resetBridgeAuthCacheForTest(t *testing.T) {
	t.Helper()
	authMu.Lock()
	cachedBridgeWSToken = ""
	cachedBridgeWSTokenBaseURL = ""
	cachedBridgeWSTokenExpiresAt = time.Time{}
	authMu.Unlock()
	t.Cleanup(func() {
		authMu.Lock()
		cachedBridgeWSToken = ""
		cachedBridgeWSTokenBaseURL = ""
		cachedBridgeWSTokenExpiresAt = time.Time{}
		authMu.Unlock()
	})
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

func TestRPCSnapshot_UsesUnauthenticatedLoopbackWebSocketUpgrade(t *testing.T) {
	resetBridgeAuthCacheForTest(t)
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = nil
	t.Cleanup(func() {
		defaultWSURLCandidates = oldDefaults
	})

	server := newT3BridgeTestServer(t, map[string]interface{}{
		"threads": []interface{}{},
	})
	server.setAuthFailures(2, http.StatusServiceUnavailable, "warming")
	defer server.Close()

	t.Setenv("T3_BEARER_TOKEN", "")
	t.Setenv("T3_HOME", t.TempDir())
	t.Setenv("T3_WS_URL", server.wsURL())
	t.Setenv("GC_T3BRIDGE_STATE_DIR", t.TempDir())

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}

	if _, err := p.rpcSnapshot(); err != nil {
		t.Fatalf("rpcSnapshot unauth loopback: %v", err)
	}
	if calls := server.authCalls(); calls != 0 {
		t.Fatalf("auth calls = %d, want 0", calls)
	}
}

func TestRPCSnapshot_FallsBackToWSURLFileWhenEnvStale(t *testing.T) {
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = nil
	t.Cleanup(func() {
		defaultWSURLCandidates = oldDefaults
	})

	server := newT3BridgeTestServer(t, map[string]interface{}{
		"threads": []interface{}{},
	})
	defer server.Close()

	t3Home := t.TempDir()
	if err := os.WriteFile(filepath.Join(t3Home, "ws-url"), []byte(server.wsURL()), 0o644); err != nil {
		t.Fatalf("write ws-url: %v", err)
	}
	t.Setenv("T3_HOME", t3Home)
	t.Setenv("T3_WS_URL", "ws://127.0.0.1:1/ws")

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}

	if _, err := p.rpcSnapshot(); err != nil {
		t.Fatalf("rpcSnapshot fallback: %v", err)
	}
}

func TestStart_TransientBridgeFailureReturnsInitializing(t *testing.T) {
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = nil
	t.Cleanup(func() {
		defaultWSURLCandidates = oldDefaults
	})

	t.Setenv("T3_HOME", t.TempDir())
	t.Setenv("T3_WS_URL", "ws://127.0.0.1:1/ws")

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}

	err := p.Start(context.Background(), "deacon", runtime.Config{
		WorkDir: "/tmp/deacon",
		Command: "codex",
		Env: map[string]string{
			"GC_CITY_PATH": "/tmp/gc",
			"GC_TEMPLATE":  "deacon",
			"GC_PROVIDER":  "codex",
			"GC_MODEL":     "gpt-5.4",
		},
	})
	if !errors.Is(err, runtime.ErrSessionInitializing) {
		t.Fatalf("Start error = %v, want ErrSessionInitializing", err)
	}
}

func TestPeek_TransientBridgeFailureSoftDegrades(t *testing.T) {
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = nil
	t.Cleanup(func() {
		defaultWSURLCandidates = oldDefaults
	})

	t.Setenv("T3_HOME", t.TempDir())
	t.Setenv("T3_WS_URL", "ws://127.0.0.1:1/ws")

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}

	out, err := p.Peek("deacon", 10)
	if err != nil {
		t.Fatalf("Peek error = %v, want nil", err)
	}
	if !strings.Contains(out, "temporarily unavailable") {
		t.Fatalf("Peek output = %q, want temporary-unavailable message", out)
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
