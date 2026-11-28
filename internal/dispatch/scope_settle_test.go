package dispatch

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// scopedFanoutFixture is a fanout control in scope "body" over a closed source
// step, with the scope body blocked by the fanout. sourceMeta decides which
// terminal branch the fanout takes (failed source, or no items).
type scopedFanoutFixture struct {
	store  bdClosePolicyStore
	body   beads.Bead
	fanout beads.Bead
}

func newScopedFanoutFixture(t *testing.T, sourceMeta map[string]string) scopedFanoutFixture {
	t.Helper()
	store := newBdClosePolicyStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
	}})
	body := mustCreateWorkflowBead(t, store, beads.Bead{Title: "body", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindScope,
		beadmeta.RootBeadIDMetadataKey: workflow.ID,
		beadmeta.StepRefMetadataKey:    "body",
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleBody,
	}})
	meta := map[string]string{
		beadmeta.RootBeadIDMetadataKey: workflow.ID,
		beadmeta.StepRefMetadataKey:    "demo.survey",
		beadmeta.ScopeRefMetadataKey:   "body",
		beadmeta.ScopeRoleMetadataKey:  "member",
	}
	maps.Copy(meta, sourceMeta)
	source := mustCreateWorkflowBead(t, store, beads.Bead{Title: "survey", Type: "task", Status: "closed", Metadata: meta})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{Title: "fanout", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindFanout,
		beadmeta.RootBeadIDMetadataKey: workflow.ID,
		beadmeta.ScopeRefMetadataKey:   "body",
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleControl,
		beadmeta.ControlForMetadataKey: "demo.survey",
		beadmeta.ForEachMetadataKey:    "output.items",
		beadmeta.BondMetadataKey:       "expansion-review",
	}})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")
	mustDepAdd(t, store, body.ID, fanout.ID, "blocks")
	store.closeOrder = nil // setup closes are not under test
	return scopedFanoutFixture{store: store, body: body, fanout: fanout}
}

var (
	emptyFanoutSource  = map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, beadmeta.OutputJSONMetadataKey: `{"items":[]}`}
	failedFanoutSource = map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail}
)

// TestFanoutSettlesScopeBeforeClosing pins the ga-l6kddq write order: the
// fanout settles its enclosing scope while it is still open and closes last,
// because once it is closed nothing re-drives the scope. The body is blocked by
// the open fanout, so its close must be the forced close bd allows.
func TestFanoutSettlesScopeBeforeClosing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		source      map[string]string
		wantOutcome string
		wantAction  string
	}{
		{name: "empty", source: emptyFanoutSource, wantOutcome: beadmeta.OutcomePass, wantAction: "fanout-empty"},
		{name: "source-failed", source: failedFanoutSource, wantOutcome: beadmeta.OutcomeFail, wantAction: "fanout-fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopedFanoutFixture(t, tc.source)
			result, err := ProcessControl(beadstest.Graph(f.store), f.fanout, ProcessOptions{Work: beadstest.Work(f.store)})
			if err != nil {
				t.Fatalf("ProcessControl: %v", err)
			}
			if result.Action != tc.wantAction {
				t.Fatalf("Action = %q, want %q", result.Action, tc.wantAction)
			}
			if got, want := f.store.closeOrder, []string{f.body.ID, f.fanout.ID}; !slices.Equal(got, want) {
				t.Fatalf("close order = %v, want %v (scope body before the fanout)", got, want)
			}
			for _, id := range []string{f.body.ID, f.fanout.ID} {
				b := mustGetBead(t, f.store, id)
				if b.Status != "closed" || b.Metadata[beadmeta.OutcomeMetadataKey] != tc.wantOutcome {
					t.Fatalf("%s = %s/%q, want closed/%s", id, b.Status, b.Metadata[beadmeta.OutcomeMetadataKey], tc.wantOutcome)
				}
			}
		})
	}
}

// TestFanoutRerunAdoptsSettledScope covers the interrupted settle: the scope
// body was settled but the fanout's own close was lost. The re-run closes the
// fanout and reports scope-settled without touching the scope again. The open
// sibling member would be skipped by a second scope abort, so it staying open
// proves the scope was not re-evaluated.
func TestFanoutRerunAdoptsSettledScope(t *testing.T) {
	for _, tc := range []struct {
		name        string
		source      map[string]string
		bodyOutcome string
	}{
		{name: "empty", source: emptyFanoutSource, bodyOutcome: beadmeta.OutcomePass},
		{name: "source-failed", source: failedFanoutSource, bodyOutcome: beadmeta.OutcomeFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopedFanoutFixture(t, tc.source)
			sibling := mustCreateWorkflowBead(t, f.store, beads.Bead{Title: "sibling", Type: "task", Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: f.fanout.Metadata[beadmeta.RootBeadIDMetadataKey],
				beadmeta.StepRefMetadataKey:    "demo.sibling",
				beadmeta.ScopeRefMetadataKey:   "body",
				beadmeta.ScopeRoleMetadataKey:  "member",
			}})
			if err := forceCloseScopeBody(beadstest.Graph(f.store), f.body.ID, map[string]string{beadmeta.OutcomeMetadataKey: tc.bodyOutcome}); err != nil {
				t.Fatalf("settling body: %v", err)
			}
			bodyBefore := mustGetBead(t, f.store, f.body.ID)

			result, err := ProcessControl(beadstest.Graph(f.store), f.fanout, ProcessOptions{Work: beadstest.Work(f.store)})
			if err != nil {
				t.Fatalf("ProcessControl: %v", err)
			}
			if result.Action != scopeSettledAction {
				t.Fatalf("Action = %q, want %q", result.Action, scopeSettledAction)
			}
			if fanout := mustGetBead(t, f.store, f.fanout.ID); fanout.Status != "closed" {
				t.Fatalf("fanout status = %s, want closed", fanout.Status)
			}
			if got := mustGetBead(t, f.store, f.body.ID); !maps.Equal(got.Metadata, bodyBefore.Metadata) || got.Status != "closed" {
				t.Fatalf("body changed on re-run: %s %v, want %s %v", got.Status, got.Metadata, bodyBefore.Status, bodyBefore.Metadata)
			}
			if got := mustGetBead(t, f.store, sibling.ID); got.Status != "open" {
				t.Fatalf("sibling status = %s, want open (scope must not be re-aborted)", got.Status)
			}
		})
	}
}

// TestDrainFinishesRecordedClose covers a drain whose terminal state and
// outcome are recorded while it is still open: the next pass settles the scope
// and closes the drain with the recorded outcome instead of leaving it open.
func TestDrainFinishesRecordedClose(t *testing.T) {
	store := newBdClosePolicyStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
	}})
	body := mustCreateWorkflowBead(t, store, beads.Bead{Title: "body", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindScope,
		beadmeta.RootBeadIDMetadataKey: workflow.ID,
		beadmeta.StepRefMetadataKey:    "body",
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleBody,
	}})
	drain := mustCreateWorkflowBead(t, store, beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindDrain,
		beadmeta.RootBeadIDMetadataKey: workflow.ID,
		beadmeta.ScopeRefMetadataKey:   "body",
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleControl,
		beadmeta.DrainStateMetadataKey: beadmeta.DrainStateFailed,
		beadmeta.OutcomeMetadataKey:    beadmeta.OutcomeFail,
	}})
	mustDepAdd(t, store, body.ID, drain.ID, "blocks")

	result, err := ProcessControl(beadstest.Graph(store), drain, ProcessOptions{Work: beadstest.Work(store)})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if result.Action != "drain-failed" {
		t.Fatalf("Action = %q, want drain-failed", result.Action)
	}
	if got, want := store.closeOrder, []string{body.ID, drain.ID}; !slices.Equal(got, want) {
		t.Fatalf("close order = %v, want %v", got, want)
	}
	for _, id := range []string{body.ID, drain.ID} {
		if b := mustGetBead(t, store, id); b.Status != "closed" || b.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
			t.Fatalf("%s = %s/%q, want closed/fail", id, b.Status, b.Metadata[beadmeta.OutcomeMetadataKey])
		}
	}
}

func TestDrainRecordedCloseWithoutOutcomeIsMalformed(t *testing.T) {
	store := beads.NewMemStore()
	drain := mustCreateWorkflowBead(t, store, beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindDrain,
		beadmeta.DrainStateMetadataKey: beadmeta.DrainStateSucceeded,
	}})
	_, err := ProcessControl(beadstest.Graph(store), drain, ProcessOptions{Work: beadstest.Work(store)})
	if !errors.Is(err, ErrControlGraphMalformed) {
		t.Fatalf("ProcessControl error = %v, want ErrControlGraphMalformed", err)
	}
	if got := mustGetBead(t, store, drain.ID).Status; got != "open" {
		t.Fatalf("drain status = %s, want open", got)
	}
}

// scopeAbortedByKey is the body marker a deferred scope abort writes. The tests
// spell the wire key so they pin it independently of the constant.
const scopeAbortedByKey = "gc.scope_aborted_by"

// inFlightAbortFixture is the issue #9507 graph: a scope body that needs
// members A (claimed, in_progress), B (closed fail hard) and C, with A and B
// each guarded by their minted scope-check. C (a scope member) and D (a step
// outside the scope) need A, so the compiler rewrote both edges onto A's
// scope-check. workflow-finalize needs the body.
type inFlightAbortFixture struct {
	store    beads.Store
	body     beads.Bead
	a        beads.Bead
	b        beads.Bead
	c        beads.Bead
	d        beads.Bead
	checkA   beads.Bead
	checkB   beads.Bead
	finalize beads.Bead
}

const inFlightAbortAssignee = "worker-1"

func newInFlightAbortFixture(t *testing.T, store beads.Store) inFlightAbortFixture {
	t.Helper()
	f := inFlightAbortFixture{store: store}
	member := func(name string) beads.Bead {
		return mustCreateWorkflowBead(t, store, beads.Bead{Title: name, Type: "task", Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: "wf-1",
			beadmeta.ScopeRefMetadataKey:   "body",
			beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleMember,
			beadmeta.OnFailMetadataKey:     "abort_scope",
			beadmeta.StepRefMetadataKey:    "demo." + name,
		}})
	}
	f.body = mustCreateWorkflowBead(t, store, beads.Bead{Title: "body", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindScope,
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleBody,
		beadmeta.RootBeadIDMetadataKey: "wf-1",
		beadmeta.StepRefMetadataKey:    "demo.body",
	}})
	f.a = member("step-a")
	f.b = member("step-b")
	f.checkA = newScopeCheckFor(t, store, "step-a")
	f.checkB = newScopeCheckFor(t, store, "step-b")
	f.c = member("step-c")
	f.d = mustCreateWorkflowBead(t, store, beads.Bead{Title: "step-d", Type: "task", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: "wf-1",
		beadmeta.StepRefMetadataKey:    "demo.step-d",
	}})
	f.finalize = mustCreateWorkflowBead(t, store, beads.Bead{Title: "finalize", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindWorkflowFinalize,
		beadmeta.RootBeadIDMetadataKey: "wf-1",
		beadmeta.StepRefMetadataKey:    "demo.workflow-finalize",
	}})
	mustDepAdd(t, store, f.body.ID, f.a.ID, "blocks")
	mustDepAdd(t, store, f.body.ID, f.b.ID, "blocks")
	mustDepAdd(t, store, f.body.ID, f.c.ID, "blocks")
	mustDepAdd(t, store, f.checkA.ID, f.a.ID, "blocks")
	mustDepAdd(t, store, f.checkB.ID, f.b.ID, "blocks")
	mustDepAdd(t, store, f.c.ID, f.checkA.ID, "blocks")
	mustDepAdd(t, store, f.d.ID, f.checkA.ID, "blocks")
	mustDepAdd(t, store, f.finalize.ID, f.body.ID, "blocks")

	claimInFlight(t, store, f.a.ID)
	closeMember(t, store, f.b.ID, map[string]string{
		beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
		beadmeta.FailureReasonMetadataKey: "step-b broke",
		"review.verdict":                  "blocked",
	})
	return f
}

func newScopeCheckFor(t *testing.T, store beads.Store, step string) beads.Bead {
	t.Helper()
	return mustCreateWorkflowBead(t, store, beads.Bead{Title: "Finalize scope for " + step, Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindScopeCheck,
		beadmeta.RootBeadIDMetadataKey: "wf-1",
		beadmeta.ScopeRefMetadataKey:   "body",
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleControl,
		beadmeta.StepRefMetadataKey:    "demo." + step + "-scope-check",
	}})
}

func claimInFlight(t *testing.T, store beads.Store, id string) {
	t.Helper()
	inProgress, assignee := "in_progress", inFlightAbortAssignee
	if err := store.Update(id, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("claiming %s: %v", id, err)
	}
}

// closeMember is a worker's own close of a step: a plain status update that
// runs no dispatch code.
func closeMember(t *testing.T, store beads.Store, id string, metadata map[string]string) {
	t.Helper()
	closed := "closed"
	if err := store.Update(id, beads.UpdateOpts{Status: &closed, Metadata: metadata}); err != nil {
		t.Fatalf("closing %s: %v", id, err)
	}
}

func processScopeControl(t *testing.T, store beads.Store, id string) ControlResult {
	t.Helper()
	result, err := ProcessControl(beadstest.Graph(store), mustGetBead(t, store, id), ProcessOptions{Work: beadstest.Work(store)})
	if err != nil {
		t.Fatalf("ProcessControl(%s): %v", id, err)
	}
	return result
}

func readyIDs(t *testing.T, store beads.Store) []string {
	t.Helper()
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("store.Ready: %v", err)
	}
	ids := make([]string, 0, len(ready))
	for _, b := range ready {
		ids = append(ids, b.ID)
	}
	return ids
}

func assertBeadState(t *testing.T, store beads.Store, id, wantStatus, wantOutcome string) beads.Bead {
	t.Helper()
	got := mustGetBead(t, store, id)
	if got.Status != wantStatus || got.Metadata[beadmeta.OutcomeMetadataKey] != wantOutcome {
		t.Fatalf("%s (%s) = %s/%q, want %s/%q", id, got.Metadata[beadmeta.StepRefMetadataKey],
			got.Status, got.Metadata[beadmeta.OutcomeMetadataKey], wantStatus, wantOutcome)
	}
	return got
}

// assertInFlightUntouched pins that the claimed member is still its worker's:
// never skipped, never closed, never reassigned.
func assertInFlightUntouched(t *testing.T, store beads.Store, id string) {
	t.Helper()
	got := mustGetBead(t, store, id)
	if got.Status != "in_progress" || got.Assignee != inFlightAbortAssignee {
		t.Fatalf("in-flight member %s = %s assignee %q, want in_progress assignee %q", id, got.Status, got.Assignee, inFlightAbortAssignee)
	}
	if outcome := got.Metadata[beadmeta.OutcomeMetadataKey]; outcome != "" {
		t.Fatalf("in-flight member %s carries gc.outcome=%q, want none", id, outcome)
	}
	if reason := got.Metadata["close_reason"]; reason != "" {
		t.Fatalf("in-flight member %s carries close_reason %q, want none", id, reason)
	}
}

// assertAbortDeferred pins the ledger right after an abort decided while a
// member is in flight: the body stays open, marked with the bead that decided
// the abort, and the in-flight member's scope-check stays open as the control
// that settles it.
func assertAbortDeferred(t *testing.T, store beads.Store, bodyID, decider, liveCheckID string) {
	t.Helper()
	body := assertBeadState(t, store, bodyID, "open", "")
	if got := body.Metadata[scopeAbortedByKey]; got != decider {
		t.Fatalf("body %s = %q, want %q", scopeAbortedByKey, got, decider)
	}
	assertBeadState(t, store, liveCheckID, "open", "")
}

// assertAbortSettled pins the body closing fail with the propagated member
// metadata and the failure stamp of the first hard-failed member.
func assertAbortSettled(t *testing.T, store beads.Store, bodyID string, culpritID string, wantMeta map[string]string) {
	t.Helper()
	body := assertBeadState(t, store, bodyID, "closed", beadmeta.OutcomeFail)
	for k, v := range wantMeta {
		if body.Metadata[k] != v {
			t.Fatalf("body %s = %q, want %q (propagated member metadata)", k, body.Metadata[k], v)
		}
	}
	if got := body.Metadata[beadmeta.FailureSubjectMetadataKey]; got != culpritID {
		t.Fatalf("body gc.failure_subject = %q, want %q", got, culpritID)
	}
	if got := body.Metadata[beadmeta.FailureClassMetadataKey]; got != beadmeta.FailureClassHard {
		t.Fatalf("body gc.failure_class = %q, want hard", got)
	}
}

// TestScopeAbortDefersBodyCloseWhileMemberInFlight is issue #9507 on bd's close
// policy: an abort decided while a sibling member is claimed must not try to
// close the body past it (bd refuses, the control retries until quarantine) nor
// destroy the in-flight member's scope-check, the one control that re-checks the
// scope once that member settles.
func TestScopeAbortDefersBodyCloseWhileMemberInFlight(t *testing.T) {
	t.Parallel()

	f := newInFlightAbortFixture(t, newStrictCloseStore())

	result := processScopeControl(t, f.store, f.checkB.ID)
	if result.Action != "scope-fail" || result.Skipped != 1 {
		t.Fatalf("result = %+v, want action scope-fail with Skipped 1 (member C)", result)
	}
	assertScopeSkipCloseReason(t, assertBeadState(t, f.store, f.c.ID, "closed", beadmeta.OutcomeSkipped))
	assertInFlightUntouched(t, f.store, f.a.ID)
	assertAbortDeferred(t, f.store, f.body.ID, f.b.ID, f.checkA.ID)
	assertBeadState(t, f.store, f.d.ID, "open", "")
	assertBeadState(t, f.store, f.checkB.ID, "closed", beadmeta.OutcomePass)
	if slices.Contains(readyIDs(t, f.store), f.d.ID) {
		t.Fatalf("step outside the scope %s is ready while the member it needs is in flight", f.d.ID)
	}
}

// inFlightSettleCase is one way the in-flight member of a deferred abort ends.
// Either way the decided abort stands: the body closes fail, never pass.
type inFlightSettleCase struct {
	name  string
	close map[string]string
}

var inFlightSettleCases = []inFlightSettleCase{
	{
		name:  "A closes pass",
		close: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, "step_a.note": "done"},
	},
	{
		name: "A closes fail hard",
		close: map[string]string{
			beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
			beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
			beadmeta.FailureReasonMetadataKey: "step-a broke",
			"step_a.note":                     "done",
		},
	},
}

// culprit is the member whose hard failure the settled body must name: the
// first hard-failed member in bead-ID order.
func (c inFlightSettleCase) culprit(f inFlightAbortFixture) string {
	if c.close[beadmeta.FailureClassMetadataKey] == beadmeta.FailureClassHard {
		return min(f.a.ID, f.b.ID)
	}
	return f.b.ID
}

// TestScopeAbortSettlesBodyWhenInFlightMemberCloses covers the second half of
// issue #9507: when the in-flight member of a deferred abort closes, however it
// ends, its own scope-check (ready because its blocker closed, whoever closed
// it) closes the body fail and only then releases the steps rewritten onto it.
func TestScopeAbortSettlesBodyWhenInFlightMemberCloses(t *testing.T) {
	t.Parallel()

	for _, tc := range inFlightSettleCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newInFlightAbortFixture(t, newStrictCloseStore())
			processScopeControl(t, f.store, f.checkB.ID)
			assertAbortDeferred(t, f.store, f.body.ID, f.b.ID, f.checkA.ID)

			closeMember(t, f.store, f.a.ID, tc.close)
			if !slices.Contains(readyIDs(t, f.store), f.checkA.ID) {
				t.Fatalf("A's scope-check %s is not ready after A closed; nothing would re-drive the scope", f.checkA.ID)
			}
			result := processScopeControl(t, f.store, f.checkA.ID)
			if result.Action != "scope-fail" {
				t.Fatalf("A's scope-check action = %q, want scope-fail", result.Action)
			}
			assertAbortSettled(t, f.store, f.body.ID, tc.culprit(f), map[string]string{
				"review.verdict": "blocked",
				"step_a.note":    "done",
			})
			assertBeadState(t, f.store, f.checkA.ID, "closed", beadmeta.OutcomePass)
			if !slices.Contains(readyIDs(t, f.store), f.d.ID) {
				t.Fatalf("step outside the scope %s is not ready after the in-flight member settled", f.d.ID)
			}
			if !slices.Contains(readyIDs(t, f.store), f.finalize.ID) {
				t.Fatalf("workflow-finalize %s is not ready after the body closed", f.finalize.ID)
			}
		})
	}
}

// TestScopeAbortTriggeredHonorsDecidedAbort pins the predicate both settlement
// sites (processScopeCheck and reconcileScopeForTerminalMember) share: a member
// that passed still settles a scope whose abort is already decided as aborted.
func TestScopeAbortTriggeredHonorsDecidedAbort(t *testing.T) {
	t.Parallel()

	passed := beads.Bead{ID: "m-1", Status: "closed", Metadata: map[string]string{
		beadmeta.OnFailMetadataKey:  "abort_scope",
		beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass,
	}}
	failed := beads.Bead{ID: "m-2", Status: "closed", Metadata: map[string]string{
		beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail,
	}}
	unmarked := beads.Bead{ID: "body", Metadata: map[string]string{}}
	marked := beads.Bead{ID: "body", Metadata: map[string]string{scopeAbortedByKey: "m-2"}}

	for _, tc := range []struct {
		name   string
		member beads.Bead
		body   beads.Bead
		want   bool
	}{
		{name: "passed member, open scope", member: passed, body: unmarked, want: false},
		{name: "passed member, decided abort", member: passed, body: marked, want: true},
		{name: "failed member, open scope", member: failed, body: unmarked, want: true},
		{name: "failed member, decided abort", member: failed, body: marked, want: true},
	} {
		if got := scopeAbortTriggered(tc.member, tc.body); got != tc.want {
			t.Errorf("%s: scopeAbortTriggered = %t, want %t", tc.name, got, tc.want)
		}
	}
}
