package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// drainedNamedHolderConfig mirrors the live gascity/builder shape: an
// on_demand named session over a canonical-singleton pool
// (max_active_sessions = 1, no namepool), wake_mode = fresh,
// idle_timeout = 10m.
func drainedNamedHolderConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "builder",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
			WakeMode:          "fresh",
			IdleTimeout:       "10m",
		}},
		NamedSessions: []config.NamedSession{{Template: "builder", Mode: "on_demand"}},
	}
}

type drainedNamedHolderResult struct {
	woken        int
	running      bool
	namedDemand  map[string]bool
	routedDemand map[string]bool
	starts       []string
	postSessions []beads.Bead
	buildStderr  string
}

// reconcileNamedHolderWithRoutedWork drives one full controller tick
// (buildDesiredState -> syncSessionBeads -> ComputePoolDesiredStates ->
// reconcileSessionBeads) for a configured named holder whose bead carries
// holderState, plus one open work bead routed to the backing template and
// assigned to nobody.
func reconcileNamedHolderWithRoutedWork(t *testing.T, cfg *config.City, holderState map[string]string) drainedNamedHolderResult {
	t.Helper()

	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 9, 26, 16, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	identity := cfg.NamedSessions[0].Template
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity)

	if _, err := store.Create(beads.Bead{
		Title:  "routed ready-to-build work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": identity,
		},
	}); err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	meta := map[string]string{
		"session_name":               sessionName,
		"alias":                      identity,
		"agent_name":                 identity,
		"template":                   identity,
		"session_origin":             "named",
		"wake_mode":                  "fresh",
		"generation":                 "1",
		"instance_token":             "canonical-token",
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: identity,
		namedSessionModeMetadata:     "on_demand",
	}
	for k, v := range holderState {
		meta[k] = v
	}
	if _, err := store.Create(beads.Bead{
		Title:    identity,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "agent:" + identity},
		Metadata: meta,
	}); err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	var stdout, stderr, buildStderr bytes.Buffer
	dsResult := buildDesiredState(cfg.EffectiveCityName(), cityPath, clk.Now().UTC(), cfg, sp, store, &buildStderr)
	cfgNames := configuredSessionNames(cfg, cfg.EffectiveCityName(), store)
	syncSessionBeads(cityPath, store, dsResult.State, sp, cfgNames, cfg, clk, &stderr, true)
	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, dsResult.AssignedWorkBeads, sessionInfosFromBeads(sessions), dsResult.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, dsResult.NamedSessionDemand, cfg)

	snap := newSessionBeadSnapshot(sessions)
	woken := reconcileSessionBeadsAtPathWithNamedDemand(
		context.Background(), cityPath, snap.OpenForReconcile(), snap, dsResult.State, cfgNames, cfg, sp,
		store, testSeatWork(cityPath, cfg, store, nil), nil, dsResult.AssignedWorkBeads, nil, nil, newDrainTracker(), nil, poolDesired,
		dsResult.NamedSessionDemand, dsResult.NamedSessionRoutedDemand, dsResult.StoreQueryPartial, nil, cfg.EffectiveCityName(),
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	var starts []string
	for _, call := range sp.SnapshotCalls() {
		if call.Method == "Start" {
			starts = append(starts, call.Name)
		}
	}
	postSessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads (post-reconcile): %v", err)
	}
	return drainedNamedHolderResult{
		woken:        woken,
		running:      sp.IsRunning(sessionName),
		namedDemand:  dsResult.NamedSessionDemand,
		routedDemand: dsResult.NamedSessionRoutedDemand,
		starts:       starts,
		postSessions: postSessions,
		buildStderr:  buildStderr.String(),
	}
}

// TestDrainedNamedHolder_RoutedDemandWakesHolder_GaQbbnr9 reproduces the
// 2026-09-26 gascity/builder starvation (ga-qbbnr9): the named holder
// drain-acked after its last task (state=asleep, sleep_reason=drained), and
// the only remaining demand is routed-but-unassigned. Every tick, the pool
// asks for a canonical session and is refused because the drained holder
// still carries the canonical name, while the named arm of ComputeAwakeSet
// drops the routed-demand wake because the holder is Drained. Nothing starts.
func TestDrainedNamedHolder_RoutedDemandWakesHolder_GaQbbnr9(t *testing.T) {
	cases := []struct {
		name  string
		state map[string]string
	}{
		{
			// Live shape 15:31Z..22:58Z: the heal pass rewrote drained to
			// asleep+sleep_reason=drained.
			name: "asleep_sleep_reason_drained",
			state: map[string]string{
				"state":        "asleep",
				"sleep_reason": "drained",
				"slept_at":     "2026-09-26T15:28:28Z",
				"drain_at":     "2026-09-26T15:27:19Z",
			},
		},
		{
			// Live shape 15:28Z..15:31Z: AcknowledgeDrainPatch output before
			// the heal pass ran.
			name: "state_drained",
			state: map[string]string{
				"state":    "drained",
				"slept_at": "2026-09-26T15:28:28Z",
				"drain_at": "2026-09-26T15:27:19Z",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := drainedNamedHolderConfig()
			sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "builder")
			r := reconcileNamedHolderWithRoutedWork(t, cfg, tc.state)

			if r.namedDemand["builder"] {
				t.Fatalf("NamedSessionDemand[builder] = true, want false: the work is routed, not assigned")
			}
			if !r.routedDemand["builder"] {
				t.Fatalf("NamedSessionRoutedDemand[builder] = false, want true for routed demand on a canonical-singleton backing pool")
			}
			if !strings.Contains(r.buildStderr, `canonical pool template "builder" is held by session`) {
				t.Fatalf("pool did not report the canonical name held by the drained holder; build stderr:\n%s", r.buildStderr)
			}
			if r.woken != 1 || !r.running {
				t.Fatalf("drained named holder was not woken by routed demand: woken=%d running=%v starts=%v "+
					"(the pool cannot create a session because the drained holder keeps the canonical name, "+
					"so the holder is the only session that can serve this work)", r.woken, r.running, r.starts)
			}
			if len(r.starts) != 1 || r.starts[0] != sessionName {
				t.Fatalf("starts = %v, want exactly [%s]", r.starts, sessionName)
			}
			if len(r.postSessions) != 1 {
				t.Fatalf("session beads after reconcile = %d, want 1: no standby may be created beside the named holder", len(r.postSessions))
			}
		})
	}
}

// TestAsleepNamedHolder_RoutedDemandWakesHolder_Control_GaQbbnr9 is the
// control for the test above: the same holder, asleep for any reason other
// than drained, already wakes from routed demand alone (#4644, #4702). The
// discriminating fact is the Drained sub-state, not routed-versus-assigned
// demand.
func TestAsleepNamedHolder_RoutedDemandWakesHolder_Control_GaQbbnr9(t *testing.T) {
	cfg := drainedNamedHolderConfig()
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "builder")
	r := reconcileNamedHolderWithRoutedWork(t, cfg, map[string]string{
		"state":        "asleep",
		"sleep_reason": "idle",
		"slept_at":     "2026-09-26T15:28:28Z",
	})
	if r.woken != 1 || !r.running {
		t.Fatalf("asleep (not drained) named holder was not woken by routed demand: woken=%d running=%v starts=%v", r.woken, r.running, r.starts)
	}
	if len(r.starts) != 1 || r.starts[0] != sessionName {
		t.Fatalf("starts = %v, want exactly [%s]", r.starts, sessionName)
	}
	if strings.Contains(r.buildStderr, "is held by session") {
		t.Fatalf("a non-drained holder must suppress the pool request outright (alias held), not stall it; build stderr:\n%s", r.buildStderr)
	}
}
