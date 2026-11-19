package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestWakeFairnessRoutedPoolWorkNotStarvedByIdlePatrolRestarts reproduces
// ga-z4joq0 (#7680): with a contended per-tick wake budget, a pool session that
// exists only because work was routed to it must get a slot ahead of (or share
// the budget with) always-mode named sessions that drained long ago. Ordering
// purely by last_woke_at / slept_at / created_at sorts the fresh pool session
// last, so it is deferred_by_wake_budget wave after wave.
//
// The named candidates carry the standing flag the reconciler's wake arm
// derives from the awake decision (AwakeDecision.Standing, pinned by the
// compute_awake_set_standing tests); the sort then has to spend the budget on
// the demand candidate first.
func TestWakeFairnessRoutedPoolWorkNotStarvedByIdlePatrolRestarts(t *testing.T) {
	now := time.Date(2026, 10, 12, 11, 18, 5, 0, time.UTC)
	const maxWakes = 5

	var ready []startCandidate
	for i := 0; i < maxWakes; i++ {
		ready = append(ready, startCandidate{info: session.Info{
			ID:                      fmt.Sprintf("gc-named-%d", i),
			SessionNameMetadata:     fmt.Sprintf("rig%d--patrol", i),
			ConfiguredNamedSession:  true,
			ConfiguredNamedIdentity: fmt.Sprintf("rig%d/patrol", i),
			ConfiguredNamedMode:     "always",
			WakeMode:                "fresh",
			CreatedAt:               now.Add(-48 * time.Hour),
			SleptAt:                 now.Add(-time.Duration(60+i) * time.Minute).Format(time.RFC3339),
		}, standing: true})
	}
	pool := startCandidate{info: session.Info{
		ID:                  "gc-pool-1",
		SessionNameMetadata: "worker-pool-1",
		PoolManaged:         true,
		PoolSlot:            "1",
		CreatedAt:           now.Add(-30 * time.Second),
	}}
	ready = append(ready, pool)

	sortCandidatesByWakeFairness(ready)

	for i := 0; i < maxWakes && i < len(ready); i++ {
		if ready[i].info.ID == pool.info.ID {
			return
		}
	}
	var order []string
	for _, c := range ready {
		order = append(order, c.info.ID)
	}
	t.Fatalf("budget of %d spent on idle always-mode named restarts; routed pool session deferred: order %v", maxWakes, order)
}

// TestWakeBudgetStartsRoutedPoolWorkBeforeIdlePatrolRestarts is the AC-1
// scenario end to end through the legacy reconciler: with
// max_wakes_per_tick=2, one dependency wave holds three asleep named
// mode="always" sessions with no assigned work and no named demand (slept an
// hour ago) and two pool sessions desired for routed work (created this
// pass). Both pool sessions must start and the three standing sessions must
// be logged deferred_by_wake_budget — the incident's symptom was the reverse:
// the always-mode restarts, oldest by slept_at, took every slot while the
// routed pool sessions waited wave after wave (#7680).
func TestWakeBudgetStartsRoutedPoolWorkBeforeIdlePatrolRestarts(t *testing.T) {
	env := newReconcilerTestEnv(t)
	budget := 2
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: &budget},
		Agents: []config.Agent{
			{Name: "patrol-a", StartCommand: "true"},
			{Name: "patrol-b", StartCommand: "true"},
			{Name: "patrol-c", StartCommand: "true"},
			{Name: "worker", StartCommand: "true"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "patrol-a", Mode: "always"},
			{Template: "patrol-b", Mode: "always"},
			{Template: "patrol-c", Mode: "always"},
		},
	}

	sleptAt := env.clk.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	var sessions []beads.Bead
	named := map[string]string{}
	for _, template := range []string{"patrol-a", "patrol-b", "patrol-c"} {
		sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, template)
		named[sessionName] = template
		env.desiredState[sessionName] = TemplateParams{
			Command:                 "true",
			SessionName:             sessionName,
			TemplateName:            template,
			ConfiguredNamedIdentity: template,
			ConfiguredNamedMode:     "always",
		}
		session := env.createSessionBead(sessionName, template)
		env.setSessionMetadata(&session, map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: template,
			namedSessionModeMetadata:     "always",
			"state":                      "asleep",
			"slept_at":                   sleptAt,
		})
		sessions = append(sessions, session)
	}
	for _, name := range []string{"worker-1", "worker-2"} {
		env.addDesired(name, "worker", false)
		session := env.createSessionBead(name, "worker")
		env.markSessionCreating(&session)
		sessions = append(sessions, session)
	}

	woken := env.reconcile(sessions)

	if woken != budget {
		t.Fatalf("woken = %d, want %d (both routed pool sessions)\nstderr:\n%s", woken, budget, env.stderr.String())
	}
	for _, name := range []string{"worker-1", "worker-2"} {
		if !env.sp.IsRunning(name) {
			t.Fatalf("routed pool session %q should have started ahead of idle patrol restarts\nstderr:\n%s", name, env.stderr.String())
		}
	}
	for sessionName := range named {
		if env.sp.IsRunning(sessionName) {
			t.Fatalf("standing named session %q started; the budget must be spent on routed pool work first", sessionName)
		}
	}
	deferred := 0
	for sessionName := range named {
		for _, line := range strings.Split(env.stderr.String(), "\n") {
			if strings.Contains(line, sessionName) && strings.Contains(line, "deferred_by_wake_budget") {
				deferred++
				break
			}
		}
	}
	if deferred != len(named) {
		t.Fatalf("deferred_by_wake_budget logged for %d of %d standing named sessions\nstderr:\n%s", deferred, len(named), env.stderr.String())
	}
}

// TestWakeBudgetStandingClassKeepsLeastRecentlyWokenOrder pins the
// within-class order of the standing candidates (AC-3): once demand work is
// admitted first, the standing restarts left compete among themselves exactly
// as the demand class does — least-recently-woken first (START-009) — and the
// budget never reorders them by anything else. With three standing candidates
// and a budget of 2, the two oldest wake; the freshest defers.
func TestWakeBudgetStandingClassKeepsLeastRecentlyWokenOrder(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 10, 12, 11, 18, 5, 0, time.UTC)}
	budget := 2
	cfg := &config.City{Daemon: config.DaemonConfig{MaxWakesPerTick: &budget}}

	mkTP := func(name string) TemplateParams {
		return TemplateParams{
			Command:      "true",
			SessionName:  name,
			TemplateName: name,
			ResolvedProvider: &config.ResolvedProvider{
				Name:          "claude",
				Command:       "claude",
				PromptMode:    "arg",
				ResumeFlag:    "--resume",
				ResumeStyle:   "flag",
				SessionIDFlag: "--session-id",
			},
		}
	}

	// Distinct wake times: old-a 3h ago, old-b 2h ago, fresh 1m ago. All are
	// standing candidates (the wake arm derives the flag from
	// AwakeDecision.Standing), so they all sit in the standing class and the
	// budget must go least-recently-woken first.
	specs := []struct{ name, lastWoke string }{
		{"old-a", "2026-10-12T08:18:05Z"},
		{"old-b", "2026-10-12T09:18:05Z"},
		{"fresh", "2026-10-12T11:17:05Z"},
	}
	desired := map[string]TemplateParams{}
	var candidates []startCandidate
	for i, s := range specs {
		sess, err := store.Create(beads.Bead{
			Title:  s.name,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name":   s.name,
				"agent_name":     s.name,
				"template":       s.name,
				"state":          "creating",
				"generation":     "1",
				"instance_token": "test-token",
				"live_hash":      runtime.LiveFingerprint(runtime.Config{Command: "test-cmd"}),
				"last_woke_at":   s.lastWoke,
			},
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", s.name, err)
		}
		tp := mkTP(s.name)
		desired[s.name] = tp
		candidates = append(candidates, startCandidate{info: sessiontest.SeedBead(t, sess), tp: tp, order: i, standing: true})
	}

	woken := executePlannedStartsTraced(
		context.Background(),
		candidates,
		cfg,
		desired,
		sp,
		store,
		"",
		t.TempDir(),
		clk,
		events.Discard,
		5*time.Second,
		ioDiscard{},
		ioDiscard{},
		nil,
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	)
	if woken != budget {
		t.Fatalf("woken = %d, want %d", woken, budget)
	}
	for _, name := range []string{"old-a", "old-b"} {
		if !sp.IsRunning(name) {
			t.Fatalf("least-recently-woken standing session %q deferred behind fresher standing siblings", name)
		}
	}
	if sp.IsRunning("fresh") {
		t.Fatal("freshest standing session started ahead of older standing siblings — standing class lost its least-recently-woken order")
	}
}

// TestWakeBudgetFreeBudgetStartsEveryStandingCandidate is the legacy half of
// AC-4: standing restarts are ordered behind demand work, never excluded.
// When the budget covers every candidate — demand and standing alike — all of
// them start in the same pass; the standing class is a priority order, not a
// filter.
func TestWakeBudgetFreeBudgetStartsEveryStandingCandidate(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 10, 12, 11, 18, 5, 0, time.UTC)}
	budget := 4
	cfg := &config.City{Daemon: config.DaemonConfig{MaxWakesPerTick: &budget}}

	mkTP := func(name string) TemplateParams {
		return TemplateParams{
			Command:      "true",
			SessionName:  name,
			TemplateName: name,
			ResolvedProvider: &config.ResolvedProvider{
				Name:          "claude",
				Command:       "claude",
				PromptMode:    "arg",
				ResumeFlag:    "--resume",
				ResumeStyle:   "flag",
				SessionIDFlag: "--session-id",
			},
		}
	}

	// Two demand pool sessions (routed work) and two standing named restarts:
	// four candidates, four slots, so none may defer.
	specs := []struct {
		name     string
		lastWoke string
		standing bool
	}{
		{"pool-1", "2026-10-12T11:17:05Z", false},
		{"pool-2", "2026-10-12T11:17:06Z", false},
		{"named-1", "2026-10-12T09:18:05Z", true},
		{"named-2", "2026-10-12T08:18:05Z", true},
	}
	desired := map[string]TemplateParams{}
	var candidates []startCandidate
	for i, s := range specs {
		sess, err := store.Create(beads.Bead{
			Title:  s.name,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name":   s.name,
				"agent_name":     s.name,
				"template":       s.name,
				"state":          "creating",
				"generation":     "1",
				"instance_token": "test-token",
				"live_hash":      runtime.LiveFingerprint(runtime.Config{Command: "test-cmd"}),
				"last_woke_at":   s.lastWoke,
			},
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", s.name, err)
		}
		tp := mkTP(s.name)
		desired[s.name] = tp
		candidates = append(candidates, startCandidate{info: sessiontest.SeedBead(t, sess), tp: tp, order: i, standing: s.standing})
	}

	woken := executePlannedStartsTraced(
		context.Background(),
		candidates,
		cfg,
		desired,
		sp,
		store,
		"",
		t.TempDir(),
		clk,
		events.Discard,
		5*time.Second,
		ioDiscard{},
		ioDiscard{},
		nil,
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	)
	if woken != len(specs) {
		t.Fatalf("woken = %d, want %d (a free budget starts every candidate)", woken, len(specs))
	}
	for _, s := range specs {
		if !sp.IsRunning(s.name) {
			t.Fatalf("standing-priority order deferred %q on a free budget — standing is an order, not a filter", s.name)
		}
	}
}
