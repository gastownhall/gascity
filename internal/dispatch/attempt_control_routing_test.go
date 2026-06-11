package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// TestIsAttemptControlKindMatchesControlKinds pins isAttemptControlKind to
// exactly beadmeta.ControlKinds. The predicate used to be a frozen 2026-04-14
// snapshot that excluded drain (added to every other routing predicate by
// PR #2784 but never here); any kind the dispatcher's ProcessControl switch
// can execute must also be routed to it on the Attach/fragment paths, or
// those beads land on worker queues no prompt knows how to process
// (ga-fux85s residual).
func TestIsAttemptControlKindMatchesControlKinds(t *testing.T) {
	for _, kind := range beadmeta.ControlKinds {
		if !isAttemptControlKind(kind) {
			t.Errorf("isAttemptControlKind(%q) = false, want true (must cover all of beadmeta.ControlKinds)", kind)
		}
	}
	for _, kind := range []string{"", beadmeta.KindTask, beadmeta.KindWorkflow, beadmeta.KindScope, beadmeta.KindSpec, beadmeta.KindRun, beadmeta.KindRetryRun, beadmeta.KindCleanup, beadmeta.KindWisp, beadmeta.KindClosed} {
		if isAttemptControlKind(kind) {
			t.Errorf("isAttemptControlKind(%q) = true, want false", kind)
		}
	}
}

// TestRouteFanoutFragmentStepsRoutesDrainToControlDispatcher proves the
// fragment-path drain residual of ga-fux85s: when a fanout fragment carries a
// gc.kind=drain step ([template.drain] is mintable on the
// CompileExpansionFragment path), routeFanoutFragmentSteps must route it to
// the control dispatcher (gc.routed_to = <rig>/control-dispatcher, execution
// lane preserved in gc.execution_routed_to) instead of stamping the worker
// execution route on it like plain work.
func TestRouteFanoutFragmentStepsRoutesDrainToControlDispatcher(t *testing.T) {
	fragment := &formula.FragmentRecipe{
		Name: "frag",
		Steps: []formula.RecipeStep{
			{ID: "frag.item.work", Metadata: map[string]string{}},
			{ID: "frag.item.drain", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindDrain}},
		},
	}
	control := beads.Bead{Metadata: map[string]string{
		beadmeta.ExecutionRoutedToMetadataKey: "gascity/worker",
	}}

	routeFanoutFragmentSteps(fragment, control, ProcessOptions{}, beads.NewMemStore())

	wantControlRoute := "gascity/" + config.ControlDispatcherAgentName
	step := fragmentStepByID(t, fragment, "frag.item.drain")
	if got := step.Metadata[beadmeta.RoutedToMetadataKey]; got != wantControlRoute {
		t.Errorf("drain gc.routed_to = %q, want %q (control beads must reach the dispatcher, not a worker queue)", got, wantControlRoute)
	}
	if got := step.Metadata[beadmeta.ExecutionRoutedToMetadataKey]; got != "gascity/worker" {
		t.Errorf("drain gc.execution_routed_to = %q, want gascity/worker (execution lane preserved)", got)
	}

	work := fragmentStepByID(t, fragment, "frag.item.work")
	if got := work.Metadata[beadmeta.RoutedToMetadataKey]; got != "gascity/worker" {
		t.Errorf("work step gc.routed_to = %q, want gascity/worker", got)
	}
}

func fragmentStepByID(t *testing.T, fragment *formula.FragmentRecipe, id string) *formula.RecipeStep {
	t.Helper()
	for i := range fragment.Steps {
		if fragment.Steps[i].ID == id {
			return &fragment.Steps[i]
		}
	}
	t.Fatalf("fragment step %q not found", id)
	return nil
}

// TestLatestAttemptCandidateSkipsAllControlKinds pins the latest-attempt
// work-bead selection's control-infrastructure skip list to the authoritative
// vocabulary: every control kind plus the workflow topology root is skipped;
// scope beads are kind-dependent (ralph iterations ARE scopes) and plain work
// is selected.
func TestLatestAttemptCandidateSkipsAllControlKinds(t *testing.T) {
	for _, kind := range beadmeta.ControlKinds {
		if !latestAttemptCandidateIsControlInfrastructure(kind) {
			t.Errorf("latestAttemptCandidateIsControlInfrastructure(%q) = false, want true", kind)
		}
	}
	if !latestAttemptCandidateIsControlInfrastructure(beadmeta.KindWorkflow) {
		t.Error("workflow root must be skipped")
	}
	for _, kind := range []string{"", beadmeta.KindTask, beadmeta.KindScope} {
		if latestAttemptCandidateIsControlInfrastructure(kind) {
			t.Errorf("latestAttemptCandidateIsControlInfrastructure(%q) = true, want false", kind)
		}
	}
}
