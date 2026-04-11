package main

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type listFailStore struct {
	beads.Store
}

func (s listFailStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("list failed")
}

func TestCollectAssignedWorkBeads_IncludesReadyOpenAssignedHandoff(t *testing.T) {
	store := beads.NewMemStore()
	handoff, err := store.Create(beads.Bead{
		Title:    "merge me",
		Type:     "task",
		Status:   "open",
		Assignee: "repo/refinery",
	})
	if err != nil {
		t.Fatalf("create handoff bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "queued pool work",
		Type:   "task",
		Status: "open",
	}); err != nil {
		t.Fatalf("create queued bead: %v", err)
	}

	got, _ := collectAssignedWorkBeads(&config.City{}, store, nil, nil)
	if len(got) != 1 {
		t.Fatalf("collectAssignedWorkBeads returned %d beads, want 1: %#v", len(got), got)
	}
	if got[0].ID != handoff.ID {
		t.Fatalf("collectAssignedWorkBeads returned %q, want %q", got[0].ID, handoff.ID)
	}
	if got[0].Assignee != "repo/refinery" || got[0].Status != "open" {
		t.Fatalf("assigned handoff bead = assignee %q status %q, want repo/refinery open", got[0].Assignee, got[0].Status)
	}
}

func TestCollectAssignedWorkBeads_ExcludesBlockedOpenAssignedHandoff(t *testing.T) {
	store := beads.NewMemStore()
	blocker, err := store.Create(beads.Bead{
		Title:  "blocker",
		Type:   "task",
		Status: "open",
	})
	if err != nil {
		t.Fatalf("create blocker bead: %v", err)
	}
	handoff, err := store.Create(beads.Bead{
		Title:    "merge me later",
		Type:     "task",
		Status:   "open",
		Assignee: "repo/refinery",
	})
	if err != nil {
		t.Fatalf("create handoff bead: %v", err)
	}
	if err := store.DepAdd(handoff.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("add blocking dep: %v", err)
	}

	got, _ := collectAssignedWorkBeads(&config.City{}, store, nil, nil)
	if len(got) != 0 {
		t.Fatalf("collectAssignedWorkBeads returned %d beads, want 0: %#v", len(got), got)
	}
}

func TestCollectAssignedWorkBeads_ExcludesRoutedToMetadataWithoutAssignee(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:    "check alpha",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "seth"},
	}); err != nil {
		t.Fatalf("create routed bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "unrouted work",
		Type:   "task",
		Status: "open",
	}); err != nil {
		t.Fatalf("create unrouted bead: %v", err)
	}
	got, _ := collectAssignedWorkBeads(&config.City{}, store, nil, nil)
	if len(got) != 0 {
		t.Fatalf("collectAssignedWorkBeads returned %d beads, want 0", len(got))
	}
}

func TestCollectAssignedWorkBeads_ExcludesSessionBeads(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	// Session bead with assignee — should be excluded.
	if _, err := store.Create(beads.Bead{
		Title:    "worker session",
		Type:     sessionBeadType,
		Status:   "open",
		Assignee: "worker-1",
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	// Message bead with assignee — excluded from Ready() (messages are
	// delivered via nudge, not the ready/dispatch loop).
	if _, err := store.Create(beads.Bead{
		Title:    "you have mail",
		Type:     "message",
		Status:   "open",
		Assignee: "worker-1",
	}); err != nil {
		t.Fatalf("create message bead: %v", err)
	}
	// Real task bead with assignee — should be included (in_progress path).
	task, err := store.Create(beads.Bead{
		Title:    "do the thing",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "worker-1",
	})
	if err != nil {
		t.Fatalf("create task bead: %v", err)
	}
	got, _ := collectAssignedWorkBeads(&config.City{}, store, nil, nil)
	if len(got) != 1 {
		t.Fatalf("collectAssignedWorkBeads returned %d beads, want 1 (task only): %#v", len(got), got)
	}
	if got[0].ID != task.ID {
		t.Fatalf("expected task %q, got %q", task.ID, got[0].ID)
	}
}

func TestBuildDesiredState_RoutedQueueDoesNotCreateOneSessionPerBead(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	for i := 0; i < 12; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  "queued claude work",
			Type:   "task",
			Status: "open",
			Metadata: map[string]string{
				"gc.routed_to": "claude",
			},
		}); err != nil {
			t.Fatalf("create queued bead %d: %v", i, err)
		}
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "claude",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(20),
			ScaleCheck:        "printf 1",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if len(dsResult.AssignedWorkBeads) != 0 {
		t.Fatalf("AssignedWorkBeads = %d, want 0 for routed-only queue", len(dsResult.AssignedWorkBeads))
	}

	claudeSessions := 0
	for _, tp := range dsResult.State {
		if tp.TemplateName == "claude" {
			claudeSessions++
		}
	}
	if claudeSessions != 1 {
		t.Fatalf("claude desired sessions = %d, want 1 (scale_check only)", claudeSessions)
	}
}

func TestBuildDesiredState_OnDemandNamedSession_RoutedMetadataAloneDoesNotMaterialize(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "queued mayor work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "mayor",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	for _, tp := range dsResult.State {
		if tp.TemplateName == "mayor" {
			t.Fatalf("routed metadata alone should not materialize on-demand named session: %+v", tp)
		}
	}
}

func TestBuildDesiredState_OnDemandNamedSession_DirectAssigneeMaterializes(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:    "assigned mayor work",
		Type:     "task",
		Status:   "open",
		Assignee: "mayor",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	found := false
	for _, tp := range dsResult.State {
		if tp.TemplateName == "mayor" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("direct assignee should materialize on-demand named session")
	}
}

func TestBuildDesiredState_AlwaysNamedSession_MaterializesWithoutWorkBeads(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "always",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	found := false
	for _, tp := range dsResult.State {
		if tp.TemplateName == "mayor" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("always-mode named session should materialize without work beads")
	}
}

func TestBuildDesiredState_OnDemandNamedSession_InProgressAssigneeMaterializes(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	// Create an in-progress bead assigned to the named session.
	b, err := store.Create(beads.Bead{
		Title:    "in-progress mayor work",
		Type:     "task",
		Status:   "open",
		Assignee: "mayor",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Transition to in_progress.
	inProgress := "in_progress"
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	found := false
	for _, tp := range dsResult.State {
		if tp.TemplateName == "mayor" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("in-progress assignee should materialize on-demand named session")
	}
}

func TestBuildDesiredState_OnDemandNamedSession_AssigneeDemandSignalsPoolDesired(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:    "assigned mayor work",
		Type:     "task",
		Status:   "open",
		Assignee: "mayor",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if !dsResult.NamedSessionDemand["mayor"] {
		t.Fatal("NamedSessionDemand should include 'mayor' when assignee-only demand exists")
	}
}

func TestMergeNamedSessionDemand_NilPoolDesiredNoPanic(t *testing.T) {
	// PoolDesiredCounts returns nil when there are no pool states. Verify
	// that mergeNamedSessionDemand handles this without panic.
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}
	demand := map[string]bool{"mayor": true}
	// Should not panic — callers now ensure poolDesired is non-nil,
	// but verify the function itself handles nil gracefully.
	poolDesired := make(map[string]int)
	mergeNamedSessionDemand(poolDesired, demand, cfg)
	if poolDesired["mayor"] != 1 {
		t.Fatalf("poolDesired[mayor] = %d, want 1", poolDesired["mayor"])
	}
}

func TestBuildDesiredState_OnDemandNamedSession_ScaleCheckMaterializes(t *testing.T) {
	// When a named-session agent has an explicit scale_check that returns
	// demand > 0, the session should materialize even without assigned work
	// or work_query results. This tests the fix for #508.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "dog",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "echo 2",
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dog",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	found := false
	for _, tp := range dsResult.State {
		if tp.TemplateName == "dog" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("on-demand named session with scale_check > 0 should materialize")
	}
	if !dsResult.NamedSessionDemand["dog"] {
		t.Fatal("NamedSessionDemand should include 'dog' when scale_check returns demand")
	}
	if dsResult.ScaleCheckCounts["dog"] != 2 {
		t.Fatalf("ScaleCheckCounts[dog] = %d, want 2", dsResult.ScaleCheckCounts["dog"])
	}
}

func TestBuildDesiredState_OnDemandNamedSession_ScaleCheckZeroDoesNotMaterialize(t *testing.T) {
	// When scale_check returns 0 and work_query returns nothing, the
	// on-demand named session should NOT materialize.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "dog",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "echo 0",
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dog",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	for _, tp := range dsResult.State {
		if tp.TemplateName == "dog" {
			t.Fatalf("scale_check=0 should not materialize on-demand named session: %+v", tp)
		}
	}
	if dsResult.ScaleCheckCounts["dog"] != 0 {
		t.Fatalf("ScaleCheckCounts[dog] = %d, want 0", dsResult.ScaleCheckCounts["dog"])
	}
}

func TestBuildDesiredState_OnDemandNamedSession_NoExplicitScaleCheckUsesDemandQuery(t *testing.T) {
	// Without an explicit ScaleCheck, the named-session path calls
	// EffectiveDemandQuery() — the same unified probe the pool path uses.
	// In this test env bd is not available, so the default demand shell
	// returns 0 and no session materializes. WorkQuery is intentionally
	// IGNORED by the demand path after #573 (the old fallback via
	// EffectiveWorkQuery silently missed molecule beads because tier-3
	// `bd ready` excludes them by policy). This test guards that the
	// single-signal semantics hold: no explicit scale_check and no bd →
	// no materialization.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			// WorkQuery is set to a value that WOULD look like ready work
			// under the old EffectiveWorkQuery fallback. After #573, the
			// demand path ignores WorkQuery entirely, so this must NOT
			// materialize the session.
			WorkQuery: `echo '["ready"]'`,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	for _, tp := range dsResult.State {
		if tp.TemplateName == "mayor" {
			t.Fatalf("WorkQuery must not drive demand after #573 — on-demand named session should not materialize: %+v", tp)
		}
	}
	// Positive assertions that the demand path ran and produced 0 via
	// EffectiveDemandQuery, NOT via the removed EffectiveWorkQuery
	// fallback. If the old fallback were still wired up, the test's
	// WorkQuery = `echo '["ready"]'` would have set NamedSessionDemand
	// to true and the loop above would have caught the phantom session.
	// The fact that both of these are zero/false is the positive proof
	// that WorkQuery is no longer consulted for demand.
	if dsResult.NamedSessionDemand["mayor"] {
		t.Fatal("NamedSessionDemand should NOT include 'mayor' — custom WorkQuery is ignored as a demand signal after #573")
	}
	if dsResult.ScaleCheckCounts["mayor"] != 0 {
		t.Fatalf("ScaleCheckCounts[mayor] = %d, want 0 (default demand probe runs bd which is unavailable in test env → 0)", dsResult.ScaleCheckCounts["mayor"])
	}
}

func TestBuildDesiredState_OnDemandNamedSession_ScaleCheckDoesNotCreatePoolSessions(t *testing.T) {
	// A named-session agent with scale_check should only create a named
	// session, not pool-managed sessions. Verifies no pool contamination.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "dog",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "echo 3",
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dog",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	// Should have exactly one session (the named session), not 3 pool instances.
	dogCount := 0
	for _, tp := range dsResult.State {
		if tp.TemplateName == "dog" {
			dogCount++
		}
	}
	if dogCount != 1 {
		t.Fatalf("expected 1 named session for dog, got %d (pool contamination?)", dogCount)
	}
}

func TestBuildDesiredState_OnDemandNamedSession_ScaleCheckErrorReturnsNoDemand(t *testing.T) {
	// When the demand probe fails (non-zero exit, dolt down, bd crash,
	// timeout), demand is treated as 0 and the session does NOT
	// materialize. This matches the pool path's behavior on the same
	// failure mode and makes the two paths consistent.
	//
	// Before #573 this test asserted that WorkQuery was consulted as a
	// defense-in-depth fallback, but that "defense" WAS the bug: the two
	// signals could diverge (and did — #528 made EffectiveScaleCheck
	// molecule-aware while EffectiveWorkQuery stayed unaware). Unifying
	// under EffectiveDemandQuery removes the divergence at the cost of
	// this fallback branch. The error is logged to stderr so operators
	// can observe probe failures.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "dog",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "exit 1",
			// WorkQuery is set to what LOOKS like positive demand under
			// the old fallback shape. The test verifies it is NOT
			// consulted after #573.
			WorkQuery: `echo '["ready"]'`,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dog",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	for _, tp := range dsResult.State {
		if tp.TemplateName == "dog" {
			t.Fatalf("demand probe error should NOT materialize on-demand named session (WorkQuery fallback removed by #573): %+v", tp)
		}
	}
	if dsResult.NamedSessionDemand["dog"] {
		t.Fatal("NamedSessionDemand should NOT include 'dog' when demand probe fails (single-signal semantics)")
	}
	if dsResult.ScaleCheckCounts["dog"] != 0 {
		t.Fatalf("ScaleCheckCounts[dog] = %d, want 0 (error → zero demand)", dsResult.ScaleCheckCounts["dog"])
	}
}

func TestBuildDesiredState_OnDemandNamedSession_ScaleCheckNonIntegerReturnsNoDemand(t *testing.T) {
	// When the demand probe produces non-integer output (e.g. an operator's
	// custom scale_check accidentally prints "ready" instead of a count),
	// the parse error is logged and demand is treated as 0. The session
	// does NOT materialize.
	//
	// Before #573 this test asserted a WorkQuery fallback after parse
	// error. That fallback was removed as part of unifying pool and
	// named-session demand under EffectiveDemandQuery (see #573 and the
	// ScaleCheckErrorReturnsNoDemand test above for the full rationale).
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "dog",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        `echo "ready"`,
			WorkQuery:         `echo '["ready"]'`,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "dog",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	for _, tp := range dsResult.State {
		if tp.TemplateName == "dog" {
			t.Fatalf("non-integer demand probe output should NOT materialize on-demand named session (WorkQuery fallback removed by #573): %+v", tp)
		}
	}
	if dsResult.NamedSessionDemand["dog"] {
		t.Fatal("NamedSessionDemand should NOT include 'dog' when demand probe parse fails")
	}
	if dsResult.ScaleCheckCounts["dog"] != 0 {
		t.Fatalf("ScaleCheckCounts[dog] = %d, want 0 (parse error should not produce demand)", dsResult.ScaleCheckCounts["dog"])
	}
}

func TestBuildDesiredState_SingletonTemplateDoesNotRealizeDependencyPoolFloorWithoutSession(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:      "api",
				DependsOn: []string{"db"},
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	desired := dsResult.State
	dbSlots := 0
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			dbSlots++
		}
	}
	if dbSlots != 0 {
		t.Fatalf("db desired slots = %d, want 0 without a realized dependent session", dbSlots)
	}
}

func TestBuildDesiredState_DoesNotRealizeDependencyFloorForZeroScaledDependentPool(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
				DependsOn: []string{"db"},
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	desired := dsResult.State
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			t.Fatalf("unexpected dependency-only db slot for zero-scaled dependent pool: %+v", tp)
		}
	}
}

func TestBuildDesiredState_DoesNotRealizeDependencyFloorForSuspendedDependent(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:      "api",
				Suspended: true,
				DependsOn: []string{"db"},
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	desired := dsResult.State
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			t.Fatalf("unexpected dependency-only db slot for suspended dependent: %+v", tp)
		}
	}
}

func TestBuildDesiredState_SingletonTemplatesDoNotRealizeTransitiveDependencyPoolFloorWithoutSession(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
				DependsOn: []string{"db"},
			},
			{
				Name:      "web",
				DependsOn: []string{"api"},
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	desired := dsResult.State
	apiSlots := 0
	dbSlots := 0
	for _, tp := range desired {
		switch tp.TemplateName {
		case "api":
			apiSlots++
		case "db":
			dbSlots++
		}
	}
	if apiSlots != 0 {
		t.Fatalf("api desired slots = %d, want 0 without a realized root session", apiSlots)
	}
	if dbSlots != 0 {
		t.Fatalf("db desired slots = %d, want 0 without a realized root session", dbSlots)
	}
}

func TestBuildDesiredState_DiscoveredSessionRootGetsDependencyPoolFloor(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"template":     "helper",
			"session_name": "s-gc-100",
			"state":        "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "helper",
				Suspended:         true,
				MaxActiveSessions: intPtr(1),
				DependsOn:         []string{"db"},
				StartCommand:      "echo",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if _, ok := desired["s-gc-100"]; !ok {
		t.Fatalf("expected discovered helper session in desired state, got keys %v", desired)
	}
	dbSlots := 0
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			dbSlots++
		}
	}
	if dbSlots != 1 {
		t.Fatalf("db desired slots = %d, want 1", dbSlots)
	}
}

func TestBuildDesiredState_ManualZeroScaledPoolSessionStaysDesiredAndKeepsDependencyFloor(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "debug api",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:api"},
		Metadata: map[string]string{
			"template":       "api",
			"session_name":   "s-gc-200",
			"state":          "creating",
			"manual_session": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				DependsOn:         []string{"db"},
				StartCommand:      "echo",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if _, ok := desired["s-gc-200"]; !ok {
		t.Fatalf("expected manual pool session in desired state, got keys %v", desired)
	}
	dbSlots := 0
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			dbSlots++
		}
	}
	if dbSlots != 1 {
		t.Fatalf("db desired slots = %d, want 1", dbSlots)
	}
}

func TestBuildDesiredState_ManualImplicitPoolSessionsStayDesired(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "prompts", "worker.md"), []byte("worker prompt"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	for _, bead := range []beads.Bead{
		{
			Title:  "helper",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "template:helper"},
			Metadata: map[string]string{
				"template":             "helper",
				"session_name":         "s-mc-4wq",
				"state":                "creating",
				"manual_session":       "true",
				"pending_create_claim": "true",
			},
		},
		{
			Title:  "hal",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "template:helper"},
			Metadata: map[string]string{
				"template":             "helper",
				"session_name":         "s-mc-bmr",
				"alias":                "hal",
				"state":                "suspended",
				"manual_session":       "true",
				"pending_create_claim": "true",
			},
		},
	} {
		if _, err := store.Create(bead); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.City{
		Workspace: config.Workspace{
			Name:     "my-city",
			Provider: "claude",
		},
		Providers: map[string]config.ProviderSpec{
			"claude": {
				Command:    "echo",
				PromptMode: "arg",
			},
		},
		Agents: []config.Agent{
			{
				Name:           "mayor",
				PromptTemplate: "prompts/mayor.md",
			},
			{
				Name:           "helper",
				PromptTemplate: "prompts/worker.md",
			},
		},
	}

	dsResult := buildDesiredState("my-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	for _, sn := range []string{"s-mc-4wq", "s-mc-bmr"} {
		tp, ok := desired[sn]
		if !ok {
			t.Fatalf("expected manual helper session %q in desired state, got keys %v", sn, mapKeys(desired))
		}
		if tp.TemplateName != "helper" {
			t.Fatalf("desired[%q].TemplateName = %q, want helper", sn, tp.TemplateName)
		}
		if !tp.ManualSession {
			t.Fatalf("desired[%q].ManualSession = false, want true", sn)
		}
	}
	if got := desired["s-mc-bmr"].Alias; got != "hal" {
		t.Fatalf("desired[s-mc-bmr].Alias = %q, want hal", got)
	}
}

func TestBuildDesiredState_DrainedPoolManagedSessionIsNotRediscovered(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:claude"},
		Metadata: map[string]string{
			"template":     "claude",
			"agent_name":   "claude",
			"session_name": "s-gc-drained",
			"state":        "asleep",
			"sleep_reason": "drained",
			"pool_managed": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "claude",
			MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5),
		}},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State

	if _, ok := desired["s-gc-drained"]; ok {
		t.Fatalf("drained pool-managed session should not be rediscovered into desired state")
	}

	claudeSessions := 0
	for _, tp := range desired {
		if tp.TemplateName == "claude" {
			claudeSessions++
		}
	}
	if claudeSessions != 1 {
		t.Fatalf("claude desired sessions = %d, want 1", claudeSessions)
	}
}

func TestBuildDesiredState_UsesBeadNamedPoolSessionsForScaleCheckDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title: "queued worker job",
		Metadata: map[string]string{
			"gc.routed_to": "worker",
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Demand is supplied by the explicit scale_check here. This test only
	// verifies that pool sessions created under demand use bead-derived names
	// and pool-managed metadata, not that routed work itself increments demand.
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "echo 1",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if len(desired) != 1 {
		t.Fatalf("desired sessions = %d, want 1", len(desired))
	}

	var (
		sessionName string
		tp          TemplateParams
	)
	for sn, got := range desired {
		sessionName = sn
		tp = got
	}
	if tp.TemplateName != "worker" {
		t.Fatalf("TemplateName = %q, want worker", tp.TemplateName)
	}
	if !strings.HasPrefix(sessionName, "worker-") {
		t.Fatalf("session name = %q, want worker-<beadID>", sessionName)
	}
	if strings.HasSuffix(sessionName, "-1") {
		t.Fatalf("session name = %q, want bead-derived name instead of slot alias", sessionName)
	}

	sessionBeads, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(%q): %v", sessionBeadLabel, err)
	}
	if len(sessionBeads) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(sessionBeads))
	}
	if got := sessionBeads[0].Metadata["session_name"]; got != sessionName {
		t.Fatalf("stored session_name = %q, want %q", got, sessionName)
	}
	if got := sessionBeads[0].Metadata[poolManagedMetadataKey]; got != "true" {
		t.Fatalf("pool_managed = %q, want true", got)
	}
}

func TestBuildDesiredState_FallsBackToLegacyPoolDemandWhenListFails(t *testing.T) {
	cityPath := t.TempDir()
	memStore := beads.NewMemStore()
	store := listFailStore{Store: memStore}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1),
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	// With min=1, max=1: both the singleton path and the pool-floor path
	// may contribute a session, yielding 1 or 2 desired entries depending
	// on timing. Accept either.
	if len(desired) < 1 || len(desired) > 2 {
		t.Fatalf("desired sessions = %d, want 1 or 2", len(desired))
	}
	// At least one session should have a worker-prefixed name.
	found := false
	for sn := range desired {
		if strings.HasPrefix(sn, "worker") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no worker-prefixed session in desired: %v", desired)
	}
}

func TestBuildDesiredState_DependencyFloorDoesNotReuseRegularPoolWorkerBead(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker active",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"template":             "worker",
			"session_name":         "worker-existing",
			"agent_name":           "worker",
			"state":                "active",
			"pool_slot":            "1",
			poolManagedMetadataKey: "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"template":     "helper",
			"session_name": "helper-session",
			"state":        "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
			},
			{
				Name:         "helper",
				Suspended:    true,
				DependsOn:    []string{"worker"},
				StartCommand: "echo",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if _, ok := desired["worker-existing"]; ok {
		t.Fatalf("dependency floor reused regular worker bead: keys=%v", mapKeys(desired))
	}
	workerSessions := 0
	for sn, tp := range desired {
		if tp.TemplateName != "worker" {
			continue
		}
		workerSessions++
		if sn == "worker-existing" {
			t.Fatalf("dependency floor kept regular worker bead %q desired", sn)
		}
	}
	if workerSessions != 1 {
		t.Fatalf("worker desired sessions = %d, want 1; desired keys=%v", workerSessions, mapKeys(desired))
	}
}

func TestBuildDesiredState_DoesNotCreateDuplicatePoolBeadForDiscoveredSession(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":             "worker",
			"session_name":         "worker-gc-existing",
			"manual_session":       "true",
			poolManagedMetadataKey: "true",
			"state":                "creating",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if _, ok := desired["worker-gc-existing"]; !ok {
		t.Fatalf("desired state missing discovered pool session: keys=%v", mapKeys(desired))
	}

	sessionBeads, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(%q): %v", sessionBeadLabel, err)
	}
	if len(sessionBeads) != 1 {
		t.Fatalf("session bead count = %d, want 1 (no duplicate bead)", len(sessionBeads))
	}
}

func TestBuildDesiredState_ZeroScaledPoolSessionKeepsDependencyFloorWhileDraining(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "api-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:api"},
		Metadata: map[string]string{
			"template":     "api",
			"session_name": "api-1",
			"agent_name":   "api-1",
			"state":        "active",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "db",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				DependsOn:         []string{"db"},
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if _, ok := desired["api-1"]; ok {
		t.Fatalf("did not expect zero-scaled pool bead to re-enter desired state: %+v", desired["api-1"])
	}
	dbSlots := 0
	for _, tp := range desired {
		if tp.TemplateName == "db" {
			dbSlots++
		}
	}
	if dbSlots != 1 {
		t.Fatalf("db desired slots = %d, want 1", dbSlots)
	}
}

func TestBuildDesiredState_PoolCheckInjectsDoltPortForRigScopedAgent(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// The check command outputs "2" only when BEADS_DOLT_SERVER_PORT is set.
	// If the fix works, buildDesiredState prefixes the command with
	// BEADS_DOLT_SERVER_PORT=9876, so the inner shell sees the variable.
	checkCmd := `sh -c 'test -n "$BEADS_DOLT_SERVER_PORT" && printf 2 || printf 0'`
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:     "myrig",
			Path:     rigPath,
			DoltPort: "9876",
		}},
		Agents: []config.Agent{
			{
				Name:              "worker",
				Dir:               "myrig",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: checkCmd,
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	workerSlots := 0
	for _, tp := range desired.State {
		if tp.TemplateName == "myrig/worker" {
			workerSlots++
		}
	}
	if workerSlots != 2 {
		t.Fatalf("worker desired slots = %d, want 2 (BEADS_DOLT_SERVER_PORT injection should make check output 2)", workerSlots)
	}
}

func TestBuildDesiredState_PoolCheckOmitsDoltPortForCityScopedAgent(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	// Same check command but for a city-scoped agent (no rig). BEADS_DOLT_SERVER_PORT
	// should NOT be injected, so the check outputs 0.
	checkCmd := `sh -c 'test -n "$BEADS_DOLT_SERVER_PORT" && printf 2 || printf 0'`
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "worker",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: checkCmd,
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	workerSlots := 0
	for _, tp := range desired.State {
		if tp.TemplateName == "worker" {
			workerSlots++
		}
	}
	if workerSlots != 0 {
		t.Fatalf("worker desired slots = %d, want 0 (no DoltPort for city-scoped agent)", workerSlots)
	}
}

func TestBuildDesiredState_PoolCheckUsesManagedCityDoltPortWhenRigHasNoOverride(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	defer func() {
		if err := ln.Close(); err != nil {
			t.Fatalf("close listener: %v", err)
		}
	}()
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	checkCmd := `sh -c 'test -n "$BEADS_DOLT_SERVER_PORT" && printf 2 || printf 0'`
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name: "myrig",
			Path: rigPath,
		}},
		Agents: []config.Agent{
			{
				Name:              "worker",
				Dir:               "myrig",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: checkCmd,
			},
		},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	workerSlots := 0
	for _, tp := range desired.State {
		if tp.TemplateName == "myrig/worker" {
			workerSlots++
		}
	}
	if workerSlots != 2 {
		t.Fatalf("worker desired slots = %d, want 2 (managed city dolt port should be injected for rig)", workerSlots)
	}
}

func TestBuildDesiredState_ManualPoolSessionInSuspendedRigStaysStopped(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "payments")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "debug api",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:payments/api"},
		Metadata: map[string]string{
			"template":       "payments/api",
			"session_name":   "s-gc-300",
			"state":          "creating",
			"manual_session": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:      "payments",
			Path:      rigPath,
			Suspended: true,
		}},
		Agents: []config.Agent{
			{
				Name:              "db",
				Dir:               "payments",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
			{
				Name:              "api",
				Dir:               "payments",
				DependsOn:         []string{"payments/db"},
				StartCommand:      "echo",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0",
			},
		},
	}

	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	desired := dsResult.State
	if _, ok := desired["s-gc-300"]; ok {
		t.Fatalf("manual pool session in suspended rig should not enter desired state: %+v", desired["s-gc-300"])
	}
	for _, tp := range desired {
		if tp.TemplateName == "payments/db" {
			t.Fatalf("suspended-rig manual session should not hold dependency floor: %+v", tp)
		}
	}
}

func TestSelectOrCreatePoolSessionBead_SkipsDrained(t *testing.T) {
	store := beads.NewMemStore()
	drained, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "claude",
			"agent_name":   "claude",
			"session_name": "claude-drained",
			"state":        "drained",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.add(drained)
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "claude", nil, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID == drained.ID {
		t.Fatal("should not reuse drained session bead for new-tier request")
	}
}

func TestSelectOrCreatePoolSessionBead_ReusesPreferredDrained(t *testing.T) {
	store := beads.NewMemStore()
	drained, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "claude",
			"agent_name":   "claude",
			"session_name": "claude-drained",
			"state":        "drained",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.add(drained)
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "claude", &drained, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != drained.ID {
		t.Fatal("resume tier should reuse preferred drained session bead")
	}
}

func TestSelectOrCreateDependencyPoolSessionBead_SkipsDrained(t *testing.T) {
	store := beads.NewMemStore()
	drained, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":        "claude",
			"agent_name":      "claude",
			"session_name":    "claude-dep-drained",
			"state":           "asleep",
			"sleep_reason":    "drained",
			"dependency_only": "true",
			"pool_managed":    "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.add(drained)
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreateDependencyPoolSessionBead(bp, &cfgAgent, "claude")
	if err != nil {
		t.Fatalf("selectOrCreateDependencyPoolSessionBead: %v", err)
	}
	if result.ID == drained.ID {
		t.Fatal("should not reuse drained dependency session bead for generic dependency demand")
	}
}

func TestSelectOrCreatePoolSessionBead_ReusesAvailableForNewTier(t *testing.T) {
	store := beads.NewMemStore()
	// Existing awake session bead without assigned work — should be reused
	// for new-tier to prevent session bead duplication across ticks.
	awake, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "claude",
			"agent_name":   "claude",
			"session_name": "claude-awake",
			"state":        "awake",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &sessionBeadSnapshot{}
	snapshot.add(awake)
	cfgAgent := config.Agent{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "claude", nil, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != awake.ID {
		t.Fatal("new-tier should reuse available (non-drained) session bead")
	}
}

func TestSelectOrCreatePoolSessionBead_SkipsAsleepBeads(t *testing.T) {
	// An asleep pool session should NOT be reused for new demand.
	// The reconciler should create a fresh session instead.
	// This prevents a deadlock where an asleep bead fills a pool slot
	// but ComputeAwakeSet correctly refuses to wake it (asleep
	// ephemerals are not reused).
	store := beads.NewMemStore()
	cfgAgent := config.Agent{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}

	asleep, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-mc-old",
			"state":        "asleep",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{asleep})
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "polecat", nil, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID == asleep.ID {
		t.Fatal("asleep pool session should not be reused — a fresh session should be created instead")
	}
}

func TestSelectOrCreatePoolSessionBead_ReusesActiveBeforeCreatingNew(t *testing.T) {
	// An active (awake) pool session IS reused — no fresh bead created.
	store := beads.NewMemStore()
	cfgAgent := config.Agent{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}

	active, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-mc-live",
			"state":        "active",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{active})
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "polecat", nil, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != active.ID {
		t.Fatalf("active pool session should be reused, got %s want %s", result.ID, active.ID)
	}
}

func TestSelectOrCreatePoolSessionBead_ReusesCreatingBeforeCreatingNew(t *testing.T) {
	// A creating pool session IS reused — no fresh bead created.
	store := beads.NewMemStore()
	cfgAgent := config.Agent{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}

	creating, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-mc-new",
			"state":        "creating",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{creating})
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "polecat", nil, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID != creating.ID {
		t.Fatalf("creating pool session should be reused, got %s want %s", result.ID, creating.ID)
	}
}

func TestSelectOrCreatePoolSessionBead_SkipsAsleepButReusesActive(t *testing.T) {
	// With both an asleep and active bead for the same template,
	// the active one is reused and the asleep one is ignored.
	store := beads.NewMemStore()
	cfgAgent := config.Agent{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}

	asleep, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-mc-old",
			"state":        "asleep",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-mc-live",
			"state":        "active",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{asleep, active})
	bp := &agentBuildParams{
		beadStore:    store,
		sessionBeads: snapshot,
		agents:       []config.Agent{cfgAgent},
	}

	result, err := selectOrCreatePoolSessionBead(bp, "polecat", nil, map[string]bool{})
	if err != nil {
		t.Fatalf("selectOrCreatePoolSessionBead: %v", err)
	}
	if result.ID == asleep.ID {
		t.Fatal("should skip asleep bead")
	}
	if result.ID != active.ID {
		t.Fatalf("should reuse active bead, got %s want %s", result.ID, active.ID)
	}
}

// PR #216 — skipped for now. Cross-rig pool work visibility is a new
// feature, not a bug fix. Left as open PR for discussion about the
// gastown experience with this flow.
