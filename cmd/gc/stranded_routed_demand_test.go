package main

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestRoutedTemplateIsUnwakeable drives routedTemplateIsUnwakeable directly,
// with no policy/rollout/store/event machinery involved, covering the three
// confirmed architecture-review defects from ga-wnqjoz against PR #4607's
// predicate:
//
//   - defect #1 (wrong field tested): min_active_sessions==0 is normal for a
//     demand-driven ephemeral-capable pool and must not alone mean unwakeable.
//   - defect #2 (wrong resolution path): findAgentByName only matches a bare
//     Agent.Name, never a Dir- or PoolName-qualified routed identity.
//   - defect #3 (pool-managed sessions invisible to the live-session check):
//     a live slotted pool-managed session must count as already wakeable.
//
// Two cases (no agent at all; min=0 with no ephemeral capacity) are
// safety-net baselines that already pass against the pre-fix predicate and
// must keep passing after the corrected predicate lands in GREEN.
func TestRoutedTemplateIsUnwakeable(t *testing.T) {
	t.Run("no resolvable agent is unwakeable", func(t *testing.T) {
		cfg := &config.City{}
		if !routedTemplateIsUnwakeable(cfg, nil, "ghost") {
			t.Fatalf("routedTemplateIsUnwakeable() = false, want true when no agent resolves for the template")
		}
	})

	t.Run("dir-qualified route resolves and is wakeable", func(t *testing.T) {
		one := 1
		cfg := &config.City{
			Agents: []config.Agent{{
				Name:              "polecat",
				Dir:               "myrig",
				MinActiveSessions: &one,
			}},
		}
		template := "myrig/polecat" // agent.QualifiedName()
		if routedTemplateIsUnwakeable(cfg, nil, template) {
			t.Fatalf("routedTemplateIsUnwakeable(%q) = true, want false: a Dir-qualified route must resolve to its agent (defect #2)", template)
		}
	})

	t.Run("pool-name-qualified route resolves and is wakeable", func(t *testing.T) {
		one := 1
		cfg := &config.City{
			Agents: []config.Agent{{
				Name:              "workhorse",
				Dir:               "myrig",
				PoolName:          "myrig/workhorse",
				MinActiveSessions: &one,
			}},
		}
		template := "myrig/workhorse" // agentutil.RoutedToIdentity() when PoolName is set
		if routedTemplateIsUnwakeable(cfg, nil, template) {
			t.Fatalf("routedTemplateIsUnwakeable(%q) = true, want false: a PoolName-qualified route must resolve to its agent (defect #2)", template)
		}
	})

	t.Run("suspended agent is unwakeable", func(t *testing.T) {
		one := 1
		cfg := &config.City{
			Agents: []config.Agent{{
				Name:              "worker",
				MinActiveSessions: &one,
				Suspended:         true,
			}},
		}
		if !routedTemplateIsUnwakeable(cfg, nil, "worker") {
			t.Fatalf("routedTemplateIsUnwakeable() = false, want true for a Suspended agent regardless of min_active_sessions")
		}
	})

	t.Run("min zero but ephemeral-capable is wakeable", func(t *testing.T) {
		zero := 0
		cfg := &config.City{
			Agents: []config.Agent{{
				Name:              "worker",
				MinActiveSessions: &zero,
			}},
		}
		if routedTemplateIsUnwakeable(cfg, nil, "worker") {
			t.Fatalf("routedTemplateIsUnwakeable() = true, want false: min_active_sessions=0 with generic ephemeral support is cold, not unwakeable (defect #1)")
		}
	})

	t.Run("min zero and not ephemeral-capable is unwakeable", func(t *testing.T) {
		zero := 0
		cfg := &config.City{
			Agents: []config.Agent{{
				Name:              "worker",
				MinActiveSessions: &zero,
				MaxActiveSessions: &zero,
			}},
		}
		if !routedTemplateIsUnwakeable(cfg, nil, "worker") {
			t.Fatalf("routedTemplateIsUnwakeable() = false, want true: no minimum and no ephemeral capacity is genuinely stranded")
		}
	})

	t.Run("live slotted pool-managed session is wakeable", func(t *testing.T) {
		cfg := &config.City{}
		infos := []session.Info{{
			ID:                  "sess-1",
			Template:            "polecat",
			AgentName:           "polecat",
			PoolManaged:         true,
			PoolSlot:            "3",
			SessionNameMetadata: "polecat-3",
		}}
		sessionBeads := newSessionBeadSnapshotFromInfos(infos)
		if routedTemplateIsUnwakeable(cfg, sessionBeads, "polecat") {
			t.Fatalf("routedTemplateIsUnwakeable(%q) = true, want false: a live slotted pool-managed session already serves the template (defect #3)", "polecat")
		}
	})
}

// TestStrandedRoutedDemandReconcileTickDefaultPolicyIsOffAndSilent covers
// corrected design item 5: the gate ships defaulted to Off, not Auto. With
// StrandedRoutePolicy left unset, a stranded routed-demand bead must produce
// no routed_demand.stranded event and no OrderFailed mirror.
func TestStrandedRoutedDemandReconcileTickDefaultPolicyIsOffAndSilent(t *testing.T) {
	bead := strandedOrderPoolDemandWisp()
	bead.CreatedAt = time.Now().UTC().Add(-2 * time.Minute)
	store := beads.NewMemStoreFrom(0, []beads.Bead{bead}, nil)
	cfg := deadAssigneeDemandConfig(0) // StrandedRoutePolicy left unset: must resolve to the Off default
	rec := events.NewFake()
	cr := strandedDemandRuntime(t, store, rec, cfg)

	cr.beadReconcileTick(context.Background(), strandedDemandResult(), newSessionBeadSnapshot(nil), nil, false)

	if got := len(routedDemandStrandedEvents(rec)); got != 0 {
		t.Fatalf("stranded events with unset policy = %d, want 0 (default must be Off); events=%+v", got, rec.Events)
	}
	if countOrderFailedEvents(rec) > 0 {
		t.Fatalf("unexpected %s with unset policy (default must be Off); events=%+v", events.OrderFailed, rec.Events)
	}
	assertBeadStillReadyAndUngated(t, store, bead.ID)
}

// TestStrandedRoutedDemandReconcileTickAutoModeWarnsForAllWriters is the
// original PR #4607 default-mode coverage across every routed-demand
// writer shape, adapted to set StrandedRoutePolicy=auto explicitly now that
// Off (not Auto) is the zero-value default (item 5).
func TestStrandedRoutedDemandReconcileTickAutoModeWarnsForAllWriters(t *testing.T) {
	for _, tc := range []struct {
		name string
		bead beads.Bead
	}{
		{
			name: "sling style",
			bead: strandedRoutedWorkBead("ga-sling"),
		},
		{
			name: "direct metadata",
			bead: strandedRoutedWorkBead("ga-direct"),
		},
		{
			name: "order dispatch pool-demand wisp",
			bead: strandedOrderPoolDemandWisp(),
		},
		{
			name: "gh-3872 incident-5 graph.v2 drain-unit member",
			bead: strandedGraphV2DrainUnitMember("ga-f4tu7c-incident-5", "worker"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bead := tc.bead
			bead.CreatedAt = time.Now().UTC().Add(-2 * time.Minute)
			store := beads.NewMemStoreFrom(0, []beads.Bead{bead}, nil)
			cfg := deadAssigneeDemandConfig(0)
			setDemandConfigString(t, cfg, "StrandedRoutePolicy", "auto")
			rec := events.NewFake()
			cr := strandedDemandRuntime(t, store, rec, cfg)

			cr.beadReconcileTick(context.Background(), strandedDemandResult(), newSessionBeadSnapshot(nil), nil, false)

			ev := requireOneRoutedDemandStrandedEvent(t, rec)
			payload := requireEventPayload(t, ev)
			requirePayloadString(t, payload, "severity", "warning")
			requirePayloadString(t, payload, "template", "worker")
			requirePayloadIncludesBeadID(t, payload, bead.ID)
			if countOrderFailedEvents(rec) > 0 {
				t.Fatalf("Auto policy emitted %s, want warning-only routed-demand event", events.OrderFailed)
			}
			assertBeadStillReadyAndUngated(t, store, bead.ID)
		})
	}
}

func TestStrandedRoutedDemandReconcileTickPolicyModes(t *testing.T) {
	for _, tc := range []struct {
		name            string
		mode            string
		wantStranded    bool
		wantSeverity    string
		wantOrderFailed bool
	}{
		{name: "off kill switch is silent", mode: "off"},
		{name: "auto warns only", mode: "auto", wantStranded: true, wantSeverity: "warning"},
		{name: "require fails and mirrors order failure", mode: "require", wantStranded: true, wantSeverity: "failure", wantOrderFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bead := strandedOrderPoolDemandWisp()
			bead.CreatedAt = time.Now().UTC().Add(-2 * time.Minute)
			store := beads.NewMemStoreFrom(0, []beads.Bead{bead}, nil)
			cfg := deadAssigneeDemandConfig(0)
			setDemandConfigString(t, cfg, "StrandedRoutePolicy", tc.mode)
			rec := events.NewFake()
			cr := strandedDemandRuntime(t, store, rec, cfg)

			cr.beadReconcileTick(context.Background(), strandedDemandResult(), newSessionBeadSnapshot(nil), nil, false)

			gotEvents := routedDemandStrandedEvents(rec)
			if !tc.wantStranded {
				if len(gotEvents) != 0 {
					t.Fatalf("stranded events = %d, want 0 when policy=%s", len(gotEvents), tc.mode)
				}
				if countOrderFailedEvents(rec) > 0 {
					t.Fatalf("unexpected %s when policy=%s", events.OrderFailed, tc.mode)
				}
				assertBeadStillReadyAndUngated(t, store, bead.ID)
				return
			}
			if len(gotEvents) != 1 {
				t.Fatalf("stranded events = %d, want 1 when policy=%s; events=%+v", len(gotEvents), tc.mode, rec.Events)
			}
			payload := requireEventPayload(t, gotEvents[0])
			requirePayloadString(t, payload, "severity", tc.wantSeverity)
			requirePayloadIncludesBeadID(t, payload, bead.ID)
			if has := countOrderFailedEvents(rec) > 0; has != tc.wantOrderFailed {
				t.Fatalf("has %s = %v, want %v for policy=%s; events=%+v", events.OrderFailed, has, tc.wantOrderFailed, tc.mode, rec.Events)
			}
			assertBeadStillReadyAndUngated(t, store, bead.ID)
		})
	}
}

func TestStrandedRoutedDemandReconcileTickDebounceAndExclusions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bead      func(now time.Time) beads.Bead
		wantEvent bool
	}{
		{
			name: "just under debounce is silent",
			bead: func(now time.Time) beads.Bead {
				b := strandedRoutedWorkBead("ga-fresh")
				b.CreatedAt = now.Add(-59 * time.Second)
				return b
			},
		},
		{
			name: "just over debounce emits",
			bead: func(now time.Time) beads.Bead {
				b := strandedRoutedWorkBead("ga-stale")
				b.CreatedAt = now.Add(-61 * time.Second)
				return b
			},
			wantEvent: true,
		},
		{
			name: "molecule without pool-demand flag remains excluded",
			bead: func(now time.Time) beads.Bead {
				return beads.Bead{
					ID:        "mol-no-demand",
					Title:     "workflow container without pool demand",
					Type:      "molecule",
					Status:    "open",
					CreatedAt: now.Add(-2 * time.Minute),
					Metadata:  map[string]string{"gc.routed_to": "worker"},
				}
			},
		},
		{
			name: "blocked routed task remains excluded",
			bead: func(now time.Time) beads.Bead {
				b := strandedRoutedWorkBead("ga-blocked")
				b.CreatedAt = now.Add(-2 * time.Minute)
				return b
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			bead := tc.bead(now)
			store := beads.NewMemStoreFrom(0, []beads.Bead{bead}, nil)
			if bead.ID == "ga-blocked" {
				blocker, err := store.Create(beads.Bead{ID: "ga-blocker", Title: "blocker", Type: "task", Status: "open"})
				if err != nil {
					t.Fatalf("create blocker: %v", err)
				}
				if err := store.DepAdd(bead.ID, blocker.ID, "blocks"); err != nil {
					t.Fatalf("add blocking dependency: %v", err)
				}
			}
			cfg := deadAssigneeDemandConfig(0)
			setDemandConfigString(t, cfg, "StrandedRoutePolicy", "auto")
			setDemandConfigString(t, cfg, "StrandedRouteDebounce", "1m")
			rec := events.NewFake()
			cr := strandedDemandRuntime(t, store, rec, cfg)

			cr.beadReconcileTick(context.Background(), strandedDemandResult(), newSessionBeadSnapshot(nil), nil, false)

			got := len(routedDemandStrandedEvents(rec))
			if tc.wantEvent && got != 1 {
				t.Fatalf("stranded events = %d, want 1; events=%+v", got, rec.Events)
			}
			if !tc.wantEvent && got != 0 {
				t.Fatalf("stranded events = %d, want 0; events=%+v", got, rec.Events)
			}
			assertBeadStillReadyAndUngated(t, store, bead.ID)
		})
	}
}

// TestStrandedRoutedDemandReconcileTickSecondTickStaysSilent covers the
// reviewer's ga-o3ko1j.4.4 finding: the reconciler ticks every PatrolInterval
// (default 30s) and a stranded route is, by this feature's own premise,
// expected to persist for hours or days. Without a persisted throttle, every
// tick would re-emit routed_demand.stranded and, under Require, re-call
// orders.MarkFailed + re-emit events.OrderFailed — indefinitely, unboundedly,
// against a live Dolt store. This asserts a second consecutive tick against
// the same still-stranded bead produces neither.
func TestStrandedRoutedDemandReconcileTickSecondTickStaysSilent(t *testing.T) {
	for _, tc := range []struct {
		name            string
		mode            string
		wantOrderFailed bool
	}{
		{name: "auto: second tick emits no new warning", mode: "auto"},
		{name: "require: second tick emits no new failure or OrderFailed", mode: "require", wantOrderFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bead := strandedOrderPoolDemandWisp()
			bead.CreatedAt = time.Now().UTC().Add(-2 * time.Minute)
			store := beads.NewMemStoreFrom(0, []beads.Bead{bead}, nil)
			cfg := deadAssigneeDemandConfig(0)
			setDemandConfigString(t, cfg, "StrandedRoutePolicy", tc.mode)
			rec := events.NewFake()
			cr := strandedDemandRuntime(t, store, rec, cfg)

			cr.beadReconcileTick(context.Background(), strandedDemandResult(), newSessionBeadSnapshot(nil), nil, false)

			if got := len(routedDemandStrandedEvents(rec)); got != 1 {
				t.Fatalf("stranded events after first tick = %d, want 1; events=%+v", got, rec.Events)
			}
			firstOrderFailed := countOrderFailedEvents(rec)
			if tc.wantOrderFailed && firstOrderFailed != 1 {
				t.Fatalf("%s count after first tick = %d, want 1; events=%+v", events.OrderFailed, firstOrderFailed, rec.Events)
			}

			cr.beadReconcileTick(context.Background(), strandedDemandResult(), newSessionBeadSnapshot(nil), nil, false)

			if got := len(routedDemandStrandedEvents(rec)); got != 1 {
				t.Fatalf("stranded events after second tick = %d, want 1 (a persisting condition must stay silent on repeat ticks); events=%+v", got, rec.Events)
			}
			if got := countOrderFailedEvents(rec); got != firstOrderFailed {
				t.Fatalf("%s count after second tick = %d, want unchanged from %d (must not re-fire every tick); events=%+v", events.OrderFailed, got, firstOrderFailed, rec.Events)
			}
			assertBeadStillReadyAndUngated(t, store, bead.ID)
		})
	}
}

// TestDetectStrandedRoutedDemandEscalatesAfterThreshold drives
// detectStrandedRoutedDemand directly (bypassing beadReconcileTick) so it can
// control now precisely: first-sight emits escalated=false, the same
// condition sitting stranded within the escalation window stays silent, and
// only once it crosses routedDemandStrandedEscalationAge does exactly one
// more emission fire with escalated=true — after which it goes silent again
// even though the bead is still stranded. Require mode's MarkFailed/
// OrderFailed mirror must follow the identical cadence.
func TestDetectStrandedRoutedDemandEscalatesAfterThreshold(t *testing.T) {
	bead := strandedOrderPoolDemandWisp()
	base := time.Now().UTC().Add(-3 * time.Hour)
	bead.CreatedAt = base
	store := beads.NewMemStoreFrom(0, []beads.Bead{bead}, nil)
	cfg := deadAssigneeDemandConfig(0)
	setDemandConfigString(t, cfg, "StrandedRoutePolicy", "require")
	rec := events.NewFake()

	t1 := base.Add(90 * time.Second) // clears the default 60s debounce
	if err := detectStrandedRoutedDemand(store, cfg, newSessionBeadSnapshot(nil), rec, io.Discard, t1); err != nil {
		t.Fatalf("detect (first sight): %v", err)
	}
	first := requireOneRoutedDemandStrandedEvent(t, rec)
	firstPayload := requireEventPayload(t, first)
	requirePayloadBool(t, firstPayload, "escalated", false)
	requirePayloadString(t, firstPayload, "first_seen", t1.UTC().Format(time.RFC3339))
	if got := countOrderFailedEvents(rec); got != 1 {
		t.Fatalf("%s count after first sight = %d, want 1; events=%+v", events.OrderFailed, got, rec.Events)
	}

	t2 := t1.Add(10 * time.Minute) // well inside the escalation window
	if err := detectStrandedRoutedDemand(store, cfg, newSessionBeadSnapshot(nil), rec, io.Discard, t2); err != nil {
		t.Fatalf("detect (mid-window): %v", err)
	}
	if got := len(routedDemandStrandedEvents(rec)); got != 1 {
		t.Fatalf("stranded events mid-window = %d, want 1 (still throttled); events=%+v", got, rec.Events)
	}
	if got := countOrderFailedEvents(rec); got != 1 {
		t.Fatalf("%s count mid-window = %d, want 1 (still throttled); events=%+v", events.OrderFailed, got, rec.Events)
	}

	t3 := t1.Add(routedDemandStrandedEscalationAge + time.Minute) // just past the threshold
	if err := detectStrandedRoutedDemand(store, cfg, newSessionBeadSnapshot(nil), rec, io.Discard, t3); err != nil {
		t.Fatalf("detect (escalation): %v", err)
	}
	escalatedEvents := routedDemandStrandedEvents(rec)
	if len(escalatedEvents) != 2 {
		t.Fatalf("stranded events after escalation window = %d, want 2; events=%+v", len(escalatedEvents), rec.Events)
	}
	escalatedPayload := requireEventPayload(t, escalatedEvents[1])
	requirePayloadBool(t, escalatedPayload, "escalated", true)
	requirePayloadString(t, escalatedPayload, "first_seen", t1.UTC().Format(time.RFC3339))
	if got := countOrderFailedEvents(rec); got != 2 {
		t.Fatalf("%s count after escalation = %d, want 2; events=%+v", events.OrderFailed, got, rec.Events)
	}

	t4 := t3.Add(routedDemandStrandedEscalationAge) // long past, still stranded: fully throttled now
	if err := detectStrandedRoutedDemand(store, cfg, newSessionBeadSnapshot(nil), rec, io.Discard, t4); err != nil {
		t.Fatalf("detect (post-escalation): %v", err)
	}
	if got := len(routedDemandStrandedEvents(rec)); got != 2 {
		t.Fatalf("stranded events post-escalation = %d, want 2 (both allotted emissions spent); events=%+v", got, rec.Events)
	}
	if got := countOrderFailedEvents(rec); got != 2 {
		t.Fatalf("%s count post-escalation = %d, want 2 (both allotted emissions spent); events=%+v", events.OrderFailed, got, rec.Events)
	}
}

// TestDetectStrandedRoutedDemandClearsMarkersOnRecovery covers the third leg
// of the throttle contract: once a template becomes wakeable again, its
// beads' throttle markers must clear, so a later recurrence of the same
// stranded condition is treated as a fresh first-sight rather than staying
// silenced forever by markers from the prior stranded episode.
func TestDetectStrandedRoutedDemandClearsMarkersOnRecovery(t *testing.T) {
	bead := strandedRoutedWorkBead("ga-recovers")
	base := time.Now().UTC().Add(-2 * time.Hour)
	bead.CreatedAt = base
	store := beads.NewMemStoreFrom(0, []beads.Bead{bead}, nil)
	deadCfg := deadAssigneeDemandConfig(0)
	setDemandConfigString(t, deadCfg, "StrandedRoutePolicy", "auto")
	rec := events.NewFake()

	t1 := base.Add(90 * time.Second)
	if err := detectStrandedRoutedDemand(store, deadCfg, newSessionBeadSnapshot(nil), rec, io.Discard, t1); err != nil {
		t.Fatalf("detect (stranded): %v", err)
	}
	requireOneRoutedDemandStrandedEvent(t, rec)

	stranded, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("get %s: %v", bead.ID, err)
	}
	if strings.TrimSpace(stranded.Metadata[beadmeta.RoutedDemandStrandedFirstSeenMetadataKey]) == "" {
		t.Fatalf("bead %s missing first-seen marker after stranded detection", bead.ID)
	}

	wakeableCfg := deadAssigneeDemandConfig(1) // max_active_sessions=1: agent supports a generic ephemeral session again
	setDemandConfigString(t, wakeableCfg, "StrandedRoutePolicy", "auto")
	t2 := t1.Add(time.Minute)
	if err := detectStrandedRoutedDemand(store, wakeableCfg, newSessionBeadSnapshot(nil), rec, io.Discard, t2); err != nil {
		t.Fatalf("detect (recovered): %v", err)
	}
	if got := len(routedDemandStrandedEvents(rec)); got != 1 {
		t.Fatalf("stranded events after recovery = %d, want 1 (no new emission while wakeable); events=%+v", got, rec.Events)
	}

	recovered, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("get %s: %v", bead.ID, err)
	}
	if v := strings.TrimSpace(recovered.Metadata[beadmeta.RoutedDemandStrandedFirstSeenMetadataKey]); v != "" {
		t.Fatalf("bead %s first-seen marker = %q, want cleared after recovery", bead.ID, v)
	}
	if v := strings.TrimSpace(recovered.Metadata[beadmeta.RoutedDemandStrandedEscalatedAtMetadataKey]); v != "" {
		t.Fatalf("bead %s escalated-at marker = %q, want cleared after recovery", bead.ID, v)
	}

	t3 := t2.Add(time.Minute) // re-stranded: must be treated as a fresh first-sight, not suppressed
	if err := detectStrandedRoutedDemand(store, deadCfg, newSessionBeadSnapshot(nil), rec, io.Discard, t3); err != nil {
		t.Fatalf("detect (re-stranded): %v", err)
	}
	got := routedDemandStrandedEvents(rec)
	if len(got) != 2 {
		t.Fatalf("stranded events after re-stranding = %d, want 2 (fresh first-sight, not suppressed); events=%+v", len(got), rec.Events)
	}
	secondPayload := requireEventPayload(t, got[1])
	requirePayloadBool(t, secondPayload, "escalated", false)
	requirePayloadString(t, secondPayload, "first_seen", t3.UTC().Format(time.RFC3339))
}

// TestDetectStrandedRoutedDemandEmitsOnlyThrottledBeadIDs covers
// architecture-review defect #4 on ga-wnqjoz: detectStrandedRoutedDemand
// computes the emitted event's bead_ids from the full per-template candidate
// group, while failStrandedOrderRuns (under Require) only ever acts on the
// throttled signal.beads subset. When part of a group is silenced by the
// throttle (already seen this episode, not yet due to escalate), the event's
// bead_ids must mirror what was actually (re)signaled this tick, not beads
// the throttle is intentionally staying silent about.
func TestDetectStrandedRoutedDemandEmitsOnlyThrottledBeadIDs(t *testing.T) {
	base := time.Now().UTC().Add(-2 * time.Hour)

	fresh := strandedRoutedWorkBead("ga-fresh-throttle")
	fresh.CreatedAt = base

	silenced := strandedRoutedWorkBead("ga-silenced-throttle")
	silenced.CreatedAt = base
	silenced.Metadata[beadmeta.RoutedDemandStrandedFirstSeenMetadataKey] = base.Add(time.Minute).UTC().Format(time.RFC3339)

	store := beads.NewMemStoreFrom(0, []beads.Bead{fresh, silenced}, nil)
	cfg := deadAssigneeDemandConfig(0)
	setDemandConfigString(t, cfg, "StrandedRoutePolicy", "auto")
	rec := events.NewFake()

	// now clears the default 60s debounce for both beads; silenced's
	// first-seen marker (base+1m) is only 9m old here, well inside the 30m
	// escalation window, so it stays throttled/silent this tick while fresh
	// (no marker at all) is newly signaled.
	now := base.Add(10 * time.Minute)
	if err := detectStrandedRoutedDemand(store, cfg, newSessionBeadSnapshot(nil), rec, io.Discard, now); err != nil {
		t.Fatalf("detect: %v", err)
	}

	ev := requireOneRoutedDemandStrandedEvent(t, rec)
	payload := requireEventPayload(t, ev)
	ids, ok := payload["bead_ids"].([]any)
	if !ok {
		t.Fatalf("payload[\"bead_ids\"] = %#v, want a JSON array", payload["bead_ids"])
	}
	if len(ids) != 1 {
		t.Fatalf("payload bead_ids = %v, want exactly 1 entry (only the freshly-signaled bead, not the still-throttled one); payload=%v", ids, payload)
	}
	if got, ok := ids[0].(string); !ok || got != fresh.ID {
		t.Fatalf("payload bead_ids = %v, want [%q]; payload=%v", ids, fresh.ID, payload)
	}
}

func deadAssigneeDemandConfig(maxSessions int) *config.City {
	return &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			MaxActiveSessions: &maxSessions,
			Provider:          "mock",
			StartCommand:      "true",
		}},
		Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
	}
}

func strandedRoutedWorkBead(id string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Title:    "stranded routed work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	}
}

func strandedOrderPoolDemandWisp() beads.Bead {
	return beads.Bead{
		ID:        "mol-worker-order",
		Title:     "order-dispatch pool-demand wisp",
		Type:      "molecule",
		Status:    "open",
		Ephemeral: true,
		Labels:    []string{"order-run:graph-drain"},
		Metadata:  strandedPoolDemandMetadata("worker"),
	}
}

func strandedPoolDemandMetadata(template string) map[string]string {
	metadata := map[string]string{"gc.routed_to": template}
	for k, v := range poolDemandMetadataPair() {
		metadata[k] = v
	}
	return metadata
}

func strandedGraphV2DrainUnitMember(id, template string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Title:  "GH #3872 incident #5 graph.v2 drain-unit member",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to":          template,
			"gc.kind":               "workflow",
			"gc.formula_contract":   "graph.v2",
			"gc.workflow_id":        "mol-drain-graph",
			"gc.workflow_member_id": "drain-unit-member",
		},
	}
}

func strandedDemandRuntime(t *testing.T, store beads.Store, rec *events.Fake, cfg *config.City) *CityRuntime {
	t.Helper()
	return &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "maintainer-city",
		cfg:                 cfg,
		sp:                  runtime.NewFake(),
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 rec,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
}

func strandedDemandResult() DesiredStateResult {
	return DesiredStateResult{
		State:             map[string]TemplateParams{},
		ScaleCheckCounts:  map[string]int{"worker": 0},
		PoolDesiredCounts: map[string]int{"worker": 0},
	}
}

func routedDemandStrandedEvents(rec *events.Fake) []events.Event {
	var out []events.Event
	for _, ev := range rec.Events {
		if ev.Type == routedDemandStrandedEventType {
			out = append(out, ev)
		}
	}
	return out
}

func requireOneRoutedDemandStrandedEvent(t *testing.T, rec *events.Fake) events.Event {
	t.Helper()
	got := routedDemandStrandedEvents(rec)
	if len(got) != 1 {
		t.Fatalf("stranded events = %d, want 1; events=%+v", len(got), rec.Events)
	}
	return got[0]
}

func requireEventPayload(t *testing.T, ev events.Event) map[string]any {
	t.Helper()
	if len(ev.Payload) == 0 {
		t.Fatalf("%s payload is empty; event=%+v", ev.Type, ev)
	}
	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal %s payload: %v; raw=%s", ev.Type, err, ev.Payload)
	}
	return payload
}

func requirePayloadString(t *testing.T, payload map[string]any, key, want string) {
	t.Helper()
	if got, ok := payload[key].(string); !ok || got != want {
		t.Fatalf("payload[%q] = %#v, want %q; payload=%v", key, payload[key], want, payload)
	}
}

func requirePayloadIncludesBeadID(t *testing.T, payload map[string]any, beadID string) {
	t.Helper()
	if got, ok := payload["bead_id"].(string); ok && got == beadID {
		return
	}
	if values, ok := payload["bead_ids"].([]any); ok {
		for _, value := range values {
			if got, ok := value.(string); ok && got == beadID {
				return
			}
		}
	}
	t.Fatalf("payload does not include bead id %q: %v", beadID, payload)
}

func countOrderFailedEvents(rec *events.Fake) int {
	n := 0
	for _, ev := range rec.Events {
		if ev.Type == events.OrderFailed {
			n++
		}
	}
	return n
}

func requirePayloadBool(t *testing.T, payload map[string]any, key string, want bool) {
	t.Helper()
	if got, ok := payload[key].(bool); !ok || got != want {
		t.Fatalf("payload[%q] = %#v, want %v; payload=%v", key, payload[key], want, payload)
	}
}

func assertBeadStillReadyAndUngated(t *testing.T, store beads.Store, beadID string) {
	t.Helper()
	got, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("get %s: %v", beadID, err)
	}
	if got.Status != "open" {
		t.Fatalf("bead %s status = %q, want open: %+v", beadID, got.Status, got)
	}
	if strings.TrimSpace(got.Assignee) != "" {
		t.Fatalf("bead %s assignee = %q, want empty", beadID, got.Assignee)
	}
	for _, label := range got.Labels {
		if strings.HasPrefix(label, "hold:") {
			t.Fatalf("bead %s labels = %v, want no fail-loud hold label", beadID, got.Labels)
		}
	}
}

func setDemandConfigString(t *testing.T, cfg *config.City, fieldName, value string) {
	t.Helper()
	root := reflect.ValueOf(cfg)
	if root.Kind() != reflect.Pointer || root.IsNil() {
		t.Fatalf("config must be a non-nil *config.City")
	}
	demand := root.Elem().FieldByName("Demand")
	if !demand.IsValid() {
		t.Fatalf("config.City missing Demand field; ga-o3ko1j.4.1 requires top-level [demand] config with %s", fieldName)
	}
	field := demand.FieldByName(fieldName)
	if !field.IsValid() {
		t.Fatalf("config.City.Demand missing %s field", fieldName)
	}
	if !field.CanSet() || field.Kind() != reflect.String {
		t.Fatalf("config.City.Demand.%s must be a settable string field, got kind=%s canSet=%v", fieldName, field.Kind(), field.CanSet())
	}
	field.SetString(value)
}
