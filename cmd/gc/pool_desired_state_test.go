package main

import (
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func intPtr(n int) *int { return &n }

func workBead(id, routedTo, assignee, status string, priority int) beads.Bead {
	p := priority
	return beads.Bead{
		ID:       id,
		Status:   status,
		Assignee: assignee,
		Priority: &p,
		Metadata: map[string]string{"gc.routed_to": routedTo},
	}
}

func sessionBead(id, status string) beads.Bead {
	return beads.Bead{ID: id, Status: status, Type: "session"}
}

func pendingPoolSessionBead(id string) beads.Bead {
	return poolSessionBeadWithState(id, "creating", boolMetadata(true))
}

func pendingPoolSessionBeadAt(id string, createdAt time.Time) beads.Bead {
	session := pendingPoolSessionBead(id)
	session.CreatedAt = createdAt
	return session
}

func poolSessionBeadWithState(id, state, pendingCreateClaim string) beads.Bead {
	const template = "claude"
	return beads.Bead{
		ID:     id,
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:" + template},
		Metadata: map[string]string{
			"template":             template,
			"session_name":         PoolSessionName(template, id),
			"state":                state,
			"pending_create_claim": pendingCreateClaim,
			poolManagedMetadataKey: boolMetadata(true),
		},
	}
}

func poolTraceDecision(t *testing.T, trace *sessionReconcilerTraceCycle, site TraceSiteCode) SessionReconcilerTraceRecord {
	t.Helper()
	for _, rec := range trace.records {
		if rec.RecordType == TraceRecordDecision && rec.SiteCode == site {
			return rec
		}
	}
	t.Fatalf("missing trace decision for %s; records=%#v", site, trace.records)
	return SessionReconcilerTraceRecord{}
}

func poolTraceFieldInt(t *testing.T, fields map[string]any, key string) int {
	t.Helper()
	got, ok := fields[key].(int)
	if !ok {
		t.Fatalf("trace field %s = %#v, want int", key, fields[key])
	}
	return got
}

func newPoolDesiredStateTestTrace(templates ...string) *sessionReconcilerTraceCycle {
	detail := make(map[string]TraceSource, len(templates))
	for _, template := range templates {
		detail[normalizedTraceTemplate(template)] = TraceSourceManual
	}
	return &sessionReconcilerTraceCycle{
		tracer:            &SessionReconcilerTracer{detail: detail},
		dropReasons:       make(map[string]int),
		pendingDetail:     make(map[string][]SessionReconcilerTraceRecord),
		pendingDropped:    make(map[string]int),
		templatesTouched:  make(map[string]struct{}),
		detailedTemplates: make(map[string]struct{}),
		decisionCounts:    make(map[string]int),
		operationCounts:   make(map[string]int),
		mutationCounts:    make(map[string]int),
		reasonCounts:      make(map[string]int),
		outcomeCounts:     make(map[string]int),
	}
}

func poolAgent(name, dir string, maxSess *int, minSess int) config.Agent {
	var minPtr *int
	if minSess > 0 {
		minPtr = &minSess
	}
	return config.Agent{
		Name:              name,
		Dir:               dir,
		MaxActiveSessions: maxSess,
		MinActiveSessions: minPtr,
	}
}

func TestComputePoolDesiredStates_ResumeBeatsNew(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(2), 0)},
	}
	// 1 assigned (resume) + 2 new demand. scale_check reports only the new
	// demand, and the max cap admits one of those two new requests.
	work := []beads.Bead{
		workBead("w1", "rig/claude", "sess-1", "in_progress", 5),
	}
	sessions := []beads.Bead{sessionBead("sess-1", "open")}
	scaleCheck := map[string]int{"rig/claude": 2}

	result := ComputePoolDesiredStates(cfg, work, sessions, scaleCheck)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	reqs := result[0].Requests
	// Max=2: resume (w1) + 1 new from scale_check, capped at max=2.
	if len(reqs) != 2 {
		t.Fatalf("len(requests) = %d, want 2 (max=2)", len(reqs))
	}
	if reqs[0].Tier != "resume" {
		t.Errorf("first request tier = %q, want resume", reqs[0].Tier)
	}
	if reqs[0].SessionBeadID != "sess-1" {
		t.Errorf("first request session = %q, want sess-1", reqs[0].SessionBeadID)
	}
	if reqs[1].Tier != "new" {
		t.Errorf("second request tier = %q, want new", reqs[1].Tier)
	}
}

func TestComputePoolDesiredStates_MaxCapsTotal(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(2), 0)},
	}
	// scale_check reports 3 demand, but max=2.
	scaleCheck := map[string]int{"rig/claude": 3}

	result := ComputePoolDesiredStates(cfg, nil, nil, scaleCheck)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	// Max=2: only 2 of the 3 requested sessions allowed.
	if len(result[0].Requests) != 2 {
		t.Errorf("len(requests) = %d, want 2 (capped by max)", len(result[0].Requests))
	}
}

func TestComputePoolDesiredStates_MaxCapsResumeBeads(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(2), 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude", "s1", "in_progress", 5),
		workBead("w2", "rig/claude", "s2", "in_progress", 3),
		workBead("w3", "rig/claude", "s3", "in_progress", 1),
	}
	sessions := []beads.Bead{
		sessionBead("s1", "open"),
		sessionBead("s2", "open"),
		sessionBead("s3", "open"),
	}

	result := ComputePoolDesiredStates(cfg, work, sessions, nil)

	// Max=2: only 2 of the 3 in-progress beads get sessions.
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if len(result[0].Requests) != 2 {
		t.Errorf("len(requests) = %d, want 2 (max caps even resume)", len(result[0].Requests))
	}
}

func TestComputePoolDesiredStates_MinFillsIdle(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("wf-ctrl", "", intPtr(1), 1)},
	}

	result := ComputePoolDesiredStates(cfg, nil, nil, nil)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if len(result[0].Requests) != 1 {
		t.Errorf("len(requests) = %d, want 1 (min=1 fills idle)", len(result[0].Requests))
	}
}

func TestComputePoolDesiredStates_MinRespectsMax(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("worker", "", intPtr(0), 5)},
	}

	result := ComputePoolDesiredStates(cfg, nil, nil, nil)

	// Max=0 should prevent any sessions even though min=5.
	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 0 {
		t.Errorf("total requests = %d, want 0 (max=0 overrides min)", total)
	}
}

func TestComputePoolDesiredStates_MaxOneTemplatesStillParticipateInDemand(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			MaxActiveSessions: intPtr(1),
		}},
	}
	work := []beads.Bead{
		workBead("w1", "worker", "worker", "open", 5),
	}
	sessions := []beads.Bead{
		sessionBead("worker", "open"),
	}

	result := ComputePoolDesiredStates(cfg, work, sessions, nil)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1 for max=1 demand", len(result))
	}
	if len(result[0].Requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1", len(result[0].Requests))
	}
	if result[0].Template != "worker" {
		t.Fatalf("template = %q, want worker", result[0].Template)
	}
}

func TestComputePoolDesiredStates_WorkspaceCap(t *testing.T) {
	wsMax := 3
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &wsMax},
		Agents: []config.Agent{
			poolAgent("claude", "rig", nil, 0),
			poolAgent("codex", "rig", nil, 0),
		},
	}
	scaleCheck := map[string]int{"rig/claude": 2, "rig/codex": 2}

	result := ComputePoolDesiredStates(cfg, nil, nil, scaleCheck)

	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 3 {
		t.Errorf("total requests = %d, want 3 (workspace cap)", total)
	}
}

func TestComputePoolDesiredStates_RigCap(t *testing.T) {
	rigMax := 2
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "rig", Path: "/tmp/rig", MaxActiveSessions: &rigMax}},
		Agents: []config.Agent{
			poolAgent("claude", "rig", nil, 0),
			poolAgent("codex", "rig", nil, 0),
		},
	}
	scaleCheck := map[string]int{"rig/claude": 2, "rig/codex": 1}

	result := ComputePoolDesiredStates(cfg, nil, nil, scaleCheck)

	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 2 {
		t.Errorf("total requests = %d, want 2 (rig cap)", total)
	}
}

func TestComputePoolDesiredStates_NestedCaps(t *testing.T) {
	wsMax := 10
	rigMax := 3
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &wsMax},
		Rigs:      []config.Rig{{Name: "rig", Path: "/tmp/rig", MaxActiveSessions: &rigMax}},
		Agents: []config.Agent{
			poolAgent("claude", "rig", intPtr(2), 0),
			poolAgent("codex", "rig", intPtr(2), 0),
		},
	}
	scaleCheck := map[string]int{"rig/claude": 2, "rig/codex": 2}

	result := ComputePoolDesiredStates(cfg, nil, nil, scaleCheck)

	total := 0
	perAgent := make(map[string]int)
	for _, ds := range result {
		perAgent[ds.Template] = len(ds.Requests)
		total += len(ds.Requests)
	}
	// Rig cap=3, agent caps=2 each. 4 beads, but rig caps at 3.
	if total != 3 {
		t.Errorf("total = %d, want 3 (rig cap)", total)
	}
	// Claude gets 2 (its max), codex gets 1 (rig cap - claude's 2).
	if perAgent["rig/claude"] != 2 {
		t.Errorf("claude = %d, want 2", perAgent["rig/claude"])
	}
	if perAgent["rig/codex"] != 1 {
		t.Errorf("codex = %d, want 1", perAgent["rig/codex"])
	}
}

func TestComputePoolDesiredStates_UnlimitedWhenUnset(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", nil, 0)},
	}
	scaleCheck := map[string]int{"claude": 5}

	result := ComputePoolDesiredStates(cfg, nil, nil, scaleCheck)

	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5 (unlimited)", total)
	}
}

func TestComputePoolDesiredStates_ClosedSessionNotResumed(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "claude", "dead-session", "in_progress", 5),
	}
	sessions := []beads.Bead{sessionBead("dead-session", "closed")}

	result := ComputePoolDesiredStates(cfg, work, sessions, nil)

	// The session bead is closed, so this shouldn't be a resume request.
	// It also shouldn't be a new request because it has an assignee.
	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0 (closed session, assigned bead — orphaned)", total)
	}
}

func TestComputePoolDesiredStates_DedupsResumeForSameSession(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", nil, 0)},
	}
	// Two beads assigned to the same session.
	work := []beads.Bead{
		workBead("w1", "claude", "sess-1", "in_progress", 5),
		workBead("w2", "claude", "sess-1", "open", 3),
	}
	sessions := []beads.Bead{sessionBead("sess-1", "open")}

	result := ComputePoolDesiredStates(cfg, work, sessions, nil)

	// Should deduplicate — only one resume request for sess-1.
	resumeCount := 0
	for _, ds := range result {
		for _, req := range ds.Requests {
			if req.Tier == "resume" {
				resumeCount++
			}
		}
	}
	if resumeCount != 1 {
		t.Errorf("resume count = %d, want 1 (deduped)", resumeCount)
	}
}

func TestComputePoolDesiredStates_ResumePriorityOrder(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(2), 0)},
	}
	// 3 assigned beads with different priorities, max=2. Highest priority wins.
	work := []beads.Bead{
		workBead("w-low", "claude", "s1", "in_progress", 1),
		workBead("w-high", "claude", "s2", "in_progress", 10),
		workBead("w-mid", "claude", "s3", "in_progress", 5),
	}
	sessions := []beads.Bead{
		sessionBead("s1", "open"),
		sessionBead("s2", "open"),
		sessionBead("s3", "open"),
	}

	result := ComputePoolDesiredStates(cfg, work, sessions, nil)

	if len(result) != 1 || len(result[0].Requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(result[0].Requests))
	}
	// Highest priority resume requests should be accepted.
	if result[0].Requests[0].BeadPriority != 10 {
		t.Errorf("first priority = %d, want 10", result[0].Requests[0].BeadPriority)
	}
	if result[0].Requests[1].BeadPriority != 5 {
		t.Errorf("second priority = %d, want 5", result[0].Requests[1].BeadPriority)
	}
}

func TestComputePoolDesiredStates_SuspendedAgentSkipped(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "claude", Suspended: true, MaxActiveSessions: intPtr(-1)},
		},
	}
	scaleCheck := map[string]int{"claude": 1}

	result := ComputePoolDesiredStates(cfg, nil, nil, scaleCheck)

	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0 (agent suspended)", total)
	}
}

func TestComputePoolDesiredStates_ScaleCheckMerge(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(5), 0)},
	}
	// No work beads visible (they're in the rig store, not passed here).
	// But scale_check says 2.
	scaleCheck := map[string]int{"rig/claude": 2}
	result := ComputePoolDesiredStates(cfg, nil, nil, scaleCheck)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if len(result[0].Requests) != 2 {
		t.Fatalf("len(requests) = %d, want 2 (from scale_check)", len(result[0].Requests))
	}
	for _, r := range result[0].Requests {
		if r.Tier != "new" {
			t.Errorf("request tier = %q, want new", r.Tier)
		}
	}
}

func TestComputePoolDesiredStates_UnassignedRoutedBeadDoesNotCreateDemand(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(5), 0)},
	}
	// Routed but unassigned queue work is handled by scale_check/work_query,
	// not bead-driven pool demand.
	work := []beads.Bead{
		workBead("w1", "rig/claude", "", "open", 5),
	}
	result := ComputePoolDesiredStates(cfg, work, nil, map[string]int{"rig/claude": 0})

	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 0 {
		t.Fatalf("total requests = %d, want 0", total)
	}
}

func TestComputePoolDesiredStates_ScaleCheckRespectsCaps(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(3), 0)},
	}
	// scale_check says 10, but max=3.
	scaleCheck := map[string]int{"rig/claude": 10}
	result := ComputePoolDesiredStates(cfg, nil, nil, scaleCheck)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if len(result[0].Requests) != 3 {
		t.Fatalf("len(requests) = %d, want 3 (capped at max)", len(result[0].Requests))
	}
}

func TestComputePoolDesiredStates_CapsNewDemandBeforeMaterializingRequests(t *testing.T) {
	workspaceMax := 2
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
		Agents:    []config.Agent{poolAgent("claude", "", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "claude", "sess-1", "in_progress", 5),
	}
	sessions := []beads.Bead{sessionBead("sess-1", "open")}
	trace := newPoolDesiredStateTestTrace("claude")

	result := computePoolDesiredStates(cfg, work, sessions, map[string]int{"claude": 10}, trace)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if len(result[0].Requests) != 2 {
		t.Fatalf("len(requests) = %d, want 2 (one resume plus one new demand within workspace cap)", len(result[0].Requests))
	}
	newCount := 0
	for _, req := range result[0].Requests {
		if req.Tier == "new" {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("new requests = %d, want 1", newCount)
	}
	capRejections := trace.decisionCounts[string(TraceSitePoolAgentCap)] +
		trace.decisionCounts[string(TraceSitePoolRigCap)] +
		trace.decisionCounts[string(TraceSitePoolWorkspaceCap)]
	if capRejections != 0 {
		t.Fatalf("cap rejections = %d, want 0; new demand should be capped before request materialization", capRejections)
	}
}

func TestComputePoolDesiredStates_OpenAssignedWorkResumes(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(5), 0)},
	}
	work := []beads.Bead{
		workBead("w1", "claude", "sess-1", "open", 5),
	}
	sessions := []beads.Bead{sessionBead("sess-1", "open")}

	result := ComputePoolDesiredStates(cfg, work, sessions, nil)

	if len(result) != 1 || len(result[0].Requests) != 1 {
		t.Fatalf("expected 1 request, got %#v", result)
	}
	if result[0].Requests[0].Tier != "resume" {
		t.Fatalf("tier = %q, want resume", result[0].Requests[0].Tier)
	}
	if result[0].Requests[0].SessionBeadID != "sess-1" {
		t.Fatalf("session = %q, want sess-1", result[0].Requests[0].SessionBeadID)
	}
}

// --- Regression tests: these define the consolidated demand behavior ---

// Regression: resume preserves assigned session even when scale_check is 0.
func TestComputePoolDesiredStates_ResumeOverridesZeroScaleCheck(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(5), 0)},
	}
	work := []beads.Bead{
		workBead("w1", "claude", "sess-1", "in_progress", 5),
	}
	sessions := []beads.Bead{sessionBead("sess-1", "open")}
	scaleCheck := map[string]int{"claude": 0}

	result := ComputePoolDesiredStates(cfg, work, sessions, scaleCheck)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if len(result[0].Requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1 (resume keeps assigned session despite scale_check=0)", len(result[0].Requests))
	}
	if result[0].Requests[0].Tier != "resume" {
		t.Errorf("tier = %q, want resume", result[0].Requests[0].Tier)
	}
}

// Regression: no demand and no assigned work → poolDesired=0.
// This was the idle-sessions-never-sleeping bug: derivePoolDesired counted
// session bead existence instead of actual demand.
func TestComputePoolDesiredStates_NoDemandNoAssignment(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(5), 0)},
	}
	// No work beads, no scale_check demand.
	result := ComputePoolDesiredStates(cfg, nil, nil, map[string]int{"claude": 0})

	counts := PoolDesiredCounts(result)
	if counts["claude"] != 0 {
		t.Fatalf("poolDesired[claude] = %d, want 0 (no demand, no assignment)", counts["claude"])
	}
}

// Regression: scale_check reports new demand, not total desired sessions.
func TestComputePoolDesiredStates_ScaleCheckAndResumeAddUp(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(5), 0)},
	}
	work := []beads.Bead{
		workBead("w1", "claude", "sess-1", "in_progress", 5),
	}
	sessions := []beads.Bead{sessionBead("sess-1", "open")}
	scaleCheck := map[string]int{"claude": 2}

	result := ComputePoolDesiredStates(cfg, work, sessions, scaleCheck)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if len(result[0].Requests) != 3 {
		t.Fatalf("len(requests) = %d, want 3 (1 resume + 2 new from scale_check)", len(result[0].Requests))
	}
	resumeCount := 0
	newCount := 0
	for _, r := range result[0].Requests {
		switch r.Tier {
		case "resume":
			resumeCount++
		case "new":
			newCount++
		}
	}
	if resumeCount != 1 || newCount != 2 {
		t.Errorf("resume=%d new=%d, want resume=1 new=2", resumeCount, newCount)
	}
}

func TestComputePoolDesiredStates_AssignedSessionsDoNotConsumeNewDemand(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(20), 0)},
	}
	var work []beads.Bead
	var sessions []beads.Bead
	for i := 1; i <= 5; i++ {
		suffix := strconv.Itoa(i)
		sessionID := "sess-" + suffix
		work = append(work, workBead("w"+suffix, "claude", sessionID, "in_progress", 0))
		sessions = append(sessions, sessionBead(sessionID, "open"))
	}

	result := ComputePoolDesiredStates(cfg, work, sessions, map[string]int{"claude": 5})

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if got := len(result[0].Requests); got != 10 {
		t.Fatalf("len(requests) = %d, want 10 (5 assigned resume + 5 new ready)", got)
	}
	resumeCount := 0
	newCount := 0
	for _, request := range result[0].Requests {
		switch request.Tier {
		case "resume":
			resumeCount++
		case "new":
			newCount++
		}
	}
	if resumeCount != 5 || newCount != 5 {
		t.Fatalf("request tiers = resume:%d new:%d, want resume:5 new:5", resumeCount, newCount)
	}
}

// Regression: scale_check counts unassigned ready work, which remains
// unassigned while just-created sessions are still starting. Those in-flight
// sessions must consume new demand or every reconciler tick can create another
// session for the same ready bead.
func TestComputePoolDesiredStates_InFlightNewSessionsConsumeScaleDemand(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	sessions := []beads.Bead{
		pendingPoolSessionBead("sess-1"),
		pendingPoolSessionBead("sess-2"),
		pendingPoolSessionBead("sess-3"),
	}
	scaleCheck := map[string]int{"claude": 3}

	result := ComputePoolDesiredStates(cfg, nil, sessions, scaleCheck)

	counts := PoolDesiredCounts(result)
	if counts["claude"] != 3 {
		t.Fatalf("poolDesired[claude] = %d, want 3 in-flight sessions preserving total demand", counts["claude"])
	}
	seen := make(map[string]bool)
	for _, req := range result[0].Requests {
		if req.Tier != "new" {
			t.Fatalf("tier = %q, want new", req.Tier)
		}
		if req.SessionBeadID == "" {
			t.Fatalf("in-flight session should be represented as an explicit desired request: %+v", req)
		}
		seen[req.SessionBeadID] = true
	}
	for _, id := range []string{"sess-1", "sess-2", "sess-3"} {
		if !seen[id] {
			t.Fatalf("missing in-flight request for %s; saw %#v", id, seen)
		}
	}
}

func TestComputePoolDesiredStates_InFlightNewSessionsDoNotCreateZeroDemand(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	sessions := []beads.Bead{
		pendingPoolSessionBead("sess-1"),
	}
	scaleCheck := map[string]int{"claude": 0}

	result := ComputePoolDesiredStates(cfg, nil, sessions, scaleCheck)

	counts := PoolDesiredCounts(result)
	if counts["claude"] != 0 {
		t.Fatalf("poolDesired[claude] = %d, want 0 when scale_check reports no new demand", counts["claude"])
	}
}

func TestComputePoolDesiredStates_InFlightNewSessionsOnlySubtractCoveredDemand(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	sessions := []beads.Bead{
		pendingPoolSessionBead("sess-1"),
		pendingPoolSessionBead("sess-2"),
	}
	scaleCheck := map[string]int{"claude": 5}

	result := ComputePoolDesiredStates(cfg, nil, sessions, scaleCheck)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	reqs := result[0].Requests
	if len(reqs) != 5 {
		t.Fatalf("len(requests) = %d, want 5 total desired sessions", len(reqs))
	}
	explicit := make(map[string]bool)
	anonymous := 0
	for _, req := range reqs {
		if req.Tier != "new" {
			t.Fatalf("tier = %q, want new", req.Tier)
		}
		if req.SessionBeadID == "" {
			anonymous++
			continue
		}
		explicit[req.SessionBeadID] = true
	}
	if anonymous != 3 {
		t.Fatalf("anonymous new requests = %d, want 3 after two in-flight sessions consume demand", anonymous)
	}
	for _, id := range []string{"sess-1", "sess-2"} {
		if !explicit[id] {
			t.Fatalf("missing explicit in-flight request for %s; saw %#v", id, explicit)
		}
	}
}

func TestComputePoolDesiredStates_InFlightResumeBeadsDoNotConsumeNewDemand(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	work := []beads.Bead{
		workBead("w1", "claude", "sess-1", "in_progress", 5),
	}
	sessions := []beads.Bead{
		pendingPoolSessionBead("sess-1"),
		pendingPoolSessionBead("sess-2"),
	}
	scaleCheck := map[string]int{"claude": 3}

	result := ComputePoolDesiredStates(cfg, work, sessions, scaleCheck)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	reqs := result[0].Requests
	if len(reqs) != 4 {
		t.Fatalf("len(requests) = %d, want 4 (one resume plus three new-demand slots)", len(reqs))
	}
	resume := 0
	explicitNew := 0
	anonymousNew := 0
	for _, req := range reqs {
		switch {
		case req.Tier == "resume":
			resume++
			if req.SessionBeadID != "sess-1" {
				t.Fatalf("resume SessionBeadID = %q, want sess-1", req.SessionBeadID)
			}
		case req.Tier == "new" && req.SessionBeadID == "sess-2":
			explicitNew++
		case req.Tier == "new" && req.SessionBeadID == "":
			anonymousNew++
		default:
			t.Fatalf("unexpected request: %+v", req)
		}
	}
	if resume != 1 || explicitNew != 1 || anonymousNew != 2 {
		t.Fatalf("resume=%d explicitNew=%d anonymousNew=%d, want 1/1/2", resume, explicitNew, anonymousNew)
	}
}

func TestComputePoolDesiredStates_InFlightPredicateBranches(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	tests := []struct {
		name    string
		session beads.Bead
	}{
		{
			name:    "pending create claim",
			session: poolSessionBeadWithState("sess-pending", "active", boolMetadata(true)),
		},
		{
			name:    "creating state",
			session: poolSessionBeadWithState("sess-creating", "creating", ""),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ComputePoolDesiredStates(cfg, nil, []beads.Bead{tt.session}, map[string]int{"claude": 1})

			if len(result) != 1 || len(result[0].Requests) != 1 {
				t.Fatalf("result = %#v, want one in-flight request", result)
			}
			if got := result[0].Requests[0].SessionBeadID; got != tt.session.ID {
				t.Fatalf("SessionBeadID = %q, want %q", got, tt.session.ID)
			}
		})
	}
}

func TestComputePoolDesiredStates_StaleCreatingBeadStillConsumesNewDemand(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	stale := poolSessionBeadWithState("sess-stale", "creating", "")
	stale.CreatedAt = time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC).Add(-2 * staleCreatingStateTimeout)

	result := ComputePoolDesiredStates(cfg, nil, []beads.Bead{stale}, map[string]int{"claude": 1})

	if len(result) != 1 || len(result[0].Requests) != 1 {
		t.Fatalf("result = %#v, want one stale creating request preserving already-spent demand", result)
	}
	if got := result[0].Requests[0].SessionBeadID; got != stale.ID {
		t.Fatalf("SessionBeadID = %q, want %q", got, stale.ID)
	}
}

func TestComputePoolDesiredStates_InFlightSelectionRespectsCapsInStableOrder(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(2), 0)},
	}
	base := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	sessions := []beads.Bead{
		pendingPoolSessionBeadAt("sess-newest", base.Add(4*time.Minute)),
		pendingPoolSessionBeadAt("sess-oldest", base.Add(time.Minute)),
		pendingPoolSessionBeadAt("sess-tie-b", base.Add(2*time.Minute)),
		pendingPoolSessionBeadAt("sess-tie-a", base.Add(2*time.Minute)),
	}

	result := ComputePoolDesiredStates(cfg, nil, sessions, map[string]int{"claude": 10})

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	reqs := result[0].Requests
	if len(reqs) != 2 {
		t.Fatalf("len(requests) = %d, want 2 after agent cap", len(reqs))
	}
	wantIDs := []string{"sess-oldest", "sess-tie-a"}
	for i, want := range wantIDs {
		if got := reqs[i].SessionBeadID; got != want {
			t.Fatalf("request[%d].SessionBeadID = %q, want %q; requests=%#v", i, got, want, reqs)
		}
	}
}

func TestComputePoolDesiredStates_InFlightDemandRecordsTrace(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	sessions := []beads.Bead{
		pendingPoolSessionBead("sess-1"),
		pendingPoolSessionBead("sess-2"),
	}
	trace := newPoolDesiredStateTestTrace("claude")

	result := computePoolDesiredStates(cfg, nil, sessions, map[string]int{"claude": 5}, trace)

	if len(result) != 1 || len(result[0].Requests) != 5 {
		t.Fatalf("result = %#v, want five desired requests", result)
	}
	if got := trace.decisionCounts[string(TraceSitePoolInFlightReuse)]; got != 1 {
		t.Fatalf("in-flight trace decisions = %d, want 1", got)
	}
	rec := poolTraceDecision(t, trace, TraceSitePoolInFlightReuse)
	for key, want := range map[string]int{
		"scale_check":   5,
		"in_flight":     2,
		"reused":        2,
		"anonymous_new": 3,
	} {
		if got := poolTraceFieldInt(t, rec.Fields, key); got != want {
			t.Fatalf("%s = %d, want %d", key, got, want)
		}
	}
}

func TestComputePoolDesiredStates_InFlightDemandRecordsTraceWhenCapsSuppressReuse(t *testing.T) {
	workspaceMax := 0
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
		Agents:    []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	sessions := []beads.Bead{
		pendingPoolSessionBead("sess-1"),
		pendingPoolSessionBead("sess-2"),
	}
	trace := newPoolDesiredStateTestTrace("claude")

	result := computePoolDesiredStates(cfg, nil, sessions, map[string]int{"claude": 5}, trace)

	if len(result) != 0 {
		t.Fatalf("result = %#v, want no desired requests when workspace cap is exhausted", result)
	}
	if got := trace.decisionCounts[string(TraceSitePoolInFlightReuse)]; got != 1 {
		t.Fatalf("in-flight trace decisions = %d, want 1", got)
	}
	rec := poolTraceDecision(t, trace, TraceSitePoolInFlightReuse)
	for key, want := range map[string]int{
		"scale_check":   5,
		"in_flight":     2,
		"reused":        0,
		"anonymous_new": 0,
	} {
		if got := poolTraceFieldInt(t, rec.Fields, key); got != want {
			t.Fatalf("%s = %d, want %d", key, got, want)
		}
	}
}

func TestApplyNestedCaps_DedupsConcreteSessionRequestsAcrossTiers(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	requests := []SessionRequest{
		{Template: "claude", Tier: "resume", SessionBeadID: "sess-1", BeadPriority: 10},
		{Template: "claude", Tier: "new", SessionBeadID: "sess-1"},
		{Template: "claude", Tier: "new", SessionBeadID: "sess-2"},
	}

	result := applyNestedCaps(cfg, requests, nil)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	reqs := result[0].Requests
	if len(reqs) != 2 {
		t.Fatalf("len(requests) = %d, want duplicate concrete session suppressed; requests=%#v", len(reqs), reqs)
	}
	seenSess1 := 0
	for _, req := range reqs {
		if req.SessionBeadID == "sess-1" {
			seenSess1++
		}
	}
	if seenSess1 != 1 {
		t.Fatalf("sess-1 accepted %d times, want once; requests=%#v", seenSess1, reqs)
	}
}

// Regression: poolDesired must be per-rig scoped. City-scoped agent sees
// only city work beads, rig-scoped agent sees only its rig's work beads.
func TestComputePoolDesiredStates_PerRigScoping(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			poolAgent("claude", "", intPtr(5), 0),      // city-scoped
			poolAgent("claude", "myrig", intPtr(5), 0), // rig-scoped
		},
	}
	// Work bead in rig scope, assigned to a session.
	work := []beads.Bead{
		workBead("w1", "myrig/claude", "sess-1", "in_progress", 5),
	}
	sessions := []beads.Bead{sessionBead("sess-1", "open")}

	result := ComputePoolDesiredStates(cfg, work, sessions, nil)

	counts := PoolDesiredCounts(result)
	if counts["claude"] != 0 {
		t.Errorf("city-scoped poolDesired = %d, want 0 (no city work)", counts["claude"])
	}
	if counts["myrig/claude"] != 1 {
		t.Errorf("rig-scoped poolDesired = %d, want 1 (resume for rig work)", counts["myrig/claude"])
	}
}

// TestResumeTier_AsleepSessionWithAssignedWork verifies that the resume tier
// fires for an asleep session bead that has in-progress work assigned to it.
// This is the exact scenario that caused the e2e failure: polecat claimed work,
// then went to asleep (e.g. city restart). The resume tier must generate a
// request pointing to the asleep bead so realizePoolDesiredSessions puts it
// back in desired state and prevents the orphan close from killing it.
func TestResumeTier_AsleepSessionWithAssignedWork(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			poolAgent("polecat", "hello-world", intPtr(5), 0),
		},
	}

	// Asleep session bead — polecat that ran, then was stopped (city restart).
	sessions := []beads.Bead{
		{ID: "mc-sctve", Status: "open", Type: "session", Metadata: map[string]string{
			"template": "hello-world/polecat", "session_name": "polecat-mc-sctve",
			"state": "asleep", "pool_managed": "true",
		}},
	}

	// Work bead assigned to the asleep polecat.
	work := []beads.Bead{
		workBead("hw-8lb", "hello-world/polecat", "mc-sctve", "in_progress", 2),
	}

	scaleCheck := map[string]int{"hello-world/polecat": 1}

	result := ComputePoolDesiredStates(cfg, work, sessions, scaleCheck)

	// Must have a resume request pointing to mc-sctve.
	var resumeFound bool
	for _, state := range result {
		for _, req := range state.Requests {
			if req.Tier == "resume" && req.SessionBeadID == "mc-sctve" {
				resumeFound = true
			}
		}
	}
	if !resumeFound {
		// Dump what we got for debugging.
		for _, state := range result {
			for i, req := range state.Requests {
				t.Logf("request[%d] tier=%s sessionBeadID=%s workBeadID=%s", i, req.Tier, req.SessionBeadID, req.WorkBeadID)
			}
		}
		t.Fatal("resume tier must fire for asleep session with assigned work")
	}
}

// Regression: routed-but-unassigned queue work must not directly create pool
// demand here. New worker creation comes from scale_check/work_query.
func TestComputePoolDesiredStates_RoutedButUnassignedDoesNotSpawnNew(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "claude", "", "open", 5),
	}

	result := ComputePoolDesiredStates(cfg, work, nil, nil)

	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 0 {
		t.Fatalf("total requests = %d, want 0", total)
	}
}

// Regression: same as above but for a rig-scoped agent.
func TestComputePoolDesiredStates_RoutedRigScopedDoesNotSpawnNew(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "myrig", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "myrig/claude", "", "open", 3),
	}

	result := ComputePoolDesiredStates(cfg, work, nil, nil)

	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 0 {
		t.Fatalf("total requests = %d, want 0", total)
	}
}
