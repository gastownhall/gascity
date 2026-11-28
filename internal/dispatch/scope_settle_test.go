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

// TestScopeAbortDeferralMatchesOnNonRefusingStore runs the same abort on a store
// that never refuses a blocked close (MemStore, matching the sqlite-beads
// binding). The ledger must match bd's at every step: the dispatcher, not the
// store's close policy, keeps the body open while the member runs, so
// workflow-finalize cannot sweep the claimed member skipped under its worker.
func TestScopeAbortDeferralMatchesOnNonRefusingStore(t *testing.T) {
	t.Parallel()

	for _, tc := range inFlightSettleCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newInFlightAbortFixture(t, beads.NewMemStore())

			result := processScopeControl(t, f.store, f.checkB.ID)
			if result.Action != "scope-fail" || result.Skipped != 1 {
				t.Fatalf("result = %+v, want action scope-fail with Skipped 1 (member C)", result)
			}
			assertInFlightUntouched(t, f.store, f.a.ID)
			assertAbortDeferred(t, f.store, f.body.ID, f.b.ID, f.checkA.ID)
			assertBeadState(t, f.store, f.checkB.ID, "closed", beadmeta.OutcomePass)
			assertBeadState(t, f.store, f.c.ID, "closed", beadmeta.OutcomeSkipped)
			ready := readyIDs(t, f.store)
			if slices.Contains(ready, f.finalize.ID) {
				t.Fatalf("workflow-finalize %s is ready while member %s is in flight", f.finalize.ID, f.a.ID)
			}
			if slices.Contains(ready, f.d.ID) {
				t.Fatalf("step outside the scope %s is ready while the member it needs is in flight", f.d.ID)
			}

			closeMember(t, f.store, f.a.ID, tc.close)
			processScopeControl(t, f.store, f.checkA.ID)
			a := mustGetBead(t, f.store, f.a.ID)
			if a.Metadata[beadmeta.OutcomeMetadataKey] != tc.close[beadmeta.OutcomeMetadataKey] || a.Assignee != inFlightAbortAssignee {
				t.Fatalf("member A = outcome %q assignee %q, want its own close %q under %q",
					a.Metadata[beadmeta.OutcomeMetadataKey], a.Assignee, tc.close[beadmeta.OutcomeMetadataKey], inFlightAbortAssignee)
			}
			assertAbortSettled(t, f.store, f.body.ID, tc.culprit(f), map[string]string{"review.verdict": "blocked", "step_a.note": "done"})
			assertBeadState(t, f.store, f.checkA.ID, "closed", beadmeta.OutcomePass)
		})
	}
}

// TestScopeAbortDefersUntilLastInFlightMemberSettles pins that each in-flight
// member keeps the abort deferred: the body closes fail only when the last one
// settles, on either close policy.
func TestScopeAbortDefersUntilLastInFlightMemberSettles(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		store func() beads.Store
	}{
		{name: "bd close policy", store: func() beads.Store { return newStrictCloseStore() }},
		{name: "non-refusing store", store: func() beads.Store { return beads.NewMemStore() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newInFlightAbortFixture(t, tc.store())
			a2 := mustCreateWorkflowBead(t, f.store, beads.Bead{Title: "step-a2", Type: "task", Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: "wf-1",
				beadmeta.ScopeRefMetadataKey:   "body",
				beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleMember,
				beadmeta.OnFailMetadataKey:     "abort_scope",
				beadmeta.StepRefMetadataKey:    "demo.step-a2",
			}})
			checkA2 := newScopeCheckFor(t, f.store, "step-a2")
			mustDepAdd(t, f.store, f.body.ID, a2.ID, "blocks")
			mustDepAdd(t, f.store, checkA2.ID, a2.ID, "blocks")
			claimInFlight(t, f.store, a2.ID)

			processScopeControl(t, f.store, f.checkB.ID)
			assertAbortDeferred(t, f.store, f.body.ID, f.b.ID, f.checkA.ID)
			assertBeadState(t, f.store, checkA2.ID, "open", "")
			assertInFlightUntouched(t, f.store, a2.ID)

			closeMember(t, f.store, f.a.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
			processScopeControl(t, f.store, f.checkA.ID)
			assertAbortDeferred(t, f.store, f.body.ID, f.b.ID, checkA2.ID)
			assertBeadState(t, f.store, f.checkA.ID, "closed", beadmeta.OutcomePass)
			assertInFlightUntouched(t, f.store, a2.ID)

			closeMember(t, f.store, a2.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
			processScopeControl(t, f.store, checkA2.ID)
			assertAbortSettled(t, f.store, f.body.ID, f.b.ID, nil)
			assertBeadState(t, f.store, checkA2.ID, "closed", beadmeta.OutcomePass)
		})
	}
}

// TestScopeAbortReconcileDriversDeferWhileMemberInFlight drives the abort
// through each reconcile path instead of a scope-check. Every one reaches the
// same deferral, so none forces the body closed past the in-flight member, and
// the member's own scope-check settles the body once it closes.
func TestScopeAbortReconcileDriversDeferWhileMemberInFlight(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// drive runs the reconcile path and returns the bead that decided the abort.
		drive func(t *testing.T, f inFlightAbortFixture, opts ProcessOptions) (string, error)
	}{
		{
			name: "retry exhaustion",
			drive: func(_ *testing.T, f inFlightAbortFixture, opts ProcessOptions) (string, error) {
				_, err := reconcileClosedScopeMemberWithOptions(beadstest.Graph(f.store), f.b.ID, opts)
				return f.b.ID, err
			},
		},
		{
			name: "quarantine reconcile",
			drive: func(t *testing.T, f inFlightAbortFixture, _ ProcessOptions) (string, error) {
				closed := "closed"
				if err := f.store.Update(f.checkB.ID, beads.UpdateOpts{
					Status:   &closed,
					Labels:   []string{"gc:control-quarantined"},
					Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail},
				}); err != nil {
					t.Fatalf("quarantining %s: %v", f.checkB.ID, err)
				}
				_, err := ReconcileClosedScopeMember(beadstest.Graph(f.store), f.checkB.ID)
				return f.checkB.ID, err
			},
		},
		{
			name: "fanout closing fail",
			drive: func(t *testing.T, f inFlightAbortFixture, opts ProcessOptions) (string, error) {
				fanout := mustCreateWorkflowBead(t, f.store, beads.Bead{Title: "fanout", Type: "task", Metadata: map[string]string{
					beadmeta.KindMetadataKey:       beadmeta.KindFanout,
					beadmeta.RootBeadIDMetadataKey: "wf-1",
					beadmeta.ScopeRefMetadataKey:   "body",
					beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleControl,
					beadmeta.StepRefMetadataKey:    "demo.survey-fanout",
				}})
				mustDepAdd(t, f.store, f.body.ID, fanout.ID, "blocks")
				_, err := closeScopedControl(beadstest.Graph(f.store), fanout.ID,
					map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail}, "fanout-fail", opts)
				if err == nil {
					assertBeadState(t, f.store, fanout.ID, "closed", beadmeta.OutcomeFail)
				}
				return fanout.ID, err
			},
		},
	} {
		for _, st := range []struct {
			name  string
			store func() beads.Store
		}{
			{name: "bd close policy", store: func() beads.Store { return newStrictCloseStore() }},
			{name: "non-refusing store", store: func() beads.Store { return beads.NewMemStore() }},
		} {
			t.Run(tc.name+"/"+st.name, func(t *testing.T) {
				t.Parallel()
				f := newInFlightAbortFixture(t, st.store())
				decider, err := tc.drive(t, f, ProcessOptions{Work: beadstest.Work(f.store)})
				if err != nil {
					t.Fatalf("reconcile path: %v", err)
				}
				assertInFlightUntouched(t, f.store, f.a.ID)
				assertAbortDeferred(t, f.store, f.body.ID, decider, f.checkA.ID)

				closeMember(t, f.store, f.a.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
				processScopeControl(t, f.store, f.checkA.ID)
				assertAbortSettled(t, f.store, f.body.ID, f.b.ID, nil)
				assertBeadState(t, f.store, f.checkA.ID, "closed", beadmeta.OutcomePass)
			})
		}
	}
}

// TestScopeAbortWithNoMemberInFlightClosesBodyAtOnce pins the unchanged path: an
// abort with nothing claimed closes the body fail in the same invocation, from a
// scope-check or a reconcile path, and writes no deferral marker.
func TestScopeAbortWithNoMemberInFlightClosesBodyAtOnce(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		drive func(t *testing.T, f inFlightAbortFixture) error
	}{
		{
			name: "scope-check",
			drive: func(t *testing.T, f inFlightAbortFixture) error {
				_, err := ProcessControl(beadstest.Graph(f.store), mustGetBead(t, f.store, f.checkB.ID), ProcessOptions{Work: beadstest.Work(f.store)})
				return err
			},
		},
		{
			name: "reconcile",
			drive: func(_ *testing.T, f inFlightAbortFixture) error {
				_, err := reconcileClosedScopeMemberWithOptions(beadstest.Graph(f.store), f.b.ID, ProcessOptions{Work: beadstest.Work(f.store)})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newInFlightAbortFixture(t, newStrictCloseStore())
			open := "open"
			if err := f.store.Update(f.a.ID, beads.UpdateOpts{Status: &open}); err != nil {
				t.Fatalf("unclaiming %s: %v", f.a.ID, err)
			}
			if err := tc.drive(t, f); err != nil {
				t.Fatalf("abort: %v", err)
			}
			assertBeadState(t, f.store, f.a.ID, "closed", beadmeta.OutcomeSkipped)
			assertAbortSettled(t, f.store, f.body.ID, f.b.ID, map[string]string{"review.verdict": "blocked"})
			if got, ok := mustGetBead(t, f.store, f.body.ID).Metadata[scopeAbortedByKey]; ok {
				t.Fatalf("body carries %s=%q, want no deferral marker", scopeAbortedByKey, got)
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

// abortBoundaryRetryFormula is issue #9507's scope with the member that stays
// in flight retry-managed. The compiler mints Y's first attempt with no
// gc.scope_ref and no scope-check of its own; an attempt spawned at run time
// carries Y's declared gc.scope_ref (TestScopeAbortDeferralFollowsTheClaimedAttemptsOwnScope).
const abortBoundaryRetryFormula = `formula = "abort-boundary"
version = 2
contract = "graph.v2"

[[steps]]
id = "body"
title = "Scope body"
needs = ["x", "y"]
metadata = { "gc.kind" = "scope", "gc.scope_name" = "work", "gc.scope_role" = "body" }

[[steps]]
id = "x"
title = "Member X (fails)"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[[steps]]
id = "y"
title = "Member Y (retry-managed)"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 2
on_exhausted = "hard_fail"
`

// abortBoundaryRalphFormula is the same scope with Y ralph-managed: the bead a
// worker claims is a child of Y's iteration, a member of the iteration's scope
// rather than of this one.
const abortBoundaryRalphFormula = `formula = "abort-boundary"
version = 2
contract = "graph.v2"

[[steps]]
id = "body"
title = "Scope body"
needs = ["x", "y"]
metadata = { "gc.kind" = "scope", "gc.scope_name" = "work", "gc.scope_role" = "body" }

[[steps]]
id = "x"
title = "Member X (fails)"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[[steps]]
id = "y"
title = "Member Y (ralph-managed)"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.ralph]
max_attempts = 2

[steps.ralph.check]
mode = "exec"
path = "` + ralphHardFailCheckScript + `"
timeout = "30s"

[[steps.children]]
id = "c"
title = "Iteration child C"
`

// TestScopeAbortDeferralCoversOnlyDirectMembers pins the boundary formula-spec-v2
// draws around the deferral: only a claimed bead whose own gc.scope_ref names
// the aborting scope, with its own scope-check, defers the abort. A claimed
// first attempt of a retry-managed member, or a claimed child of a
// ralph-managed member's iteration, names no such scope, so on either close
// policy the abort skips the managing control and its scope-check and closes
// the body fail at once, leaving workflow-finalize ready while the claimed bead
// still runs. The abort itself never touches the claimed bead. Extending the
// deferral to these shapes flips this test.
func TestScopeAbortDeferralCoversOnlyDirectMembers(t *testing.T) {
	t.Parallel()

	for _, shape := range []struct {
		name    string
		formula string
		// claimed is the step_ref suffix of the bead a worker holds.
		claimed string
		// skipped are the step_ref suffixes of the managing chain the abort
		// closes skipped around it.
		skipped []string
	}{
		{
			name:    "first retry attempt",
			formula: abortBoundaryRetryFormula,
			claimed: "abort-boundary.y.attempt.1",
			skipped: []string{"abort-boundary.y", "abort-boundary.y-scope-check"},
		},
		{
			name:    "ralph iteration child",
			formula: abortBoundaryRalphFormula,
			claimed: "y.iteration.1.c",
			skipped: []string{"abort-boundary.y", "abort-boundary.y-scope-check", "y.iteration.1", "y.iteration.1.c-scope-check"},
		},
	} {
		for _, tc := range []drainStoreCase{drainUnforcedAbortStores()[0], drainForcedSettleStores()[0]} {
			t.Run(shape.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				fx := cookRalphDrainFixture(t, tc, newCompiledRalphCity(t, "abort-boundary", shape.formula, 0))
				all := drainStoreAll(t, fx.store)
				claimed := drainBeadByRefSuffix(t, all, shape.claimed)
				drainClaimed(t, fx.store, claimed)
				closeMember(t, fx.store, drainBeadByRefSuffix(t, all, "abort-boundary.x").ID, workerFailure(beadmeta.FailureClassHard, "x broke"))

				if result := processScopeControl(t, fx.store, drainBeadByRefSuffix(t, all, "abort-boundary.x-scope-check").ID); result.Action != "scope-fail" {
					t.Fatalf("X's scope-check action = %q, want scope-fail", result.Action)
				}

				body := assertBeadState(t, fx.store, drainBeadByRefSuffix(t, all, "abort-boundary.body").ID, "closed", beadmeta.OutcomeFail)
				if got, ok := body.Metadata[scopeAbortedByKey]; ok {
					t.Fatalf("body carries %s=%q, want no deferral marker: no direct member is claimed", scopeAbortedByKey, got)
				}
				for _, ref := range shape.skipped {
					assertBeadState(t, fx.store, drainBeadByRefSuffix(t, all, ref).ID, "closed", beadmeta.OutcomeSkipped)
				}
				held := mustGetBead(t, fx.store, claimed.ID)
				if held.Status != "in_progress" || held.Assignee != "pool-worker" || held.Metadata[beadmeta.OutcomeMetadataKey] != "" {
					t.Fatalf("claimed %s = %s assignee %q outcome %q, want still in_progress under pool-worker with no outcome",
						shape.claimed, held.Status, held.Assignee, held.Metadata[beadmeta.OutcomeMetadataKey])
				}
				if finalize := drainBeadByRefSuffix(t, all, "abort-boundary.workflow-finalize"); !slices.Contains(readyIDs(t, fx.store), finalize.ID) {
					t.Fatalf("workflow-finalize %s is not ready after the body closed", finalize.ID)
				}
			})
		}
	}
}

// abortBoundaryRetryInRalphFormula puts a retry-managed member inside a ralph
// iteration: Y's compile-time first attempt is namespaced into the iteration's
// scope, while an attempt spawned at run time copies Y's declared metadata,
// which names no scope.
const abortBoundaryRetryInRalphFormula = `formula = "abort-boundary"
version = 2
contract = "graph.v2"

[[steps]]
id = "loop"
title = "Loop"

[steps.ralph]
max_attempts = 2

[steps.ralph.check]
mode = "exec"
path = "` + ralphHardFailCheckScript + `"
timeout = "30s"

[[steps.children]]
id = "a"
title = "Worker A (fails)"

[[steps.children]]
id = "y"
title = "Retry child Y"

[steps.children.retry]
max_attempts = 2
on_exhausted = "hard_fail"
`

// spawnSecondRetryAttempt fails the compile-time first attempt of the retry
// control whose gc.step_ref ends in retryRef with a transient failure, serves
// until the control has spawned attempt 2, and returns attempt 2.
func spawnSecondRetryAttempt(t *testing.T, fx ralphDrainFixture, retryRef string) beads.Bead {
	t.Helper()
	attempt1 := drainBeadByRefSuffix(t, drainStoreAll(t, fx.store), retryRef+".attempt.1")
	if err := updateMetadataAndClose(beadstest.Graph(fx.store), attempt1.ID, workerFailure(beadmeta.FailureClassTransient, "rate_limited")); err != nil {
		t.Fatalf("closing attempt 1: %v", err)
	}
	drainServeHeld(t, fx.crashFixture)
	return drainBeadByRefSuffix(t, drainStoreAll(t, fx.store), retryRef+".attempt.2")
}

// assertAbortDeferredUntilClaimedCloses pins a deferral end to end: the body
// stays open, marked by the decider, with the claimed bead's scope-check open
// and the claimed bead still its worker's; once the claimed bead closes
// passed, the body closes fail with the decider's hard record.
func assertAbortDeferredUntilClaimedCloses(t *testing.T, fx ralphDrainFixture, bodyID, deciderID string, claimed beads.Bead, liveCheckID string) {
	t.Helper()
	assertAbortDeferred(t, fx.store, bodyID, deciderID, liveCheckID)
	if held := mustGetBead(t, fx.store, claimed.ID); held.Status != "in_progress" || held.Assignee != "pool-worker" {
		t.Fatalf("claimed %s = %s assignee %q, want still in_progress under pool-worker", claimed.ID, held.Status, held.Assignee)
	}
	if err := updateMetadataAndClose(beadstest.Graph(fx.store), claimed.ID, workerPass); err != nil {
		t.Fatalf("closing claimed %s: %v", claimed.ID, err)
	}
	if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
		t.Fatalf("serving after the claimed bead closed did not converge: %v", errs)
	}
	assertAbortSettled(t, fx.store, bodyID, deciderID, nil)
}

// TestScopeAbortDeferralFollowsTheClaimedAttemptsOwnScope pins the deferral
// boundary for claimed retry attempts: an attempt defers the abort of the
// scope its own gc.scope_ref names, through its own scope-check, and no other.
// The compiler mints a retry's first attempt without the retry step's scope
// keys, so at top level it defers nothing (TestScopeAbortDeferralCoversOnlyDirectMembers),
// and inside a ralph iteration it takes the iteration's ref, like every
// iteration child, so it defers the iteration's abort. An attempt spawned at
// run time copies the retry step's declared metadata instead: at top level
// attempt 2 is a member of the declared scope and defers its abort, and inside
// a ralph iteration it is no member of the iteration, so the iteration body
// closes fail at once under it and only the ralph control's drain gate, a
// blocks edge on the claimed attempt, holds the verdict.
func TestScopeAbortDeferralFollowsTheClaimedAttemptsOwnScope(t *testing.T) {
	t.Parallel()

	for _, tc := range []drainStoreCase{drainUnforcedAbortStores()[0], drainForcedSettleStores()[0]} {
		t.Run("top-level attempt 2 defers/"+tc.name, func(t *testing.T) {
			t.Parallel()
			fx := cookRalphDrainFixture(t, tc, newCompiledRalphCity(t, "abort-boundary", abortBoundaryRetryFormula, 0))
			attempt2 := spawnSecondRetryAttempt(t, fx, "abort-boundary.y")
			if got := attempt2.Metadata[beadmeta.ScopeRefMetadataKey]; got != "body" {
				t.Fatalf("attempt 2 gc.scope_ref = %q, want body, the scope its retry step declares", got)
			}
			drainClaimed(t, fx.store, attempt2)
			all := drainStoreAll(t, fx.store)
			x := drainBeadByRefSuffix(t, all, "abort-boundary.x")
			closeMember(t, fx.store, x.ID, workerFailure(beadmeta.FailureClassHard, "x broke"))

			if result := processScopeControl(t, fx.store, drainBeadByRefSuffix(t, all, "abort-boundary.x-scope-check").ID); result.Action != "scope-fail" {
				t.Fatalf("X's scope-check action = %q, want scope-fail", result.Action)
			}
			if finalize := drainBeadByRefSuffix(t, all, "abort-boundary.workflow-finalize"); slices.Contains(readyIDs(t, fx.store), finalize.ID) {
				t.Fatalf("workflow-finalize %s is ready while attempt 2 runs", finalize.ID)
			}
			assertAbortDeferredUntilClaimedCloses(t, fx, drainBeadByRefSuffix(t, all, "abort-boundary.body").ID, x.ID, attempt2,
				drainBeadByRefSuffix(t, all, "abort-boundary.y.attempt.2-scope-check").ID)
		})

		// Graph-apply instantiation links a compile-time attempt to its retry
		// control; the MemStore cook does not, and an unlinked attempt's
		// transient failure would abort the iteration through its own
		// scope-check instead of spawning attempt 2.
		t.Run("attempt 1 in a ralph iteration defers/"+tc.name, func(t *testing.T) {
			t.Parallel()
			fx := cookRalphDrainFixture(t, tc, newCompiledRalphCity(t, "abort-boundary", abortBoundaryRetryInRalphFormula, 0))
			linkCompileTimeRetryAttempts(t, fx.store, fx.rootID)
			all := drainStoreAll(t, fx.store)
			attempt1 := drainBeadByRefSuffix(t, all, "loop.iteration.1.y.attempt.1")
			if got := attempt1.Metadata[beadmeta.ScopeRefMetadataKey]; got != "loop.iteration.1" {
				t.Fatalf("attempt 1 gc.scope_ref = %q, want the iteration loop.iteration.1", got)
			}
			drainClaimed(t, fx.store, attempt1)
			a := drainBeadByRefSuffix(t, all, "loop.iteration.1.a")
			if err := updateMetadataAndClose(beadstest.Graph(fx.store), a.ID, workerFailure(beadmeta.FailureClassHard, "a broke")); err != nil {
				t.Fatalf("closing worker A: %v", err)
			}

			drainServeHeld(t, fx.crashFixture)

			assertAbortDeferredUntilClaimedCloses(t, fx, drainBeadByRefSuffix(t, all, "loop.iteration.1").ID, a.ID, attempt1,
				drainBeadByRefSuffix(t, all, "loop.iteration.1.y.attempt.1-scope-check").ID)
		})

		t.Run("attempt 2 in a ralph iteration closes at once/"+tc.name, func(t *testing.T) {
			t.Parallel()
			fx := cookRalphDrainFixture(t, tc, newCompiledRalphCity(t, "abort-boundary", abortBoundaryRetryInRalphFormula, 0))
			linkCompileTimeRetryAttempts(t, fx.store, fx.rootID)
			attempt2 := spawnSecondRetryAttempt(t, fx, "loop.iteration.1.y")
			all := drainStoreAll(t, fx.store)
			if got := attempt2.Metadata[beadmeta.ScopeRefMetadataKey]; got != "" {
				t.Fatalf("attempt 2 gc.scope_ref = %q, want none: retry step Y declares no scope", got)
			}
			if checks := drainBeadsByRefSuffix(all, "loop.iteration.1.y.attempt.2-scope-check"); len(checks) != 0 {
				t.Fatalf("attempt 2 has %d scope-checks, want none", len(checks))
			}
			drainClaimed(t, fx.store, attempt2)
			a := drainBeadByRefSuffix(t, all, "loop.iteration.1.a")
			if err := updateMetadataAndClose(beadstest.Graph(fx.store), a.ID, workerFailure(beadmeta.FailureClassHard, "a broke")); err != nil {
				t.Fatalf("closing worker A: %v", err)
			}

			drainServeHeld(t, fx.crashFixture)

			body := drainBeadByRefSuffix(t, all, "loop.iteration.1")
			assertAbortSettled(t, fx.store, body.ID, a.ID, nil)
			if got, ok := mustGetBead(t, fx.store, body.ID).Metadata[scopeAbortedByKey]; ok {
				t.Fatalf("iteration body carries %s=%q, want no deferral marker: attempt 2 is no member of the iteration", scopeAbortedByKey, got)
			}
			if held := mustGetBead(t, fx.store, attempt2.ID); held.Status != "in_progress" || held.Assignee != "pool-worker" {
				t.Fatalf("attempt 2 = %s assignee %q, want still in_progress under pool-worker", held.Status, held.Assignee)
			}
			control := assertBeadState(t, fx.store, drainBeadByRefSuffix(t, all, "abort-boundary.loop").ID, "open", "")
			if log := control.Metadata[beadmeta.AttemptLogMetadataKey]; log != "" {
				t.Fatalf("ralph control gc.attempt_log = %s, want empty while attempt 2 runs", log)
			}
			if !drainBlocksOn(t, fx.store, control, attempt2.ID) {
				t.Fatalf("ralph control %s has no blocks edge on the claimed attempt 2 %s", control.ID, attempt2.ID)
			}
			drainRequireNoIteration(t, drainStoreAll(t, fx.store), 2)

			if err := updateMetadataAndClose(beadstest.Graph(fx.store), attempt2.ID, workerPass); err != nil {
				t.Fatalf("closing attempt 2: %v", err)
			}
			if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
				t.Fatalf("serving after attempt 2 closed did not converge: %v", errs)
			}
			loop := assertBeadState(t, fx.store, control.ID, "closed", beadmeta.OutcomeFail)
			if got := loop.Metadata[beadmeta.FinalDispositionMetadataKey]; got != beadmeta.DispositionHardFail {
				t.Fatalf("ralph control %s = %q, want %q", beadmeta.FinalDispositionMetadataKey, got, beadmeta.DispositionHardFail)
			}
			drainRequireNoIteration(t, drainStoreAll(t, fx.store), 2)
		})
	}
}

// nestedRalphAbortFormula nests a ralph in a ralph iteration: the outer loop's
// iteration runs worker A beside an inner loop over child C. The compiler
// flattens the inner loop into the outer iteration, so C and the inner
// iteration body both carry the outer iteration as their gc.scope_ref, and the
// inner iteration body is minted before the outer one.
const nestedRalphAbortFormula = `formula = "nested-abort"
version = 2
contract = "graph.v2"

[[steps]]
id = "loop"
title = "Outer loop"

[steps.ralph]
max_attempts = 2

[steps.ralph.check]
mode = "exec"
path = "` + ralphHardFailCheckScript + `"
timeout = "30s"

[[steps.children]]
id = "a"
title = "Worker A"

[[steps.children]]
id = "inner"
title = "Inner loop"

[steps.children.ralph]
max_attempts = 2

[steps.children.ralph.check]
mode = "exec"
path = "` + ralphHardFailCheckScript + `"
timeout = "30s"

[[steps.children.children]]
id = "c"
title = "Inner child C"
`

// TestScopeAbortInNestedRalphSettlesTheOuterIterationBody pins the abort of an
// iteration that holds a nested ralph: worker A fails hard while the inner
// loop's child C is claimed. The abort resolves the outer iteration body, the
// one whose own gc.step_ref names the scope, not the inner iteration body that
// only carries the scope as its gc.scope_ref, so the outer body defers, marked
// by A, and the inner body stays unmarked. Once C closes, even passed, the
// outer body closes fail with A's hard record, the outer loop hard-fails on it
// with no iteration 2, and the workflow root ends fail, whether or not the
// loop's check would pass.
func TestScopeAbortInNestedRalphSettlesTheOuterIterationBody(t *testing.T) {
	t.Parallel()

	for _, check := range []struct {
		name     string
		passFrom int
	}{
		{name: "check fails", passFrom: 0},
		{name: "check passes", passFrom: 1},
	} {
		for _, tc := range []drainStoreCase{drainUnforcedAbortStores()[0], drainForcedSettleStores()[0]} {
			t.Run(check.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				fx := cookRalphDrainFixture(t, tc, newCompiledRalphCity(t, "nested-abort", nestedRalphAbortFormula, check.passFrom))
				all := drainStoreAll(t, fx.store)
				a := drainBeadByRefSuffix(t, all, "loop.iteration.1.a")
				c := drainBeadByRefSuffix(t, all, "loop.iteration.1.inner.iteration.1.c")
				outerBody := drainBeadByRefSuffix(t, all, "loop.iteration.1")
				innerBody := drainBeadByRefSuffix(t, all, "loop.iteration.1.inner.iteration.1")
				control := drainBeadByRefSuffix(t, all, "nested-abort.loop")
				drainClaimed(t, fx.store, c)
				if err := updateMetadataAndClose(beadstest.Graph(fx.store), a.ID, workerFailure(beadmeta.FailureClassHard, "a broke")); err != nil {
					t.Fatalf("closing worker A: %v", err)
				}

				drainServeHeld(t, fx.crashFixture)

				assertAbortDeferred(t, fx.store, outerBody.ID, a.ID,
					drainBeadByRefSuffix(t, all, "loop.iteration.1.inner.iteration.1.c-scope-check").ID)
				if got := mustGetBead(t, fx.store, innerBody.ID).Metadata[scopeAbortedByKey]; got != "" {
					t.Fatalf("inner iteration body %s = %q, want unmarked: the abort is the outer iteration's", scopeAbortedByKey, got)
				}
				if held := mustGetBead(t, fx.store, c.ID); held.Status != "in_progress" || held.Assignee != "pool-worker" {
					t.Fatalf("child C = %s assignee %q, want still in_progress under pool-worker", held.Status, held.Assignee)
				}
				assertBeadState(t, fx.store, control.ID, "open", "")
				drainRequireNoIteration(t, drainStoreAll(t, fx.store), 2)

				if err := updateMetadataAndClose(beadstest.Graph(fx.store), c.ID, workerPass); err != nil {
					t.Fatalf("closing child C: %v", err)
				}
				if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
					t.Fatalf("serving after child C closed did not converge: %v", errs)
				}

				assertAbortSettled(t, fx.store, outerBody.ID, a.ID, nil)
				loop := assertBeadState(t, fx.store, control.ID, "closed", beadmeta.OutcomeFail)
				for key, want := range map[string]string{
					beadmeta.FinalDispositionMetadataKey: beadmeta.DispositionHardFail,
					beadmeta.FailureReasonMetadataKey:    "a broke",
					beadmeta.FailedAttemptMetadataKey:    "1",
				} {
					if got := loop.Metadata[key]; got != want {
						t.Errorf("outer ralph control %s = %q, want %q", key, got, want)
					}
				}
				drainRequireNoIteration(t, drainStoreAll(t, fx.store), 2)
				assertBeadState(t, fx.store, fx.rootID, "closed", beadmeta.OutcomeFail)
			})
		}
	}
}

// TestScopeAbortInNestedRalphWithNothingClaimedClosesTheOuterBody pins the same
// resolution with no claimed bead: worker A's hard failure closes the outer
// iteration body fail at once, with A's hard record, and the outer loop and the
// workflow root end fail with no iteration 2.
func TestScopeAbortInNestedRalphWithNothingClaimedClosesTheOuterBody(t *testing.T) {
	t.Parallel()

	for _, tc := range []drainStoreCase{drainUnforcedAbortStores()[0], drainForcedSettleStores()[0]} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := cookRalphDrainFixture(t, tc, newCompiledRalphCity(t, "nested-abort", nestedRalphAbortFormula, 0))
			all := drainStoreAll(t, fx.store)
			a := drainBeadByRefSuffix(t, all, "loop.iteration.1.a")
			if err := updateMetadataAndClose(beadstest.Graph(fx.store), a.ID, workerFailure(beadmeta.FailureClassHard, "a broke")); err != nil {
				t.Fatalf("closing worker A: %v", err)
			}
			if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
				t.Fatalf("serving after worker A failed did not converge: %v", errs)
			}

			assertAbortSettled(t, fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.1").ID, a.ID, nil)
			loop := assertBeadState(t, fx.store, drainBeadByRefSuffix(t, all, "nested-abort.loop").ID, "closed", beadmeta.OutcomeFail)
			if got := loop.Metadata[beadmeta.FinalDispositionMetadataKey]; got != beadmeta.DispositionHardFail {
				t.Fatalf("outer ralph control %s = %q, want %q", beadmeta.FinalDispositionMetadataKey, got, beadmeta.DispositionHardFail)
			}
			drainRequireNoIteration(t, drainStoreAll(t, fx.store), 2)
			assertBeadState(t, fx.store, fx.rootID, "closed", beadmeta.OutcomeFail)
		})
	}
}
