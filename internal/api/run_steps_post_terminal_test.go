package api

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// TestRunStepsDoNotSkipStepsThatRunAfterTheRootSettles replays the negative
// qualification run: validation failed, the workflow finalizer closed the root
// failed, and only then closed itself with gc.outcome=pass, 8 s later. In that
// window /steps projected the still-open finalizer as skipped, and the next read
// reported it completed. A step that executes after the root settles by
// contract (the workflow finalizer and the teardown tail) is not skipped while
// it is open; it reads its own durable status until its own close lands. An
// ordinary open step under the failed root is still skipped: the finalizer's
// sweep closes it skipped.
func TestRunStepsDoNotSkipStepsThatRunAfterTheRootSettles(t *testing.T) {
	root := runRootBead("sqr-ywn", "implement", "open")
	validate := runChildBead("sqr-an9", "sqr-ywn", "closed", map[string]string{"gc.outcome": "fail"})
	finalizer := runChildBead("sqr-eo1", "sqr-ywn", "open", map[string]string{
		"gc.kind":    "workflow-finalize",
		"gc.step_id": "implement.workflow-finalize",
	})
	teardown := runChildBead("sqr-td1", "sqr-ywn", "open", map[string]string{
		"gc.scope_role": "teardown",
		"gc.step_id":    "implement.cleanup",
	})
	// A member generated for the teardown step (same step id, no scope role)
	// runs with it, as the dispatcher's teardown-tail exclusion has it.
	teardownAttempt := runChildBead("sqr-td2", "sqr-ywn", "open", map[string]string{
		"gc.step_id": "implement.cleanup",
	})
	never := runChildBead("sqr-9dv", "sqr-ywn", "open", nil)

	created := []events.Event{
		beadCreatedEvent(1, root),
		beadCreatedEvent(2, validate),
		beadCreatedEvent(3, finalizer),
		beadCreatedEvent(4, teardown),
		beadCreatedEvent(5, teardownAttempt),
		beadCreatedEvent(6, never),
	}
	failedRoot := root
	failedRoot.Status = "closed"
	failedRoot.Metadata = cloneStringMap(root.Metadata)
	failedRoot.Metadata["gc.outcome"] = "fail"
	rootClosed := append(append([]events.Event(nil), created...), beadEvent(7, events.BeadClosed, failedRoot))

	steps := runStepStatuses(t, newRunServer(t, rootClosed...), "sqr-ywn")
	for id, want := range map[string]RunStepStatus{
		"sqr-an9": RunStepStatusFailed,
		"sqr-eo1": RunStepStatusPending,
		"sqr-td1": RunStepStatusPending,
		"sqr-td2": RunStepStatusPending,
		"sqr-9dv": RunStepStatusSkipped,
	} {
		if got := steps[id]; got != want {
			t.Errorf("root closed failed, %s still open: status = %q, want %q", id, got, want)
		}
	}

	closedFinalizer := finalizer
	closedFinalizer.Status = "closed"
	closedFinalizer.Metadata = cloneStringMap(finalizer.Metadata)
	closedFinalizer.Metadata["gc.outcome"] = "pass"
	finalized := append(append([]events.Event(nil), rootClosed...), beadEvent(8, events.BeadClosed, closedFinalizer))

	steps = runStepStatuses(t, newRunServer(t, finalized...), "sqr-ywn")
	if got := steps["sqr-eo1"]; got != RunStepStatusCompleted {
		t.Errorf("finalizer closed pass: status = %q, want completed", got)
	}
}

func beadEvent(seq uint64, typ string, b beads.Bead) events.Event {
	evt := beadCreatedEvent(seq, b)
	evt.Type = typ
	return evt
}

func runStepStatuses(t *testing.T, s *Server, runID string) map[string]RunStepStatus {
	t.Helper()
	out, err := s.humaHandleRunSteps(context.Background(), &RunStepsInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     runID,
	})
	if err != nil {
		t.Fatalf("humaHandleRunSteps error: %v", err)
	}
	statuses := make(map[string]RunStepStatus, len(out.Body.Steps))
	for _, st := range out.Body.Steps {
		statuses[st.ID] = st.Status
	}
	return statuses
}

// TestRunStepsTerminalClampPredictsTheDurableClose pins that a clamped step
// reads the status dispatch durably writes when it closes the step, so the
// projection never moves from one terminal status to another. A root settled
// by the workflow finalizer (pass or fail), or closed skipped, leaves its open
// members to the finalizer's sweep, which closes each gc.outcome=skipped,
// started or not. A run canceled via POST /runs/{id}/cancel closes its open
// members gc.outcome=canceled (and a finalizer settling a canceled workflow
// sweeps them canceled), started or not. The clamp used to present a failed
// run's in-progress step as canceled, which the sweep then contradicted with
// skipped, and a canceled run's never-started step as skipped, which the
// cancel's close contradicted with canceled.
func TestRunStepsTerminalClampPredictsTheDurableClose(t *testing.T) {
	for _, tc := range []struct {
		outcome string
		want    RunStepStatus
	}{
		{"pass", RunStepStatusSkipped},
		{"fail", RunStepStatusSkipped},
		{"skipped", RunStepStatusSkipped},
		{"canceled", RunStepStatusCanceled},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			root := runRootBead("runp", "implement", "closed")
			root.Metadata["gc.outcome"] = tc.outcome
			s := newRunServer(t,
				beadCreatedEvent(1, root),
				beadCreatedEvent(2, runChildBead("runp.started", "runp", "in_progress", nil)),
				beadCreatedEvent(3, runChildBead("runp.never", "runp", "open", nil)),
				beadCreatedEvent(4, runChildBead("runp.done", "runp", "closed", map[string]string{"gc.outcome": "pass"})),
			)
			steps := runStepStatuses(t, s, "runp")
			for _, id := range []string{"runp.started", "runp.never"} {
				if got := steps[id]; got != tc.want {
					t.Errorf("root closed %s, %s still open: status = %q, want %q (what dispatch writes when it closes it)", tc.outcome, id, got, tc.want)
				}
			}
			if got := steps["runp.done"]; got != RunStepStatusCompleted {
				t.Errorf("recorded outcome: runp.done = %q, want completed", got)
			}
		})
	}
}
