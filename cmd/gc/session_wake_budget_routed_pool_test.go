package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// TestWakeFairnessRoutedPoolWorkNotStarvedByIdlePatrolRestarts reproduces
// ga-z4joq0 (#7680): with a contended per-tick wake budget, a pool session that
// exists only because work was routed to it must get a slot ahead of
// always-mode named sessions that drained long ago and have no work. Ordering
// purely by last_woke_at / slept_at / created_at sorts the fresh pool session
// last, so it is deferred_by_wake_budget wave after wave. The named candidates
// carry standing, as the reconciler's wake arm sets it from their awake
// decision (named-always with no work and no demand).
func TestWakeFairnessRoutedPoolWorkNotStarvedByIdlePatrolRestarts(t *testing.T) {
	now := time.Date(2026, 10, 12, 11, 18, 5, 0, time.UTC)
	const maxWakes = 5

	var ready []startCandidate
	for i := 0; i < maxWakes; i++ {
		ready = append(ready, startCandidate{standing: true, info: session.Info{
			ID:                      fmt.Sprintf("gc-named-%d", i),
			SessionNameMetadata:     fmt.Sprintf("rig%d--patrol", i),
			ConfiguredNamedSession:  true,
			ConfiguredNamedIdentity: fmt.Sprintf("rig%d/patrol", i),
			ConfiguredNamedMode:     "always",
			WakeMode:                "fresh",
			CreatedAt:               now.Add(-48 * time.Hour),
			SleptAt:                 now.Add(-time.Duration(60+i) * time.Minute).Format(time.RFC3339),
		}})
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

// TestReconcileWakeBudgetStartsDemandBeforeStandingNamedSessions is AC-1 of
// ga-z4joq0 through the legacy reconciler: with max_wakes_per_tick=2, one
// wave holds three asleep always-mode named sessions with no work (slept an
// hour ago) and two pool sessions created this pass for work. Both pool
// sessions start; the three named sessions are deferred_by_wake_budget.
func TestReconcileWakeBudgetStartsDemandBeforeStandingNamedSessions(t *testing.T) {
	env := newReconcilerTestEnv(t)
	budget := 2
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: &budget},
		Agents: []config.Agent{
			{Name: "patrol-0", StartCommand: "true"},
			{Name: "patrol-1", StartCommand: "true"},
			{Name: "patrol-2", StartCommand: "true"},
			{Name: "worker", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
		},
	}
	var sessions []beads.Bead
	var standingNames []string
	standingTemplates := map[string]string{}
	for i := 0; i < 3; i++ {
		template := fmt.Sprintf("patrol-%d", i)
		env.cfg.NamedSessions = append(env.cfg.NamedSessions, config.NamedSession{Template: template, Mode: "always"})
		name := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, template)
		env.desiredState[name] = TemplateParams{
			Command:                 "true",
			SessionName:             name,
			TemplateName:            template,
			ConfiguredNamedIdentity: template,
			ConfiguredNamedMode:     "always",
		}
		b := env.createSessionBead(name, template)
		env.setSessionMetadata(&b, map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: template,
			namedSessionModeMetadata:     "always",
			"state":                      "asleep",
			"sleep_reason":               "drained",
			"slept_at":                   env.clk.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		})
		sessions = append(sessions, b)
		standingNames = append(standingNames, name)
		standingTemplates[name] = template
	}
	var poolNames []string
	for i := 1; i <= 2; i++ {
		name := fmt.Sprintf("worker-%d", i)
		env.desiredState[name] = TemplateParams{Command: "true", SessionName: name, TemplateName: "worker"}
		b := env.createSessionBead(name, "worker")
		env.setSessionMetadata(&b, map[string]string{
			"state":                "creating",
			"pending_create_claim": "true",
			"pool_slot":            fmt.Sprint(i),
			poolManagedMetadataKey: boolMetadata(true),
			"session_origin":       "ephemeral",
		})
		sessions = append(sessions, b)
		poolNames = append(poolNames, name)
	}

	woken := env.reconcileWithPoolDesired(sessions, map[string]int{"worker": 2})

	if woken != budget {
		t.Fatalf("woken = %d, want %d\nstderr:\n%s", woken, budget, env.stderr.String())
	}
	for _, name := range poolNames {
		if !env.sp.IsRunning(name) {
			t.Errorf("pool session %q created for work did not start; stderr:\n%s", name, env.stderr.String())
		}
	}
	for _, name := range standingNames {
		if env.sp.IsRunning(name) {
			t.Errorf("standing named session %q started ahead of demand", name)
		}
		if want := "session=" + name + " template=" + standingTemplates[name] + " outcome=deferred_by_wake_budget"; !strings.Contains(env.stderr.String(), want) {
			t.Errorf("standing named session %q not logged deferred_by_wake_budget; stderr:\n%s", name, env.stderr.String())
		}
	}
}
