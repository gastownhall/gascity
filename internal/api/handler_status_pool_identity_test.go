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
// row must be there even before the named session has ever started — without
// taking over the session of a pool instance running beside it.
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

	// Routed work mints a <template>-1 instance while the on-demand named
	// session has never started, so the only session bead recorded against
	// the template is that sibling's, whether the instance runs under its
	// canonical name or a bead-scoped one. A bead an older build left on a
	// bead-scoped name, recording no identity or the template itself, is the
	// same case: status lists that session as a row of its own. The named
	// row must stay on its own (stopped) session, leaving one live session
	// counted once, on the row that owns it.
	for _, tt := range []struct {
		name        string
		bead        beads.Bead
		sessionName string
		row         string
	}{
		{
			name:        "stopped beside a running instance",
			bead:        poolSessionBeadWithAgentName("beads/witness", "beads/witness-1", "beads--witness-1"),
			sessionName: "beads--witness-1",
			row:         "beads/witness-1",
		},
		{
			name:        "stopped beside an instance on a bead-scoped name",
			bead:        poolSessionBeadWithAgentName("beads/witness", "beads/witness-1", "witness-gc-41"),
			sessionName: "witness-gc-41",
			row:         "beads/witness-1",
		},
		{
			name:        "stopped beside a running bead with no recorded identity",
			bead:        poolSessionBead("beads/witness", "witness-gc-41"),
			sessionName: "witness-gc-41",
			row:         "witness-gc-41",
		},
		{
			name:        "stopped beside a running bead recording the template",
			bead:        poolSessionBeadWithAgentName("beads/witness", "beads/witness", "witness-gc-41"),
			sessionName: "witness-gc-41",
			row:         "witness-gc-41",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, sessions := poolIdentityState(t, config.Agent{
				Name: "witness", Dir: "beads", Provider: "test-agent",
			})
			st.cfg.NamedSessions = []config.NamedSession{{Template: "witness", Dir: "beads"}}
			if _, err := sessions.Create(tt.bead); err != nil {
				t.Fatalf("create session bead: %v", err)
			}
			if err := st.sp.Start(context.Background(), tt.sessionName, runtime.Config{}); err != nil {
				t.Fatalf("start runtime session: %v", err)
			}

			body := New(st).buildStatusBody(context.Background(), false)

			if body.Agents.Total != 2 || body.Agents.Running != 1 {
				t.Fatalf("Agents = %+v, want Total=2 Running=1 (rows=%+v)", body.Agents, body.AgentDetails)
			}
			if row := agentRow(t, body, "beads/witness"); row.SessionName != "beads--witness" || row.Running {
				t.Fatalf("beads/witness = {SessionName:%q Running:%v}, want {beads--witness false}: it must not report a session another row lists (row=%+v)",
					row.SessionName, row.Running, row)
			}
			if row := agentRow(t, body, tt.row); row.SessionName != tt.sessionName || !row.Running {
				t.Fatalf("%s = {SessionName:%q Running:%v}, want {%s true} (row=%+v)",
					tt.row, row.SessionName, row.Running, tt.sessionName, row)
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

// TestStatusReportsSingletonSeatUnderStaleSlotIdentity is the other side of
// the rule above. A single-seat template (max_active_sessions = 1) lists no
// session beads as rows of their own, yet it can hold a bead recorded under a
// slot-suffixed identity. An older build may have written one; the controller
// reclaims it as the canonical seat (normalizeNonExpandingPoolSessionInfo in
// cmd/gc), so until it does, that bead IS the seat. A namepool seat is never
// reclaimed, because a namepool template does not use the canonical singleton
// identity; when its file loads no names, the seat still lists under the
// template's name. Status must keep measuring the seat in both cases.
func TestStatusReportsSingletonSeatUnderStaleSlotIdentity(t *testing.T) {
	for _, tt := range []struct {
		name  string
		agent config.Agent
	}{
		{
			name: "singleton pool",
			agent: config.Agent{
				Name: "deployer", Dir: "gascity", Provider: "test-agent",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1),
			},
		},
		{
			name: "namepool seat without names",
			agent: config.Agent{
				Name: "deployer", Dir: "gascity", Provider: "test-agent",
				MaxActiveSessions: intPtr(1), Namepool: "names.txt",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, sessions := poolIdentityState(t, tt.agent)
			if _, err := sessions.Create(poolSessionBeadWithAgentName(
				"gascity/deployer", "gascity/deployer-1", "deployer-gc-41")); err != nil {
				t.Fatalf("create session bead: %v", err)
			}
			if err := st.sp.Start(context.Background(), "deployer-gc-41", runtime.Config{}); err != nil {
				t.Fatalf("start runtime session: %v", err)
			}

			body := New(st).buildStatusBody(context.Background(), false)

			if body.Agents.Total != 1 || body.Agents.Running != 1 {
				t.Fatalf("Agents = %+v, want Total=1 Running=1 (rows=%+v)", body.Agents, body.AgentDetails)
			}
			if row := agentRow(t, body, "gascity/deployer"); row.SessionName != "deployer-gc-41" || !row.Running {
				t.Fatalf("gascity/deployer = {SessionName:%q Running:%v}, want {deployer-gc-41 true} (row=%+v)",
					row.SessionName, row.Running, row)
			}
		})
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

// TestStatusRuntimeSessionNameTemplateFallback pins how statusRuntimeSessionName
// resolves a row whose identity is the template itself and whose canonical
// name has no bead. An unlimited template lists every bead recorded against
// it as a row of its own, so its template row keeps the canonical name; a
// bounded one still takes the one bead recorded against it (step 3).
func TestStatusRuntimeSessionNameTemplateFallback(t *testing.T) {
	const qn = "beads/witness"
	canonical := agentSessionName("city", qn, "")
	unlimited := config.Agent{Name: "witness", Dir: "beads"}
	singleSeat := config.Agent{Name: "witness", Dir: "beads", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)}
	namepoolSeat := config.Agent{Name: "witness", Dir: "beads", MaxActiveSessions: intPtr(1), Namepool: "names.txt"}
	sibling := statusSessionInfo{sessionName: "beads--witness-1", agentName: "beads/witness-1", template: qn}
	unrecorded := statusSessionInfo{sessionName: "witness-gc-41", template: qn}
	templateRecorded := statusSessionInfo{sessionName: "witness-gc-41", agentName: qn, template: qn}
	staleSlot := statusSessionInfo{sessionName: "witness-gc-7", agentName: "beads/witness-1", template: qn}
	tests := []struct {
		name  string
		agent config.Agent
		beads []statusSessionInfo
		want  string
	}{
		{name: "unlimited template does not take a sibling instance's session", agent: unlimited, beads: []statusSessionInfo{sibling}, want: canonical},
		{name: "unlimited template does not take a bead with no recorded identity", agent: unlimited, beads: []statusSessionInfo{unrecorded}, want: canonical},
		{name: "unlimited template does not take a bead recording the template", agent: unlimited, beads: []statusSessionInfo{templateRecorded}, want: canonical},
		{name: "single-seat template keeps a stale slot identity", agent: singleSeat, beads: []statusSessionInfo{staleSlot}, want: staleSlot.sessionName},
		{name: "namepool seat without names keeps its slot identity", agent: namepoolSeat, beads: []statusSessionInfo{staleSlot}, want: staleSlot.sessionName},
		{name: "single-seat template keeps a bead with no recorded identity", agent: singleSeat, beads: []statusSessionInfo{unrecorded}, want: unrecorded.sessionName},
		{name: "two beads on a single-seat template are ambiguous", agent: singleSeat, beads: []statusSessionInfo{unrecorded, staleSlot}, want: canonical},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := statusSessionSnapshot{
				bySessionName: make(map[string]statusSessionInfo, len(tt.beads)),
				byTemplate:    map[string][]statusSessionInfo{qn: tt.beads},
			}
			for _, info := range tt.beads {
				snapshot.bySessionName[info.sessionName] = info
			}
			if got := statusRuntimeSessionName("city", "", qn, qn, statusListsTemplateBeads(tt.agent), snapshot); got != tt.want {
				t.Fatalf("statusRuntimeSessionName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStatusScaleLabelMatchesCLI pins the banner on a pool group's first row
// to the one the CLI renders client-side (buildCityStatusSnapshot via
// scaleParamsFor): an unset max_active_sessions is unlimited, not max=1.
func TestStatusScaleLabelMatchesCLI(t *testing.T) {
	tests := []struct {
		name  string
		agent config.Agent
		named bool
		want  string
	}{
		{name: "unset max is unlimited", agent: config.Agent{Name: "witness", Dir: "beads", Provider: "test-agent"}, want: "scaled (min=0, max=unlimited)"},
		{name: "unset max with own named session is unlimited", agent: config.Agent{Name: "witness", Dir: "beads", Provider: "test-agent"}, named: true, want: "scaled (min=0, max=unlimited)"},
		{name: "negative max is unlimited", agent: config.Agent{Name: "witness", Dir: "beads", Provider: "test-agent", MaxActiveSessions: intPtr(-1)}, want: "scaled (min=0, max=unlimited)"},
		{name: "bounded pool keeps its bounds", agent: config.Agent{Name: "witness", Dir: "beads", Provider: "test-agent", MinActiveSessions: intPtr(2), MaxActiveSessions: intPtr(5)}, want: "scaled (min=2, max=5)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, sessions := poolIdentityState(t, tt.agent)
			if tt.named {
				st.cfg.NamedSessions = []config.NamedSession{{Template: "witness", Dir: "beads"}}
			}
			// An unlimited pool lists only the instances it has, so give every
			// group a running one for its first row.
			if _, err := sessions.Create(poolSessionBeadWithAgentName("beads/witness", "beads/witness-1", "beads--witness-1")); err != nil {
				t.Fatalf("create session bead: %v", err)
			}
			if err := st.sp.Start(context.Background(), "beads--witness-1", runtime.Config{}); err != nil {
				t.Fatalf("start runtime session: %v", err)
			}

			body := New(st).buildStatusBody(context.Background(), false)

			var labels []string
			for _, d := range body.AgentDetails {
				if d.GroupName == "beads/witness" && d.ScaleLabel != "" {
					labels = append(labels, d.ScaleLabel)
				}
			}
			if len(labels) != 1 || labels[0] != tt.want {
				t.Fatalf("beads/witness scale labels = %q, want exactly [%q] (rows=%+v)", labels, tt.want, body.AgentDetails)
			}
		})
	}
}
