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
)

func TestCityRuntimeBeadReconcileTickStrandedRoutedDemandAutoDefaultWarnsForAllWriters(t *testing.T) {
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
			rec := events.NewFake()
			cr := strandedDemandRuntime(t, store, rec, deadAssigneeDemandConfig(0))

			cr.beadReconcileTick(context.Background(), strandedDemandResult(), newSessionBeadSnapshot(nil), nil, false)

			ev := requireOneRoutedDemandStrandedEvent(t, rec)
			payload := requireEventPayload(t, ev)
			requirePayloadString(t, payload, "severity", "warning")
			requirePayloadString(t, payload, "template", "worker")
			requirePayloadIncludesBeadID(t, payload, bead.ID)
			if hasEventType(rec, events.OrderFailed) {
				t.Fatalf("Auto policy emitted %s, want warning-only routed-demand event", events.OrderFailed)
			}
			assertBeadStillReadyAndUngated(t, store, bead.ID)
		})
	}
}

func TestCityRuntimeBeadReconcileTickStrandedRoutedDemandPolicyModes(t *testing.T) {
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
				if hasEventType(rec, events.OrderFailed) {
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
			if has := hasEventType(rec, events.OrderFailed); has != tc.wantOrderFailed {
				t.Fatalf("has %s = %v, want %v for policy=%s; events=%+v", events.OrderFailed, has, tc.wantOrderFailed, tc.mode, rec.Events)
			}
			assertBeadStillReadyAndUngated(t, store, bead.ID)
		})
	}
}

func TestCityRuntimeBeadReconcileTickStrandedRoutedDemandDebounceAndExclusions(t *testing.T) {
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

// TestCityRuntimeBeadReconcileTickStrandedRoutedDemandSecondTickStaysSilent
// covers the reviewer's ga-o3ko1j.4.4 finding: the reconciler ticks every
// PatrolInterval (default 30s) and a stranded route is, by this feature's own
// premise, expected to persist for hours or days. Without a persisted
// throttle, every tick would re-emit routed_demand.stranded and, under
// Require, re-call orders.MarkFailed + re-emit events.OrderFailed —
// indefinitely, unboundedly, against a live Dolt store. This asserts a
// second consecutive tick against the same still-stranded bead produces
// neither.
func TestCityRuntimeBeadReconcileTickStrandedRoutedDemandSecondTickStaysSilent(t *testing.T) {
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

	wakeableCfg := deadAssigneeDemandConfig(1) // min_active_sessions=1: template is wakeable again
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

func deadAssigneeDemandConfig(minSessions int) *config.City {
	maxSessions := 1
	return &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			MaxActiveSessions: &maxSessions,
			MinActiveSessions: &minSessions,
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

func hasEventType(rec *events.Fake, eventType string) bool {
	for _, ev := range rec.Events {
		if ev.Type == eventType {
			return true
		}
	}
	return false
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
