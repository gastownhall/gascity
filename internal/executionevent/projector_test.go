package executionevent

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func TestProjectCurrentUsesOnlyTracksFromConvoyStore(t *testing.T) {
	graph := beads.NewMemStore()
	work := beads.NewMemStore()
	convoy := mustCreateProjectionBead(t, work, beads.Bead{ID: "mc-convoy", Type: "convoy"})
	tracked := mustCreateProjectionBead(t, work, beads.Bead{ID: "mc-tracked"})
	metadataOnly := mustCreateProjectionBead(t, work, beads.Bead{
		ID: "mc-metadata",
		Metadata: map[string]string{
			"legacy.tracking_convoy_id": convoy.ID,
		},
	})
	parentChild := mustCreateProjectionBead(t, work, beads.Bead{ID: "mc-parent-child"})
	if err := work.DepAdd(convoy.ID, tracked.ID, "tracks"); err != nil {
		t.Fatalf("add tracks edge: %v", err)
	}
	if err := work.DepAdd(convoy.ID, parentChild.ID, "parent-child"); err != nil {
		t.Fatalf("add parent-child edge: %v", err)
	}
	root := mustCreateProjectionRoot(t, graph, convoy.ID)

	got, err := ProjectCurrent(
		beads.GraphStore{Store: graph},
		beads.WorkStore{Store: work},
		root.ID,
	)
	if err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	want := []WorkAssociation{{WorkBeadID: tracked.ID, ExecutionRunID: root.ID}}
	if !reflect.DeepEqual(got.WorkAssociations, want) {
		t.Fatalf("work associations = %#v, want %#v (metadata=%s parent-child=%s)", got.WorkAssociations, want, metadataOnly.ID, parentChild.ID)
	}
}

func TestProjectCurrentAnchorsExactGenericSourceWithoutReplacingWorkAssociation(t *testing.T) {
	graph := beads.NewMemStore()
	work := beads.NewMemStore()
	convoy := mustCreateProjectionBead(t, work, beads.Bead{ID: "mc-convoy", Type: "convoy"})
	launch := beads.Bead{
		ID: "ga-k4e47",
		Metadata: map[string]string{
			beadmeta.SourceBeadIDMetadataKey: "mc-wkbj",
		},
	}
	root := mustCreateProjectionRoot(t, graph, convoy.ID)

	got, err := ProjectCurrent(beads.GraphStore{Store: graph}, beads.WorkStore{Store: projectionDepStore{
		Store:    projectionSourceStore{Store: work, requestedID: launch.ID, source: launch},
		convoyID: convoy.ID,
		deps:     []beads.Dep{{IssueID: convoy.ID, DependsOnID: launch.ID, Type: "tracks"}},
	}}, root.ID)
	if err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	if want := []WorkAssociation{{WorkBeadID: "ga-k4e47", ExecutionRunID: root.ID}}; !reflect.DeepEqual(got.WorkAssociations, want) {
		t.Fatalf("work associations = %#v, want %#v", got.WorkAssociations, want)
	}
	if want := []RunAnchor{{SourceBeadID: "mc-wkbj", ExecutionRunID: root.ID}}; !reflect.DeepEqual(got.RunAnchors, want) {
		t.Fatalf("run anchors = %#v, want %#v", got.RunAnchors, want)
	}
	if want := []events.Event{
		{Type: events.ExecutionWorkAssociated, Actor: "graph-projector", Subject: "ga-k4e47", RunID: root.ID},
		{Type: events.ExecutionRunAnchored, Actor: "graph-projector", Subject: "mc-wkbj", RunID: root.ID},
	}; !reflect.DeepEqual(got.Events("graph-projector"), want) {
		t.Fatalf("events = %#v, want %#v", got.Events("graph-projector"), want)
	}
}

func TestProjectCurrentAnchorsExactReadableSourceChainWithoutRootDeclaration(t *testing.T) {
	graph := beads.NewMemStore()
	work := beads.NewMemStore()
	convoy := mustCreateProjectionBead(t, work, beads.Bead{ID: "mc-convoy", Type: "convoy"})
	launch := mustCreateProjectionBead(t, work, beads.Bead{
		ID: "ga-k4e47",
		Metadata: map[string]string{
			beadmeta.SourceBeadIDMetadataKey: "mc-wkbj",
		},
	})
	if err := work.DepAdd(convoy.ID, launch.ID, "tracks"); err != nil {
		t.Fatalf("add tracks edge: %v", err)
	}
	root := mustCreateProjectionRoot(t, graph, convoy.ID)

	got, err := ProjectCurrent(beads.GraphStore{Store: graph}, beads.WorkStore{Store: work}, root.ID)
	if err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	want := []RunAnchor{{SourceBeadID: "mc-wkbj", ExecutionRunID: root.ID}}
	if !reflect.DeepEqual(got.RunAnchors, want) {
		t.Fatalf("run anchors = %#v, want %#v", got.RunAnchors, want)
	}
}

func TestProjectCurrentDoesNotAnchorAmbiguousLaunchAssociations(t *testing.T) {
	graph := beads.NewMemStore()
	work := beads.NewMemStore()
	convoy := mustCreateProjectionBead(t, work, beads.Bead{ID: "mc-convoy", Type: "convoy"})
	launch := mustCreateProjectionBead(t, work, beads.Bead{ID: "ga-k4e47", Metadata: map[string]string{beadmeta.SourceBeadIDMetadataKey: "mc-wkbj"}})
	other := mustCreateProjectionBead(t, work, beads.Bead{ID: "ga-other", Metadata: map[string]string{beadmeta.SourceBeadIDMetadataKey: "mc-wkbj"}})
	for _, member := range []beads.Bead{launch, other} {
		if err := work.DepAdd(convoy.ID, member.ID, "tracks"); err != nil {
			t.Fatalf("add tracks edge: %v", err)
		}
	}
	root := mustCreateProjectionRoot(t, graph, convoy.ID)
	if err := graph.SetMetadata(root.ID, beadmeta.SourceBeadIDMetadataKey, launch.ID); err != nil {
		t.Fatalf("set root source: %v", err)
	}

	got, err := ProjectCurrent(beads.GraphStore{Store: graph}, beads.WorkStore{Store: work}, root.ID)
	if err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	if len(got.RunAnchors) != 0 {
		t.Fatalf("run anchors = %#v, want none for ambiguous launch associations", got.RunAnchors)
	}
}

func TestProjectCurrentAnchorsEachAssociatedLaunch(t *testing.T) {
	graph := beads.NewMemStore()
	work := beads.NewMemStore()
	convoy := mustCreateProjectionBead(t, work, beads.Bead{ID: "mc-convoy", Type: "convoy"})
	launchA := mustCreateProjectionBead(t, work, beads.Bead{ID: "ga-a", Metadata: map[string]string{beadmeta.SourceBeadIDMetadataKey: "mc-a"}})
	launchB := mustCreateProjectionBead(t, work, beads.Bead{ID: "ga-b", Metadata: map[string]string{beadmeta.SourceBeadIDMetadataKey: "mc-b"}})
	for _, member := range []beads.Bead{launchA, launchB} {
		if err := work.DepAdd(convoy.ID, member.ID, "tracks"); err != nil {
			t.Fatalf("add tracks edge: %v", err)
		}
	}
	root := mustCreateProjectionRoot(t, graph, convoy.ID)
	if err := graph.SetMetadata(root.ID, beadmeta.SourceBeadIDMetadataKey, launchA.ID); err != nil {
		t.Fatalf("set root source: %v", err)
	}
	got, err := ProjectCurrent(beads.GraphStore{Store: graph}, beads.WorkStore{Store: work}, root.ID)
	if err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	want := []RunAnchor{{SourceBeadID: "mc-a", ExecutionRunID: root.ID}, {SourceBeadID: "mc-b", ExecutionRunID: root.ID}}
	if !reflect.DeepEqual(got.RunAnchors, want) {
		t.Fatalf("run anchors = %#v, want %#v", got.RunAnchors, want)
	}
}

func TestProjectCurrentOmitsRunAnchorForInvalidOrAmbiguousSourceLinks(t *testing.T) {
	const launchID = "ga-k4e47"
	for _, tc := range []struct {
		name     string
		rootLink string
		launch   beads.Bead
		getErr   error
	}{
		{name: "invalid root link", rootLink: "ga k4e47"},
		{name: "unreadable launch", rootLink: launchID, getErr: errors.New("store unavailable")},
		{name: "mismatched launch identity", rootLink: launchID, launch: beads.Bead{ID: "ga-other", Metadata: map[string]string{beadmeta.SourceBeadIDMetadataKey: "mc-wkbj"}}},
		{name: "missing hosted source", rootLink: launchID, launch: beads.Bead{ID: launchID}},
		{name: "invalid hosted source", rootLink: launchID, launch: beads.Bead{ID: launchID, Metadata: map[string]string{beadmeta.SourceBeadIDMetadataKey: "mc wkbj"}}},
		{name: "non-exact hosted source", rootLink: launchID, launch: beads.Bead{ID: launchID, Metadata: map[string]string{beadmeta.SourceBeadIDMetadataKey: " mc-wkbj "}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := beads.NewMemStore()
			root := mustCreateProjectionRoot(t, graph, "mc-convoy")
			if err := graph.SetMetadata(root.ID, beadmeta.SourceBeadIDMetadataKey, tc.rootLink); err != nil {
				t.Fatalf("set root source: %v", err)
			}
			got, err := ProjectCurrent(beads.GraphStore{Store: graph}, beads.WorkStore{Store: projectionDepStore{
				Store: projectionSourceStore{
					Store:       beads.NewMemStore(),
					requestedID: launchID,
					source:      tc.launch,
					getErr:      tc.getErr,
				},
				convoyID: "mc-convoy",
				deps:     []beads.Dep{{IssueID: "mc-convoy", DependsOnID: launchID, Type: "tracks"}},
			}}, root.ID)
			if err != nil {
				t.Fatalf("ProjectCurrent: %v", err)
			}
			if len(got.RunAnchors) != 0 {
				t.Fatalf("run anchors = %#v, want none", got.RunAnchors)
			}
		})
	}
}

func TestProjectCurrentDoesNotInterpretPackMetadataAsRunAnchor(t *testing.T) {
	graph := beads.NewMemStore()
	work := beads.NewMemStore()
	convoy := mustCreateProjectionBead(t, work, beads.Bead{ID: "convoy", Type: "convoy"})
	launch := mustCreateProjectionBead(t, work, beads.Bead{
		ID: "launch",
		Metadata: map[string]string{
			"pr_review.city_source_bead_id": "source-work",
			"formula":                       "customer-pack-specific-formula",
		},
	})
	if err := work.DepAdd(convoy.ID, launch.ID, "tracks"); err != nil {
		t.Fatalf("add tracks edge: %v", err)
	}
	root := mustCreateProjectionRoot(t, graph, convoy.ID)

	got, err := ProjectCurrent(beads.GraphStore{Store: graph}, beads.WorkStore{Store: work}, root.ID)
	if err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	if len(got.RunAnchors) != 0 {
		t.Fatalf("run anchors = %#v, want none when only pack metadata names a source", got.RunAnchors)
	}
}

func TestProjectCurrentRetainsDanglingOpaqueTrackedID(t *testing.T) {
	graph := beads.NewMemStore()
	work := beads.NewMemStore()
	root := mustCreateProjectionRoot(t, graph, "mc-convoy")
	store := projectionDepStore{
		Store:    work,
		convoyID: "mc-convoy",
		deps: []beads.Dep{
			{IssueID: "mc-convoy", DependsOnID: "mc-dangling", Type: "tracks"},
			{IssueID: "mc-other", DependsOnID: "mc-wrong-source", Type: "tracks"},
			{IssueID: "mc-convoy", DependsOnID: "MC invalid", Type: "tracks"},
		},
	}

	got, err := ProjectCurrent(
		beads.GraphStore{Store: graph},
		beads.WorkStore{Store: store},
		root.ID,
	)
	if err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	want := []WorkAssociation{{WorkBeadID: "mc-dangling", ExecutionRunID: root.ID}}
	if !reflect.DeepEqual(got.WorkAssociations, want) {
		t.Fatalf("work associations = %#v, want %#v", got.WorkAssociations, want)
	}
}

func TestProjectCurrentSortsFactsAndPreservesPhysicalAttempts(t *testing.T) {
	graph := beads.NewMemStore()
	work := beads.NewMemStore()
	root := mustCreateProjectionRoot(t, graph, "mc-convoy")
	stepZ := mustCreateProjectionStep(t, graph, "gcg-step-z", root.ID, "build", `["prepare"]`)
	stepA := mustCreateProjectionStep(t, graph, "gcg-step-a", root.ID, "build", `["prepare"]`)
	closed := "closed"
	if err := graph.Update(stepZ.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close physical attempt: %v", err)
	}
	store := projectionDepStore{
		Store:    work,
		convoyID: "mc-convoy",
		deps: []beads.Dep{
			{IssueID: "mc-convoy", DependsOnID: "mc-work-z", Type: "tracks"},
			{IssueID: "mc-convoy", DependsOnID: "mc-work-a", Type: "tracks"},
			{IssueID: "mc-convoy", DependsOnID: "mc-work-z", Type: "tracks"},
		},
	}

	got, err := ProjectCurrent(
		beads.GraphStore{Store: graph},
		beads.WorkStore{Store: store},
		root.ID,
	)
	if err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	wantWork := []WorkAssociation{
		{WorkBeadID: "mc-work-a", ExecutionRunID: root.ID},
		{WorkBeadID: "mc-work-z", ExecutionRunID: root.ID},
	}
	if !reflect.DeepEqual(got.WorkAssociations, wantWork) {
		t.Fatalf("work associations = %#v, want %#v", got.WorkAssociations, wantWork)
	}
	wantSteps := []StepDefinition{
		{BeadID: stepA.ID, ExecutionRunID: root.ID, StepID: "build", DependsOnStepIDs: projectionStringsPtr([]string{"prepare"})},
		{BeadID: stepZ.ID, ExecutionRunID: root.ID, StepID: "build", DependsOnStepIDs: projectionStringsPtr([]string{"prepare"})},
	}
	sort.Slice(wantSteps, func(i, j int) bool { return wantSteps[i].BeadID < wantSteps[j].BeadID })
	if !reflect.DeepEqual(got.Steps, wantSteps) {
		t.Fatalf("steps = %#v, want %#v", got.Steps, wantSteps)
	}
}

func TestProjectCurrentMissingInputConvoyStillProjectsSteps(t *testing.T) {
	graph := beads.NewMemStore()
	root := mustCreateProjectionRoot(t, graph, "")
	step := mustCreateProjectionStep(t, graph, "gcg-step", root.ID, "build", "[]")

	got, err := ProjectCurrent(
		beads.GraphStore{Store: graph},
		beads.WorkStore{},
		root.ID,
	)
	if err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	if len(got.WorkAssociations) != 0 {
		t.Fatalf("work associations = %#v, want none", got.WorkAssociations)
	}
	want := []StepDefinition{{
		BeadID:           step.ID,
		ExecutionRunID:   root.ID,
		StepID:           "build",
		DependsOnStepIDs: projectionStringsPtr([]string{}),
	}}
	if !reflect.DeepEqual(got.Steps, want) {
		t.Fatalf("steps = %#v, want %#v", got.Steps, want)
	}
}

func TestProjectCurrentPreservesTopologyTriState(t *testing.T) {
	graph := beads.NewMemStore()
	root := mustCreateProjectionRoot(t, graph, "")
	invalid := mustCreateProjectionStep(t, graph, "gcg-step-invalid", root.ID, "invalid", `["z","a"]`)
	invalidWhitespace := mustCreateProjectionStep(t, graph, "gcg-step-invalid-whitespace", root.ID, "whitespace-dep", `[" "]`)
	known := mustCreateProjectionStep(t, graph, "gcg-step-known", root.ID, "known", `["root"]`)
	rootStep := mustCreateProjectionStep(t, graph, "gcg-step-root", root.ID, "root", "[]")
	unknown := mustCreateProjectionStep(t, graph, "gcg-step-unknown", root.ID, "unknown", "")
	mustCreateProjectionStep(t, graph, "gcg-step-blank-id", root.ID, " ", "[]")

	got, err := ProjectCurrent(beads.GraphStore{Store: graph}, beads.WorkStore{}, root.ID)
	if err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	want := []StepDefinition{
		{BeadID: invalid.ID, ExecutionRunID: root.ID, StepID: "invalid"},
		{BeadID: invalidWhitespace.ID, ExecutionRunID: root.ID, StepID: "whitespace-dep"},
		{BeadID: known.ID, ExecutionRunID: root.ID, StepID: "known", DependsOnStepIDs: projectionStringsPtr([]string{"root"})},
		{BeadID: rootStep.ID, ExecutionRunID: root.ID, StepID: "root", DependsOnStepIDs: projectionStringsPtr([]string{})},
		{BeadID: unknown.ID, ExecutionRunID: root.ID, StepID: "unknown"},
	}
	sort.Slice(want, func(i, j int) bool { return want[i].BeadID < want[j].BeadID })
	if !reflect.DeepEqual(got.Steps, want) {
		t.Fatalf("steps = %#v, want %#v", got.Steps, want)
	}
}

func TestProjectCurrentRejectsNonGraphV2Root(t *testing.T) {
	graph := beads.NewMemStore()
	plain := mustCreateProjectionBead(t, graph, beads.Bead{ID: "gcg-plain"})
	if _, err := ProjectCurrent(beads.GraphStore{Store: graph}, beads.WorkStore{}, plain.ID); err == nil {
		t.Fatal("ProjectCurrent accepted a non-graph.v2 root")
	}
}

func TestProjectionEventsPreserveFactsAndRepeatSnapshots(t *testing.T) {
	rootTopology := []string{}
	dependentTopology := []string{"root"}
	projection := Projection{
		WorkAssociations: []WorkAssociation{
			{WorkBeadID: "mc-a", ExecutionRunID: "gcg-root"},
			{WorkBeadID: "mc-b", ExecutionRunID: "gcg-root"},
		},
		Steps: []StepDefinition{
			{BeadID: "gcg-step-a", ExecutionRunID: "gcg-root", StepID: "root", DependsOnStepIDs: &rootTopology},
			{BeadID: "gcg-step-b", ExecutionRunID: "gcg-root", StepID: "build", DependsOnStepIDs: &dependentTopology},
		},
	}
	want := []events.Event{
		{Type: events.ExecutionWorkAssociated, Actor: "graph-projector", Subject: "mc-a", RunID: "gcg-root"},
		{Type: events.ExecutionWorkAssociated, Actor: "graph-projector", Subject: "mc-b", RunID: "gcg-root"},
		{Type: events.ExecutionStepDefined, Actor: "graph-projector", Subject: "gcg-step-a", RunID: "gcg-root", StepID: "root", DependsOnStepIDs: projectionStringsPtr([]string{})},
		{Type: events.ExecutionStepDefined, Actor: "graph-projector", Subject: "gcg-step-b", RunID: "gcg-root", StepID: "build", DependsOnStepIDs: projectionStringsPtr([]string{"root"})},
	}

	first := projection.Events("graph-projector")
	second := projection.Events("graph-projector")
	if !reflect.DeepEqual(first, want) || !reflect.DeepEqual(second, want) {
		t.Fatalf("repeated snapshot events = %#v / %#v, want %#v", first, second, want)
	}
	dependentTopology[0] = "mutated"
	if first[3].DependsOnStepIDs == projection.Steps[1].DependsOnStepIDs || (*first[3].DependsOnStepIDs)[0] != "root" {
		t.Fatalf("event retained mutable projector topology: %#v", first[3].DependsOnStepIDs)
	}
}

func TestEmitCurrentProjectsAndRecordsSnapshotFacts(t *testing.T) {
	graph := beads.NewMemStore()
	root := mustCreateProjectionRoot(t, graph, "")
	step := mustCreateProjectionStep(t, graph, "gcg-step", root.ID, "build", "[]")
	recorder := events.NewFake()

	if err := EmitCurrent(recorder, beads.GraphStore{Store: graph}, beads.WorkStore{}, root.ID, "formula-cook"); err != nil {
		t.Fatalf("EmitCurrent: %v", err)
	}

	if len(recorder.Events) != 1 {
		t.Fatalf("recorded events = %#v, want one", recorder.Events)
	}
	got := recorder.Events[0]
	if got.Type != events.ExecutionStepDefined || got.Actor != "formula-cook" || got.Subject != step.ID || got.RunID != root.ID || got.StepID != "build" {
		t.Fatalf("recorded event = %#v, want projected step fact", got)
	}
}

func TestEmitCurrentNilRecorderIsNoOp(t *testing.T) {
	if err := EmitCurrent(nil, beads.GraphStore{}, beads.WorkStore{}, "missing", "formula-cook"); err != nil {
		t.Fatalf("EmitCurrent with nil recorder: %v", err)
	}
}

// TestEmitCurrentEmitsStepDefinedOncePerStep pins the idempotent-restatement
// contract (ga-rd8le): EmitCurrent keeps re-projecting the full graph every
// tick, but step_defined is emitted exactly once per step against a durable
// per-step marker. A steady tick therefore restates nothing, while a step that
// is still unmarked — one just created, or one whose emit crashed before the
// marker landed — is re-emitted on the next tick, no matter which creator made
// it. That is convergence-via-restatement without threading created IDs.
func TestEmitCurrentEmitsStepDefinedOncePerStep(t *testing.T) {
	graph := beads.NewMemStore()
	root := mustCreateProjectionRoot(t, graph, "")
	stepA := mustCreateProjectionStep(t, graph, "gcg-step-a", root.ID, "build", "[]")
	stepB := mustCreateProjectionStep(t, graph, "gcg-step-b", root.ID, "test", `["build"]`)
	recorder := events.NewFake()

	// First tick emits step_defined for both unmarked steps and marks them.
	if err := EmitCurrent(recorder, beads.GraphStore{Store: graph}, beads.WorkStore{}, root.ID, "control-dispatch"); err != nil {
		t.Fatalf("EmitCurrent first tick: %v", err)
	}
	if got := countStepDefined(recorder.Events, root.ID); got != 2 {
		t.Fatalf("first tick step_defined = %d, want 2", got)
	}
	for _, step := range []beads.Bead{stepA, stepB} {
		reloaded, err := graph.Get(step.ID)
		if err != nil {
			t.Fatalf("reload step %s: %v", step.ID, err)
		}
		if reloaded.Metadata[beadmeta.StepDefinedEmittedMetadataKey] == "" {
			t.Fatalf("step %s was emitted but not durably marked", step.ID)
		}
	}

	// Second tick re-projects the same graph and must emit nothing: both steps
	// are now durably marked. On base (no marker) this re-emits the full graph.
	recorder.Events = nil
	if err := EmitCurrent(recorder, beads.GraphStore{Store: graph}, beads.WorkStore{}, root.ID, "control-dispatch"); err != nil {
		t.Fatalf("EmitCurrent second tick: %v", err)
	}
	if got := countStepDefined(recorder.Events, root.ID); got != 0 {
		t.Fatalf("second tick step_defined = %d, want 0 (idempotent restatement)", got)
	}

	// A step created after the first pass is unmarked and self-heals: only it
	// is emitted on the next tick, regardless of the steps already marked.
	stepC := mustCreateProjectionStep(t, graph, "gcg-step-c", root.ID, "ship", `["test"]`)
	recorder.Events = nil
	if err := EmitCurrent(recorder, beads.GraphStore{Store: graph}, beads.WorkStore{}, root.ID, "control-dispatch"); err != nil {
		t.Fatalf("EmitCurrent third tick: %v", err)
	}
	if got := countStepDefined(recorder.Events, root.ID); got != 1 {
		t.Fatalf("third tick step_defined = %d, want 1 (only the new step)", got)
	}
	if recorder.Events[0].Subject != stepC.ID {
		t.Fatalf("third tick emitted %q, want the new step %q", recorder.Events[0].Subject, stepC.ID)
	}
}

// TestEmitCurrentDoesNotMarkOnDroppedOrDiscardEmit pins CRITICAL 1 of the
// ga-rd8le council review: EmitCurrent must never durably mark a step
// (StepDefinedEmittedMetadataKey) after a best-effort emit that may have been
// dropped, or the step is skipped forever — a permanent miss.
// events.Recorder.Record is void: a FileRecorder drops the event on a lock
// timeout or a write error (ENOSPC), and events.Discard (which openCityRecorderAt
// returns on a recorder-open failure) drops every event. Marking after such an
// emit destroys the self-healing the level-triggered restatement provided, so
// the mark lands only once the emit is acknowledged durable; an unacknowledged
// tick leaves the step unmarked and it re-emits on the next healthy tick.
//
// RED on this PR's first commit, before the confirm-before-mark change: that
// revision marks unconditionally once graphStore.Store is non-nil, regardless
// of the recorder, so the first assertion (still unmarked after a Discard tick)
// fails there.
func TestEmitCurrentDoesNotMarkOnDroppedOrDiscardEmit(t *testing.T) {
	graph := beads.NewMemStore()
	root := mustCreateProjectionRoot(t, graph, "")
	step := mustCreateProjectionStep(t, graph, "gcg-step-a", root.ID, "build", "[]")
	graphStore := beads.GraphStore{Store: graph}

	assertUnmarked := func(when string) {
		t.Helper()
		reloaded, err := graph.Get(step.ID)
		if err != nil {
			t.Fatalf("reload step %s (%s): %v", step.ID, when, err)
		}
		if reloaded.Metadata[beadmeta.StepDefinedEmittedMetadataKey] != "" {
			t.Fatalf("step marked emitted after %s: a dropped/void emit must never mark", when)
		}
	}

	// events.Discard cannot acknowledge durability, so the void emit must not mark.
	if err := EmitCurrent(events.Discard, graphStore, beads.WorkStore{}, root.ID, "control-dispatch"); err != nil {
		t.Fatalf("EmitCurrent (discard): %v", err)
	}
	assertUnmarked("a Discard tick")

	// A recorder whose append fails (the FileRecorder lock-timeout / ENOSPC drop
	// shape) reports the drop, so the step must still stay unmarked.
	if err := EmitCurrent(events.NewFailFake(), graphStore, beads.WorkStore{}, root.ID, "control-dispatch"); err != nil {
		t.Fatalf("EmitCurrent (failing recorder): %v", err)
	}
	assertUnmarked("a failed-append tick")

	// A healthy recorder acknowledges the append, so the still-unmarked step is
	// re-emitted AND marked this tick — the definition was never permanently lost.
	healthy := events.NewFake()
	if err := EmitCurrent(healthy, graphStore, beads.WorkStore{}, root.ID, "control-dispatch"); err != nil {
		t.Fatalf("EmitCurrent (healthy): %v", err)
	}
	if got := countStepDefined(healthy.Events, root.ID); got != 1 {
		t.Fatalf("healthy tick step_defined = %d, want 1 (re-emit after the dropped ticks)", got)
	}
	reloaded, err := graph.Get(step.ID)
	if err != nil {
		t.Fatalf("reload step %s: %v", step.ID, err)
	}
	if reloaded.Metadata[beadmeta.StepDefinedEmittedMetadataKey] == "" {
		t.Fatalf("step not marked after a healthy acknowledged emit")
	}
}

func countStepDefined(recorded []events.Event, rootID string) int {
	count := 0
	for _, event := range recorded {
		if event.Type == events.ExecutionStepDefined && event.RunID == rootID {
			count++
		}
	}
	return count
}

func mustCreateProjectionRoot(t *testing.T, store beads.Store, convoyID string) beads.Bead {
	t.Helper()
	metadata := map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
	}
	if convoyID != "" {
		metadata[beadmeta.InputConvoyIDMetadataKey] = convoyID
	}
	return mustCreateProjectionBead(t, store, beads.Bead{Metadata: metadata})
}

func mustCreateProjectionStep(t *testing.T, store beads.Store, id, rootID, stepID, topology string) beads.Bead {
	t.Helper()
	metadata := map[string]string{
		beadmeta.RootBeadIDMetadataKey: rootID,
		beadmeta.StepIDMetadataKey:     stepID,
	}
	if topology != "" {
		metadata[beadmeta.NativeStepDependenciesMetadataKey] = topology
	}
	return mustCreateProjectionBead(t, store, beads.Bead{ID: id, Metadata: metadata})
}

func mustCreateProjectionBead(t *testing.T, store beads.Store, bead beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(bead)
	if err != nil {
		t.Fatalf("create %s: %v", bead.ID, err)
	}
	return created
}

func projectionStringsPtr(values []string) *[]string { return &values }

type projectionDepStore struct {
	beads.Store
	convoyID string
	deps     []beads.Dep
}

type projectionSourceStore struct {
	beads.Store
	requestedID string
	source      beads.Bead
	getErr      error
}

func (s projectionSourceStore) Get(id string) (beads.Bead, error) {
	if id == s.requestedID {
		return s.source, s.getErr
	}
	return s.Store.Get(id)
}

func (s projectionDepStore) DepList(id, direction string) ([]beads.Dep, error) {
	if id != s.convoyID || direction != "down" {
		return nil, nil
	}
	return append([]beads.Dep(nil), s.deps...), nil
}
