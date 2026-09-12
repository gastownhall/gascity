package t3bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
	"github.com/gorilla/websocket"
)

// statefulT3BridgeServer is a hermetic, in-memory T3 WebSocket bridge that
// tracks project/thread state across requests, so the shared
// runtimetest.RunProviderTestsWithOptions conformance suite can exercise the
// full create/reuse/observe/stop lifecycle against a real
// t3bridge.NewSeamBacked provider without a live T3 Code process.
//
// It mirrors t3BridgeTestServer's per-connection request/response handling
// (one WebSocket upgrade per RPC call, matching the production client's
// rpcCallOnce dial-per-request behavior) but replaces the static snapshot
// fixture with mutable state that dispatchCommand calls actually update.
type statefulT3BridgeServer struct {
	server *httptest.Server

	mu       sync.Mutex
	projects map[string]map[string]interface{}
	threads  map[string]map[string]interface{}
}

func newStatefulT3BridgeServer(t *testing.T) *statefulT3BridgeServer {
	t.Helper()
	ts := &statefulT3BridgeServer{
		projects: make(map[string]map[string]interface{}),
		threads:  make(map[string]map[string]interface{}),
	}
	upgrader := websocket.Upgrader{}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		value := ts.handle(t, req.Tag, req.Payload)

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

func (ts *statefulT3BridgeServer) Close() {
	ts.server.Close()
}

func (ts *statefulT3BridgeServer) wsURL() string {
	return "ws" + strings.TrimPrefix(ts.server.URL, "http")
}

func (ts *statefulT3BridgeServer) handle(t *testing.T, tag string, rawPayload json.RawMessage) map[string]interface{} {
	t.Helper()
	ts.mu.Lock()
	defer ts.mu.Unlock()

	switch tag {
	case "orchestration.getSnapshot":
		return ts.snapshotLocked()
	case "orchestration.dispatchCommand":
		var payload map[string]interface{}
		if err := json.Unmarshal(rawPayload, &payload); err != nil {
			t.Errorf("decode dispatch payload: %v", err)
			return map[string]interface{}{}
		}
		ts.dispatchLocked(commandType(payload), payload)
		return map[string]interface{}{}
	default:
		return map[string]interface{}{}
	}
}

func (ts *statefulT3BridgeServer) snapshotLocked() map[string]interface{} {
	threads := make([]interface{}, 0, len(ts.threads))
	for _, thread := range ts.threads {
		threads = append(threads, thread)
	}
	projects := make([]interface{}, 0, len(ts.projects))
	for _, project := range ts.projects {
		projects = append(projects, project)
	}
	return map[string]interface{}{"threads": threads, "projects": projects}
}

func (ts *statefulT3BridgeServer) dispatchLocked(typ string, payload map[string]interface{}) {
	switch typ {
	case "project.create":
		ts.handleProjectCreate(payload)
	case "thread.create":
		ts.handleThreadCreate(payload)
	case "thread.session.stop":
		ts.setThreadSessionStatus(payload, "stopped")
	case "thread.meta.update":
		ts.handleThreadMetaUpdate(payload)
	case "thread.archive":
		ts.handleThreadArchive(payload)
	case "thread.activity.append", "thread.turn.start", "thread.turn.interrupt":
		// No state mutation needed: the conformance suite only asserts on
		// project/thread/session/metadata shape, not on activity/turn history.
	}
}

func (ts *statefulT3BridgeServer) handleProjectCreate(payload map[string]interface{}) {
	projectID, _ := payload["projectId"].(string)
	if projectID == "" {
		return
	}
	ts.projects[projectID] = map[string]interface{}{
		"id":            projectID,
		"workspaceRoot": payload["workspaceRoot"],
		"title":         payload["title"],
		"deletedAt":     nil,
	}
}

func (ts *statefulT3BridgeServer) handleThreadCreate(payload map[string]interface{}) {
	threadID, _ := payload["threadId"].(string)
	if threadID == "" {
		return
	}
	provider, model := "", ""
	if modelSelection, ok := payload["modelSelection"].(map[string]interface{}); ok {
		provider, _ = modelSelection["provider"].(string)
		model, _ = modelSelection["model"].(string)
	}
	customMetadata, ok := payload["customMetadata"].(map[string]interface{})
	if !ok || customMetadata == nil {
		customMetadata = map[string]interface{}{}
	} else {
		customMetadata = cloneStringInterfaceMap(customMetadata)
	}
	ts.threads[threadID] = map[string]interface{}{
		"id":             threadID,
		"projectId":      payload["projectId"],
		"title":          payload["title"],
		"provider":       provider,
		"model":          model,
		"customMetadata": customMetadata,
		"session":        map[string]interface{}{"status": "running"},
		"deletedAt":      nil,
	}
}

func (ts *statefulT3BridgeServer) setThreadSessionStatus(payload map[string]interface{}, status string) {
	threadID, _ := payload["threadId"].(string)
	thread := ts.threads[threadID]
	if thread == nil {
		return
	}
	thread["session"] = map[string]interface{}{"status": status}
}

func (ts *statefulT3BridgeServer) handleThreadMetaUpdate(payload map[string]interface{}) {
	threadID, _ := payload["threadId"].(string)
	thread := ts.threads[threadID]
	if thread == nil {
		return
	}
	if customMetadata, ok := payload["customMetadata"].(map[string]interface{}); ok {
		existing, _ := thread["customMetadata"].(map[string]interface{})
		if existing == nil {
			existing = map[string]interface{}{}
		}
		for k, v := range customMetadata {
			existing[k] = v
		}
		thread["customMetadata"] = existing
	}
	if modelSelection, ok := payload["modelSelection"].(map[string]interface{}); ok {
		if provider, ok := modelSelection["provider"].(string); ok && provider != "" {
			thread["provider"] = provider
		}
		if model, ok := modelSelection["model"].(string); ok && model != "" {
			thread["model"] = model
		}
	}
}

func (ts *statefulT3BridgeServer) handleThreadArchive(payload map[string]interface{}) {
	threadID, _ := payload["threadId"].(string)
	thread := ts.threads[threadID]
	if thread == nil {
		return
	}
	thread["deletedAt"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func cloneStringInterfaceMap(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

var t3BridgeConformanceSessionCounter int64

// newT3BridgeConformanceSession is a runtimetest.Factory that starts a fresh
// statefulT3BridgeServer per session and points t3bridge.NewSeamBacked at it
// via a hand-built StartupEnvelope supplied through GC_STARTUP_ENVELOPE.
//
// Assignment.BeadID is populated (so gc.bead metadata is non-empty and
// Stop's isPersistentAgent check takes the normal, non-persistent-agent
// branch) while Resume is left zero-valued and GC_BEAD is left unset (so
// Start's legacyNamed auto-defaulting sets Resume.AllowThreadReuse=true).
// Together these let a second Start for the same session name hit
// DecideThreadReuse's Reuse branch instead of erroring, matching t3bridge's
// intentional durable-reconnect design.
func newT3BridgeConformanceSession(t *testing.T) (runtime.Provider, runtime.Config, string) {
	t.Helper()
	resetBridgeAuthCacheForTest(t)
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = nil
	t.Cleanup(func() { defaultWSURLCandidates = oldDefaults })

	server := newStatefulT3BridgeServer(t)
	t.Cleanup(server.Close)

	t.Setenv("GC_EXEC_STATE_DIR", t.TempDir())
	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
	t.Setenv("T3_HOME", t.TempDir())
	t.Setenv("T3_WS_URL", server.wsURL())
	t.Setenv("GC_T3BRIDGE_STATE_DIR", t.TempDir())

	name := fmt.Sprintf("gc-t3bridge-conformance-%d", atomic.AddInt64(&t3BridgeConformanceSessionCounter, 1))
	workDir := t.TempDir()

	envelope := StartupEnvelope{
		GC: GCSection{
			Agent:       "conformance-agent",
			Template:    "conformance-template",
			SessionName: name,
		},
		Runtime: RuntimeSection{
			WorkDir: workDir,
		},
		Assignment: AssignmentSection{
			BeadID: "conformance-bead-" + name,
		},
	}
	envJSON, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal startup envelope: %v", err)
	}

	cfg := runtime.Config{
		WorkDir: workDir,
		Env: map[string]string{
			"GC_STARTUP_ENVELOPE": string(envJSON),
		},
	}
	return NewSeamBacked(), cfg, name
}

func TestT3Bridge_RunProviderConformance(t *testing.T) {
	runtimetest.RunProviderTestsWithOptions(t, newT3BridgeConformanceSession, runtimetest.Options{
		DuplicateStartReconnects: true,
	})
}
