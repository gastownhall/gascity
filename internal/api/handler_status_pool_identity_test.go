package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// These tests pin the liveness contract of GET /v0/status: an agent whose
// runtime session is alive must not be reported as not running, and must not
// disappear from the agent list entirely. `gc status` is the fleet's liveness
// probe, so a live seat read as dead escalates as an outage — on 2026-09-16 one
// reached the mayor as a P0 against a deployer that was working normally.
//
// The incident report blamed the session-snapshot timeout. These tests are
// built so that story cannot be what makes them pass: the snapshot here is
// HEALTHY and contains the session bead recording the real runtime name.

// poolSessionBead is a session-class bead recording that template runs under
// runtime session sessionName. It is the record the status path would have to
// read to learn a runtime name it cannot derive.
func poolSessionBead(template, sessionName string) beads.Bead {
	return beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        string(session.StateActive),
			"template":     template,
			"session_name": sessionName,
		},
	}
}

// poolIdentityState builds a one-rig city whose single agent carries the
// capacity shape that flips a singleton onto pool identity
// (min_active_sessions = 0 with max_active_sessions = 1 — the live gascity
// deployer/architect shape), plus a healthy session-class store.
func poolIdentityState(t *testing.T, agent config.Agent) (*fakeState, *beads.MemStore) {
	t.Helper()
	st := newFakeState(t)
	sessions := beads.NewMemStore()
	st.cityBeadStore = beads.NewMemStore()
	st.sessionsBeadStore = sessions
	st.stores = map[string]beads.Store{}
	st.cfg.Agents = []config.Agent{agent}
	st.cfg.NamedSessions = nil
	st.cfg.Rigs = []config.Rig{{Name: agent.Dir, Path: t.TempDir()}}
	return st, sessions
}

func agentRow(t *testing.T, body StatusBody, qualifiedName string) StatusAgentDetail {
	t.Helper()
	for _, d := range body.AgentDetails {
		if d.QualifiedName == qualifiedName {
			return d
		}
	}
	var names []string
	for _, d := range body.AgentDetails {
		names = append(names, d.QualifiedName)
	}
	t.Fatalf("no agent row for %q; rows present = %v", qualifiedName, names)
	return StatusAgentDetail{}
}

// TestStatusReportsPoolSuffixedSessionAsRunning is the acceptance case for the
// incident's deployer. The session snapshot is healthy and names the runtime
// session; the session is genuinely running under that name. Status must say so.
func TestStatusReportsPoolSuffixedSessionAsRunning(t *testing.T) {
	st, sessions := poolIdentityState(t, config.Agent{
		Name: "deployer", Dir: "gascity", Provider: "test-agent",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1),
	})
	if _, err := sessions.Create(poolSessionBead("gascity/deployer", "gascity--deployer-pool")); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := st.sp.Start(context.Background(), "gascity--deployer-pool", runtime.Config{}); err != nil {
		t.Fatalf("start runtime session: %v", err)
	}

	body := New(st).buildStatusBody(context.Background(), false)

	// Guard: the snapshot must actually be healthy, or this test would pass
	// for the wrong reason (or fail blaming the timeout).
	for _, pe := range body.PartialErrors {
		if strings.HasPrefix(pe, "sessions:") {
			t.Fatalf("session snapshot is degraded (%q); this test must exercise the healthy read", pe)
		}
	}

	row := agentRow(t, body, "gascity/deployer")
	if !row.Running {
		t.Fatalf("gascity/deployer running = false, want true: its session %q is live and the session bead records that name (row=%+v)",
			"gascity--deployer-pool", row)
	}
}

// TestStatusReportsCanonicallyNamedSessionAsRunning is the positive control.
// The same agent shape whose runtime session DOES sit on the derived canonical
// name must read running — otherwise the test above could be satisfied by a
// harness that never observes anything.
func TestStatusReportsCanonicallyNamedSessionAsRunning(t *testing.T) {
	st, sessions := poolIdentityState(t, config.Agent{
		Name: "deployer", Dir: "beads", Provider: "test-agent",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1),
	})
	if _, err := sessions.Create(poolSessionBead("beads/deployer", "beads--deployer")); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := st.sp.Start(context.Background(), "beads--deployer", runtime.Config{}); err != nil {
		t.Fatalf("start runtime session: %v", err)
	}

	body := New(st).buildStatusBody(context.Background(), false)

	row := agentRow(t, body, "beads/deployer")
	if !row.Running {
		t.Fatalf("beads/deployer running = false, want true — the canonical name is the one status derives (row=%+v)", row)
	}
}

// TestStatusKeepsUnlimitedPoolAgentVisible covers the second population from
// the same live capture: an unlimited-capacity agent (no max_active_sessions)
// whose live session sits on the bare canonical name. discoverUnlimitedPool
// lists the "<name>-" instance prefix, which that name does not match, so the
// agent must still be carried by some other leg — vanishing from agent_details
// entirely is strictly worse than reporting it stopped, because a consumer
// cannot even see that it was not measured.
func TestStatusKeepsUnlimitedPoolAgentVisible(t *testing.T) {
	st, sessions := poolIdentityState(t, config.Agent{
		Name: "witness", Dir: "beads", Provider: "test-agent",
	})
	if _, err := sessions.Create(poolSessionBead("beads/witness", "beads--witness")); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := st.sp.Start(context.Background(), "beads--witness", runtime.Config{}); err != nil {
		t.Fatalf("start runtime session: %v", err)
	}

	body := New(st).buildStatusBody(context.Background(), false)

	row := agentRow(t, body, "beads/witness")
	if !row.Running {
		t.Fatalf("beads/witness running = false, want true: session %q is live (row=%+v)", "beads--witness", row)
	}
}

// TestStatusCountsOwnNamedSessionOnce covers the same agent once it is also a
// configured named session. The agent expansion now lists that identity
// itself, so the session-bead leg must not add a second row for it, and the
// row must be there even before the named session has ever started.
func TestStatusCountsOwnNamedSessionOnce(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(fmt.Sprintf("running=%v", running), func(t *testing.T) {
			st, sessions := poolIdentityState(t, config.Agent{
				Name: "witness", Dir: "beads", Provider: "test-agent",
			})
			st.cfg.NamedSessions = []config.NamedSession{{Template: "witness", Dir: "beads", Mode: "always"}}
			if running {
				if _, err := sessions.Create(poolSessionBead("beads/witness", "beads--witness")); err != nil {
					t.Fatalf("create session bead: %v", err)
				}
				if err := st.sp.Start(context.Background(), "beads--witness", runtime.Config{}); err != nil {
					t.Fatalf("start runtime session: %v", err)
				}
			}

			body := New(st).buildStatusBody(context.Background(), false)

			if body.Agents.Total != 1 {
				t.Fatalf("Agents.Total = %d, want 1 (rows=%+v)", body.Agents.Total, body.AgentDetails)
			}
			if row := agentRow(t, body, "beads/witness"); row.Running != running {
				t.Fatalf("beads/witness running = %v, want %v (row=%+v)", row.Running, running, row)
			}
		})
	}
}

// poolSessionBeadWithAgentName is the production shape of a pool session bead.
// createPoolSessionBeadWithGuardedAliasUsingLock
// (cmd/gc/build_desired_state.go:4836) records agent_name as the qualified
// INSTANCE name, falling back to the template when the pool collapses onto a
// single canonical identity. The tests above deliberately omit agent_name to
// pin the degraded path; these pin the path production actually writes.
func poolSessionBeadWithAgentName(template, agentName, sessionName string) beads.Bead {
	b := poolSessionBead(template, sessionName)
	b.Metadata["agent_name"] = agentName
	return b
}

// TestStatusReportsPoolSuffixedSessionAsRunningWithRecordedAgentName is the
// incident's deployer in the shape the create path really persists.
func TestStatusReportsPoolSuffixedSessionAsRunningWithRecordedAgentName(t *testing.T) {
	st, sessions := poolIdentityState(t, config.Agent{
		Name: "deployer", Dir: "gascity", Provider: "test-agent",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1),
	})
	if _, err := sessions.Create(poolSessionBeadWithAgentName(
		"gascity/deployer", "gascity/deployer", "gascity--deployer-pool")); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := st.sp.Start(context.Background(), "gascity--deployer-pool", runtime.Config{}); err != nil {
		t.Fatalf("start runtime session: %v", err)
	}

	row := agentRow(t, New(st).buildStatusBody(context.Background(), false), "gascity/deployer")
	if !row.Running {
		t.Fatalf("gascity/deployer running = false, want true (row=%+v)", row)
	}
	if row.SessionName != "gascity--deployer-pool" {
		t.Fatalf("SessionName = %q, want %q — the row must name the session it measured",
			row.SessionName, "gascity--deployer-pool")
	}
}

// TestStatusMapsEachPoolInstanceToItsOwnSession guards the disambiguation rule.
// A multi-instance pool must not cross-wire instances onto each other's
// sessions: reporting the right boolean off the wrong session would be a
// coincidence, not a fix.
func TestStatusMapsEachPoolInstanceToItsOwnSession(t *testing.T) {
	st, sessions := poolIdentityState(t, config.Agent{
		Name: "deployer", Dir: "gascity", Provider: "test-agent",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2),
	})
	want := map[string]string{
		"gascity/deployer-1": "gascity--deployer-1-pool",
		"gascity/deployer-2": "gascity--deployer-2-pool",
	}
	for qn, sn := range want {
		if _, err := sessions.Create(poolSessionBeadWithAgentName("gascity/deployer", qn, sn)); err != nil {
			t.Fatalf("create session bead %s: %v", qn, err)
		}
		if err := st.sp.Start(context.Background(), sn, runtime.Config{}); err != nil {
			t.Fatalf("start runtime session %s: %v", sn, err)
		}
	}

	body := New(st).buildStatusBody(context.Background(), false)
	for qn, sn := range want {
		row := agentRow(t, body, qn)
		if row.SessionName != sn {
			t.Fatalf("%s SessionName = %q, want %q (cross-wired instance)", qn, row.SessionName, sn)
		}
		if !row.Running {
			t.Fatalf("%s running = false, want true (row=%+v)", qn, row)
		}
	}
}
