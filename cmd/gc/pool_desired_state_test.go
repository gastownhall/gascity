package main

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

func intPtr(n int) *int { return &n }

// TestNestedCapUsageRejectionTyped verifies that the retyped rejection producer
// returns the typed site/reason constants for each cap kind, and that the
// underlying string values are byte-identical to the pre-S26b literals.
func TestNestedCapUsageRejectionTyped(t *testing.T) {
	cases := []struct {
		name       string
		cfg        *config.City
		wantSite   TraceSiteCode
		wantReason TraceReasonCode
		wantSiteS  string
		wantRsnS   string
	}{
		{
			name:       "agent_cap",
			cfg:        &config.City{Agents: []config.Agent{poolAgent("claude", "rig", intPtr(1), 0)}},
			wantSite:   TraceSitePoolAgentCap,
			wantReason: TraceReasonAgentCap,
			wantSiteS:  "reconciler.pool.agent_cap",
			wantRsnS:   "agent_cap",
		},
		{
			name: "rig_cap",
			cfg: &config.City{
				Rigs:   []config.Rig{{Name: "rig", Path: "/tmp/rig", MaxActiveSessions: intPtr(1)}},
				Agents: []config.Agent{poolAgent("claude", "rig", intPtr(5), 0)},
			},
			wantSite:   TraceSitePoolRigCap,
			wantReason: TraceReasonRigCap,
			wantSiteS:  "reconciler.pool.rig_cap",
			wantRsnS:   "rig_cap",
		},
		{
			name: "workspace_cap",
			cfg: &config.City{
				Workspace: config.Workspace{MaxActiveSessions: intPtr(1)},
				Agents:    []config.Agent{poolAgent("claude", "", intPtr(5), 0)},
			},
			wantSite:   TraceSitePoolWorkspaceCap,
			wantReason: TraceReasonWorkspaceCap,
			wantSiteS:  "reconciler.pool.workspace_cap",
			wantRsnS:   "workspace_cap",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limits := newNestedCapLimits(tc.cfg)
			usage := newNestedCapUsage()
			template := tc.cfg.Agents[0].QualifiedName()
			// Fill to the cap so the next request is rejected.
			usage.accept(SessionRequest{Template: template, Tier: "new"}, limits)

			site, reason, _, rejected := usage.rejection(SessionRequest{Template: template, Tier: "new"}, limits)
			if !rejected {
				t.Fatalf("expected rejection at cap")
			}
			if site != tc.wantSite {
				t.Errorf("site = %q, want %q", site, tc.wantSite)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if string(site) != tc.wantSiteS {
				t.Errorf("string(site) = %q, want legacy literal %q", string(site), tc.wantSiteS)
			}
			if string(reason) != tc.wantRsnS {
				t.Errorf("string(reason) = %q, want legacy literal %q", string(reason), tc.wantRsnS)
			}
		})
	}
}

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

func protectedPoolSessionBeadAt(id string, createdAt time.Time) beads.Bead {
	session := poolSessionBeadWithState(id, "awake", "")
	session.CreatedAt = createdAt
	session.Metadata["state_reason"] = "creation_complete"
	session.Metadata["creation_complete_at"] = createdAt.UTC().Format(time.RFC3339)
	return session
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

func poolTraceFieldStrings(t *testing.T, fields map[string]any, key string) []string {
	t.Helper()
	got, ok := fields[key].([]string)
	if !ok {
		t.Fatalf("trace field %s = %#v, want []string", key, fields[key])
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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), scaleCheck)

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

func TestComputePoolDesiredStates_ResumeResolvesAssigneeByAlias(t *testing.T) {
	// Regression: a polecat session bead has its human-readable alias in
	// Metadata["alias"], and the work bead's assignee is typically the
	// alias. The resume-tier lookup must resolve alias→session so the
	// active session stays alive — otherwise the pool sees the work as
	// unowned and spawns a second polecat for the same bead.
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(2), 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude", "nux", "in_progress", 5),
	}
	sessions := []beads.Bead{{
		ID:     "sess-vi6hhp",
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "polecat-sess-vi6hhp",
			"alias":                "nux",
			"template":             "rig/claude",
			poolManagedMetadataKey: boolMetadata(true),
		},
	}}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	reqs := result[0].Requests
	if len(reqs) != 1 {
		t.Fatalf("len(requests) = %d, want 1 resume for alias-assigned work", len(reqs))
	}
	if reqs[0].Tier != "resume" {
		t.Errorf("tier = %q, want resume", reqs[0].Tier)
	}
	if reqs[0].SessionBeadID != "sess-vi6hhp" {
		t.Errorf("SessionBeadID = %q, want sess-vi6hhp", reqs[0].SessionBeadID)
	}
}

func TestComputePoolDesiredStates_ResumeUsesLegacyWorkflowRunTarget(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			poolAgent("claude", "rig", intPtr(2), 0),
			poolAgent("reviewer", "rig", intPtr(2), 0),
		},
	}
	work := []beads.Bead{{
		ID:       "legacy-workflow-root",
		Status:   "in_progress",
		Assignee: "sess-1",
		Priority: intPtr(5),
		Metadata: map[string]string{
			"gc.kind":       "workflow",
			"gc.run_target": "rig/claude",
		},
	}}
	sessions := []beads.Bead{{
		ID:     "sess-1",
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			poolManagedMetadataKey: boolMetadata(true),
		},
	}}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	reqs := result[0].Requests
	if len(reqs) != 1 {
		t.Fatalf("len(requests) = %d, want 1 resume request", len(reqs))
	}
	if reqs[0].Tier != "resume" || reqs[0].SessionBeadID != "sess-1" {
		t.Fatalf("request = %+v, want resume for sess-1", reqs[0])
	}
}

func TestComputePoolDesiredStates_ResumesInstanceSuffixedRouteTarget(t *testing.T) {
	// A writer that stamps gc.routed_to with a live instance suffix directly
	// (e.g. `bd update --set-metadata gc.routed_to=rig/claude-1`), bypassing gc
	// sling's write-side normalization (agentutil.NormalizePoolRouteTarget,
	// applied in doSling), must not strand the resume-tier match: the raw
	// instance name has to resolve back to the base template before comparing
	// against `template`, or the live session backing this work looks
	// unrouted and never resumes.
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(3), 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude-1", "sess-1", "in_progress", 5),
	}
	sessions := []beads.Bead{sessionBead("sess-1", "open")}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	reqs := result[0].Requests
	if len(reqs) != 1 {
		t.Fatalf("len(requests) = %d, want 1 resume for instance-suffixed route target", len(reqs))
	}
	if reqs[0].Tier != "resume" || reqs[0].SessionBeadID != "sess-1" {
		t.Fatalf("request = %+v, want resume for sess-1", reqs[0])
	}
}

func TestComputePoolDesiredStates_LeavesUnmatchedInstanceSuffixAlone(t *testing.T) {
	// Guard against over-normalization: an instance number outside the
	// agent's configured capacity is not a pool instance identity and must
	// not be rewritten or matched against the base template.
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(3), 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude-99", "sess-1", "in_progress", 5),
	}
	sessions := []beads.Bead{sessionBead("sess-1", "open")}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0 (out-of-range instance suffix must not resolve to the base template)", total)
	}
}

func TestComputePoolDesiredStates_ResumeResolvesAssigneeByAliasHistory(t *testing.T) {
	// Regression: alias rotation preserves prior aliases in alias_history.
	// Work assigned under a prior alias must still resolve to its owning
	// session.
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(2), 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude", "nux", "in_progress", 5),
	}
	sessions := []beads.Bead{{
		ID:     "sess-vi6hhp",
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "polecat-sess-vi6hhp",
			"alias":                "rictus",
			"alias_history":        "nux",
			"template":             "rig/claude",
			poolManagedMetadataKey: boolMetadata(true),
		},
	}}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

	reqs := result[0].Requests
	if len(reqs) != 1 || reqs[0].SessionBeadID != "sess-vi6hhp" {
		t.Fatalf("requests = %+v, want 1 resume for sess-vi6hhp", reqs)
	}
}

func TestComputePoolDesiredStates_ResumeResolvesPersistedBoundTemplate(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("implementation-worker", "gascity-packs", intPtr(8), 0)},
	}
	sessionName := "gc__implementation-worker-mc-xbvk5"
	work := []beads.Bead{
		workBead("gp-qx0o", "gascity-packs/gc.implementation-worker", sessionName, "in_progress", 5),
	}
	sessions := []beads.Bead{{
		ID:     "mc-xbvk5",
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":             "gascity-packs/gc.implementation-worker",
			"session_name":         sessionName,
			poolManagedMetadataKey: boolMetadata(true),
		},
	}}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if result[0].Template != "gascity-packs/implementation-worker" {
		t.Fatalf("result template = %q, want current canonical template", result[0].Template)
	}
	reqs := result[0].Requests
	if len(reqs) != 1 || reqs[0].Tier != "resume" || reqs[0].SessionBeadID != "mc-xbvk5" {
		t.Fatalf("requests = %+v, want one resume for mc-xbvk5", reqs)
	}
}

// TestComputePoolDesiredStates_WakeKnownIdentityResolvesPersistedBoundAssignee
// covers the wake-known-identity tier one migration class over from the
// resume tier: in-progress work assigned to the pool identity itself under
// the legacy bound form, with no open session bead. The assignee/template
// comparison must use identity equivalence so the work still produces a wake
// request for the current canonical template instead of being treated as
// orphaned.
func TestComputePoolDesiredStates_WakeKnownIdentityResolvesPersistedBoundAssignee(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("implementation-worker", "gascity-packs", intPtr(8), 0)},
	}
	legacyIdentity := "gascity-packs/gc.implementation-worker"
	work := []beads.Bead{
		workBead("gp-qx1o", legacyIdentity, legacyIdentity, "in_progress", 5),
	}

	result := ComputePoolDesiredStates(cfg, work, nil, nil)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if result[0].Template != "gascity-packs/implementation-worker" {
		t.Fatalf("result template = %q, want current canonical template", result[0].Template)
	}
	reqs := result[0].Requests
	if len(reqs) != 1 || reqs[0].Tier != "wake-known-identity" || reqs[0].WorkBeadID != "gp-qx1o" {
		t.Fatalf("requests = %+v, want one wake-known-identity for gp-qx1o", reqs)
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

func TestComputePoolDesiredStates_TerminalProviderErrorSessionsDoNotBlockNewDemand(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(1), 0)},
	}
	work := []beads.Bead{
		workBead("w-stale", "claude", "sess-stale", "in_progress", 5),
	}
	stale := sessionBead("sess-stale", "open")
	stale.Type = sessionBeadType
	stale.Metadata = map[string]string{
		"template":                              "claude",
		"session_name":                          "claude-sess-stale",
		sessionHealthStateMetadataKey:           "unhealthy",
		sessionHealthReasonMetadataKey:          "model_not_found",
		sessionDrainableMetadataKey:             boolMetadata(true),
		sessionProviderTerminalErrorMetadataKey: "model_not_found",
	}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads([]beads.Bead{stale}), map[string]int{"claude": 1})

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	reqs := result[0].Requests
	if len(reqs) != 1 {
		t.Fatalf("len(requests) = %d, want 1 new request; got %#v", len(reqs), reqs)
	}
	if reqs[0].Tier != "new" || reqs[0].SessionBeadID != "" {
		t.Fatalf("request = %+v, want anonymous new demand replacing unhealthy stale owner", reqs[0])
	}
}

func TestComputePoolDesiredStates_TraceListsActiveCapacityBlockers(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(1), 0)},
	}
	work := []beads.Bead{
		workBead("w-active", "claude", "sess-active", "in_progress", 5),
	}
	sessions := []beads.Bead{sessionBead("sess-active", "open")}
	trace := newPoolDesiredStateTestTrace("claude")

	result := computePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), map[string]int{"claude": 1}, nil, trace)

	if len(result) != 1 || len(result[0].Requests) != 1 || result[0].Requests[0].Tier != "resume" {
		t.Fatalf("result = %#v, want only the active resume request under max_active_sessions=1", result)
	}
	if got := trace.decisionCounts[string(TraceSitePoolNewDemandCap)]; got != 1 {
		t.Fatalf("new-demand cap trace decisions = %d, want 1; records=%#v", got, trace.records)
	}
	rec := poolTraceDecision(t, trace, TraceSitePoolNewDemandCap)
	for key, want := range map[string]int{
		"scale_check":  1,
		"accepted_new": 0,
		"blocked_new":  1,
	} {
		if got := poolTraceFieldInt(t, rec.Fields, key); got != want {
			t.Fatalf("%s = %d, want %d", key, got, want)
		}
	}
	if got := poolTraceFieldStrings(t, rec.Fields, "blocking_sessions"); len(got) != 1 || got[0] != "sess-active" {
		t.Fatalf("blocking_sessions = %#v, want [sess-active]", got)
	}
	if got := poolTraceFieldStrings(t, rec.Fields, "blocking_work_beads"); len(got) != 1 || got[0] != "w-active" {
		t.Fatalf("blocking_work_beads = %#v, want [w-active]", got)
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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

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

func TestComputePoolDesiredStatesCarriesWorktreeOwnerEvidence(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(1), 0)},
	}
	spec := &worktree.Spec{BeadID: "gc-test", Owner: "gc-sling"}
	result := computePoolDesiredStates(
		cfg,
		nil,
		nil,
		map[string]int{"rig/claude": 1},
		map[string]scaleCheckDemand{
			"rig/claude": {
				WorkBeadIDs:    []string{"gc-test"},
				StoreRefs:      map[string]string{"gc-test": "rig:gascity"},
				WorktreeSpecs:  map[string]*worktree.Spec{"gc-test": spec},
				WorktreeErrors: map[string]string{"other": "ignored"},
			},
		},
		nil,
	)
	if len(result) != 1 || len(result[0].Requests) != 1 {
		t.Fatalf("result = %+v, want one new request", result)
	}
	request := result[0].Requests[0]
	if request.WorktreeSpec != spec || request.WorktreeError != "" {
		t.Fatalf("request owner evidence = spec %+v error %q, want exact spec and no error", request.WorktreeSpec, request.WorktreeError)
	}
}

func TestComputePoolDesiredStates_ManualSessionDoesNotConsumeSingletonNewDemand(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(1), 0)},
	}
	manual := beads.Bead{
		ID:     "manual-claude",
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:claude", "template:claude"},
		Metadata: map[string]string{
			"template":       "claude",
			"agent_name":     "claude",
			"session_name":   "s-manual-claude",
			"state":          "start-pending",
			"session_origin": "manual",
			"manual_session": "true",
		},
	}

	result := ComputePoolDesiredStates(cfg, nil, sessionInfosFromBeads([]beads.Bead{manual}), map[string]int{"claude": 1})

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	reqs := result[0].Requests
	if len(reqs) != 1 {
		t.Fatalf("len(requests) = %d, want 1 new pool request despite manual singleton session", len(reqs))
	}
	if reqs[0].Tier != "new" {
		t.Fatalf("request tier = %q, want new", reqs[0].Tier)
	}
	if reqs[0].SessionBeadID != "" {
		t.Fatalf("request SessionBeadID = %q, want anonymous new demand rather than reusing manual session", reqs[0].SessionBeadID)
	}
}

// TestComputePoolDesiredStates_NamedSessionBeadSkipsPoolResume verifies that
// when work is assigned to a configured named session, the pool path does NOT
// emit a resume request for the named session bead. Without this guard, the
// named-session bead leaks into realizePoolDesiredSessions, which renames it
// to a phantom "{name}-1" pool-instance form even when the agent has
// max_active_sessions=1 and SupportsInstanceExpansion()=false.
func TestComputePoolDesiredStates_NamedSessionBeadSkipsPoolResume(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("refinery", "rig", intPtr(1), 0)},
		NamedSessions: []config.NamedSession{
			{Template: "refinery", Scope: "rig", Mode: "on_demand"},
		},
	}
	// Work routed to the canonical named-session identity, with a
	// matching named-session bead present.
	work := []beads.Bead{
		workBead("w1", "rig/refinery", "rig/refinery", "in_progress", 5),
	}
	namedBead := beads.Bead{
		ID:     "sess-refinery",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"session_name":               "rig--refinery",
			"template":                   "rig/refinery",
			"agent_name":                 "rig/refinery",
			"state":                      "active",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "rig/refinery",
			namedSessionModeMetadata:     "on_demand",
		},
	}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads([]beads.Bead{namedBead}), nil)

	resumeCount := 0
	for _, ds := range result {
		for _, req := range ds.Requests {
			if req.Tier == "resume" {
				resumeCount++
			}
		}
	}
	if resumeCount != 0 {
		t.Errorf("resume count = %d, want 0 (named-session beads are materialized by the named-session loop, not pool resume)", resumeCount)
	}
}

// TestComputePoolDesiredStates_WakeKnownIdentitySkipsConfiguredNamedSession is
// the ga-p0u752 fix: the wake-known-identity tier (no live session bead
// resolves the assignee) must NOT treat a bare assignee as generic pool demand
// when that identity is structurally a configured [[named_session]] — even
// though the resume-tier guard (namedSessionBeadIDs, lines 194-196) never
// fires here because there is no session bead at all. Without this fix,
// isKnownPoolTemplate has zero awareness of cfg.NamedSessions and wakes a
// competing pool worker for a named session's own orphaned bare self-claim
// (ga-i1d0tr Candidate B).
func TestComputePoolDesiredStates_WakeKnownIdentitySkipsConfiguredNamedSession(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("builder", "gascity", intPtr(3), 0)},
		NamedSessions: []config.NamedSession{
			{Template: "builder", Dir: "gascity", Mode: "on_demand"},
		},
	}
	// No live session bead at all: the named session's canonical bead is
	// reaped/closed, but the bare-identity in-progress claim survives.
	work := []beads.Bead{
		workBead("w1", "gascity/builder", "gascity/builder", "in_progress", 5),
	}

	result := ComputePoolDesiredStates(cfg, work, nil, nil)

	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 0 {
		t.Errorf("total pool requests = %d, want 0 (bare identity belongs to a configured named session; pool must not compete for it)", total)
	}
}

// TestComputePoolDesiredStates_WakeKnownIdentityFiresForSuspendedNamedSession
// covers the suspension-awareness risk the architecture review flagged
// (ga-wfcfoe): a suspended named session must not be silently exempted from
// pool demand, or the bead would orphan with neither side picking it up.
// isConfiguredNamedSessionIdentity is unit-tested directly below
// (TestIsConfiguredNamedSessionIdentity) because computePoolDesiredStates'
// own outer loop already skips a suspended agent's entire per-template pass
// (line ~151), so this exact scenario can't be reconstructed end-to-end
// through ComputePoolDesiredStates for the overlapping bare-identity shape
// this bug family targets — suspending the shared agent suspends both tiers
// uniformly by design, which is correct (not orphaning), not a gap.
func TestIsConfiguredNamedSessionIdentity(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			poolAgent("builder", "gascity", intPtr(3), 0),
			{Name: "reviewer", Dir: "gascity", Suspended: true, MaxActiveSessions: intPtr(1)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "builder", Dir: "gascity", Mode: "on_demand"},
			{Template: "reviewer", Dir: "gascity", Mode: "on_demand"},
		},
	}

	tests := []struct {
		name     string
		assignee string
		want     bool
	}{
		{"matches active named session", "gascity/builder", true},
		{"matches named session with suspended backing agent", "gascity/reviewer", false},
		{"no matching named session", "gascity/other", false},
		{"empty assignee", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isConfiguredNamedSessionIdentity(cfg, tt.assignee); got != tt.want {
				t.Errorf("isConfiguredNamedSessionIdentity(%q) = %v, want %v", tt.assignee, got, tt.want)
			}
		})
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

	result := computePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), map[string]int{"claude": 10}, nil, trace)

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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), scaleCheck)

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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), scaleCheck)

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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), map[string]int{"claude": 5})

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

	result := ComputePoolDesiredStates(cfg, nil, sessionInfosFromBeads(sessions), scaleCheck)

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

	result := ComputePoolDesiredStates(cfg, nil, sessionInfosFromBeads(sessions), scaleCheck)

	counts := PoolDesiredCounts(result)
	if counts["claude"] != 0 {
		t.Fatalf("poolDesired[claude] = %d, want 0 when scale_check reports no new demand", counts["claude"])
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionRetainsFreshSessionsAboveScaleDemand(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	sessions := []beads.Bead{
		protectedPoolSessionBeadAt("sess-1", now.Add(-30*time.Second)),
		protectedPoolSessionBeadAt("sess-2", now.Add(-20*time.Second)),
	}

	result := ComputePoolDesiredStatesAt(cfg, nil, sessionInfosFromBeads(sessions), map[string]int{"claude": 1}, now)

	if len(result) != 1 || len(result[0].Requests) != 2 {
		t.Fatalf("result = %#v, want both fresh sessions retained while scale_check reports one", result)
	}
	for i, want := range []string{"sess-1", "sess-2"} {
		if got := result[0].Requests[i].SessionBeadID; got != want {
			t.Fatalf("request[%d].SessionBeadID = %q, want %q", i, got, want)
		}
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionRetainsFreshSessionsAtZeroScale(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	sessions := []beads.Bead{
		protectedPoolSessionBeadAt("sess-1", now.Add(-30*time.Second)),
		protectedPoolSessionBeadAt("sess-2", now.Add(-20*time.Second)),
	}

	result := ComputePoolDesiredStatesAt(cfg, nil, sessionInfosFromBeads(sessions), map[string]int{"claude": 0}, now)

	if len(result) != 1 || len(result[0].Requests) != 2 {
		t.Fatalf("result = %#v, want both fresh sessions retained at zero scale", result)
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionDoesNotRequireScaleEntry(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	tests := []struct {
		name   string
		counts map[string]int
	}{
		{name: "nil", counts: nil},
		{name: "empty", counts: map[string]int{}},
		{name: "missing template", counts: map[string]int{"other": 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fresh := protectedPoolSessionBeadAt("sess-fresh", now.Add(-30*time.Second))
			result := ComputePoolDesiredStatesAt(
				cfg,
				nil,
				sessionInfosFromBeads([]beads.Bead{fresh}),
				tt.counts,
				now,
			)
			if len(result) != 1 || len(result[0].Requests) != 1 ||
				result[0].Requests[0].SessionBeadID != fresh.ID {
				t.Fatalf("result = %#v, want fresh session retained without a scale entry", result)
			}

			pending := pendingPoolSessionBead("sess-pending")
			pendingResult := ComputePoolDesiredStatesAt(
				cfg,
				nil,
				sessionInfosFromBeads([]beads.Bead{pending}),
				tt.counts,
				now,
			)
			if got := PoolDesiredCounts(pendingResult)["claude"]; got != 0 {
				t.Fatalf("pending poolDesired[claude] = %d, want 0 without authoritative scale demand", got)
			}
		})
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionUsesFreshRecomputeTime(t *testing.T) {
	createTime := time.Date(2026, 7, 28, 20, 59, 0, 0, time.UTC)
	completeTime := createTime.Add(30 * time.Second)
	recomputeTime := completeTime.Add(15 * time.Second)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	session := pendingPoolSessionBeadAt("sess-1", createTime)
	initial := ComputePoolDesiredStatesAt(
		cfg,
		nil,
		sessionInfosFromBeads([]beads.Bead{session}),
		map[string]int{"claude": 1},
		createTime,
	)
	if len(initial) != 1 || len(initial[0].Requests) != 1 {
		t.Fatalf("initial result = %#v, want pending create to cover demand", initial)
	}

	session.Metadata["pending_create_claim"] = ""
	session.Metadata["state"] = "awake"
	session.Metadata["state_reason"] = "creation_complete"
	session.Metadata["creation_complete_at"] = completeTime.Format(time.RFC3339)
	recomputed := ComputePoolDesiredStatesAt(
		cfg,
		nil,
		sessionInfosFromBeads([]beads.Bead{session}),
		nil,
		recomputeTime,
	)
	if len(recomputed) != 1 || len(recomputed[0].Requests) != 1 ||
		recomputed[0].Requests[0].SessionBeadID != session.ID {
		t.Fatalf("recomputed result = %#v, want completion at T2 retained by fresh T3 decision", recomputed)
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionExpires(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	expired := protectedPoolSessionBeadAt("sess-expired", now.Add(-postCreateProtectionTimeout))

	result := ComputePoolDesiredStatesAt(cfg, nil, sessionInfosFromBeads([]beads.Bead{expired}), map[string]int{"claude": 0}, now)

	if got := PoolDesiredCounts(result)["claude"]; got != 0 {
		t.Fatalf("poolDesired[claude] = %d, want 0 at the protection boundary", got)
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionCoexistsWithResume(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(2), 0)},
	}
	resume := poolSessionBeadWithState("sess-resume", "awake", "")
	fresh := protectedPoolSessionBeadAt("sess-fresh", now.Add(-30*time.Second))
	work := []beads.Bead{
		workBead("work-1", "claude", resume.ID, "in_progress", 5),
	}

	result := ComputePoolDesiredStatesAt(cfg, work, sessionInfosFromBeads([]beads.Bead{resume, fresh}), map[string]int{"claude": 0}, now)

	if len(result) != 1 || len(result[0].Requests) != 2 {
		t.Fatalf("result = %#v, want one resume plus one protected fresh session", result)
	}
	if got := result[0].Requests[0]; got.Tier != "resume" || got.SessionBeadID != resume.ID {
		t.Fatalf("first request = %+v, want resume for %s", got, resume.ID)
	}
	if got := result[0].Requests[1]; got.Tier != "new" || got.SessionBeadID != fresh.ID {
		t.Fatalf("second request = %+v, want protected new request for %s", got, fresh.ID)
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionBindsWakeKnownCapacity(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	tests := []struct {
		name           string
		workCount      int
		protectedCount int
		scaleCount     int
		wantCount      int
		wantBound      int
	}{
		{name: "one wake known one protected", workCount: 1, protectedCount: 1, wantCount: 1, wantBound: 1},
		{name: "one wake known two protected", workCount: 1, protectedCount: 2, wantCount: 2, wantBound: 1},
		{name: "one wake known one protected plus scale demand", workCount: 1, protectedCount: 1, scaleCount: 1, wantCount: 2, wantBound: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var work []beads.Bead
			for i := 0; i < tt.workCount; i++ {
				work = append(work, workBead(
					"work-"+strconv.Itoa(i+1),
					"claude",
					"claude",
					"in_progress",
					tt.workCount-i,
				))
			}
			var sessions []beads.Bead
			protectedIDs := make(map[string]bool)
			for i := 0; i < tt.protectedCount; i++ {
				id := "sess-" + strconv.Itoa(i+1)
				sessions = append(sessions, protectedPoolSessionBeadAt(id, now.Add(time.Duration(-30-i)*time.Second)))
				protectedIDs[id] = true
			}

			result := ComputePoolDesiredStatesAt(
				cfg,
				work,
				sessionInfosFromBeads(sessions),
				map[string]int{"claude": tt.scaleCount},
				now,
			)

			if len(result) != 1 || len(result[0].Requests) != tt.wantCount {
				t.Fatalf("result = %#v, want %d total requests", result, tt.wantCount)
			}
			bound := 0
			wakeKnown := 0
			for _, request := range result[0].Requests {
				if request.Tier == "wake-known-identity" {
					wakeKnown++
					if protectedIDs[request.SessionBeadID] {
						bound++
					}
				}
			}
			if wakeKnown != 1 {
				t.Fatalf("wake-known requests = %d, want 1; result=%#v", wakeKnown, result)
			}
			if bound != tt.wantBound {
				t.Fatalf("protected-bound requests = %d, want %d; result=%#v", bound, tt.wantBound, result)
			}
		})
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionRespectsCapShrink(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(1), 0)},
	}
	sessions := []beads.Bead{
		protectedPoolSessionBeadAt("sess-oldest", now.Add(-30*time.Second)),
		protectedPoolSessionBeadAt("sess-newest", now.Add(-20*time.Second)),
	}

	result := ComputePoolDesiredStatesAt(cfg, nil, sessionInfosFromBeads(sessions), map[string]int{"claude": 0}, now)

	if len(result) != 1 || len(result[0].Requests) != 1 {
		t.Fatalf("result = %#v, want cap shrink to retain only one session", result)
	}
	if got := result[0].Requests[0].SessionBeadID; got != "sess-oldest" {
		t.Fatalf("SessionBeadID = %q, want stable oldest session", got)
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionDoesNotOvercreate(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	sessions := []beads.Bead{
		protectedPoolSessionBeadAt("sess-1", now.Add(-30*time.Second)),
		protectedPoolSessionBeadAt("sess-2", now.Add(-20*time.Second)),
	}

	result := ComputePoolDesiredStatesAt(cfg, nil, sessionInfosFromBeads(sessions), map[string]int{"claude": 2}, now)

	if len(result) != 1 || len(result[0].Requests) != 2 {
		t.Fatalf("result = %#v, want protected sessions to cover demand without anonymous creates", result)
	}
	for _, request := range result[0].Requests {
		if request.SessionBeadID == "" {
			t.Fatalf("anonymous request overcreated despite protected sessions covering demand: %#v", result)
		}
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionExcludesNonReusableSessions(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	tests := []struct {
		name   string
		mutate func(*beads.Bead)
	}{
		{
			name: "non-creation reason",
			mutate: func(session *beads.Bead) {
				session.Metadata["state_reason"] = "reconciler_heal"
			},
		},
		{
			name: "malformed marker",
			mutate: func(session *beads.Bead) {
				session.Metadata["creation_complete_at"] = "not-a-timestamp"
			},
		},
		{
			name: "future marker",
			mutate: func(session *beads.Bead) {
				session.Metadata["creation_complete_at"] = now.Add(time.Minute).Format(time.RFC3339)
			},
		},
		{
			name: "draining",
			mutate: func(session *beads.Bead) {
				session.Metadata["state"] = "draining"
			},
		},
		{
			name: "drained",
			mutate: func(session *beads.Bead) {
				session.Metadata["state"] = "drained"
			},
		},
		{
			name: "asleep",
			mutate: func(session *beads.Bead) {
				session.Metadata["state"] = "asleep"
			},
		},
		{
			name: "failed create",
			mutate: func(session *beads.Bead) {
				session.Metadata["state"] = "failed-create"
			},
		},
		{
			name: "provider terminal error",
			mutate: func(session *beads.Bead) {
				session.Metadata[sessionProviderTerminalErrorMetadataKey] = "model_not_found"
			},
		},
		{
			name: "dependency only",
			mutate: func(session *beads.Bead) {
				session.Metadata["dependency_only"] = "true"
			},
		},
		{
			name: "wait hold",
			mutate: func(session *beads.Bead) {
				session.Metadata["wait_hold"] = "true"
			},
		},
		{
			name: "active user hold",
			mutate: func(session *beads.Bead) {
				session.Metadata["held_until"] = now.Add(time.Minute).Format(time.RFC3339)
			},
		},
		{
			name: "active quarantine",
			mutate: func(session *beads.Bead) {
				session.Metadata["quarantined_until"] = now.Add(time.Minute).Format(time.RFC3339)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := protectedPoolSessionBeadAt("sess-1", now.Add(-30*time.Second))
			tt.mutate(&session)

			result := ComputePoolDesiredStatesAt(
				cfg,
				nil,
				sessionInfosFromBeads([]beads.Bead{session}),
				map[string]int{"claude": 0},
				now,
			)

			if got := PoolDesiredCounts(result)["claude"]; got != 0 {
				t.Fatalf("poolDesired[claude] = %d, want 0 for non-reusable session", got)
			}
		})
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionDependencyOnlyNeverCreatesDemandFloor(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	session := protectedPoolSessionBeadAt("sess-dependency", now.Add(-30*time.Second))
	session.Metadata["dependency_only"] = "true"

	for _, scaleCounts := range []map[string]int{
		nil,
		{},
		{"claude": 0},
	} {
		result := ComputePoolDesiredStatesAt(
			cfg,
			nil,
			sessionInfosFromBeads([]beads.Bead{session}),
			scaleCounts,
			now,
		)
		if got := PoolDesiredCounts(result)["claude"]; got != 0 {
			t.Fatalf("scale=%v poolDesired[claude] = %d, want 0 for dependency-only session", scaleCounts, got)
		}
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionDoesNotBindBlockedCapacity(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	tests := []struct {
		name   string
		mutate func(*beads.Bead)
	}{
		{
			name: "wait hold",
			mutate: func(session *beads.Bead) {
				session.Metadata["wait_hold"] = "true"
			},
		},
		{
			name: "active user hold",
			mutate: func(session *beads.Bead) {
				session.Metadata["held_until"] = now.Add(time.Minute).Format(time.RFC3339)
			},
		},
		{
			name: "active quarantine",
			mutate: func(session *beads.Bead) {
				session.Metadata["quarantined_until"] = now.Add(time.Minute).Format(time.RFC3339)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := protectedPoolSessionBeadAt("sess-blocked", now.Add(-30*time.Second))
			tt.mutate(&session)
			work := []beads.Bead{
				workBead("work-1", "claude", "claude", "in_progress", 5),
			}

			result := ComputePoolDesiredStatesAt(
				cfg,
				work,
				sessionInfosFromBeads([]beads.Bead{session}),
				map[string]int{"claude": 0},
				now,
			)

			if len(result) != 1 || len(result[0].Requests) != 1 {
				t.Fatalf("result = %#v, want one wake-known request", result)
			}
			request := result[0].Requests[0]
			if request.Tier != "wake-known-identity" || request.SessionBeadID != "" {
				t.Fatalf("request = %+v, want unbound wake-known request", request)
			}
		})
	}
}

func TestPoolSessionPostCreateProtectionSeparatesSweepRetentionFromDemandEligibility(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*beads.Bead)
	}{
		{
			name: "dependency only",
			mutate: func(session *beads.Bead) {
				session.Metadata["dependency_only"] = "true"
			},
		},
		{
			name: "wait hold",
			mutate: func(session *beads.Bead) {
				session.Metadata["wait_hold"] = "true"
			},
		},
		{
			name: "active user hold",
			mutate: func(session *beads.Bead) {
				session.Metadata["held_until"] = now.Add(time.Minute).Format(time.RFC3339)
			},
		},
		{
			name: "active quarantine",
			mutate: func(session *beads.Bead) {
				session.Metadata["quarantined_until"] = now.Add(time.Minute).Format(time.RFC3339)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := protectedPoolSessionBeadAt("sess-fresh", now.Add(-30*time.Second))
			tt.mutate(&session)
			info := sessionInfosFromBeads([]beads.Bead{session})[0]

			if !poolSessionWithinPostCreateProtection(info, now) {
				t.Fatal("fresh session lost sweep retention protection")
			}
			if poolSessionEligibleForProtectedDemand(info, now) {
				t.Fatal("blocked/dependency-only session became generic protected demand")
			}
		})
	}
}

func TestReusablePoolSessionInfosForRequestPreservesPromotionAndPendingCreate(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	dependency := protectedPoolSessionBeadAt("sess-dependency", now.Add(-30*time.Second))
	dependency.Metadata["dependency_only"] = "true"
	held := protectedPoolSessionBeadAt("sess-held", now.Add(-30*time.Second))
	held.Metadata["held_until"] = now.Add(time.Minute).Format(time.RFC3339)
	pendingHeld := pendingPoolSessionBeadAt("sess-pending-held", now.Add(-30*time.Second))
	pendingHeld.Metadata["wait_hold"] = "true"
	snapshot := newSessionBeadSnapshot([]beads.Bead{dependency, held, pendingHeld})
	bp := &agentBuildParams{
		city:         cfg,
		agents:       cfg.Agents,
		sessionBeads: snapshot,
	}

	candidates := reusablePoolSessionInfosForRequest(
		bp,
		&cfg.Agents[0],
		"claude",
		SessionRequest{Tier: "new"},
		now,
		nil,
	)
	got := make(map[string]bool, len(candidates))
	for _, info := range candidates {
		got[info.ID] = true
	}
	if !got[dependency.ID] {
		t.Fatal("dependency-only slot was not reusable for real generic demand promotion")
	}
	if !got[pendingHeld.ID] {
		t.Fatal("pending-create slot lost its in-flight completion semantics")
	}
	if got[held.ID] {
		t.Fatal("held active slot remained eligible for anonymous generic reuse")
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionReusesProtectedBeforePending(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	pending := pendingPoolSessionBeadAt("sess-pending", now.Add(-time.Minute))
	protected := protectedPoolSessionBeadAt("sess-protected", now.Add(-30*time.Second))

	result := ComputePoolDesiredStatesAt(
		cfg,
		nil,
		sessionInfosFromBeads([]beads.Bead{pending, protected}),
		map[string]int{"claude": 2},
		now,
	)

	if len(result) != 1 || len(result[0].Requests) != 2 {
		t.Fatalf("result = %#v, want protected and pending requests", result)
	}
	if got := result[0].Requests[0].SessionBeadID; got != protected.ID {
		t.Fatalf("first SessionBeadID = %q, want protected session %q", got, protected.ID)
	}
	if got := result[0].Requests[1].SessionBeadID; got != pending.ID {
		t.Fatalf("second SessionBeadID = %q, want pending session %q", got, pending.ID)
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionBindingPreservesScaleDemandIndex(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	protected := protectedPoolSessionBeadAt("sess-protected", now.Add(-30*time.Second))
	work := []beads.Bead{
		workBead("work-assigned", "claude", "claude", "in_progress", 5),
	}
	demand := map[string]scaleCheckDemand{
		"claude": {
			Count:       1,
			WorkBeadIDs: []string{"work-scale"},
		},
	}

	result := computePoolDesiredStatesAt(
		cfg,
		work,
		sessionInfosFromBeads([]beads.Bead{protected}),
		map[string]int{"claude": 1},
		demand,
		now,
		nil,
	)

	if len(result) != 1 || len(result[0].Requests) != 2 {
		t.Fatalf("result = %#v, want bound wake-known plus anonymous scale request", result)
	}
	if got := result[0].Requests[0]; got.Tier != "wake-known-identity" || got.SessionBeadID != protected.ID {
		t.Fatalf("first request = %+v, want wake-known bound to %q", got, protected.ID)
	}
	anonymous := result[0].Requests[1]
	if anonymous.Tier != "new" || anonymous.SessionBeadID != "" || anonymous.WorkBeadID != "work-scale" {
		t.Fatalf("second request = %+v, want anonymous new request indexed to work-scale", anonymous)
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionAdvancesDemandIndex(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	protected := protectedPoolSessionBeadAt("sess-protected", now.Add(-30*time.Second))
	demand := map[string]scaleCheckDemand{
		"claude": {
			Count:       2,
			WorkBeadIDs: []string{"work-covered", "work-anonymous"},
		},
	}

	result := computePoolDesiredStatesAt(
		cfg,
		nil,
		sessionInfosFromBeads([]beads.Bead{protected}),
		map[string]int{"claude": 2},
		demand,
		now,
		nil,
	)

	if len(result) != 1 || len(result[0].Requests) != 2 {
		t.Fatalf("result = %#v, want one protected and one anonymous request", result)
	}
	if got := result[0].Requests[0].SessionBeadID; got != protected.ID {
		t.Fatalf("first SessionBeadID = %q, want %q", got, protected.ID)
	}
	if got := result[0].Requests[0].WorkBeadID; got != "work-covered" {
		t.Fatalf("protected WorkBeadID = %q, want deterministic binding to work-covered", got)
	}
	anonymous := result[0].Requests[1]
	if anonymous.SessionBeadID != "" {
		t.Fatalf("second request SessionBeadID = %q, want anonymous", anonymous.SessionBeadID)
	}
	if anonymous.WorkBeadID != "work-anonymous" {
		t.Fatalf("anonymous WorkBeadID = %q, want demand index after protected coverage", anonymous.WorkBeadID)
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionAllocatesDemandByTriggerIdentity(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	protected := protectedPoolSessionBeadAt("sess-protected", now.Add(-30*time.Second))
	protected.Metadata[beadmeta.TriggerBeadIDMetadataKey] = "work-b"
	demand := map[string]scaleCheckDemand{
		"claude": {
			Count:       2,
			WorkBeadIDs: []string{"work-a", "work-b"},
			Titles:      map[string]string{"work-a": "A title", "work-b": "B title"},
			Packs:       map[string]string{"work-a": "pack-a", "work-b": "pack-b"},
			Workspaces:  map[string]string{"work-a": "workspace-a", "work-b": "workspace-b"},
			StoreRefs:   map[string]string{"work-a": "rig-a", "work-b": "rig-b"},
			ParentSIDs:  map[string]string{"work-a": "parent-a", "work-b": "parent-b"},
		},
	}

	result := computePoolDesiredStatesAt(
		cfg,
		nil,
		sessionInfosFromBeads([]beads.Bead{protected}),
		map[string]int{"claude": 2},
		demand,
		now,
		nil,
	)

	if len(result) != 1 || len(result[0].Requests) != 2 {
		t.Fatalf("result = %#v, want one protected and one anonymous request", result)
	}
	concrete := result[0].Requests[0]
	if concrete.SessionBeadID != protected.ID ||
		concrete.WorkBeadID != "work-b" ||
		concrete.WorkBeadTitle != "B title" ||
		concrete.WorkPack != "pack-b" ||
		concrete.WorkWorkspace != "workspace-b" ||
		concrete.WorkStoreRef != "rig-b" ||
		concrete.BrainParentSID != "parent-b" {
		t.Fatalf("concrete request = %+v, want protected session with full work-b provenance", concrete)
	}
	anonymous := result[0].Requests[1]
	if anonymous.SessionBeadID != "" || anonymous.WorkBeadID != "work-a" {
		t.Fatalf("anonymous request = %+v, want residual work-a", anonymous)
	}
}

func TestComputePoolDesiredStates_PostCreateProtectionRebindsUnmatchedConcreteDemand(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	tests := []struct {
		name      string
		triggerID string
	}{
		{name: "stale trigger", triggerID: "work-x"},
		{name: "blank trigger"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			protected := protectedPoolSessionBeadAt("sess-protected", now.Add(-30*time.Second))
			protected.Metadata[beadmeta.TriggerBeadIDMetadataKey] = tt.triggerID
			demand := map[string]scaleCheckDemand{
				"claude": {
					Count:       2,
					WorkBeadIDs: []string{"work-a", "work-b"},
					Titles:      map[string]string{"work-a": "A title", "work-b": "B title"},
					Packs:       map[string]string{"work-a": "pack-a", "work-b": "pack-b"},
					Workspaces:  map[string]string{"work-a": "workspace-a", "work-b": "workspace-b"},
					StoreRefs:   map[string]string{"work-a": "rig-a", "work-b": "rig-b"},
					ParentSIDs:  map[string]string{"work-a": "parent-a", "work-b": "parent-b"},
				},
			}

			result := computePoolDesiredStatesAt(
				cfg,
				nil,
				sessionInfosFromBeads([]beads.Bead{protected}),
				map[string]int{"claude": 2},
				demand,
				now,
				nil,
			)

			if len(result) != 1 || len(result[0].Requests) != 2 {
				t.Fatalf("result = %#v, want one rebound concrete and one anonymous request", result)
			}
			concrete := result[0].Requests[0]
			if concrete.SessionBeadID != protected.ID ||
				concrete.WorkBeadID != "work-a" ||
				concrete.WorkBeadTitle != "A title" ||
				concrete.WorkPack != "pack-a" ||
				concrete.WorkWorkspace != "workspace-a" ||
				concrete.WorkStoreRef != "rig-a" ||
				concrete.BrainParentSID != "parent-a" {
				t.Fatalf("rebound concrete request = %+v, want full work-a provenance", concrete)
			}
			anonymous := result[0].Requests[1]
			if anonymous.SessionBeadID != "" || anonymous.WorkBeadID != "work-b" {
				t.Fatalf("anonymous request = %+v, want residual work-b", anonymous)
			}
		})
	}
}

func TestPoolSessionWithinPostCreateProtectionBoundaries(t *testing.T) {
	now := time.Date(2026, 7, 28, 21, 0, 0, 123456789, time.UTC)
	tests := []struct {
		name       string
		completed  time.Time
		wantWithin bool
	}{
		{name: "age zero", completed: now, wantWithin: true},
		{name: "one nanosecond future", completed: now.Add(time.Nanosecond), wantWithin: false},
		{name: "exact timeout", completed: now.Add(-postCreateProtectionTimeout), wantWithin: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := sessionInfosFromBeads([]beads.Bead{
				protectedPoolSessionBeadAt("sess-boundary", now),
			})[0]
			info.CreationCompleteAt = tt.completed.Format(time.RFC3339Nano)

			if got := poolSessionWithinPostCreateProtection(info, now); got != tt.wantWithin {
				t.Fatalf("poolSessionWithinPostCreateProtection = %v, want %v (completed=%s now=%s)",
					got, tt.wantWithin, tt.completed, now)
			}
		})
	}
}

func TestBuildDesiredState_PostCreateProtectionUsesInjectedDecisionTime(t *testing.T) {
	beaconTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	decisionTime := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "claude",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		}},
	}
	work := workBead("work-1", "claude", "", "open", 0)
	sessions := []beads.Bead{
		protectedPoolSessionBeadAt("sess-1", decisionTime.Add(-30*time.Second)),
		protectedPoolSessionBeadAt("sess-2", decisionTime.Add(-20*time.Second)),
	}
	seed := append([]beads.Bead{work}, sessions...)
	store := beads.NewMemStoreFrom(1, seed, nil)
	snapshot := newSessionBeadSnapshot(sessions)

	result := buildDesiredStateWithSessionBeadsAt(
		"test-city",
		t.TempDir(),
		beaconTime,
		decisionTime,
		cfg,
		runtime.NewFake(),
		store,
		nil,
		snapshot,
		nil,
		io.Discard,
	)

	if got := result.ScaleCheckCounts["claude"]; got != 1 {
		t.Fatalf("ScaleCheckCounts[claude] = %d, want 1", got)
	}
	if !result.BeaconTime.Equal(beaconTime) {
		t.Fatalf("BeaconTime = %s, want %s", result.BeaconTime, beaconTime)
	}
	for _, session := range sessions {
		name := session.Metadata["session_name"]
		if _, ok := result.BaseState[name]; !ok {
			t.Fatalf("fresh session %q absent from base desired state: keys=%v", name, mapKeys(result.BaseState))
		}
	}
}

func TestBuildDesiredState_PostCreateProtectionExpiresAgainstDecisionTimeNotBeacon(t *testing.T) {
	beaconTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	decisionTime := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	creationTime := decisionTime.Add(-postCreateProtectionTimeout)
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "claude",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		}},
	}
	work := workBead("work-1", "claude", "", "open", 0)
	sessions := []beads.Bead{
		protectedPoolSessionBeadAt("sess-1", creationTime),
		protectedPoolSessionBeadAt("sess-2", creationTime.Add(-time.Second)),
	}
	seed := append([]beads.Bead{work}, sessions...)
	store := beads.NewMemStoreFrom(1, seed, nil)

	result := buildDesiredStateWithSessionBeadsAt(
		"test-city",
		t.TempDir(),
		beaconTime,
		decisionTime,
		cfg,
		runtime.NewFake(),
		store,
		nil,
		newSessionBeadSnapshot(sessions),
		nil,
		io.Discard,
	)

	workerCount := 0
	for _, params := range result.BaseState {
		if params.TemplateName == "claude" {
			workerCount++
		}
	}
	if workerCount != 1 {
		t.Fatalf("worker desired count = %d, want 1 after grace expiry; scale=%v beacon=%s creation=%s decision=%s keys=%v",
			workerCount, result.ScaleCheckCounts, beaconTime, creationTime, decisionTime, mapKeys(result.BaseState))
	}
}

func TestBuildDesiredState_PostCreateProtectionPreservesPersistedProvenance(t *testing.T) {
	beaconTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	decisionTime := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "claude",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		}},
	}
	protected := protectedPoolSessionBeadAt("sess-protected", decisionTime.Add(-30*time.Second))
	protected.Metadata["agent_name"] = "claude-1"
	protected.Metadata["alias"] = "claude-1"
	protected.Metadata["pool_slot"] = "1"
	protected.Metadata[beadmeta.TriggerBeadIDMetadataKey] = "work-x"
	protected.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = "rig-x"
	protected.Metadata[beadmeta.BrainParentSIDMetadataKey] = "parent-x"
	protected.Metadata[beadmeta.PackMetadataKey] = "pack-x"
	protected.Metadata[beadmeta.PackWorkspaceMetadataKey] = "workspace-x"
	protected.Metadata[beadmeta.WorkDirMetadataKey] = "/tmp/work-x"
	protected.Metadata[beadmeta.LegacyWorkDirMetadataKey] = "/tmp/work-x"
	protected.Labels = append(protected.Labels, "agent:claude-1")
	snapshot := newSessionBeadSnapshot([]beads.Bead{protected})
	store := beads.NewMemStoreFrom(1, []beads.Bead{protected}, nil)

	result := buildDesiredStateWithSessionBeadsAt(
		"test-city",
		t.TempDir(),
		beaconTime,
		decisionTime,
		cfg,
		runtime.NewFake(),
		store,
		nil,
		snapshot,
		nil,
		io.Discard,
	)

	if _, ok := result.BaseState[protected.Metadata["session_name"]]; !ok {
		t.Fatalf("protected session absent from base desired state: keys=%v", mapKeys(result.BaseState))
	}
	stored, err := store.Get(protected.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", protected.ID, err)
	}
	wantMetadata := map[string]string{
		beadmeta.TriggerBeadIDMetadataKey:       "work-x",
		beadmeta.TriggerBeadStoreRefMetadataKey: "rig-x",
		beadmeta.BrainParentSIDMetadataKey:      "parent-x",
		beadmeta.PackMetadataKey:                "pack-x",
		beadmeta.PackWorkspaceMetadataKey:       "workspace-x",
		beadmeta.WorkDirMetadataKey:             "/tmp/work-x",
		beadmeta.LegacyWorkDirMetadataKey:       "/tmp/work-x",
	}
	for key, want := range wantMetadata {
		if got := stored.Metadata[key]; got != want {
			t.Fatalf("persisted metadata[%s] = %q, want %q", key, got, want)
		}
	}
}

func TestBuildDesiredState_BlockedFreshSessionMaterializesRunnableReplacement(t *testing.T) {
	beaconTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	decisionTime := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		work beads.Bead
	}{
		{
			name: "scale demand",
			work: workBead("work-scale", "claude", "", "open", 5),
		},
		{
			name: "wake known demand",
			work: workBead("work-wake-known", "claude", "claude", "in_progress", 5),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.City{
				Agents: []config.Agent{{
					Name:              "claude",
					StartCommand:      "true",
					MinActiveSessions: intPtr(0),
					MaxActiveSessions: intPtr(2),
				}},
			}
			blocked := protectedPoolSessionBeadAt("sess-blocked", decisionTime.Add(-30*time.Second))
			blocked.Metadata["agent_name"] = "claude-1"
			blocked.Metadata["alias"] = "claude-1"
			blocked.Metadata["pool_slot"] = "1"
			blocked.Metadata["held_until"] = decisionTime.Add(time.Minute).Format(time.RFC3339)
			blocked.Labels = append(blocked.Labels, "agent:claude-1")
			snapshot := newSessionBeadSnapshot([]beads.Bead{blocked})
			store := beads.NewMemStoreFrom(1, []beads.Bead{tt.work, blocked}, nil)
			cityPath := t.TempDir()

			result := buildDesiredStateWithSessionBeadsAt(
				"test-city",
				cityPath,
				beaconTime,
				decisionTime,
				cfg,
				runtime.NewFake(),
				store,
				nil,
				snapshot,
				nil,
				io.Discard,
			)

			var replacement *sessionpkg.Info
			for _, info := range snapshot.OpenInfos() {
				if info.ID != blocked.ID {
					infoCopy := info
					replacement = &infoCopy
					break
				}
			}
			if replacement == nil {
				t.Fatalf("no replacement materialized; snapshot=%#v", snapshot.OpenInfos())
			}
			if replacement.SessionNameMetadata == blocked.Metadata["session_name"] {
				t.Fatalf("replacement reused blocked runtime name %q", replacement.SessionNameMetadata)
			}
			if !replacement.PendingCreateClaim {
				t.Fatalf("replacement %s is not pending create: %+v", replacement.ID, *replacement)
			}
			if replacement.PoolSlot != "2" {
				t.Fatalf("replacement pool_slot = %q, want next free slot 2 while live holder retains slot 1", replacement.PoolSlot)
			}
			if _, ok := result.BaseState[blocked.Metadata["session_name"]]; ok {
				t.Fatalf("blocked session %q leaked into base desired state", blocked.Metadata["session_name"])
			}
			if _, ok := result.BaseState[replacement.SessionNameMetadata]; !ok {
				t.Fatalf("replacement %q absent from base desired state: keys=%v",
					replacement.SessionNameMetadata, mapKeys(result.BaseState))
			}

			readyFlags := make([]bool, len(result.AssignedWorkBeads))
			for i, work := range result.AssignedWorkBeads {
				storeRef := ""
				if i < len(result.AssignedWorkStoreRefs) {
					storeRef = result.AssignedWorkStoreRefs[i]
				}
				readyFlags[i] = result.ReadyAssigned[storeScopedBeadKey{StoreRef: storeRef, ID: work.ID}]
			}
			openInfos := snapshot.OpenInfos()
			poolWork := filterAssignedWorkBeadsForPoolDemand(
				cfg,
				cityPath,
				store,
				openInfos,
				result.AssignedWorkBeads,
				result.AssignedWorkStoreRefs,
			)
			poolDesired := PoolDesiredCounts(ComputePoolDesiredStatesAt(
				cfg,
				poolWork,
				openInfos,
				result.ScaleCheckCounts,
				decisionTime,
			))
			if got := poolDesired["claude"]; got != 1 {
				t.Fatalf("recomputed poolDesired[claude] = %d, want 1", got)
			}
			awakeInput := buildAwakeInputFromReconciler(
				cfg,
				cityPath,
				snapshot.OpenInfos(),
				poolDesired,
				nil,
				nil,
				nil,
				nil,
				result.AssignedWorkBeads,
				readyFlags,
				nil,
				runtime.NewFake(),
				decisionTime,
			)
			awake := ComputeAwakeSet(awakeInput)
			if got := awake[blocked.Metadata["session_name"]]; got.ShouldWake {
				t.Fatalf("blocked session unexpectedly wakes: %+v", got)
			}
			if got := awake[replacement.SessionNameMetadata]; !got.ShouldWake {
				t.Fatalf("replacement is not runnable: %+v", got)
			}
		})
	}
}

func TestBuildDesiredState_RealDemandPromotesDependencyOnlySession(t *testing.T) {
	beaconTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	decisionTime := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "claude",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		}},
	}
	dependency := protectedPoolSessionBeadAt("sess-dependency", decisionTime.Add(-30*time.Second))
	dependency.Metadata["agent_name"] = "claude-1"
	dependency.Metadata["alias"] = "claude-1"
	dependency.Metadata["pool_slot"] = "1"
	dependency.Metadata["dependency_only"] = "true"
	dependency.Labels = append(dependency.Labels, "agent:claude-1")
	work := workBead("work-scale", "claude", "", "open", 5)
	snapshot := newSessionBeadSnapshot([]beads.Bead{dependency})
	store := beads.NewMemStoreFrom(1, []beads.Bead{work, dependency}, nil)

	result := buildDesiredStateWithSessionBeadsAt(
		"test-city",
		t.TempDir(),
		beaconTime,
		decisionTime,
		cfg,
		runtime.NewFake(),
		store,
		nil,
		snapshot,
		nil,
		io.Discard,
	)

	if got := result.ScaleCheckCounts["claude"]; got != 1 {
		t.Fatalf("ScaleCheckCounts[claude] = %d, want 1", got)
	}
	if infos := snapshot.OpenInfos(); len(infos) != 1 || infos[0].ID != dependency.ID {
		t.Fatalf("snapshot = %#v, want dependency-only bead reused without a second bead", infos)
	}
	tp, ok := result.BaseState[dependency.Metadata["session_name"]]
	if !ok {
		t.Fatalf("dependency-only session %q absent from base desired state: keys=%v",
			dependency.Metadata["session_name"], mapKeys(result.BaseState))
	}
	if tp.DependencyOnly {
		t.Fatal("real scale demand did not promote the reused dependency-only session")
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

	result := ComputePoolDesiredStates(cfg, nil, sessionInfosFromBeads(sessions), scaleCheck)

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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), scaleCheck)

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

func TestComputePoolDesiredStates_DoesNotResumeSessionAcrossExplicitRouteMismatch(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			poolAgent("codex-max", "", intPtr(10), 0),
			poolAgent("codex-min", "", intPtr(10), 0),
		},
	}
	session := beads.Bead{
		ID:     "mc-codex-max",
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:codex-max"},
		Metadata: map[string]string{
			"template":     "codex-max",
			"session_name": "workflows__codex-max-mc-codex-max",
			"state":        "asleep",
		},
	}
	work := []beads.Bead{
		workBead("w-mismatched-route", "codex-min", "workflows__codex-max-mc-codex-max", "in_progress", 5),
	}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads([]beads.Bead{session}), nil)

	for _, state := range result {
		for _, req := range state.Requests {
			if req.SessionBeadID == session.ID {
				t.Fatalf("mismatched routed work produced resume request under %q: %+v", state.Template, req)
			}
		}
	}
}

func TestComputePoolDesiredStates_DoesNotResumeLegacySessionAcrossExplicitRouteMismatch(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			poolAgent("codex-max", "", intPtr(10), 0),
			poolAgent("codex-min", "", intPtr(10), 0),
		},
	}
	session := beads.Bead{
		ID:     "mc-codex-max",
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"agent_name":   "codex-max-1",
			"session_name": "workflows__codex-max-mc-codex-max",
			"state":        "asleep",
		},
	}
	work := []beads.Bead{
		workBead("w-mismatched-route", "codex-min", "workflows__codex-max-mc-codex-max", "in_progress", 5),
	}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads([]beads.Bead{session}), nil)

	for _, state := range result {
		for _, req := range state.Requests {
			if req.SessionBeadID == session.ID {
				t.Fatalf("legacy mismatched routed work produced resume request under %q: %+v", state.Template, req)
			}
		}
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
			result := ComputePoolDesiredStates(cfg, nil, sessionInfosFromBeads([]beads.Bead{tt.session}), map[string]int{"claude": 1})

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

	result := ComputePoolDesiredStates(cfg, nil, sessionInfosFromBeads([]beads.Bead{stale}), map[string]int{"claude": 1})

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

	result := ComputePoolDesiredStates(cfg, nil, sessionInfosFromBeads(sessions), map[string]int{"claude": 10})

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

	result := computePoolDesiredStates(cfg, nil, sessionInfosFromBeads(sessions), map[string]int{"claude": 5}, nil, trace)

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

	result := computePoolDesiredStates(cfg, nil, sessionInfosFromBeads(sessions), map[string]int{"claude": 5}, nil, trace)

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

	result := applyNestedCaps(cfg, requests, nil, nil)

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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), scaleCheck)

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

// TestCanonicalSingletonAliasHeldTemplates_ExcludesFailedCreateHolder is the
// regression guard for the failed-create over-suppression hang found during the
// gc-7e40y fix review (opencode+Fugu Ultra). A failed-create bead RELEASES its
// alias (failedCreateIdentityReleased, names.go), so it must NOT count as a live
// holder -- otherwise pool demand is suppressed while the canonical alias is
// actually free, hanging routed work for the template.
func TestCanonicalSingletonAliasHeldTemplates_ExcludesFailedCreateHolder(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("mayor", "", intPtr(1), 0)}, // canonical singleton
	}
	holder := func(state string) beads.Bead {
		return beads.Bead{
			ID:     "sess-" + state,
			Status: "open",
			Type:   sessionBeadType,
			Metadata: map[string]string{
				"session_name":   "mayor",
				"template":       "mayor",
				"alias":          "mayor",
				"session_origin": "named",
				"state":          state,
			},
		}
	}

	// A live named holder occupies the singleton's slot.
	if _, ok := canonicalSingletonAliasHeldTemplates(cfg, sessionInfosFromBeads([]beads.Bead{holder("active")}))["mayor"]; !ok {
		t.Fatalf("live named alias-holder should mark mayor held; got %v", canonicalSingletonAliasHeldTemplates(cfg, sessionInfosFromBeads([]beads.Bead{holder("active")})))
	}

	// A failed-create holder released the alias -> must NOT be treated as held.
	if _, ok := canonicalSingletonAliasHeldTemplates(cfg, sessionInfosFromBeads([]beads.Bead{holder("failed-create")}))["mayor"]; ok {
		t.Fatalf("failed-create holder released its alias and must NOT mark mayor held (over-suppression hang); got %v", canonicalSingletonAliasHeldTemplates(cfg, sessionInfosFromBeads([]beads.Bead{holder("failed-create")})))
	}

	// A closed holder no longer owns the alias.
	closed := holder("active")
	closed.Status = "closed"
	if _, ok := canonicalSingletonAliasHeldTemplates(cfg, sessionInfosFromBeads([]beads.Bead{closed}))["mayor"]; ok {
		t.Fatalf("closed holder released its alias and must NOT mark mayor held; got held")
	}

	// A pool-managed bead is the pool's own instance, not the named alias holder.
	poolManaged := holder("active")
	poolManaged.Metadata[poolManagedMetadataKey] = boolMetadata(true)
	if _, ok := canonicalSingletonAliasHeldTemplates(cfg, sessionInfosFromBeads([]beads.Bead{poolManaged}))["mayor"]; ok {
		t.Fatalf("pool-managed bead is not the named alias holder and must NOT mark mayor held; got held")
	}

	// A drained holder released its alias.
	if _, ok := canonicalSingletonAliasHeldTemplates(cfg, sessionInfosFromBeads([]beads.Bead{holder("drained")}))["mayor"]; ok {
		t.Fatalf("drained holder released its alias and must NOT mark mayor held; got held")
	}
}

// Rebinding reused capacity to a different bead must rebind its worktree
// evidence too. Carrying the previous bead's spec forward would hand the
// session a workspace verified for other work.
func TestRequestWithScaleDemandProvenanceRebindsWorktreeEvidence(t *testing.T) {
	stale := &worktree.Spec{BeadID: "gc-old", Path: "/tmp/old"}
	fresh := &worktree.Spec{BeadID: "gc-new", Path: "/tmp/new"}
	demand := scaleCheckDemand{
		WorktreeSpecs:  map[string]*worktree.Spec{"gc-new": fresh},
		WorktreeErrors: map[string]string{},
	}
	got := requestWithScaleDemandProvenance(SessionRequest{WorkBeadID: "gc-old", WorktreeSpec: stale}, demand, "gc-new")
	if got.WorktreeSpec != fresh {
		t.Errorf("WorktreeSpec = %+v, want the spec for gc-new", got.WorktreeSpec)
	}

	got = requestWithScaleDemandProvenance(SessionRequest{WorkBeadID: "gc-old", WorktreeSpec: stale}, demand, "gc-unknown")
	if got.WorktreeSpec != nil {
		t.Errorf("WorktreeSpec = %+v for a bead with no evidence, want nil", got.WorktreeSpec)
	}

	demand.WorktreeErrors["gc-bad"] = " conflicting evidence "
	got = requestWithScaleDemandProvenance(SessionRequest{WorkBeadID: "gc-old", WorktreeError: "stale"}, demand, "gc-bad")
	if got.WorktreeError != "conflicting evidence" {
		t.Errorf("WorktreeError = %q, want the trimmed error for gc-bad", got.WorktreeError)
	}
}

// Unusable worktree ownership evidence must be distinguishable from an
// ordinary bind failure: buildDesiredState skips the item on this error
// rather than continuing without trigger env.
func TestVerifiedPoolTriggerWorkDirMarksEvidenceFailures(t *testing.T) {
	req := SessionRequest{WorkBeadID: "gc-a", WorktreeError: "conflicting work dir metadata"}
	if _, err := verifiedPoolTriggerWorkDir(nil, nil, "rig/pool", req); !errors.Is(err, errPoolTriggerWorktreeEvidence) {
		t.Errorf("WorktreeError path err = %v, want errPoolTriggerWorktreeEvidence", err)
	}

	req = SessionRequest{WorkBeadID: "gc-a", WorktreeSpec: &worktree.Spec{BeadID: "gc-b"}}
	if _, err := verifiedPoolTriggerWorkDir(nil, nil, "rig/pool", req); !errors.Is(err, errPoolTriggerWorktreeEvidence) {
		t.Errorf("bead mismatch err = %v, want errPoolTriggerWorktreeEvidence", err)
	}
}

// A failed binding write on a managed-worktree request leaves the session
// bead stamped with the previous bead's work dir, so it is marked as an
// evidence failure and skipped rather than reused. An unmanaged request
// keeps the ordinary continue-without-trigger-env path.
func TestBindWriteFailureMarksManagedRequests(t *testing.T) {
	cause := errors.New("store write failed")

	managed := bindWriteFailure(SessionRequest{WorktreeSpec: &worktree.Spec{BeadID: "gc-a"}}, cause)
	if !errors.Is(managed, errPoolTriggerWorktreeEvidence) {
		t.Errorf("managed bind write failure = %v, want the evidence sentinel", managed)
	}
	if !errors.Is(managed, cause) {
		t.Errorf("managed bind write failure lost its cause: %v", managed)
	}

	unmanaged := bindWriteFailure(SessionRequest{}, cause)
	if errors.Is(unmanaged, errPoolTriggerWorktreeEvidence) {
		t.Errorf("unmanaged bind write failure = %v, want no evidence sentinel", unmanaged)
	}
	if !errors.Is(unmanaged, cause) {
		t.Errorf("unmanaged bind write failure lost its cause: %v", unmanaged)
	}
}

// Demand records the probe shorthand ("city") while the workspace provenance
// on the bead carries the canonical spelling ("city:<name>"). Those are one
// store, so the cross-store guard must not reject the pair; two different rigs
// still must not match.
func TestVerifiedPoolTriggerWorkDirAcceptsCanonicalStoreRefSpelling(t *testing.T) {
	const mismatch = "does not match request store"

	req := SessionRequest{
		WorkBeadID:   "gc-a",
		WorkStoreRef: "city",
		WorktreeSpec: &worktree.Spec{BeadID: "gc-a", StoreRef: "city:test-city"},
	}
	_, err := verifiedPoolTriggerWorkDir(nil, nil, "rig/pool", req)
	if err != nil && strings.Contains(err.Error(), mismatch) {
		t.Errorf("canonical city spelling rejected as a store mismatch: %v", err)
	}

	req.WorkStoreRef = "alpha"
	req.WorktreeSpec = &worktree.Spec{BeadID: "gc-a", StoreRef: "rig:alpha"}
	_, err = verifiedPoolTriggerWorkDir(nil, nil, "rig/pool", req)
	if err != nil && strings.Contains(err.Error(), mismatch) {
		t.Errorf("bare rig shorthand rejected as a store mismatch: %v", err)
	}

	req.WorkStoreRef = "alpha"
	req.WorktreeSpec = &worktree.Spec{BeadID: "gc-a", StoreRef: "rig:beta"}
	_, err = verifiedPoolTriggerWorkDir(nil, nil, "rig/pool", req)
	if err == nil || !strings.Contains(err.Error(), mismatch) {
		t.Errorf("alpha vs rig:beta err = %v, want a store mismatch", err)
	}

	req.WorkStoreRef = "rig:a"
	req.WorktreeSpec = &worktree.Spec{BeadID: "gc-a", StoreRef: "rig:b"}
	_, err = verifiedPoolTriggerWorkDir(nil, nil, "rig/pool", req)
	if err == nil || !strings.Contains(err.Error(), mismatch) {
		t.Errorf("rig:a vs rig:b err = %v, want a store mismatch", err)
	}
}
