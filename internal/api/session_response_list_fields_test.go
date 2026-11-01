package api

import (
	"encoding/json"
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
// local rendering: agent_name, transport and worker_dir (trimmed). They are
// omitted when empty, and a closed session keeps them.
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
	for _, key := range []string{"agent_name", "transport", "worker_dir"} {
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
