package dispatch

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// These tests pin gastownhall/gascity#9507: a scope under abort_scope whose
// member A fails while sibling B is claimed by a worker. The abort leaves B to
// its worker and keeps B's scope-check alive; the body records
// gc.outcome=fail while it stays open and closes fail once the last kept
// scope-check runs, on every store.

const claimedScopeWorker = "worker-b"

// claimedScopeStoreCase opens one store arm of the claimed-member fixtures.
type claimedScopeStoreCase struct {
	name string
	open func(t *testing.T) beads.Store
}

// claimedScopeRefusingStores refuse an unforced close of a bead whose blocker
// is still in_progress, as bd does.
func claimedScopeRefusingStores() []claimedScopeStoreCase {
	return []claimedScopeStoreCase{
		{name: "strict-close", open: func(*testing.T) beads.Store { return newStrictCloseStore() }},
		{name: "bd-close-policy", open: func(*testing.T) beads.Store { return newBdClosePolicyStore() }},
	}
}

// claimedScopePermissiveStores accept closing a blocked bead.
func claimedScopePermissiveStores() []claimedScopeStoreCase {
	return []claimedScopeStoreCase{
		{name: "mem", open: func(*testing.T) beads.Store { return beads.NewMemStore() }},
		{name: "sqlite", open: func(t *testing.T) beads.Store {
			store, _ := drainSQLiteStore(t)
			return store
		}},
	}
}

func claimedScopeAllStores() []claimedScopeStoreCase {
	return append(claimedScopeRefusingStores(), claimedScopePermissiveStores()...)
}

// claimedScopeFixture is one scope "body" under root wf-1 with abort_scope
// members, each paired with its scope-check: A closed with a failure, B
// claimed by a worker, C open and needing B (through B's scope-check, as the
// graph compiler rewrites it), and optionally D, a second claimed member. The
// body needs every work member, so a refusing store will not close it while B
// is claimed.
type claimedScopeFixture struct {
	store     beads.Store
	body      beads.Bead
	a, aCheck beads.Bead
	b, bCheck beads.Bead
	c, cCheck beads.Bead
	d, dCheck beads.Bead
	hasD      bool
}

type claimedScopeOptions struct {
	secondClaimed bool
	// aSoft closes A with a failure that carries no hard class.
	aSoft bool
}

func newClaimedScopeFixture(t *testing.T, store beads.Store, opts claimedScopeOptions) claimedScopeFixture {
	t.Helper()
	fx := claimedScopeFixture{store: store, hasD: opts.secondClaimed}
	fx.body = mustCreateWorkflowBead(t, store, beads.Bead{Title: "body", Type: "task", Metadata: map[string]string{
		"gc.kind":         "scope",
		"gc.scope_role":   "body",
		"gc.root_bead_id": "wf-1",
		"gc.step_ref":     "demo.body",
	}})
	member := func(title, step string, status string, extra map[string]string) beads.Bead {
		meta := map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.step_ref":     "demo." + step,
			"gc.on_fail":      "abort_scope",
		}
		maps.Copy(meta, extra)
		return mustCreateWorkflowBead(t, store, beads.Bead{Title: title, Type: "task", Status: status, Metadata: meta})
	}
	check := func(subject beads.Bead, step string) beads.Bead {
		created := mustCreateWorkflowBead(t, store, beads.Bead{Title: "Finalize scope for " + step, Type: "task", Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
			"gc.step_ref":     "demo." + step + "-scope-check",
			"gc.control_for":  "demo." + step,
		}})
		mustDepAdd(t, store, created.ID, subject.ID, "blocks")
		return mustGetBead(t, store, created.ID)
	}

	aMeta := map[string]string{"gc.outcome": "fail", "gc.failure_reason": "tests_failed"}
	if !opts.aSoft {
		aMeta["gc.failure_class"] = "hard"
	}
	fx.a = member("step-a", "step-a", "closed", aMeta)
	fx.aCheck = check(fx.a, "step-a")
	fx.b = member("step-b", "step-b", "", nil)
	fx.bCheck = check(fx.b, "step-b")
	fx.c = member("step-c", "step-c", "", nil)
	mustDepAdd(t, store, fx.c.ID, fx.bCheck.ID, "blocks")
	fx.cCheck = check(fx.c, "step-c")
	workIDs := []string{fx.a.ID, fx.b.ID, fx.c.ID}
	if opts.secondClaimed {
		fx.d = member("step-d", "step-d", "", nil)
		fx.dCheck = check(fx.d, "step-d")
		workIDs = append(workIDs, fx.d.ID)
	}
	for _, id := range workIDs {
		mustDepAdd(t, store, fx.body.ID, id, "blocks")
	}
	fx.b = claimScopeMember(t, store, fx.b.ID)
	if opts.secondClaimed {
		fx.d = claimScopeMember(t, store, fx.d.ID)
	}
	fx.body = mustGetBead(t, store, fx.body.ID)
	return fx
}

func claimScopeMember(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	status, assignee := "in_progress", claimedScopeWorker
	if err := store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("claim %s: %v", id, err)
	}
	return mustGetBead(t, store, id)
}

// workerCloses closes a claimed member the way its worker does: metadata
// first, then a plain close.
func workerCloses(t *testing.T, store beads.Store, id string, metadata map[string]string) {
	t.Helper()
	if err := store.SetMetadataBatch(id, metadata); err != nil {
		t.Fatalf("worker metadata on %s: %v", id, err)
	}
	if err := store.Close(id); err != nil {
		t.Fatalf("worker close %s: %v", id, err)
	}
}

func processScopeCheckOK(t *testing.T, store beads.Store, checkID string) ControlResult {
	t.Helper()
	result, err := ProcessControl(beadstest.Graph(store), mustGetBead(t, store, checkID), ProcessOptions{Work: beadstest.Work(store)})
	if err != nil {
		t.Fatalf("ProcessControl(%s): %v", checkID, err)
	}
	return result
}

func assertStatusOutcome(t *testing.T, store beads.Store, label, id, wantStatus, wantOutcome string) beads.Bead {
	t.Helper()
	got := mustGetBead(t, store, id)
	if got.Status != wantStatus || got.Metadata["gc.outcome"] != wantOutcome {
		t.Fatalf("%s %s = status %q outcome %q, want status %q outcome %q", label, id, got.Status, got.Metadata["gc.outcome"], wantStatus, wantOutcome)
	}
	return got
}

// assertClaimedUntouched pins that the abort left a claimed member and its
// scope-check exactly as its worker holds them.
func assertClaimedUntouched(t *testing.T, store beads.Store, member, memberCheck beads.Bead) {
	t.Helper()
	got := mustGetBead(t, store, member.ID)
	if got.Status != "in_progress" || got.Assignee != claimedScopeWorker || got.Metadata["gc.outcome"] != "" {
		t.Fatalf("claimed member %s = status %q assignee %q outcome %q, want in_progress/%s/none",
			member.ID, got.Status, got.Assignee, got.Metadata["gc.outcome"], claimedScopeWorker)
	}
	gotCheck := mustGetBead(t, store, memberCheck.ID)
	if gotCheck.Status != "open" || gotCheck.Metadata["gc.outcome"] != "" {
		t.Fatalf("claimed member's scope-check %s = status %q outcome %q, want open with no outcome",
			memberCheck.ID, gotCheck.Status, gotCheck.Metadata["gc.outcome"])
	}
}

// assertDeferredBody pins the body of a deferred abort: open, carrying
// gc.outcome=fail and the culprit's hard-failure record.
func assertDeferredBody(t *testing.T, store beads.Store, fx claimedScopeFixture, culpritID string) {
	t.Helper()
	body := assertStatusOutcome(t, store, "body", fx.body.ID, "open", "fail")
	if got := body.Metadata["gc.failure_subject"]; got != culpritID {
		t.Fatalf("body gc.failure_subject = %q, want %q", got, culpritID)
	}
	if culpritID != "" && body.Metadata["gc.failure_class"] != "hard" {
		t.Fatalf("body gc.failure_class = %q, want hard", body.Metadata["gc.failure_class"])
	}
}

// claimedScopeLedger renders the fixture's beads for a cross-store comparison:
// fixture role, status, assignee and every gc.outcome/failure key.
func claimedScopeLedger(t *testing.T, fx claimedScopeFixture) string {
	t.Helper()
	roles := []struct {
		name string
		id   string
	}{
		{"body", fx.body.ID},
		{"a", fx.a.ID},
		{"a-check", fx.aCheck.ID},
		{"b", fx.b.ID},
		{"b-check", fx.bCheck.ID},
		{"c", fx.c.ID},
		{"c-check", fx.cCheck.ID},
	}
	idRole := map[string]string{}
	for _, r := range roles {
		idRole[r.id] = r.name
	}
	var lines []string
	for _, r := range roles {
		got := mustGetBead(t, fx.store, r.id)
		line := fmt.Sprintf("%s status=%s assignee=%s", r.name, got.Status, got.Assignee)
		for _, key := range []string{"gc.outcome", "gc.failure_class", "gc.failure_reason", "gc.failure_subject"} {
			value := got.Metadata[key]
			if role, ok := idRole[value]; ok {
				value = role
			}
			line += fmt.Sprintf(" %s=%s", key, value)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// TestScopeAbortDefersBodyWhileMemberClaimed covers AC-1 and AC-2: the abort
// skips the open members, keeps the claimed member and its scope-check, and
// records the failure on the still-open body, identically on refusing and
// permissive stores.
func TestScopeAbortDefersBodyWhileMemberClaimed(t *testing.T) {
	t.Parallel()

	ledgers := map[string]string{}
	stores := claimedScopeAllStores()
	for _, tc := range stores {
		store := tc.open(t)
		fx := newClaimedScopeFixture(t, store, claimedScopeOptions{})

		result, err := ProcessControl(beadstest.Graph(store), fx.aCheck, ProcessOptions{Work: beadstest.Work(store)})
		if err != nil {
			t.Fatalf("%s: ProcessControl(A's scope-check): %v", tc.name, err)
		}
		if !result.Processed || result.Action != "scope-fail" || result.Skipped != 2 {
			t.Fatalf("%s: result = %+v, want processed scope-fail skipping C and C's scope-check", tc.name, result)
		}
		aCheck := assertStatusOutcome(t, store, tc.name+": A's scope-check", fx.aCheck.ID, "closed", "pass")
		for key := range aCheck.Metadata {
			if strings.Contains(key, "retry") || strings.Contains(key, "quarantine") {
				t.Fatalf("%s: A's scope-check carries %s=%q: the abort consumed retry budget", tc.name, key, aCheck.Metadata[key])
			}
		}
		assertStatusOutcome(t, store, tc.name+": C", fx.c.ID, "closed", "skipped")
		assertStatusOutcome(t, store, tc.name+": C's scope-check", fx.cCheck.ID, "closed", "skipped")
		assertClaimedUntouched(t, store, fx.b, fx.bCheck)
		assertDeferredBody(t, store, fx, fx.a.ID)
		ledgers[tc.name] = claimedScopeLedger(t, fx)
	}
	want := ledgers[stores[0].name]
	for _, tc := range stores[1:] {
		if ledgers[tc.name] != want {
			t.Fatalf("ledger on %s diverges from %s:\n%s\n--- want ---\n%s", tc.name, stores[0].name, ledgers[tc.name], want)
		}
	}
}

// TestScopeLatePassAfterDeferredAbortSettlesFail covers AC-3: the last claimed
// member closing pass settles the deferred body fail, through its scope-check
// or through ReconcileClosedScopeMember.
func TestScopeLatePassAfterDeferredAbortSettlesFail(t *testing.T) {
	t.Parallel()

	settle := map[string]func(t *testing.T, fx claimedScopeFixture){
		"via_scope_check": func(t *testing.T, fx claimedScopeFixture) {
			result := processScopeCheckOK(t, fx.store, fx.bCheck.ID)
			if !result.Processed || result.Action != "scope-pass" {
				t.Fatalf("B's scope-check result = %+v, want processed scope-pass", result)
			}
		},
		"via_reconcile": func(t *testing.T, fx claimedScopeFixture) {
			// B's scope-check has already closed, so only the reconcile of
			// B's close is left to settle the scope.
			if err := updateMetadataAndClose(beadstest.Graph(fx.store), fx.bCheck.ID, map[string]string{"gc.outcome": "pass"}); err != nil {
				t.Fatalf("close B's scope-check: %v", err)
			}
			result, err := ReconcileClosedScopeMember(beadstest.Graph(fx.store), fx.b.ID)
			if err != nil {
				t.Fatalf("ReconcileClosedScopeMember(B): %v", err)
			}
			if !result.Processed {
				t.Fatalf("ReconcileClosedScopeMember(B) = %+v, want the scope settled", result)
			}
		},
	}
	for name, run := range settle {
		for _, tc := range claimedScopeAllStores() {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				store := tc.open(t)
				fx := newClaimedScopeFixture(t, store, claimedScopeOptions{})
				processScopeCheckOK(t, store, fx.aCheck.ID)
				workerCloses(t, store, fx.b.ID, map[string]string{
					"gc.outcome":     "pass",
					"review.verdict": "ok",
					"gc.output_json": `{"items":[]}`,
				})

				run(t, fx)

				body := assertStatusOutcome(t, store, "body", fx.body.ID, "closed", "fail")
				if body.Metadata["gc.failure_subject"] != fx.a.ID || body.Metadata["gc.failure_class"] != "hard" {
					t.Fatalf("body hard-failure record = subject %q class %q, want A (%s) hard",
						body.Metadata["gc.failure_subject"], body.Metadata["gc.failure_class"], fx.a.ID)
				}
				if got := body.Metadata["review.verdict"]; got != "ok" {
					t.Fatalf("body review.verdict = %q, want ok propagated from B", got)
				}
				if got := body.Metadata["gc.output_json"]; got != "" {
					t.Fatalf("body gc.output_json = %q, want none on a failed body", got)
				}
				b := assertStatusOutcome(t, store, "B", fx.b.ID, "closed", "pass")
				if b.Assignee != claimedScopeWorker {
					t.Fatalf("B assignee = %q, want %q", b.Assignee, claimedScopeWorker)
				}
				assertStatusOutcome(t, store, "B's scope-check", fx.bCheck.ID, "closed", "pass")
			})
		}
	}
}

// TestScopeLatePassOnClosedFailedBodyPropagatesMetadataOnly covers the
// closed-body branch of AC-3: a member passing after its scope's body already
// closed fail contributes its non-gc metadata and nothing else.
func TestScopeLatePassOnClosedFailedBodyPropagatesMetadataOnly(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	fx := newClaimedScopeFixture(t, store, claimedScopeOptions{})
	if err := updateMetadataAndClose(beadstest.Graph(store), fx.body.ID, map[string]string{"gc.outcome": "fail"}); err != nil {
		t.Fatalf("close body fail: %v", err)
	}
	for _, id := range []string{fx.aCheck.ID, fx.c.ID, fx.cCheck.ID} {
		if err := updateMetadataAndClose(beadstest.Graph(store), id, map[string]string{"gc.outcome": "skipped"}); err != nil {
			t.Fatalf("close %s: %v", id, err)
		}
	}
	workerCloses(t, store, fx.b.ID, map[string]string{
		"gc.outcome":     "pass",
		"review.verdict": "ok",
		"gc.output_json": `{"items":[]}`,
	})

	processScopeCheckOK(t, store, fx.bCheck.ID)

	body := assertStatusOutcome(t, store, "body", fx.body.ID, "closed", "fail")
	if got := body.Metadata["review.verdict"]; got != "ok" {
		t.Fatalf("body review.verdict = %q, want ok", got)
	}
	if got := body.Metadata["gc.output_json"]; got != "" {
		t.Fatalf("body gc.output_json = %q, want none on a failed body", got)
	}
}

// TestReproScopeMemberPassAfterSiblingFailureClosesBodyFail is the triage
// repro of gastownhall/gascity#9507 on a ledger written before the abort
// recorded its verdict on the body: the failed sibling's scope-check was
// quarantined, so the body carries no outcome. The late pass must still settle
// the scope fail.
func TestReproScopeMemberPassAfterSiblingFailureClosesBodyFail(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
		"gc.kind":             "workflow",
		"gc.formula_contract": "graph.v2",
	}})
	body := mustCreateWorkflowBead(t, store, beads.Bead{Title: "body", Type: "task", Metadata: map[string]string{
		"gc.kind":         "scope",
		"gc.scope_role":   "body",
		"gc.root_bead_id": workflow.ID,
		"gc.step_ref":     "demo.body",
	}})
	member := func(title, stepRef string, extra map[string]string) beads.Bead {
		meta := map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.step_ref":     stepRef,
			"gc.on_fail":      "abort_scope",
		}
		maps.Copy(meta, extra)
		return mustCreateWorkflowBead(t, store, beads.Bead{Title: title, Type: "task", Status: "closed", Metadata: meta})
	}
	member("step-b (failed)", "demo.step-b", map[string]string{"gc.outcome": "fail"})
	member("step-b scope-check (quarantined)", "demo.step-b-scope-check", map[string]string{
		"gc.kind": "scope-check", "gc.control_for": "demo.step-b", "gc.outcome": "fail",
	})
	member("step-a scope-check (skipped by the abort)", "demo.step-a-scope-check", map[string]string{
		"gc.kind": "scope-check", "gc.control_for": "demo.step-a", "gc.outcome": "skipped",
	})
	inFlight := member("step-a (in flight, closes pass last)", "demo.step-a", map[string]string{"gc.outcome": "pass"})

	if _, err := ReconcileClosedScopeMember(beadstest.Graph(store), inFlight.ID); err != nil {
		t.Fatalf("ReconcileClosedScopeMember(%s): %v", inFlight.ID, err)
	}
	got := mustGetBead(t, store, body.ID)
	if got.Status != "closed" {
		t.Fatalf("body status = %q, want closed", got.Status)
	}
	if outcome := got.Metadata["gc.outcome"]; outcome != "fail" {
		t.Fatalf("body gc.outcome = %q, want fail: sibling step-b failed with on_fail=abort_scope, so the scope must not settle as pass", outcome)
	}
}

// TestScopeLateFailAfterDeferredAbortSettlesFail covers AC-4: the claimed
// member closing fail settles the body fail, and the hard-failure record names
// the first hard-failed member in bead-ID order.
func TestScopeLateFailAfterDeferredAbortSettlesFail(t *testing.T) {
	t.Parallel()

	for _, aSoft := range []bool{false, true} {
		for _, tc := range claimedScopeAllStores() {
			t.Run(fmt.Sprintf("%s/a_soft=%t", tc.name, aSoft), func(t *testing.T) {
				t.Parallel()
				store := tc.open(t)
				fx := newClaimedScopeFixture(t, store, claimedScopeOptions{aSoft: aSoft})
				processScopeCheckOK(t, store, fx.aCheck.ID)
				workerCloses(t, store, fx.b.ID, map[string]string{
					"gc.outcome":        "fail",
					"gc.failure_class":  "hard",
					"gc.failure_reason": "build_failed",
				})

				result := processScopeCheckOK(t, store, fx.bCheck.ID)
				if result.Action != "scope-fail" {
					t.Fatalf("B's scope-check action = %q, want scope-fail", result.Action)
				}

				body := assertStatusOutcome(t, store, "body", fx.body.ID, "closed", "fail")
				wantCulprit, ok := firstHardFailedMember([]beads.Bead{mustGetBead(t, store, fx.a.ID), mustGetBead(t, store, fx.b.ID)}, fx.body.ID)
				if !ok {
					t.Fatal("no hard-failed member among A and B")
				}
				if aSoft && wantCulprit.ID != fx.b.ID {
					t.Fatalf("culprit rule picked %s, want B (%s): A failed soft", wantCulprit.ID, fx.b.ID)
				}
				if got := body.Metadata["gc.failure_subject"]; got != wantCulprit.ID {
					t.Fatalf("body gc.failure_subject = %q, want %q", got, wantCulprit.ID)
				}
				if got := body.Metadata["gc.failure_class"]; got != "hard" {
					t.Fatalf("body gc.failure_class = %q, want hard", got)
				}
				assertStatusOutcome(t, store, "B's scope-check", fx.bCheck.ID, "closed", "pass")
			})
		}
	}
}

// TestScopeDeferredAbortWithTwoClaimedMembers covers AC-5: with two claimed
// members, the first to close leaves the body open and failed, and the last
// one's scope-check closes it.
func TestScopeDeferredAbortWithTwoClaimedMembers(t *testing.T) {
	t.Parallel()

	for _, firstOutcome := range []string{"pass", "fail"} {
		for _, tc := range claimedScopeAllStores() {
			t.Run(firstOutcome+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				store := tc.open(t)
				fx := newClaimedScopeFixture(t, store, claimedScopeOptions{secondClaimed: true})
				processScopeCheckOK(t, store, fx.aCheck.ID)
				assertClaimedUntouched(t, store, fx.b, fx.bCheck)
				assertClaimedUntouched(t, store, fx.d, fx.dCheck)
				assertDeferredBody(t, store, fx, fx.a.ID)

				workerCloses(t, store, fx.b.ID, map[string]string{"gc.outcome": firstOutcome})
				processScopeCheckOK(t, store, fx.bCheck.ID)
				assertStatusOutcome(t, store, "B's scope-check", fx.bCheck.ID, "closed", "pass")
				assertClaimedUntouched(t, store, fx.d, fx.dCheck)
				assertDeferredBody(t, store, fx, fx.a.ID)

				workerCloses(t, store, fx.d.ID, map[string]string{"gc.outcome": "pass"})
				processScopeCheckOK(t, store, fx.dCheck.ID)
				assertStatusOutcome(t, store, "D's scope-check", fx.dCheck.ID, "closed", "pass")
				body := assertStatusOutcome(t, store, "body", fx.body.ID, "closed", "fail")
				if got := body.Metadata["gc.failure_subject"]; got != fx.a.ID {
					t.Fatalf("body gc.failure_subject = %q, want A (%s)", got, fx.a.ID)
				}
				assertStatusOutcome(t, store, "B", fx.b.ID, "closed", firstOutcome)
				assertStatusOutcome(t, store, "D", fx.d.ID, "closed", "pass")
			})
		}
	}
}

// TestScopedControlFailureDefersBodyWhileMemberClaimed covers AC-6: a failing
// fanout or drain control settles its scope through closeScopedControl
// without force-closing the body over a claimed sibling.
func TestScopedControlFailureDefersBodyWhileMemberClaimed(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{beadmeta.KindFanout, beadmeta.KindDrain} {
		for _, tc := range claimedScopeAllStores() {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				store := tc.open(t)
				fx := newClaimedScopeFixture(t, store, claimedScopeOptions{})
				// The scope's failure comes from the control alone.
				if err := store.SetMetadataBatch(fx.a.ID, map[string]string{"gc.outcome": "pass", "gc.failure_class": "", "gc.failure_reason": ""}); err != nil {
					t.Fatalf("pass A: %v", err)
				}
				if err := updateMetadataAndClose(beadstest.Graph(store), fx.aCheck.ID, map[string]string{"gc.outcome": "pass"}); err != nil {
					t.Fatalf("close A's scope-check: %v", err)
				}
				control := mustCreateWorkflowBead(t, store, beads.Bead{Title: kind + " control", Type: "task", Metadata: map[string]string{
					"gc.kind":         kind,
					"gc.root_bead_id": "wf-1",
					"gc.scope_ref":    "body",
					"gc.scope_role":   "control",
					"gc.step_ref":     "demo.step-a-" + kind,
				}})
				mustDepAdd(t, store, fx.body.ID, control.ID, "blocks")

				result, err := closeScopedControl(beadstest.Graph(store), control.ID, map[string]string{
					"gc.outcome":        "fail",
					"gc.failure_class":  "hard",
					"gc.failure_reason": kind + "_failed",
				}, kind+"-failed", ProcessOptions{})
				if err != nil {
					t.Fatalf("closeScopedControl: %v", err)
				}
				if result.Action != kind+"-failed" {
					t.Fatalf("result = %+v, want action %s-failed", result, kind)
				}
				assertStatusOutcome(t, store, kind+" control", control.ID, "closed", "fail")
				assertClaimedUntouched(t, store, fx.b, fx.bCheck)
				assertStatusOutcome(t, store, "C", fx.c.ID, "closed", "skipped")
				assertDeferredBody(t, store, fx, control.ID)

				workerCloses(t, store, fx.b.ID, map[string]string{"gc.outcome": "pass"})
				processScopeCheckOK(t, store, fx.bCheck.ID)
				body := assertStatusOutcome(t, store, "body", fx.body.ID, "closed", "fail")
				if got := body.Metadata["gc.failure_subject"]; got != control.ID {
					t.Fatalf("body gc.failure_subject = %q, want the %s control (%s)", got, kind, control.ID)
				}
				assertStatusOutcome(t, store, "B", fx.b.ID, "closed", "pass")
			})
		}
	}
}

// TestScopeAbortWithoutClaimedMembersClosesBodyAtOnce covers AC-7: with no
// claimed member, the abort skips the open members and closes the body fail
// in the same invocation, through a scope-check and through a failing
// scoped control.
func TestScopeAbortWithoutClaimedMembersClosesBodyAtOnce(t *testing.T) {
	t.Parallel()

	// unclaim returns B to the open queue, the shape of a scope with no
	// claimed member.
	unclaim := func(t *testing.T, store beads.Store, id string) {
		t.Helper()
		status, assignee := "open", ""
		if err := store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			t.Fatalf("unclaim %s: %v", id, err)
		}
	}
	// Each driver settles the scope and returns its result and the member
	// the body's hard-failure record must name.
	drivers := map[string]func(t *testing.T, fx claimedScopeFixture) (ControlResult, string){
		"scope_check": func(t *testing.T, fx claimedScopeFixture) (ControlResult, string) {
			return processScopeCheckOK(t, fx.store, fx.aCheck.ID), fx.a.ID
		},
	}
	failMeta := func(kind string) map[string]string {
		return map[string]string{"gc.outcome": "fail", "gc.failure_class": "hard", "gc.failure_reason": kind + "_exhausted"}
	}
	newControl := func(t *testing.T, fx claimedScopeFixture, kind, status string) beads.Bead {
		t.Helper()
		// The scope's failure comes from the control alone.
		if err := fx.store.SetMetadataBatch(fx.a.ID, map[string]string{"gc.outcome": "pass", "gc.failure_class": "", "gc.failure_reason": ""}); err != nil {
			t.Fatalf("pass A: %v", err)
		}
		if err := updateMetadataAndClose(beadstest.Graph(fx.store), fx.aCheck.ID, map[string]string{"gc.outcome": "pass"}); err != nil {
			t.Fatalf("close A's scope-check: %v", err)
		}
		meta := map[string]string{
			"gc.kind":         kind,
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.on_fail":      "abort_scope",
			"gc.step_ref":     "demo.loop-" + kind,
		}
		if status == "closed" {
			maps.Copy(meta, failMeta(kind))
		}
		return mustCreateWorkflowBead(t, fx.store, beads.Bead{Title: kind + " control", Type: "task", Status: status, Metadata: meta})
	}
	// Retry and ralph controls close themselves and the dispatcher reconciles
	// the closed control.
	for _, kind := range []string{beadmeta.KindRetry, beadmeta.KindRalph} {
		drivers[kind] = func(t *testing.T, fx claimedScopeFixture) (ControlResult, string) {
			control := newControl(t, fx, kind, "closed")
			result, err := ReconcileClosedScopeMember(beadstest.Graph(fx.store), control.ID)
			if err != nil {
				t.Fatalf("ReconcileClosedScopeMember(%s): %v", kind, err)
			}
			return result, control.ID
		}
	}
	// Fanout and drain controls settle their scope before they close.
	for _, kind := range []string{beadmeta.KindFanout, beadmeta.KindDrain} {
		drivers[kind] = func(t *testing.T, fx claimedScopeFixture) (ControlResult, string) {
			control := newControl(t, fx, kind, "")
			result, err := closeScopedControl(beadstest.Graph(fx.store), control.ID, failMeta(kind), kind+"-failed", ProcessOptions{})
			if err != nil {
				t.Fatalf("closeScopedControl(%s): %v", kind, err)
			}
			return result, control.ID
		}
	}
	for name, drive := range drivers {
		for _, tc := range claimedScopeAllStores() {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				store := tc.open(t)
				fx := newClaimedScopeFixture(t, store, claimedScopeOptions{})
				unclaim(t, store, fx.b.ID)

				result, culpritID := drive(t, fx)
				if !result.Processed || result.Skipped != 4 {
					t.Fatalf("result = %+v, want 4 members skipped (B, C and their scope-checks)", result)
				}
				for label, id := range map[string]string{"B": fx.b.ID, "B's scope-check": fx.bCheck.ID, "C": fx.c.ID, "C's scope-check": fx.cCheck.ID} {
					assertStatusOutcome(t, store, label, id, "closed", "skipped")
				}
				body := assertStatusOutcome(t, store, "body", fx.body.ID, "closed", "fail")
				if got := body.Metadata["gc.failure_subject"]; got != culpritID {
					t.Fatalf("body gc.failure_subject = %q, want %s", got, culpritID)
				}
			})
		}
	}
}
