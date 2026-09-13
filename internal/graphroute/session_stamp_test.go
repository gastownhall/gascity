package graphroute

import (
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

// #2843: run/step beads must carry a durable session back-reference so
// consumers (the dashboard run-detail session + diff views) can resolve a
// step's session after the transient Assignee is cleared on close.

func TestApplyGraphRouteBinding_StampsSessionName(t *testing.T) {
	step := &formula.RecipeStep{ID: "s1", Metadata: map[string]string{}}
	ApplyGraphRouteBinding(step, GraphRouteBinding{QualifiedName: "worker", SessionName: "polecat-gc-123"})

	if got := step.Metadata["gc.session_name"]; got != "polecat-gc-123" {
		t.Errorf("gc.session_name = %q, want polecat-gc-123 (durable session back-ref)", got)
	}
	if step.Assignee != "polecat-gc-123" {
		t.Errorf("Assignee = %q, want polecat-gc-123", step.Assignee)
	}
	if step.Metadata["gc.routed_to"] != "worker" {
		t.Errorf("gc.routed_to = %q, want worker", step.Metadata["gc.routed_to"])
	}
}

func TestApplyGraphRouteBinding_PoolMetadataOnly_NoSessionName(t *testing.T) {
	// Pool agents resolve MetadataOnly — no concrete session at route time,
	// so no gc.session_name (it binds when a slot claims the step).
	step := &formula.RecipeStep{ID: "s1", Metadata: map[string]string{}}
	ApplyGraphRouteBinding(step, GraphRouteBinding{QualifiedName: "/home/ds/gascity/polecat", MetadataOnly: true})

	if _, ok := step.Metadata["gc.session_name"]; ok {
		t.Errorf("pool MetadataOnly step must not carry gc.session_name, got %q", step.Metadata["gc.session_name"])
	}
	if step.Assignee != "" {
		t.Errorf("pool Assignee = %q, want empty", step.Assignee)
	}
	if step.Metadata["gc.routed_to"] != "/home/ds/gascity/polecat" {
		t.Errorf("gc.routed_to = %q, want the pool target", step.Metadata["gc.routed_to"])
	}
}

func TestApplyGraphRouteBinding_DirectSession_StampsSessionID(t *testing.T) {
	step := &formula.RecipeStep{ID: "s1", Metadata: map[string]string{"gc.routed_to": "stale"}}
	ApplyGraphRouteBinding(step, GraphRouteBinding{DirectSessionID: "gc-abc123"})

	if got := step.Metadata["gc.session_id"]; got != "gc-abc123" {
		t.Errorf("gc.session_id = %q, want gc-abc123", got)
	}
	if step.Assignee != "gc-abc123" {
		t.Errorf("Assignee = %q, want gc-abc123", step.Assignee)
	}
	if _, ok := step.Metadata["gc.routed_to"]; ok {
		t.Errorf("direct-session step should drop gc.routed_to, still present")
	}
}

func TestApplyGraphControlRouteBinding_UsesRoutedQueue(t *testing.T) {
	step := &formula.RecipeStep{ID: "ctl", Metadata: map[string]string{}}
	ApplyGraphControlRouteBinding(step, GraphRouteBinding{
		QualifiedName: "gascity/control-dispatcher",
		SessionName:   "gascity--control-dispatcher",
	})

	if got := step.Metadata["gc.session_name"]; got != "" {
		t.Errorf("control gc.session_name = %q, want empty for routed control-dispatcher queue", got)
	}
	if step.Assignee != "" {
		t.Errorf("control Assignee = %q, want empty routed control-dispatcher queue", step.Assignee)
	}
	if got := step.Metadata["gc.routed_to"]; got != "gascity/control-dispatcher" {
		t.Errorf("control gc.routed_to = %q, want gascity/control-dispatcher", got)
	}
}
