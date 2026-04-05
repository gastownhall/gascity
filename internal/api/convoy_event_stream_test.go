package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

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
