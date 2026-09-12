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

// t3BridgeConformanceSessionSeq disambiguates session names across separate
// newSession(t) calls that share the identical *testing.T — several
// runtimetest conformance cases (e.g. ListRunning_PrefixFiltering, the
// *_ConcurrentDistinctSessions family) call the factory 2-3 times from one
// subtest to obtain distinct sessions, so a name derived from t.Name() alone
// would collide. newSession is never invoked concurrently anywhere in the
// conformance suite (no t.Parallel(), no goroutines around factory calls), so
// a package-level counter needs no more than atomic increment/load to stay
// race-free across those sequential calls.
var t3BridgeConformanceSessionSeq int64

// t3BridgeConformanceNextSessionName advances the shared sequence and derives
// a new session name from it. Call exactly once per factory invocation, to
// build the startup envelope embedded in the returned runtime.Config.
func t3BridgeConformanceNextSessionName(t *testing.T) string {
	t.Helper()
	seq := atomic.AddInt64(&t3BridgeConformanceSessionSeq, 1)
	return t3BridgeConformanceSessionNameForSeq(t, seq)
}

// t3BridgeConformanceCurrentSessionName reads the sequence value without
// advancing it, reproducing the name t3BridgeConformanceNextSessionName most
// recently produced. providerledger's proof validator allows only one
// statement (a return) inside the inline conformance factory, so no
// intermediate variable can carry one generated name into both the startup
// envelope and the returned session name; Go evaluates a return statement's
// expressions left to right, so calling Next first (building the config) then
// Current (producing the returned name) yields the same string in both
// positions without one.
func t3BridgeConformanceCurrentSessionName(t *testing.T) string {
	t.Helper()
	seq := atomic.LoadInt64(&t3BridgeConformanceSessionSeq)
	return t3BridgeConformanceSessionNameForSeq(t, seq)
}

func t3BridgeConformanceSessionNameForSeq(t *testing.T, seq int64) string {
	t.Helper()
	replacer := strings.NewReplacer("/", "--", " ", "-")
	return fmt.Sprintf("gc-t3bridge-%s-%d", strings.ToLower(replacer.Replace(t.Name())), seq)
}

// t3BridgeConformanceConfig starts a fresh statefulT3BridgeServer and points
// t3bridge.NewSeamBacked at it via a hand-built StartupEnvelope supplied
// through GC_STARTUP_ENVELOPE.
//
// Assignment.BeadID is populated (so gc.bead metadata is non-empty and
// Stop's isPersistentAgent check takes the normal, non-persistent-agent
// branch) while Resume is left zero-valued and GC_BEAD is left unset (so
// Start's legacyNamed auto-defaulting sets Resume.AllowThreadReuse=true).
// Together these let a second Start for the same session name hit
// DecideThreadReuse's Reuse branch instead of erroring, matching t3bridge's
// intentional durable-reconnect design.
func t3BridgeConformanceConfig(t *testing.T, name string) runtime.Config {
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

	return runtime.Config{
		WorkDir: workDir,
		Env: map[string]string{
			"GC_STARTUP_ENVELOPE": string(envJSON),
		},
	}
}

func TestT3Bridge_RunProviderConformance(t *testing.T) {
	runtimetest.RunProviderTestsWithOptions(t, func(caseT *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBacked(),
			t3BridgeConformanceConfig(caseT, t3BridgeConformanceNextSessionName(caseT)),
			t3BridgeConformanceCurrentSessionName(caseT)
	}, runtimetest.Options{
		DuplicateStartReconnects: true,
	})
}
