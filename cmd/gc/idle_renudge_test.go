package main

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// makeAliveEphemeralPoolBead builds a session bead that looks like a live
// pool worker. Tests tweak individual fields after construction to cover
// the eligibility predicates. The template parameter is retained for
// call-site clarity even though current tests always pass "polecat";
// future eligibility tests may vary it.
//
//nolint:unparam // template kept as a parameter intentionally; see above.
func makeAliveEphemeralPoolBead(id, sessionName, template, lastWokeAt string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Status: "open",
		Metadata: map[string]string{
			"template":       template,
			"session_name":   sessionName,
			"pool_slot":      "1",
			"session_origin": "ephemeral",
			"state":          "active",
			"last_woke_at":   lastWokeAt,
		},
	}
}

// polecatPoolCfg returns a config with one pool agent whose nudge text
// matches the polecat agent.toml.
func polecatPoolCfg() *config.City {
	return &config.City{
		Agents: []config.Agent{
			{
				Name:              "polecat",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
				ScaleCheck:        "echo 1",
				Nudge:             "Run gc hook; it checks assigned work first, then routed pool work.",
			},
		},
	}
}

func TestDetectIdleReloadSurvivors_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := polecatPoolCfg()
	session := makeAliveEphemeralPoolBead("b1", "pol-1", "polecat",
		now.Add(-10*time.Minute).Format(time.RFC3339))
	workSet := map[string]bool{"polecat": true}

	got := detectIdleReloadSurvivors([]beads.Bead{session}, cfg, nil, workSet, clk)
	if len(got) != 1 {
		t.Fatalf("want 1 target, got %d: %+v", len(got), got)
	}
	if got[0].SessionID != "b1" || got[0].SessionName != "pol-1" || got[0].Template != "polecat" {
		t.Errorf("target fields mismatch: %+v", got[0])
	}
	if got[0].NudgeText != cfg.Agents[0].Nudge {
		t.Errorf("nudge text = %q; want %q", got[0].NudgeText, cfg.Agents[0].Nudge)
	}
}

func TestDetectIdleReloadSurvivors_SkipsWhenAssignedWorkInProgress(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := polecatPoolCfg()
	session := makeAliveEphemeralPoolBead("b1", "pol-1", "polecat",
		now.Add(-10*time.Minute).Format(time.RFC3339))
	workSet := map[string]bool{"polecat": true}

	// Work assigned to this session (by bead ID): not idle.
	workAssignedByID := beads.Bead{ID: "w1", Status: "in_progress", Assignee: "b1"}
	if got := detectIdleReloadSurvivors([]beads.Bead{session}, cfg, []beads.Bead{workAssignedByID}, workSet, clk); len(got) != 0 {
		t.Errorf("assigned by ID should skip, got %+v", got)
	}

	// Work assigned to this session (by session_name): not idle.
	workAssignedByName := beads.Bead{ID: "w2", Status: "open", Assignee: "pol-1"}
	if got := detectIdleReloadSurvivors([]beads.Bead{session}, cfg, []beads.Bead{workAssignedByName}, workSet, clk); len(got) != 0 {
		t.Errorf("assigned by name should skip, got %+v", got)
	}

	// Closed work doesn't count — session still idle.
	workClosed := beads.Bead{ID: "w3", Status: "closed", Assignee: "pol-1"}
	if got := detectIdleReloadSurvivors([]beads.Bead{session}, cfg, []beads.Bead{workClosed}, workSet, clk); len(got) != 1 {
		t.Errorf("closed work should not block re-nudge, got %+v", got)
	}
}

func TestDetectIdleReloadSurvivors_SkipsWhenNoRoutedPoolWork(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := polecatPoolCfg()
	session := makeAliveEphemeralPoolBead("b1", "pol-1", "polecat",
		now.Add(-10*time.Minute).Format(time.RFC3339))

	// Empty workSet → no work routed → nothing to do.
	if got := detectIdleReloadSurvivors([]beads.Bead{session}, cfg, nil, nil, clk); len(got) != 0 {
		t.Errorf("empty workSet should not re-nudge, got %+v", got)
	}

	// workSet for a different template → still nothing.
	if got := detectIdleReloadSurvivors([]beads.Bead{session}, cfg, nil, map[string]bool{"refinery": true}, clk); len(got) != 0 {
		t.Errorf("mismatched workSet should not re-nudge, got %+v", got)
	}
}

func TestDetectIdleReloadSurvivors_RespectsMinAge(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := polecatPoolCfg()
	workSet := map[string]bool{"polecat": true}

	// Just-woken session (well under threshold) → skip.
	freshSession := makeAliveEphemeralPoolBead("b1", "pol-1", "polecat",
		now.Add(-1*time.Minute).Format(time.RFC3339))
	if got := detectIdleReloadSurvivors([]beads.Bead{freshSession}, cfg, nil, workSet, clk); len(got) != 0 {
		t.Errorf("fresh session (under threshold) should not re-nudge, got %+v", got)
	}

	// At threshold exactly → eligible (>=).
	atThreshold := makeAliveEphemeralPoolBead("b2", "pol-2", "polecat",
		now.Add(-idleRenudgeMinAge).Format(time.RFC3339))
	if got := detectIdleReloadSurvivors([]beads.Bead{atThreshold}, cfg, nil, workSet, clk); len(got) != 1 {
		t.Errorf("session at threshold should re-nudge, got %+v", got)
	}

	// Missing last_woke_at → skip (no anchor for age).
	noAnchor := makeAliveEphemeralPoolBead("b3", "pol-3", "polecat", "")
	if got := detectIdleReloadSurvivors([]beads.Bead{noAnchor}, cfg, nil, workSet, clk); len(got) != 0 {
		t.Errorf("missing last_woke_at should skip, got %+v", got)
	}
}

func TestDetectIdleReloadSurvivors_HonorsCooldownAfterPreviousRenudge(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := polecatPoolCfg()
	workSet := map[string]bool{"polecat": true}

	// Session woken long ago, but re-nudged 1 minute ago → cooldown blocks.
	session := makeAliveEphemeralPoolBead("b1", "pol-1", "polecat",
		now.Add(-1*time.Hour).Format(time.RFC3339))
	session.Metadata[idleRenudgeLastAtKey] = now.Add(-1 * time.Minute).Format(time.RFC3339)

	if got := detectIdleReloadSurvivors([]beads.Bead{session}, cfg, nil, workSet, clk); len(got) != 0 {
		t.Errorf("cooldown should block re-nudge, got %+v", got)
	}

	// Cooldown elapsed → eligible again.
	session.Metadata[idleRenudgeLastAtKey] = now.Add(-idleRenudgeMinAge).Format(time.RFC3339)
	if got := detectIdleReloadSurvivors([]beads.Bead{session}, cfg, nil, workSet, clk); len(got) != 1 {
		t.Errorf("elapsed cooldown should allow re-nudge, got %+v", got)
	}
}

func TestDetectIdleReloadSurvivors_SkipsNonAliveOrSuppressed(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := polecatPoolCfg()
	workSet := map[string]bool{"polecat": true}
	wokenLongAgo := now.Add(-1 * time.Hour).Format(time.RFC3339)

	cases := []struct {
		name   string
		mutate func(b *beads.Bead)
	}{
		{
			name:   "state=asleep",
			mutate: func(b *beads.Bead) { b.Metadata["state"] = "asleep" },
		},
		{
			name:   "state=drained",
			mutate: func(b *beads.Bead) { b.Metadata["state"] = "drained" },
		},
		{
			name:   "state=creating",
			mutate: func(b *beads.Bead) { b.Metadata["state"] = "creating" },
		},
		{
			name:   "pending_create_claim=true",
			mutate: func(b *beads.Bead) { b.Metadata["pending_create_claim"] = "true" },
		},
		{
			name:   "status=closed",
			mutate: func(b *beads.Bead) { b.Status = "closed" },
		},
		{
			name:   "held_until in future",
			mutate: func(b *beads.Bead) { b.Metadata["held_until"] = now.Add(time.Hour).Format(time.RFC3339) },
		},
		{
			name:   "quarantined_until in future",
			mutate: func(b *beads.Bead) { b.Metadata["quarantined_until"] = now.Add(time.Hour).Format(time.RFC3339) },
		},
		{
			name:   "wait_hold set",
			mutate: func(b *beads.Bead) { b.Metadata["wait_hold"] = "true" },
		},
		{
			name: "asleep+sleep_reason=drained",
			mutate: func(b *beads.Bead) {
				b.Metadata["state"] = "asleep"
				b.Metadata["sleep_reason"] = "drained"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := makeAliveEphemeralPoolBead("b1", "pol-1", "polecat", wokenLongAgo)
			tc.mutate(&session)
			if got := detectIdleReloadSurvivors([]beads.Bead{session}, cfg, nil, workSet, clk); len(got) != 0 {
				t.Errorf("%s should skip, got %+v", tc.name, got)
			}
		})
	}
}

func TestDetectIdleReloadSurvivors_SkipsNonEphemeralSessions(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := polecatPoolCfg()
	workSet := map[string]bool{"polecat": true}
	wokenLongAgo := now.Add(-1 * time.Hour).Format(time.RFC3339)

	// Named session → skip.
	named := makeAliveEphemeralPoolBead("b1", "pol-1", "polecat", wokenLongAgo)
	named.Metadata["session_origin"] = "named"
	named.Metadata["named_session"] = "true"
	delete(named.Metadata, "pool_slot")
	if got := detectIdleReloadSurvivors([]beads.Bead{named}, cfg, nil, workSet, clk); len(got) != 0 {
		t.Errorf("named session should skip, got %+v", got)
	}

	// Manual session → skip.
	manual := makeAliveEphemeralPoolBead("b2", "pol-2", "polecat", wokenLongAgo)
	manual.Metadata["session_origin"] = "manual"
	delete(manual.Metadata, "pool_slot")
	if got := detectIdleReloadSurvivors([]beads.Bead{manual}, cfg, nil, workSet, clk); len(got) != 0 {
		t.Errorf("manual session should skip, got %+v", got)
	}
}

func TestDetectIdleReloadSurvivors_SkipsWhenAgentHasNoNudge(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := polecatPoolCfg()
	cfg.Agents[0].Nudge = "" // remove configured nudge text
	workSet := map[string]bool{"polecat": true}

	session := makeAliveEphemeralPoolBead("b1", "pol-1", "polecat",
		now.Add(-1*time.Hour).Format(time.RFC3339))

	if got := detectIdleReloadSurvivors([]beads.Bead{session}, cfg, nil, workSet, clk); len(got) != 0 {
		t.Errorf("agent without nudge text should skip, got %+v", got)
	}
}

func TestDetectIdleReloadSurvivors_HandlesMultipleSessionsDeterministically(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := polecatPoolCfg()
	workSet := map[string]bool{"polecat": true}
	wokenLongAgo := now.Add(-1 * time.Hour).Format(time.RFC3339)

	idleA := makeAliveEphemeralPoolBead("b1", "pol-1", "polecat", wokenLongAgo)
	idleC := makeAliveEphemeralPoolBead("b3", "pol-3", "polecat", wokenLongAgo)

	// Mix in one busy session and one fresh session.
	busy := makeAliveEphemeralPoolBead("b2", "pol-2", "polecat", wokenLongAgo)
	fresh := makeAliveEphemeralPoolBead("b4", "pol-4", "polecat",
		now.Add(-30*time.Second).Format(time.RFC3339))

	workBeads := []beads.Bead{{ID: "w1", Status: "in_progress", Assignee: "pol-2"}}

	got := detectIdleReloadSurvivors([]beads.Bead{idleA, busy, idleC, fresh}, cfg, workBeads, workSet, clk)
	if len(got) != 2 {
		t.Fatalf("want 2 idle targets, got %d: %+v", len(got), got)
	}
	ids := []string{got[0].SessionID, got[1].SessionID}
	sort.Strings(ids)
	if ids[0] != "b1" || ids[1] != "b3" {
		t.Errorf("targets = %v; want [b1 b3]", ids)
	}
}

// stubNudgeProvider records every Nudge() call. Other Provider methods
// are not exercised by renudgeIdleSessions, so this struct deliberately
// embeds runtime.Provider via a nil interface and only implements Nudge.
type stubNudgeProvider struct {
	runtime.Provider
	calls []nudgeCall
	err   error
}

type nudgeCall struct {
	name string
	text string
}

func (p *stubNudgeProvider) Nudge(name string, content []runtime.ContentBlock) error {
	p.calls = append(p.calls, nudgeCall{name: name, text: runtime.FlattenText(content)})
	return p.err
}

func TestRenudgeIdleSessions_DeliversAndRecordsTimestamp(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	sp := &stubNudgeProvider{}
	store := newTestStore()

	targets := []idleRenudgeTarget{
		{SessionID: "b1", SessionName: "pol-1", Template: "polecat", NudgeText: "Run gc hook"},
		{SessionID: "b2", SessionName: "pol-2", Template: "polecat", NudgeText: "Run gc hook"},
	}

	var stderr bytes.Buffer
	renudgeIdleSessions(context.Background(), targets, sp, store, clk, &stderr)

	if len(sp.calls) != 2 {
		t.Fatalf("want 2 nudge calls, got %d: %+v", len(sp.calls), sp.calls)
	}
	if sp.calls[0].name != "pol-1" || sp.calls[1].name != "pol-2" {
		t.Errorf("nudge call names = %+v", sp.calls)
	}
	if sp.calls[0].text != "Run gc hook" {
		t.Errorf("nudge text = %q", sp.calls[0].text)
	}

	wantTs := now.UTC().Format(time.RFC3339)
	if got := store.metadata["b1"][idleRenudgeLastAtKey]; got != wantTs {
		t.Errorf("b1 timestamp = %q; want %q", got, wantTs)
	}
	if got := store.metadata["b2"][idleRenudgeLastAtKey]; got != wantTs {
		t.Errorf("b2 timestamp = %q; want %q", got, wantTs)
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr output on success, got %q", stderr.String())
	}
}

func TestRenudgeIdleSessions_NudgeFailureDoesNotUpdateTimestamp(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	sp := &stubNudgeProvider{err: errors.New("session not found")}
	store := newTestStore()

	targets := []idleRenudgeTarget{
		{SessionID: "b1", SessionName: "pol-1", Template: "polecat", NudgeText: "Run gc hook"},
	}

	var stderr bytes.Buffer
	renudgeIdleSessions(context.Background(), targets, sp, store, clk, &stderr)

	if _, ok := store.metadata["b1"]; ok {
		t.Errorf("failed nudge should not write metadata, got %+v", store.metadata)
	}
	if stderr.Len() == 0 {
		t.Errorf("expected stderr log on failure")
	}
}

func TestRenudgeIdleSessions_RespectsCanceledContext(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	sp := &stubNudgeProvider{}
	store := newTestStore()

	targets := []idleRenudgeTarget{
		{SessionID: "b1", SessionName: "pol-1", Template: "polecat", NudgeText: "Run gc hook"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	renudgeIdleSessions(ctx, targets, sp, store, clk, nil)

	if len(sp.calls) != 0 {
		t.Errorf("canceled context should skip nudge calls, got %+v", sp.calls)
	}
}

// TestReconcileSessionBeads_IdleReloadSurvivorIsRenudged is the end-to-end
// proof for ga-6lj: a reconciler tick handed an alive ephemeral pool session
// with no in-progress work, routed pool work, and last_woke_at past the
// re-nudge threshold must fire sp.Nudge with the configured agent text and
// stamp idleRenudgeLastAtKey on the bead.
func TestReconcileSessionBeads_IdleReloadSurvivorIsRenudged(t *testing.T) {
	env := newReconcilerTestEnv()
	env.clk = &clock.Fake{Time: time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)}
	const nudgeText = "Run gc hook; it checks assigned work first, then routed pool work."
	env.cfg = &config.City{
		Agents: []config.Agent{{
			Name:              "polecat",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "echo 1",
			Nudge:             nudgeText,
		}},
	}
	env.addDesired("polecat-1", "polecat", true)

	session := env.createSessionBead("polecat-1", "polecat")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"pool_slot":      "1",
		"session_origin": "ephemeral",
		// Survived past the re-nudge threshold without claiming work.
		"last_woke_at": env.clk.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339),
	})

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"polecat": true},
		env.cfg,
		env.sp,
		env.store,
		newFakeDrainOps(),
		nil, // assignedWorkBeads — none, the polecat is idle
		nil, // rigStores
		nil,
		env.dt,
		map[string]int{"polecat": 1},
		false,
		map[string]bool{"polecat": true}, // workSet — routed pool work waiting
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)

	var nudgeCalls []runtime.Call
	for _, c := range env.sp.Calls {
		if c.Method == "Nudge" && c.Name == "polecat-1" {
			nudgeCalls = append(nudgeCalls, c)
		}
	}
	if len(nudgeCalls) != 1 {
		t.Fatalf("want 1 Nudge to polecat-1, got %d (all calls: %+v)", len(nudgeCalls), env.sp.Calls)
	}
	if nudgeCalls[0].Message != nudgeText {
		t.Errorf("Nudge text = %q; want %q", nudgeCalls[0].Message, nudgeText)
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata[idleRenudgeLastAtKey] == "" {
		t.Errorf("expected %s to be stamped on session bead, got metadata=%+v", idleRenudgeLastAtKey, got.Metadata)
	}
}

// TestReconcileSessionBeads_IdleReloadSurvivor_SkipsWhenAssigned proves the
// idempotence contract: an alive polecat with open work assigned to it must
// NOT be re-nudged, even if everything else looks idle. This is the
// "session is now working" exemption from the ga-6lj acceptance criteria.
func TestReconcileSessionBeads_IdleReloadSurvivor_SkipsWhenAssigned(t *testing.T) {
	env := newReconcilerTestEnv()
	env.clk = &clock.Fake{Time: time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)}
	env.cfg = &config.City{
		Agents: []config.Agent{{
			Name:              "polecat",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "echo 1",
			Nudge:             "Run gc hook",
		}},
	}
	env.addDesired("polecat-1", "polecat", true)

	session := env.createSessionBead("polecat-1", "polecat")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"pool_slot":      "1",
		"session_origin": "ephemeral",
		"last_woke_at":   env.clk.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339),
	})

	// Work bead assigned to this session — session is busy, not idle.
	workBead := beads.Bead{
		ID:       "w1",
		Status:   "in_progress",
		Assignee: "polecat-1",
	}

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"polecat": true},
		env.cfg,
		env.sp,
		env.store,
		newFakeDrainOps(),
		[]beads.Bead{workBead},
		nil,
		nil,
		env.dt,
		map[string]int{"polecat": 1},
		false,
		map[string]bool{"polecat": true},
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)

	for _, c := range env.sp.Calls {
		if c.Method == "Nudge" && c.Name == "polecat-1" {
			t.Errorf("session with assigned work must not be re-nudged, got %+v", c)
		}
	}
}
