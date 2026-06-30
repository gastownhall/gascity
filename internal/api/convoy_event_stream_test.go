package api

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func TestCityLifecycleEventsSharePayloadTypeForOneOfValidation(t *testing.T) {
	registered := events.RegisteredPayloadTypes()
	cityEvents := []string{
		events.CityCreated,
		events.CityUnregisterRequested,
	}

	firstType := reflect.TypeOf(registered[cityEvents[0]])
	if firstType == nil {
		t.Fatalf("%s payload type is not registered", cityEvents[0])
	}
	for _, eventType := range cityEvents[1:] {
		got := reflect.TypeOf(registered[eventType])
		if got != firstType {
			t.Fatalf("%s payload type = %v, want shared %v so EventPayload oneOf has a single city lifecycle branch", eventType, got, firstType)
		}
	}
}

func TestWorkflowEventScope(t *testing.T) {
	info := workflowStoreInfo{ref: "rig:alpha", scopeKind: "rig", scopeRef: "alpha"}

	scopeKind, scopeRef := workflowEventScope(info, beads.Bead{
		Metadata: map[string]string{
			"gc.scope_kind": "rig",
			"gc.scope_ref":  "beta",
		},
	}, "gascity")
	if scopeKind != "rig" || scopeRef != "beta" {
		t.Fatalf("explicit scope = %s:%s, want rig:beta", scopeKind, scopeRef)
	}

	scopeKind, scopeRef = workflowEventScope(info, beads.Bead{}, "gascity")
	if scopeKind != "city" || scopeRef != "gascity" {
		t.Fatalf("legacy scope = %s:%s, want city:gascity", scopeKind, scopeRef)
	}

	scopeKind, scopeRef = workflowEventScope(info, beads.Bead{}, "")
	if scopeKind != "city" || scopeRef != "city" {
		t.Fatalf("normalized city fallback scope = %s:%s, want city:city", scopeKind, scopeRef)
	}
}

func TestProjectWorkflowEventUsesRootStoreRefHint(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "gascity"
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore

	cityRoot, err := cityStore.Create(beads.Bead{
		Title: "City workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":        "workflow",
			"gc.workflow_id": "wf_city",
			"gc.scope_kind":  "city",
			"gc.scope_ref":   "gascity",
		},
	})
	if err != nil {
		t.Fatalf("Create(cityRoot): %v", err)
	}
	rigStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     cityRoot.ID,
		Title:  "Rig workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.workflow_id":    "wf_rig",
			"gc.scope_kind":     "rig",
			"gc.scope_ref":      "alpha",
			"gc.root_store_ref": "rig:alpha",
		},
	}}, nil)
	state.stores = map[string]beads.Store{"alpha": rigStore}

	child, err := rigStore.Create(beads.Bead{
		Title: "Rig step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id":    cityRoot.ID,
			"gc.root_store_ref":  "rig:alpha",
			"gc.logical_bead_id": "node-1",
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	payload, err := json.Marshal(child)
	if err != nil {
		t.Fatalf("Marshal(child): %v", err)
	}

	projection := projectWorkflowEvent(state, events.Event{
		Type:    events.BeadUpdated,
		Seq:     7,
		Ts:      time.Unix(1711300000, 0).UTC(),
		Payload: payload,
	})
	if projection == nil {
		t.Fatal("projection = nil, want workflow event")
	}
	if projection.WorkflowID != "wf_rig" {
		t.Fatalf("workflow_id = %q, want wf_rig", projection.WorkflowID)
	}
	if projection.RootStoreRef != "rig:alpha" {
		t.Fatalf("root_store_ref = %q, want rig:alpha", projection.RootStoreRef)
	}
	if projection.ScopeKind != "rig" || projection.ScopeRef != "alpha" {
		t.Fatalf("scope = %s:%s, want rig:alpha", projection.ScopeKind, projection.ScopeRef)
	}
}

func TestProjectWorkflowEventDropsAmbiguousLegacyWorkflowWithoutStoreHint(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "gascity"
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore

	cityRoot, err := cityStore.Create(beads.Bead{
		Title: "City workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":        "workflow",
			"gc.workflow_id": "wf_city",
			"gc.scope_kind":  "city",
			"gc.scope_ref":   "gascity",
		},
	})
	if err != nil {
		t.Fatalf("Create(cityRoot): %v", err)
	}
	rigStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     cityRoot.ID,
		Title:  "Legacy rig workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":        "workflow",
			"gc.workflow_id": "wf_rig_legacy",
		},
	}}, nil)
	state.stores = map[string]beads.Store{"alpha": rigStore}

	child, err := rigStore.Create(beads.Bead{
		Title: "Legacy rig step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id":    cityRoot.ID,
			"gc.logical_bead_id": "node-legacy",
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	payload, err := json.Marshal(child)
	if err != nil {
		t.Fatalf("Marshal(child): %v", err)
	}

	projection := projectWorkflowEvent(state, events.Event{
		Type:    events.BeadUpdated,
		Seq:     9,
		Ts:      time.Unix(1711300050, 0).UTC(),
		Payload: payload,
	})
	if projection != nil {
		t.Fatalf("projection = %+v, want nil for ambiguous legacy root lookup", projection)
	}
}

func TestProjectWorkflowEventUsesLegacyCityScopeForRigStoredWorkflow(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "gascity"
	state.cityBeadStore = beads.NewMemStore()
	rigStore := beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": rigStore}

	root, err := rigStore.Create(beads.Bead{
		Title: "Legacy workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":        "workflow",
			"gc.workflow_id": "wf_legacy",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := rigStore.Create(beads.Bead{
		Title: "Legacy step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id":    root.ID,
			"gc.root_store_ref":  "rig:alpha",
			"gc.logical_bead_id": "node-legacy",
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	payload, err := json.Marshal(child)
	if err != nil {
		t.Fatalf("Marshal(child): %v", err)
	}

	projection := projectWorkflowEvent(state, events.Event{
		Type:    events.BeadUpdated,
		Seq:     11,
		Ts:      time.Unix(1711300100, 0).UTC(),
		Payload: payload,
	})
	if projection == nil {
		t.Fatal("projection = nil, want workflow event")
	}
	if projection.RootStoreRef != "rig:alpha" {
		t.Fatalf("root_store_ref = %q, want rig:alpha", projection.RootStoreRef)
	}
	if projection.ScopeKind != "city" || projection.ScopeRef != "gascity" {
		t.Fatalf("scope = %s:%s, want city:gascity", projection.ScopeKind, projection.ScopeRef)
	}
	if !projection.RequiresResync {
		t.Fatal("requires_resync = false, want true for update event")
	}
	if len(projection.ChangedFields) != 1 || projection.ChangedFields[0] != "snapshot" {
		t.Fatalf("changed_fields = %v, want [snapshot]", projection.ChangedFields)
	}
	if projection.WatchGeneration != "pending" {
		t.Fatalf("watch_generation = %q, want pending", projection.WatchGeneration)
	}
}

func TestProjectWorkflowEventFallsBackToSubjectWhenPayloadMissing(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "gascity"
	state.cityBeadStore = beads.NewMemStore()
	rigStore := beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": rigStore}

	root, err := rigStore.Create(beads.Bead{
		Title: "Workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.workflow_id":    "wf_payloadless",
			"gc.scope_kind":     "rig",
			"gc.scope_ref":      "alpha",
			"gc.root_store_ref": "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := rigStore.Create(beads.Bead{
		Title: "Step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id":    root.ID,
			"gc.root_store_ref":  "rig:alpha",
			"gc.logical_bead_id": "node-1",
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	projection := projectWorkflowEvent(state, events.Event{
		Type:    events.BeadClosed,
		Seq:     13,
		Ts:      time.Unix(1711300200, 0).UTC(),
		Subject: child.ID,
	})
	if projection == nil {
		t.Fatal("projection = nil, want workflow event")
	}
	if projection.WorkflowID != "wf_payloadless" {
		t.Fatalf("workflow_id = %q, want wf_payloadless", projection.WorkflowID)
	}
	if projection.Bead.ID != child.ID {
		t.Fatalf("bead.id = %q, want %q", projection.Bead.ID, child.ID)
	}
}

func TestProjectWorkflowEventFallsBackToSubjectWhenPayloadIsNotWorkflowShaped(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "gascity"
	state.cityBeadStore = beads.NewMemStore()
	rigStore := beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": rigStore}

	root, err := rigStore.Create(beads.Bead{
		Title: "Workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.workflow_id":    "wf_subject_fallback",
			"gc.scope_kind":     "rig",
			"gc.scope_ref":      "alpha",
			"gc.root_store_ref": "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := rigStore.Create(beads.Bead{
		Title: "Step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id":    root.ID,
			"gc.root_store_ref":  "rig:alpha",
			"gc.logical_bead_id": "node-1",
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	payload, err := json.Marshal(beads.Bead{
		ID:    child.ID,
		Title: "Unrelated payload shape",
		Type:  "task",
	})
	if err != nil {
		t.Fatalf("Marshal(payload): %v", err)
	}

	projection := projectWorkflowEvent(state, events.Event{
		Type:    events.BeadUpdated,
		Seq:     14,
		Ts:      time.Unix(1711300250, 0).UTC(),
		Subject: child.ID,
		Payload: payload,
	})
	if projection == nil {
		t.Fatal("projection = nil, want workflow event")
	}
	if projection.WorkflowID != "wf_subject_fallback" {
		t.Fatalf("workflow_id = %q, want wf_subject_fallback", projection.WorkflowID)
	}
	if projection.Bead.ID != child.ID {
		t.Fatalf("bead.id = %q, want %q", projection.Bead.ID, child.ID)
	}
}

func TestProjectWorkflowEventDropsNonWorkflowRoot(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "gascity"
	state.cityBeadStore = beads.NewMemStore()
	rigStore := beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": rigStore}

	root, err := rigStore.Create(beads.Bead{
		Title: "Not a workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.scope_kind":     "rig",
			"gc.scope_ref":      "alpha",
			"gc.root_store_ref": "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := rigStore.Create(beads.Bead{
		Title: "Step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id":    root.ID,
			"gc.root_store_ref":  "rig:alpha",
			"gc.logical_bead_id": "node-1",
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	payload, err := json.Marshal(child)
	if err != nil {
		t.Fatalf("Marshal(child): %v", err)
	}

	projection := projectWorkflowEvent(state, events.Event{
		Type:    events.BeadUpdated,
		Seq:     15,
		Ts:      time.Unix(1711300300, 0).UTC(),
		Payload: payload,
	})
	if projection != nil {
		t.Fatalf("projection = %+v, want nil for non-workflow root", projection)
	}
}

// Correlation ids shared by the event-wire forwarding assertions below.
const (
	testEventRunID     = "run_abc"
	testEventSessionID = "sess_xyz"
	testEventStepID    = "step_42"
)

// TestEventWireBuildersForwardCorrelationFields guards the run_id/session_id/
// step_id copy that every wire builder makes from events.Event. The same
// three-field forwarding is duplicated across the list and SSE builders, so a
// future refactor could silently drop one — exactly the omission this fix
// closed. Each builder is exercised for both the Go struct field and the
// emitted JSON key.
func TestEventWireBuildersForwardCorrelationFields(t *testing.T) {
	base := events.Event{
		Seq:       7,
		Type:      "custom.correlation.test",
		Ts:        time.Unix(1711300000, 0).UTC(),
		Actor:     "cache-reconcile",
		RunID:     testEventRunID,
		SessionID: testEventSessionID,
		StepID:    testEventStepID,
	}
	tagged := events.TaggedEvent{Event: base, City: "gascity"}

	t.Run("toWireEvent", func(t *testing.T) {
		wire, ok := toWireEvent(base)
		if !ok {
			t.Fatal("toWireEvent ok = false, want true")
		}
		assertCorrelationFields(t, wire.RunID, wire.SessionID, wire.StepID)
		assertJSONCarriesCorrelation(t, wire)
	})

	t.Run("toWireTaggedEvent", func(t *testing.T) {
		wire, ok := toWireTaggedEvent(tagged)
		if !ok {
			t.Fatal("toWireTaggedEvent ok = false, want true")
		}
		assertCorrelationFields(t, wire.RunID, wire.SessionID, wire.StepID)
		assertJSONCarriesCorrelation(t, wire)
	})

	t.Run("wireEventFrom", func(t *testing.T) {
		env, err := wireEventFrom(base, nil)
		if err != nil {
			t.Fatalf("wireEventFrom: %v", err)
		}
		assertCorrelationFields(t, env.RunID, env.SessionID, env.StepID)
		assertJSONCarriesCorrelation(t, env)
	})

	t.Run("wireTaggedEventFrom", func(t *testing.T) {
		env, err := wireTaggedEventFrom(tagged, nil)
		if err != nil {
			t.Fatalf("wireTaggedEventFrom: %v", err)
		}
		assertCorrelationFields(t, env.RunID, env.SessionID, env.StepID)
		assertJSONCarriesCorrelation(t, env)
	})
}

// TestEventWireBuildersOmitEmptyCorrelationFields locks in the `omitempty`
// contract: events recorded without correlation ids (mail, session, and
// request-result paths carry empty run_id) must not emit the keys at all,
// so clients can distinguish "absent" from "present but empty".
func TestEventWireBuildersOmitEmptyCorrelationFields(t *testing.T) {
	base := events.Event{
		Seq:   8,
		Type:  "custom.correlation.empty",
		Ts:    time.Unix(1711300000, 0).UTC(),
		Actor: "session",
	}
	tagged := events.TaggedEvent{Event: base, City: "gascity"}

	wire, ok := toWireEvent(base)
	if !ok {
		t.Fatal("toWireEvent ok = false, want true")
	}
	assertJSONOmitsCorrelation(t, wire)

	taggedWire, ok := toWireTaggedEvent(tagged)
	if !ok {
		t.Fatal("toWireTaggedEvent ok = false, want true")
	}
	assertJSONOmitsCorrelation(t, taggedWire)

	env, err := wireEventFrom(base, nil)
	if err != nil {
		t.Fatalf("wireEventFrom: %v", err)
	}
	assertJSONOmitsCorrelation(t, env)

	taggedEnv, err := wireTaggedEventFrom(tagged, nil)
	if err != nil {
		t.Fatalf("wireTaggedEventFrom: %v", err)
	}
	assertJSONOmitsCorrelation(t, taggedEnv)
}

func assertCorrelationFields(t *testing.T, gotRun, gotSession, gotStep string) {
	t.Helper()
	if gotRun != testEventRunID {
		t.Errorf("RunID = %q, want %q", gotRun, testEventRunID)
	}
	if gotSession != testEventSessionID {
		t.Errorf("SessionID = %q, want %q", gotSession, testEventSessionID)
	}
	if gotStep != testEventStepID {
		t.Errorf("StepID = %q, want %q", gotStep, testEventStepID)
	}
}

func assertJSONCarriesCorrelation(t *testing.T, v any) {
	t.Helper()
	fields := marshalToJSONFields(t, v)
	for key, want := range map[string]string{"run_id": testEventRunID, "session_id": testEventSessionID, "step_id": testEventStepID} {
		raw, ok := fields[key]
		if !ok {
			t.Errorf("JSON missing %q", key)
			continue
		}
		var got string
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("unmarshal JSON %q: %v", key, err)
			continue
		}
		if got != want {
			t.Errorf("JSON %q = %q, want %q", key, got, want)
		}
	}
}

func assertJSONOmitsCorrelation(t *testing.T, v any) {
	t.Helper()
	fields := marshalToJSONFields(t, v)
	for _, key := range []string{"run_id", "session_id", "step_id"} {
		if _, ok := fields[key]; ok {
			t.Errorf("JSON carries %q for an empty value; want omitempty", key)
		}
	}
}

func marshalToJSONFields(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("Unmarshal wire JSON: %v", err)
	}
	return fields
}
