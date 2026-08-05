package executionevent

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func TestLifecycleEventsPreserveNativeGraphIdentityAndTopology(t *testing.T) {
	root := beads.Bead{ID: "gcg-run", Metadata: map[string]string{
		beadmeta.KindMetadataKey: "workflow", beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
	}}
	rootDeps := "[]"
	fanoutDeps := `["root"]`
	joinDeps := `["fan-a","fan-b"]`
	for _, tc := range []struct {
		name, ref, step, topology string
		wantDeps                  *[]string
	}{
		{"root", "gcg-root-attempt", "root", rootDeps, lifecycleStrings([]string{})},
		{"fanout", "gcg-fan-a-attempt", "fan-a", fanoutDeps, lifecycleStrings([]string{"root"})},
		{"join", "gcg-join-attempt", "join", joinDeps, lifecycleStrings([]string{"fan-a", "fan-b"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := beads.Bead{ID: tc.ref, Status: "in_progress", Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: root.ID, beadmeta.StepIDMetadataKey: tc.step,
				beadmeta.SessionIDMetadataKey: "gcs-session", beadmeta.NativeStepDependenciesMetadataKey: tc.topology,
			}}
			started, ok := LifecycleEvent(events.ExecutionStepStarted, root, step, "worker")
			if !ok {
				t.Fatal("LifecycleEvent(started) = false")
			}
			if started.Type != events.ExecutionStepStarted || started.Subject != tc.ref || started.RunID != root.ID || started.SessionID != "gcs-session" || started.StepID != tc.step || !reflect.DeepEqual(started.DependsOnStepIDs, tc.wantDeps) {
				t.Fatalf("started = %#v", started)
			}
			step.Status = "closed"
			completed, ok := LifecycleEvent(events.ExecutionStepCompleted, root, step, "close-hook")
			if !ok || completed.Type != events.ExecutionStepCompleted || !reflect.DeepEqual(completed.DependsOnStepIDs, tc.wantDeps) {
				t.Fatalf("completed = %#v, ok=%v", completed, ok)
			}
		})
	}
}

func TestEmitCompletedFromClosedNotificationUsesPhysicalSnapshot(t *testing.T) {
	graph := beads.NewMemStore()
	root := mustCreateProjectionRoot(t, graph, "")
	step := mustCreateProjectionStep(t, graph, "gcg-retry-attempt", root.ID, "build", `["prepare"]`)
	step.Status = "closed"
	step.Metadata[beadmeta.SessionIDMetadataKey] = "gcs-session"
	payload, err := json.Marshal(step)
	if err != nil {
		t.Fatal(err)
	}
	rec := events.NewFake()
	if !EmitCompletedFromClosedNotification(rec, graph, payload, "close-hook") {
		t.Fatal("close notification did not emit completed")
	}
	if len(rec.Events) != 1 {
		t.Fatalf("events = %#v", rec.Events)
	}
	got := rec.Events[0]
	if got.Type != events.ExecutionStepCompleted || got.Subject != step.ID || got.RunID != root.ID || got.SessionID != "gcs-session" || got.StepID != "build" || !reflect.DeepEqual(got.DependsOnStepIDs, lifecycleStrings([]string{"prepare"})) {
		t.Fatalf("completed = %#v", got)
	}
	legacy := step
	legacy.Metadata[beadmeta.RootBeadIDMetadataKey] = "unknown"
	payload, _ = json.Marshal(legacy)
	if EmitCompletedFromClosedNotification(rec, graph, payload, "close-hook") {
		t.Fatal("unresolved close notification emitted")
	}
}

func TestReconcileCompletedRepairsMissingFactAndRetainsConflictingHistory(t *testing.T) {
	graph := beads.NewMemStore()
	root := mustCreateProjectionRoot(t, graph, "")
	step := mustCreateProjectionStep(t, graph, "gcg-attempt", root.ID, "build", `["prepare"]`)
	closed := "closed"
	if err := graph.Update(step.ID, beads.UpdateOpts{Status: &closed, Metadata: map[string]string{beadmeta.SessionIDMetadataKey: "gcs-session"}}); err != nil {
		t.Fatal(err)
	}
	recorder := events.NewFake()
	// This looks like an already-emitted lifecycle fact by subject, but its
	// session is stale. It must not suppress the authoritative correction.
	recorder.Record(events.Event{
		Type: events.ExecutionStepCompleted, Subject: step.ID, RunID: root.ID,
		SessionID: "gcs-stale", StepID: "build", DependsOnStepIDs: lifecycleStrings([]string{"prepare"}),
	})

	if got := ReconcileCompleted(recorder, beads.GraphStore{Store: graph}, "execution-reconcile"); got != 1 {
		t.Fatalf("ReconcileCompleted = %d, want 1 correction", got)
	}
	if got := ReconcileCompleted(recorder, beads.GraphStore{Store: graph}, "execution-reconcile"); got != 0 {
		t.Fatalf("second ReconcileCompleted = %d, want exact-fact no-op", got)
	}
	completed, err := recorder.List(events.Filter{Type: events.ExecutionStepCompleted, Subject: step.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 2 || completed[1].SessionID != "gcs-session" || completed[1].RunID != root.ID || completed[1].StepID != "build" {
		t.Fatalf("completed facts = %#v, want stale history plus authoritative correction", completed)
	}
}

func TestLifecycleEventRetainsUnknownAndRejectsNonNativeOrInvalidFacts(t *testing.T) {
	root := beads.Bead{ID: "gcg-run", Metadata: map[string]string{beadmeta.KindMetadataKey: "workflow", beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2}}
	base := beads.Bead{ID: "gcg-attempt", Status: "in_progress", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID, beadmeta.StepIDMetadataKey: "build", beadmeta.SessionIDMetadataKey: "gcs-session"}}
	got, ok := LifecycleEvent(events.ExecutionStepStarted, root, base, "worker")
	if !ok || got.DependsOnStepIDs != nil {
		t.Fatalf("unknown topology = %#v, ok=%v", got, ok)
	}
	for _, mutate := range []func(*beads.Bead){
		func(b *beads.Bead) { b.Metadata[beadmeta.SessionIDMetadataKey] = "" },
		func(b *beads.Bead) { b.Metadata[beadmeta.StepIDMetadataKey] = " " },
		func(b *beads.Bead) { b.Metadata[beadmeta.RootBeadIDMetadataKey] = "external-root" },
	} {
		step := base
		step.Metadata = map[string]string{}
		for k, v := range base.Metadata {
			step.Metadata[k] = v
		}
		mutate(&step)
		if _, ok := LifecycleEvent(events.ExecutionStepStarted, root, step, "worker"); ok {
			t.Fatalf("invalid step emitted: %#v", step)
		}
	}
	invalidTopology := base
	invalidTopology.Metadata = map[string]string{}
	for k, v := range base.Metadata {
		invalidTopology.Metadata[k] = v
	}
	invalidTopology.Metadata[beadmeta.NativeStepDependenciesMetadataKey] = `["build"]`
	if event, ok := LifecycleEvent(events.ExecutionStepStarted, root, invalidTopology, "worker"); !ok || event.DependsOnStepIDs != nil {
		t.Fatalf("malformed topology must degrade to unknown, got %#v ok=%v", event, ok)
	}
	legacy := root
	legacy.Metadata = map[string]string{beadmeta.KindMetadataKey: "workflow"}
	if _, ok := LifecycleEvent(events.ExecutionStepStarted, legacy, base, "worker"); ok {
		t.Fatal("v1 root emitted lifecycle event")
	}
	control := base
	control.Metadata = map[string]string{}
	for k, v := range base.Metadata {
		control.Metadata[k] = v
	}
	control.Metadata[beadmeta.KindMetadataKey] = "check"
	if _, ok := LifecycleEvent(events.ExecutionStepCompleted, root, control, "close-hook"); ok {
		t.Fatal("control close emitted lifecycle event")
	}
}

func lifecycleStrings(v []string) *[]string { return &v }
