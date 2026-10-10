package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// These tests pin the liveness contract of GET /v0/city/{city}/agents, the
// endpoint the dashboard's Agents page reads. They are the agent-list sibling
// of the /v0/status contract in handler_status_pool_identity_test.go.
//
// The two endpoints disagreed in production (srvcity, 2026-10-01): /status
// reported 13 of 27 agents running while /agents reported 2, because /agents
// derived the runtime session name from config alone. poolRuntimeSessionName
// (cmd/gc) steps a pool slot aside onto "<name>-pool" whenever the slot's agent
// is also a configured named session's template, so on a city where every pool
// agent holds a [[named_session]] reservation EVERY pool session runs under the
// suffixed name — and the canonical-name probe missed all of them. An operator
// saw a live 13-agent fleet rendered as two.

// TestAgentListReportsPoolSuffixedSessionAsRunning is the acceptance case: the
// session bead records the stepped-aside runtime name, the runtime is live
// under exactly that name, and nothing is listening on the canonical name.
func TestAgentListReportsPoolSuffixedSessionAsRunning(t *testing.T) {
	st, sessions := poolIdentityState(t, config.Agent{
		Name: "deployer", Dir: "gascity", Provider: "test-agent",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1),
	})
	if _, err := sessions.Create(poolSessionBeadWithAgentName("gascity/deployer", "gascity/deployer", "gascity--deployer-pool")); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := st.sp.Start(context.Background(), "gascity--deployer-pool", runtime.Config{}); err != nil {
		t.Fatalf("start runtime session: %v", err)
	}

	row := getAgentByName(t, st, "gascity/deployer")
	if !row.Running {
		t.Fatalf("gascity/deployer running = false, want true: its session %q is live and the session bead records that name (row=%+v)",
			"gascity--deployer-pool", row)
	}
	if row.Session == nil || row.Session.Name != "gascity--deployer-pool" {
		t.Fatalf("session block = %+v, want it to name the resolved runtime session %q — every per-session read (env, roster, peek, quarantine) keys off this name",
			row.Session, "gascity--deployer-pool")
	}
}

// TestAgentListReportsCanonicallyNamedSessionAsRunning is the positive control:
// the same agent shape whose runtime sits on the derived canonical name must
// still read running, or the test above could pass under a harness that reports
// everything running.
func TestAgentListReportsCanonicallyNamedSessionAsRunning(t *testing.T) {
	st, sessions := poolIdentityState(t, config.Agent{
		Name: "deployer", Dir: "beads", Provider: "test-agent",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1),
	})
	if _, err := sessions.Create(poolSessionBeadWithAgentName("beads/deployer", "beads/deployer", "beads--deployer")); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := st.sp.Start(context.Background(), "beads--deployer", runtime.Config{}); err != nil {
		t.Fatalf("start runtime session: %v", err)
	}

	row := getAgentByName(t, st, "beads/deployer")
	if !row.Running {
		t.Fatalf("beads/deployer running = false, want true — the canonical name is the one the agent list derives (row=%+v)", row)
	}
}

// TestAgentListFallsBackToCanonicalNameWithoutSessionBead pins the degraded
// path: with no session bead claiming the identity there is nothing to resolve
// from, so the canonical name must still be probed. This is the pre-fix
// behavior and it must survive, because an empty or unavailable session store
// has to leave the agent list working rather than blank it.
func TestAgentListFallsBackToCanonicalNameWithoutSessionBead(t *testing.T) {
	st, _ := poolIdentityState(t, config.Agent{
		Name: "deployer", Dir: "beads", Provider: "test-agent",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1),
	})
	if err := st.sp.Start(context.Background(), "beads--deployer", runtime.Config{}); err != nil {
		t.Fatalf("start runtime session: %v", err)
	}

	row := getAgentByName(t, st, "beads/deployer")
	if !row.Running {
		t.Fatalf("beads/deployer running = false, want true: with no session bead the canonical name %q is the only honest probe (row=%+v)",
			"beads--deployer", row)
	}
}

// TestAgentGetReportsPoolSuffixedSessionAsRunning covers the per-agent read
// GET /v0/city/{city}/agent/{dir}/{base}, which the dashboard's agent-detail
// view uses. It shares agentByName with the unqualified route and carried the
// identical canonical-name defect, so fixing only the list would have left a
// live agent reading "stopped" the moment anyone clicked into it.
func TestAgentGetReportsPoolSuffixedSessionAsRunning(t *testing.T) {
	st, sessions := poolIdentityState(t, config.Agent{
		Name: "deployer", Dir: "gascity", Provider: "test-agent",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1),
	})
	if _, err := sessions.Create(poolSessionBeadWithAgentName("gascity/deployer", "gascity/deployer", "gascity--deployer-pool")); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := st.sp.Start(context.Background(), "gascity--deployer-pool", runtime.Config{}); err != nil {
		t.Fatalf("start runtime session: %v", err)
	}

	srv := New(st)
	h := newTestCityHandlerWith(t, st, srv)
	req := httptest.NewRequest("GET", cityURL(st, "/agent/gascity/deployer"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agent/gascity/deployer = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Running bool   `json:"running"`
		State   string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if !body.Running {
		t.Fatalf("running = false, want true: session %q is live and the bead records that name (body=%s)",
			"gascity--deployer-pool", rec.Body.String())
	}
}

// TestAgentListReportsStoppedWhenNoRuntimeSessionExists guards the other
// direction: resolving through session beads must not manufacture liveness for
// an agent whose runtime is genuinely down. The bead names a runtime session
// that was never started.
func TestAgentListReportsStoppedWhenNoRuntimeSessionExists(t *testing.T) {
	st, sessions := poolIdentityState(t, config.Agent{
		Name: "deployer", Dir: "gascity", Provider: "test-agent",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1),
	})
	if _, err := sessions.Create(poolSessionBeadWithAgentName("gascity/deployer", "gascity/deployer", "gascity--deployer-pool")); err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	row := getAgentByName(t, st, "gascity/deployer")
	if row.Running {
		t.Fatalf("gascity/deployer running = true, want false: no runtime session was ever started (row=%+v)", row)
	}
}
