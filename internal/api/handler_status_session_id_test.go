package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// statusRowJSON marshals a status row into a generic object so the test can
// assert presence or absence of the session_id key on the wire.
func statusRowJSON(t *testing.T, row StatusAgentDetail) map[string]any {
	t.Helper()
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal row: %v", err)
	}
	return out
}

func sessionIDBead(agentName, sessionName, template string) beads.Bead {
	return beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        string(session.StateActive),
			"template":     template,
			"agent_name":   agentName,
			"session_name": sessionName,
		},
	}
}

// TestStatusAgentDetailsCarrySessionID pins that /status rows carry the bead
// ID of the session their runtime session resolved to, so `gc rig status
// --json` can print the same session_id on the API route as locally (#8453).
func TestStatusAgentDetailsCarrySessionID(t *testing.T) {
	t.Run("singleton", func(t *testing.T) {
		st, sessions := poolIdentityState(t, config.Agent{
			Name: "worker", Dir: "frontend", Provider: "test-agent", MaxActiveSessions: intPtr(1),
		})
		st.cfg.Agents = append(st.cfg.Agents, config.Agent{
			Name: "helper", Dir: "frontend", Provider: "test-agent", MaxActiveSessions: intPtr(1),
		})
		created, err := sessions.Create(sessionIDBead("frontend/worker", "frontend--worker", "frontend/worker"))
		if err != nil {
			t.Fatalf("create session bead: %v", err)
		}

		body := New(st).buildStatusBody(context.Background(), false)

		worker := agentRow(t, body, "frontend/worker")
		if got := statusRowJSON(t, worker)["session_id"]; got != created.ID {
			t.Fatalf("frontend/worker session_id = %v, want %q (row=%+v)", got, created.ID, worker)
		}
		if worker.SessionID != created.ID {
			t.Fatalf("frontend/worker SessionID = %q, want %q", worker.SessionID, created.ID)
		}
		helper := agentRow(t, body, "frontend/helper")
		if got, ok := statusRowJSON(t, helper)["session_id"]; ok {
			t.Fatalf("frontend/helper session_id = %v, want key absent (row=%+v)", got, helper)
		}
	})

	t.Run("bounded pool instance", func(t *testing.T) {
		st, sessions := poolIdentityState(t, config.Agent{
			Name: "builder", Dir: "frontend", Provider: "test-agent", MaxActiveSessions: intPtr(2),
		})
		created, err := sessions.Create(sessionIDBead("frontend/builder-1", "frontend--builder-1", "frontend/builder"))
		if err != nil {
			t.Fatalf("create session bead: %v", err)
		}

		body := New(st).buildStatusBody(context.Background(), false)

		one := agentRow(t, body, "frontend/builder-1")
		if got := statusRowJSON(t, one)["session_id"]; got != created.ID {
			t.Fatalf("frontend/builder-1 session_id = %v, want %q (row=%+v)", got, created.ID, one)
		}
		two := agentRow(t, body, "frontend/builder-2")
		if got, ok := statusRowJSON(t, two)["session_id"]; ok {
			t.Fatalf("frontend/builder-2 session_id = %v, want key absent (row=%+v)", got, two)
		}
	})
}
