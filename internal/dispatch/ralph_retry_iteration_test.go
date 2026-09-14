package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// A Ralph loop advances an outer iteration by CLONING the prior iteration's
// beads (appendRalphRetryLegacy / buildRalphRetryGraphNode), not by re-minting
// them from the frozen spec. The clone therefore has to reproduce the same
// iteration/attempt contract the mint paths establish (internal/formula/ralph.go
// + dispatch.buildAttemptRecipe):
//
//   - gc.iteration is the outer loop counter and advances to the NEW iteration on
//     EVERY cloned bead — the scope/subject, the check, and every nested member.
//   - gc.attempt is the loop counter only on the loop-level beads (scope/subject
//     and check); a nested body member's gc.attempt follows its own local step
//     semantics (RalphBodyChildAttempt): a retry/ralph control resets to 1, an
//     already-materialized attempt.N bead keeps the attempt its ref carries, and a
//     plain child inherits the iteration index.
//
// Before the fix both clone paths blanket-stamped gc.attempt=nextAttempt onto
// subject/members/check and never wrote gc.iteration, so a retried iteration N+1
// carried gc.iteration=N (stale) and every nested retry counter was overwritten
// with the outer iteration number. These tests pin the contract on both paths.

// ralphIterationOneBead is a compact description of one iteration-1 bead used to
// seed the clone-path tests. The step_ref strings are the logical namespaced
// refs the producers emit; the store assigns opaque bead IDs on top.
type ralphIterationOneBead struct {
	title    string
	kind     string
	stepID   string
	stepRef  string
	scopeRef string
	attempt  string
}

func (b ralphIterationOneBead) metadata(rootID string) map[string]string {
	meta := map[string]string{
		beadmeta.StepIDMetadataKey:      b.stepID,
		beadmeta.RalphStepIDMetadataKey: "review-loop",
		beadmeta.AttemptMetadataKey:     b.attempt,
		beadmeta.IterationMetadataKey:   "1",
		beadmeta.StepRefMetadataKey:     b.stepRef,
		beadmeta.RootBeadIDMetadataKey:  rootID,
	}
	if b.kind != "" {
		meta[beadmeta.KindMetadataKey] = b.kind
	}
	if b.scopeRef != "" {
		meta[beadmeta.ScopeRefMetadataKey] = b.scopeRef
		meta[beadmeta.ScopeRoleMetadataKey] = beadmeta.ScopeRoleMember
	}
	return meta
}

// The five iteration-1 beads a nested Ralph body produces: a scope subject, a
// retry-controlled member and its first attempt run, a plain member, and the
// ralph check. Refs mirror internal/formula/ralph.go's namespacing.
var (
	ralphIterOneSubject = ralphIterationOneBead{
		title: "review-loop.iteration.1", kind: beadmeta.KindScope,
		stepID: "review-loop", stepRef: "review-loop.iteration.1", attempt: "1",
	}
	ralphIterOneRetryControl = ralphIterationOneBead{
		title: "review-loop.iteration.1.scorecard", kind: beadmeta.KindRetry,
		stepID: "scorecard", stepRef: "review-loop.iteration.1.scorecard",
		scopeRef: "review-loop.iteration.1", attempt: "1",
	}
	ralphIterOneAttemptRun = ralphIterationOneBead{
		title:  "review-loop.iteration.1.scorecard.attempt.1",
		stepID: "scorecard", stepRef: "review-loop.iteration.1.scorecard.attempt.1",
		scopeRef: "review-loop.iteration.1", attempt: "1",
	}
	ralphIterOnePlainMember = ralphIterationOneBead{
		title:  "review-loop.iteration.1.publish",
		stepID: "publish", stepRef: "review-loop.iteration.1.publish",
		scopeRef: "review-loop.iteration.1", attempt: "1",
	}
	ralphIterOneCheck = ralphIterationOneBead{
		title: "review-loop.check.1", kind: beadmeta.KindCheck,
		stepID: "review-loop", stepRef: "review-loop.check.1", attempt: "1",
	}
)

// clonedRalphIteration is the outer iteration every cloned bead must advance to:
// these tests seed iteration-1 beads and clone them forward one outer iteration.
const clonedRalphIteration = "2"

// assertClonedCounters pins the two counters a cloned bead must carry. Every
// clone advances gc.iteration to the new outer iteration (clonedRalphIteration);
// gc.attempt follows local step semantics and so varies per bead.
func assertClonedCounters(t *testing.T, label string, meta map[string]string, wantAttempt string) {
	t.Helper()
	if got := meta[beadmeta.IterationMetadataKey]; got != clonedRalphIteration {
		t.Errorf("%s: gc.iteration = %q, want %q", label, got, clonedRalphIteration)
	}
	if got := meta[beadmeta.AttemptMetadataKey]; got != wantAttempt {
		t.Errorf("%s: gc.attempt = %q, want %q", label, got, wantAttempt)
	}
}

func TestAppendRalphRetryLegacyAdvancesIterationAndResetsNestedCounters(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review-loop",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindRalph,
			beadmeta.StepIDMetadataKey:     "review-loop",
			beadmeta.RootBeadIDMetadataKey: workflow.ID,
		},
	})

	newBead := func(desc ralphIterationOneBead) beads.Bead {
		meta := desc.metadata(workflow.ID)
		if desc.kind == beadmeta.KindScope || desc.kind == beadmeta.KindCheck {
			meta[beadmeta.LogicalBeadIDMetadataKey] = logical.ID
		}
		return mustCreateWorkflowBead(t, store, beads.Bead{Title: desc.title, Type: "task", Metadata: meta})
	}

	subject := newBead(ralphIterOneSubject)
	retryControl := newBead(ralphIterOneRetryControl)
	attemptRun := newBead(ralphIterOneAttemptRun)
	plainMember := newBead(ralphIterOnePlainMember)
	check := newBead(ralphIterOneCheck)

	attemptSet := map[string]beads.Bead{
		subject.ID:      subject,
		retryControl.ID: retryControl,
		attemptRun.ID:   attemptRun,
		plainMember.ID:  plainMember,
	}

	mapping, err := appendRalphRetryLegacy(store, logical.ID, subject, check, attemptSet,
		1, 2, "review-loop.iteration.1", "review-loop.iteration.2", nil)
	if err != nil {
		t.Fatalf("appendRalphRetryLegacy: %v", err)
	}

	cloneMeta := func(oldID string) map[string]string {
		newID := mapping[oldID]
		if newID == "" {
			t.Fatalf("no clone mapped for %s", oldID)
		}
		return mustGetBead(t, store, newID).Metadata
	}

	// Loop-level beads advance both counters to the new outer iteration.
	assertClonedCounters(t, "scope/subject", cloneMeta(subject.ID), "2")
	assertClonedCounters(t, "check", cloneMeta(check.ID), "2")
	// Nested members advance gc.iteration but derive gc.attempt locally.
	assertClonedCounters(t, "retry control", cloneMeta(retryControl.ID), "1")
	assertClonedCounters(t, "attempt.1 run", cloneMeta(attemptRun.ID), "1")
	assertClonedCounters(t, "plain member", cloneMeta(plainMember.ID), "2")
}

func TestBuildRalphRetryGraphNodeAdvancesIterationAndResetsNestedCounters(t *testing.T) {
	t.Parallel()

	// buildRalphRetryGraphNode is a pure function: it maps one iteration-1 bead to
	// the GraphApplyNode for its iteration-2 clone with no store involved.
	oldBead := func(desc ralphIterationOneBead) beads.Bead {
		return beads.Bead{ID: desc.stepRef, Ref: desc.stepRef, Metadata: desc.metadata("workflow")}
	}
	subject := oldBead(ralphIterOneSubject)
	retryControl := oldBead(ralphIterOneRetryControl)
	attemptRun := oldBead(ralphIterOneAttemptRun)
	plainMember := oldBead(ralphIterOnePlainMember)
	check := oldBead(ralphIterOneCheck)

	attemptIDs := map[string]bool{
		subject.ID:      true,
		retryControl.ID: true,
		attemptRun.ID:   true,
		plainMember.ID:  true,
		check.ID:        true,
	}
	node := func(old beads.Bead) map[string]string {
		return buildRalphRetryGraphNode(old, "logical",
			"review-loop.iteration.1", "review-loop.iteration.2", 1, 2, attemptIDs, nil).Metadata
	}

	assertClonedCounters(t, "scope/subject", node(subject), "2")
	assertClonedCounters(t, "check", node(check), "2")
	assertClonedCounters(t, "retry control", node(retryControl), "1")
	assertClonedCounters(t, "attempt.1 run", node(attemptRun), "1")
	assertClonedCounters(t, "plain member", node(plainMember), "2")
}
