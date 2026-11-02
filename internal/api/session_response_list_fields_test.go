package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// sessionResponseJSONMap marshals a sessionResponse and decodes it back into a
// generic map so a test can assert key presence and omission on the wire.
func sessionResponseJSONMap(t *testing.T, r sessionResponse) map[string]any {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal sessionResponse: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal sessionResponse: %v", err)
	}
	return out
}

// TestSessionToResponseCarriesListIdentityFields pins the optional identity
// fields `gc session list` needs from the API so its API-routed rows match the
// local rendering: agent_name, transport and worker_dir (trimmed). agent_name
// is always sent ("" when the session records none) so a client can tell a
// current server from one that predates it; transport and worker_dir are
// omitted when empty. A closed session keeps them.
func TestSessionToResponseCarriesListIdentityFields(t *testing.T) {
	created := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	full := session.Info{
		ID:          "gc-1",
		Template:    "rig/worker",
		State:       session.StateActive,
		SessionName: "rig--worker-1",
		AgentName:   "rig/worker-1",
		Transport:   "tmux",
		WorkerDir:   " /w/x ",
		CreatedAt:   created,
	}

	got := sessionResponseJSONMap(t, sessionToResponse(full, nil))
	for key, want := range map[string]string{"agent_name": "rig/worker-1", "transport": "tmux", "worker_dir": "/w/x"} {
		if got[key] != want {
			t.Errorf("%s = %#v, want %q", key, got[key], want)
		}
	}

	empty := sessionResponseJSONMap(t, sessionToResponse(session.Info{ID: "gc-2", Template: "worker", CreatedAt: created, WorkerDir: "  "}, nil))
	if got, ok := empty["agent_name"]; !ok || got != "" {
		t.Errorf("agent_name on a session that records none = %#v (present %v), want \"\": a current server always sends it", got, ok)
	}
	for _, key := range []string{"transport", "worker_dir"} {
		if _, ok := empty[key]; ok {
			t.Errorf("%s present on a session that records none: %#v", key, empty[key])
		}
	}

	closed := full
	closed.State = ""
	closed.Closed = true
	gotClosed := sessionResponseJSONMap(t, sessionToResponse(closed, nil))
	if gotClosed["state"] != "closed" {
		t.Errorf("closed state = %#v, want \"closed\"", gotClosed["state"])
	}
	if gotClosed["agent_name"] != "rig/worker-1" {
		t.Errorf("closed agent_name = %#v, want rig/worker-1", gotClosed["agent_name"])
	}
}

// TestHandleSessionListSendsAgentNameOnEveryRow pins the capability signal
// the CLI reads to tell a current server from an older one: every GET
// /sessions row carries the agent_name key, including a session that records
// no agent and a closed one.
func TestHandleSessionListSendsAgentNameOnEveryRow(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandlerWith(t, fs, New(fs))
	open := createTestSession(t, fs.cityBeadStore, fs.sp, "no agent")
	closed := createTestSession(t, fs.cityBeadStore, fs.sp, "closed")
	if err := fs.cityBeadStore.Close(closed.ID); err != nil {
		t.Fatalf("Close(%s): %v", closed.ID, err)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", cityURL(fs, "/sessions?state=all"), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	seen := map[string]bool{}
	for _, item := range body.Items {
		id, _ := item["id"].(string)
		seen[id] = true
		if _, ok := item["agent_name"]; !ok {
			t.Errorf("row %s (state %v) has no agent_name key: %v", id, item["state"], item)
		}
	}
	for _, id := range []string{open.ID, closed.ID} {
		if !seen[id] {
			t.Errorf("row %s missing from GET /sessions?state=all: %v", id, body.Items)
		}
	}
}

// sessionListWakeReasonerState is a fakeState that reports canned wake
// reasons and records what the list handler asked about.
type sessionListWakeReasonerState struct {
	*fakeState
	reasons   map[string][]string
	calls     int
	listed    []string
	persisted []string
}

var _ SessionListWakeReasoner = (*sessionListWakeReasonerState)(nil)

func (s *sessionListWakeReasonerState) SessionListWakeReasons(listed []session.Info, persisted map[string]session.Info) map[string][]string {
	s.calls++
	s.listed, s.persisted = nil, nil
	for _, info := range listed {
		s.listed = append(s.listed, info.ID)
		if p, ok := persisted[info.ID]; ok && p.ID == info.ID {
			s.persisted = append(s.persisted, info.ID)
		}
	}
	return s.reasons
}

// TestHandleSessionListReportsWakeReasonsOnRequest pins GET
// /sessions?wake_reasons=true: the handler asks the State's wake reasoner
// once for the page, passing the page rows and their persisted Infos, and
// sets wake_reasons on the rows it names. Without the parameter it never
// asks, and no row carries the key.
func TestHandleSessionListReportsWakeReasonsOnRequest(t *testing.T) {
	fs := newSessionFakeState(t)
	first := createTestSession(t, fs.cityBeadStore, fs.sp, "first")
	second := createTestSession(t, fs.cityBeadStore, fs.sp, "second")
	state := &sessionListWakeReasonerState{fakeState: fs, reasons: map[string][]string{first.ID: {"session", "config"}}}
	h := newTestCityHandlerWith(t, state, New(state))

	list := func(query string) map[string]map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", cityURL(state, "/sessions"+query), nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET /sessions%s status = %d; body=%s", query, w.Code, w.Body.String())
		}
		var body struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		rows := map[string]map[string]any{}
		for _, item := range body.Items {
			id, _ := item["id"].(string)
			rows[id] = item
		}
		return rows
	}

	rows := list("")
	if state.calls != 0 {
		t.Errorf("wake reasoner called %d times without wake_reasons=true", state.calls)
	}
	for id, row := range rows {
		if _, ok := row["wake_reasons"]; ok {
			t.Errorf("row %s carries wake_reasons without the parameter: %v", id, row["wake_reasons"])
		}
	}

	rows = list("?wake_reasons=true")
	if state.calls != 1 {
		t.Fatalf("wake reasoner called %d times, want once per page", state.calls)
	}
	got, _ := json.Marshal(rows[first.ID]["wake_reasons"])
	if string(got) != `["session","config"]` {
		t.Errorf("row %s wake_reasons = %s, want [\"session\",\"config\"]", first.ID, got)
	}
	if _, ok := rows[second.ID]["wake_reasons"]; ok {
		t.Errorf("row %s carries wake_reasons the reasoner did not name: %v", second.ID, rows[second.ID]["wake_reasons"])
	}
	for _, want := range []string{first.ID, second.ID} {
		if !slices.Contains(state.listed, want) || !slices.Contains(state.persisted, want) {
			t.Errorf("reasoner got listed %v persisted %v, want both to include %s", state.listed, state.persisted, want)
		}
	}
}
