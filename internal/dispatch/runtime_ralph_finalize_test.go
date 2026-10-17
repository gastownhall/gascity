package dispatch

// These tests pin the finalize scan's verdict for a ralph loop that recovered:
// a body member of an earlier iteration that closed failed must not fail the
// workflow when the loop's own control closed pass, and must keep failing it in
// every other case. They build the workflow's beads by hand in a MemStore and
// call terminalAbortScopeFailureMember, resolveFinalizeOutcome and
// resolveFinalizeFailureDiagnostics directly.
//
// The retry-attempt and one-bead predicate tests in runtime_test.go
// (TestBeadOutcomeFailedRetryAttemptExemptionAndOptInTrim,
// TestBeadOutcomeFailedConvergesAcrossAbortScopeDecisionSites,
// TestTerminalAbortScopeFailureSupersededHardAttemptIsNotTerminal) cover the
// existing retry-attempt exemption and are not repeated here.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// ralphScan is a hand-built workflow root plus its finalizer, the inputs of
// the finalize scan terminalAbortScopeFailureMember.
type ralphScan struct {
	t         *testing.T
	store     *beads.MemStore
	root      beads.Bead
	finalizer beads.Bead
}

func newRalphScan(t *testing.T) *ralphScan {
	t.Helper()
	store := beads.NewMemStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": root.ID,
		},
	})
	return &ralphScan{t: t, store: store, root: root, finalizer: finalizer}
}

// add creates one member of the root; creation order is call order.
func (s *ralphScan) add(closed bool, meta map[string]string) beads.Bead {
	s.t.Helper()
	m := map[string]string{"gc.root_bead_id": s.root.ID}
	for k, v := range meta {
		m[k] = v
	}
	bead := beads.Bead{Title: m["gc.step_ref"], Type: "task", Metadata: m}
	if closed {
		bead.Status = "closed"
	}
	return mustCreateWorkflowBead(s.t, s.store, bead)
}

func (s *ralphScan) terminal() (beads.Bead, bool) {
	s.t.Helper()
	got, found, err := terminalAbortScopeFailureMember(s.store, s.root.ID, s.finalizer.ID, ProcessOptions{})
	if err != nil {
		s.t.Fatalf("terminalAbortScopeFailureMember: %v", err)
	}
	return got, found
}

// ralphMetaEdit changes a base metadata map: set overrides keys, del removes them. A
// value may name a bead created earlier in the same fixture as "$B" / "$C".
type ralphMetaEdit struct {
	set map[string]string
	del []string
}

func (e ralphMetaEdit) apply(base map[string]string, subst *strings.Replacer) map[string]string {
	out := make(map[string]string, len(base)+len(e.set))
	for k, v := range base {
		out[k] = v
	}
	for _, k := range e.del {
		delete(out, k)
	}
	for k, v := range e.set {
		out[k] = subst.Replace(v)
	}
	return out
}

// ralphControlMeta is a ralph control; outcome "" leaves gc.outcome unset.
func ralphControlMeta(stepRef, stepID, outcome string) map[string]string {
	meta := map[string]string{"gc.kind": "ralph", "gc.step_ref": stepRef, "gc.step_id": stepID}
	if outcome != "" {
		meta["gc.outcome"] = outcome
	}
	return meta
}

// ralphScopeBodyMeta is an iteration-1 scope body as the compile stamps it
// (formula/ralph.go:142-159): gc.ralph_step_id, gc.attempt=1, gc.control_for =
// the loop step id, no logical id and no gc.on_fail. An iteration-2+ body
// (dispatch/control.go:978-1005) differs in gc.attempt and carries both
// gc.control_for and gc.logical_bead_id = the control's bead id; a test needing
// one edits this base (see the iteration 2+ row of the key-chain test).
func ralphScopeBodyMeta(stepRef, scopeRef, loopID string) map[string]string {
	meta := map[string]string{
		"gc.kind":          "scope",
		"gc.scope_role":    "body",
		"gc.step_ref":      stepRef,
		"gc.outcome":       "fail",
		"gc.ralph_step_id": loopID,
		"gc.attempt":       "1",
		"gc.control_for":   loopID,
	}
	if scopeRef != "" {
		meta["gc.scope_ref"] = scopeRef
	}
	return meta
}

// ralphSyntheticLogicalID sets gc.logical_bead_id on an iteration-1 body. No
// real compile writes it there (formula/ralph.go:142-159); the nested-list and
// duplicate-candidate rows state it so that a resolver following a body's
// logical id is the thing under test.
func ralphSyntheticLogicalID(meta map[string]string, id string) map[string]string {
	meta["gc.logical_bead_id"] = id
	return meta
}

// ralphBodyMemberMeta is the baseline member: a failed ralph body member of
// iteration 1 with no failure class and no logical id.
func ralphBodyMemberMeta() map[string]string {
	return map[string]string{
		"gc.on_fail":       "abort_scope",
		"gc.outcome":       "fail",
		"gc.ralph_step_id": "loop",
		"gc.attempt":       "1",
		"gc.scope_ref":     "loop.iteration.1",
		"gc.step_ref":      "wf.loop.iteration.1.m1",
	}
}

// ralphControlSpec shapes the baseline ralph control.
type ralphControlSpec struct {
	absent  bool   // the control is not created under the root
	closed  bool   // false: the control stays open
	outcome string // "" leaves gc.outcome unset
}

var ralphPassedControl = ralphControlSpec{closed: true, outcome: beadmeta.OutcomePass}

// ralphBaselineSpec is the baseline fixture: a ralph control, its iteration-1
// scope body, then a failed body member, in creation order. Each part can be
// edited; the zero edit is the baseline itself.
type ralphBaselineSpec struct {
	control ralphControlSpec
	body    ralphMetaEdit
	member  ralphMetaEdit
}

// baseline builds the baseline fixture on s and returns the control (zero when
// absent) and the member.
func (s *ralphScan) baseline(spec ralphBaselineSpec) (c, m beads.Bead) {
	s.t.Helper()
	if !spec.control.absent {
		c = s.add(spec.control.closed, ralphControlMeta("wf.loop", "loop", spec.control.outcome))
	}
	// A logical id naming an absent control still has to be non-empty.
	cID := c.ID
	if spec.control.absent {
		cID = "ga-absent-control"
	}
	subst := strings.NewReplacer("$C", cID)
	b := s.add(true, spec.body.apply(
		ralphScopeBodyMeta("wf.loop.iteration.1", "", "loop"), subst))
	subst = strings.NewReplacer("$C", cID, "$B", b.ID)
	m = s.add(true, spec.member.apply(ralphBodyMemberMeta(), subst))
	return c, m
}

func ralphAssertTerminal(t *testing.T, s *ralphScan, want beads.Bead, wantFound bool) {
	t.Helper()
	got, found := s.terminal()
	if found != wantFound {
		t.Fatalf("terminalAbortScopeFailureMember found = %t (bead %q), want %t", found, got.Metadata["gc.step_ref"], wantFound)
	}
	if wantFound && got.ID != want.ID {
		t.Fatalf("terminalAbortScopeFailureMember bead = %q (%s), want %q (%s)",
			got.Metadata["gc.step_ref"], got.ID, want.Metadata["gc.step_ref"], want.ID)
	}
}

// A superseded ralph body member whose owning control passed is not a
// terminal failure, whatever else the member carries.
func TestTerminalAbortScopeFailureMemberRalphOwnerPassExempts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		member ralphMetaEdit
	}{
		{"hard class", ralphMetaEdit{set: map[string]string{"gc.failure_class": "hard"}}},
		{"empty class", ralphMetaEdit{set: map[string]string{"gc.failure_class": ""}}},
		{"task kind", ralphMetaEdit{set: map[string]string{"gc.kind": "task"}}},
		{"nested retry control", ralphMetaEdit{set: map[string]string{
			"gc.kind":     "retry",
			"gc.step_ref": "wf.loop.iteration.1.r1",
		}}},
		{"nested ralph control", ralphMetaEdit{set: map[string]string{
			"gc.kind":     "ralph",
			"gc.step_ref": "wf.loop.iteration.1.n1",
		}}},
		// Already exempt through isRetryAttemptSubject.
		{"with logical id", ralphMetaEdit{set: map[string]string{"gc.logical_bead_id": "$B"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newRalphScan(t)
			s.baseline(ralphBaselineSpec{control: ralphPassedControl, member: tc.member})
			ralphAssertTerminal(t, s, beads.Bead{}, false)
		})
	}
}

// The iteration-1, iteration-2+ and nested-list key chains each resolve
// the owning control, which passed, so the member is not terminal.
func TestTerminalAbortScopeFailureMemberRalphKeyChains(t *testing.T) {
	t.Parallel()

	t.Run("iteration 1 short scope_ref", func(t *testing.T) {
		t.Parallel()

		s := newRalphScan(t)
		s.baseline(ralphBaselineSpec{control: ralphPassedControl})
		ralphAssertTerminal(t, s, beads.Bead{}, false)
	})

	t.Run("iteration 2+ full scope_ref", func(t *testing.T) {
		t.Parallel()

		s := newRalphScan(t)
		s.baseline(ralphBaselineSpec{
			control: ralphPassedControl,
			body: ralphMetaEdit{set: map[string]string{
				"gc.step_ref":        "wf.loop.iteration.2",
				"gc.attempt":         "2",
				"gc.control_for":     "$C",
				"gc.logical_bead_id": "$C",
			}},
			member: ralphMetaEdit{set: map[string]string{
				"gc.attempt":   "2",
				"gc.scope_ref": "wf.loop.iteration.2",
				"gc.step_ref":  "wf.loop.iteration.2.m1",
			}},
		})
		ralphAssertTerminal(t, s, beads.Bead{}, false)
	})

	// Hand-built: a compiled nested ralph does not run today. The inner loop's
	// compile walk overwrites the scope_ref of its own scope body with the outer
	// iteration id (formula/ralph.go:259-263), and the inner body is created
	// before the outer one, so a first-match scope lookup would resolve the
	// outer member to the failed inner control.
	t.Run("nested list outer member outer control passed", func(t *testing.T) {
		t.Parallel()

		s := newRalphScan(t)
		co := s.add(true, ralphControlMeta("wf.outer", "outer", beadmeta.OutcomePass))
		ci := s.add(true, ralphControlMeta("wf.outer.iteration.1.inner", "inner", beadmeta.OutcomeFail))
		s.add(true, ralphSyntheticLogicalID(ralphScopeBodyMeta("wf.outer.iteration.1.inner.iteration.1", "outer.iteration.1", "inner"), ci.ID))
		s.add(true, ralphSyntheticLogicalID(ralphScopeBodyMeta("wf.outer.iteration.1", "", "outer"), co.ID))
		s.add(true, map[string]string{
			"gc.on_fail":       "abort_scope",
			"gc.outcome":       "fail",
			"gc.step_ref":      "wf.outer.iteration.1.o1",
			"gc.ralph_step_id": "outer",
			"gc.attempt":       "1",
			"gc.scope_ref":     "outer.iteration.1",
		})
		ralphAssertTerminal(t, s, beads.Bead{}, false)
	})
}

// A control that does not enclose the member, or two candidate controls, keep
// the member terminal: the owner must be unambiguous. Each row replaces the
// baseline member, control and body entirely. Every row is hand-built: a
// compiled nested ralph does not run today, and no compile produces two
// controls with one step ref or two bodies matching one scope_ref.
func TestTerminalAbortScopeFailureMemberRalphNotEnclosedFailsClosed(t *testing.T) {
	t.Parallel()

	outerMember := func(stepRef, ralphStepID, scopeRef string) map[string]string {
		meta := map[string]string{
			"gc.on_fail":       "abort_scope",
			"gc.outcome":       "fail",
			"gc.step_ref":      stepRef,
			"gc.ralph_step_id": ralphStepID,
			"gc.attempt":       "1",
		}
		if scopeRef != "" {
			meta["gc.scope_ref"] = scopeRef
		}
		return meta
	}

	cases := []struct {
		name  string
		build func(s *ralphScan) beads.Bead // returns M
	}{
		{"non-enclosing control passed", func(s *ralphScan) beads.Bead {
			co := s.add(true, ralphControlMeta("wf.outer", "outer", beadmeta.OutcomeFail))
			ci := s.add(true, ralphControlMeta("wf.outer.iteration.1.inner", "inner", beadmeta.OutcomePass))
			s.add(true, ralphSyntheticLogicalID(ralphScopeBodyMeta("wf.outer.iteration.1.inner.iteration.1", "outer.iteration.1", "inner"), ci.ID))
			s.add(true, ralphSyntheticLogicalID(ralphScopeBodyMeta("wf.outer.iteration.1", "", "outer"), co.ID))
			return s.add(true, outerMember("wf.outer.iteration.1.o1", "outer", "outer.iteration.1"))
		}},
		{"inner control fail outer pass", func(s *ralphScan) beads.Bead {
			s.add(true, ralphControlMeta("wf.outer.iteration.1.inner", "inner", beadmeta.OutcomeFail))
			s.add(true, ralphControlMeta("wf.outer", "outer", beadmeta.OutcomePass))
			return s.add(true, outerMember("wf.outer.iteration.1.inner.iteration.1.i1", "outer", "outer.iteration.1"))
		}},
		{"inner step id inner control not closed", func(s *ralphScan) beads.Bead {
			s.add(false, ralphControlMeta("wf.outer.iteration.1.inner", "inner", ""))
			s.add(true, ralphControlMeta("wf.outer", "outer", beadmeta.OutcomePass))
			return s.add(true, outerMember("wf.outer.iteration.1.inner.iteration.1.i1", "inner", "inner.iteration.1"))
		}},
		// A control whose ref only ends in the loop ref, without a dot, is not
		// the owner.
		{"dotted boundary", func(s *ralphScan) beads.Bead {
			s.add(true, ralphControlMeta("wf.myloop", "myloop", beadmeta.OutcomePass))
			return s.add(true, outerMember("loop.iteration.1.m1", "loop", "loop.iteration.1"))
		}},
		// Only a gc.kind=ralph bead can own: a passed task bead carrying the
		// loop ref is not an owner.
		{"non-ralph bead with the loop ref", func(s *ralphScan) beads.Bead {
			s.add(true, map[string]string{
				"gc.kind":     "task",
				"gc.step_ref": "wf.loop",
				"gc.outcome":  beadmeta.OutcomePass,
			})
			return s.add(true, ralphBodyMemberMeta())
		}},
		// A loop id that is a dotted suffix of another loop's id leaves the
		// owner of an iteration-1 member (short step_ref) ambiguous.
		{"loop id is a dotted suffix of another loop's id", func(s *ralphScan) beads.Bead {
			s.add(true, ralphControlMeta("wf.loop", "loop", beadmeta.OutcomePass))
			s.add(true, ralphControlMeta("wf.review.loop", "loop", beadmeta.OutcomePass))
			return s.add(true, outerMember("loop.iteration.1.m1", "loop", "loop.iteration.1"))
		}},
		{"two candidates", func(s *ralphScan) beads.Bead {
			s.add(true, ralphControlMeta("wf.loop", "loop", beadmeta.OutcomePass))
			s.add(true, ralphControlMeta("wf.loop", "loop", beadmeta.OutcomePass))
			return s.add(true, ralphBodyMemberMeta())
		}},
		// M has no gc.step_ref, so only the ambiguous scope lookup is left.
		{"duplicate suffix", func(s *ralphScan) beads.Bead {
			c1 := s.add(true, ralphControlMeta("wf.loop", "loop", beadmeta.OutcomePass))
			c2 := s.add(true, ralphControlMeta("wf.grp.loop", "loop", beadmeta.OutcomePass))
			s.add(true, ralphSyntheticLogicalID(ralphScopeBodyMeta("wf.loop.iteration.1", "", "loop"), c1.ID))
			s.add(true, ralphSyntheticLogicalID(ralphScopeBodyMeta("wf.grp.loop.iteration.1", "", "loop"), c2.ID))
			meta := ralphBodyMemberMeta()
			delete(meta, "gc.step_ref")
			return s.add(true, meta)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newRalphScan(t)
			m := tc.build(s)
			ralphAssertTerminal(t, s, m, true)
		})
	}
}

// Only a closed gc.outcome=pass control exempts; every other owner
// state keeps the member terminal.
func TestTerminalAbortScopeFailureMemberRalphOwnerNotPassKeepsTerminal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		control ralphControlSpec
	}{
		{"control failed", ralphControlSpec{closed: true, outcome: beadmeta.OutcomeFail}},
		{"control still open", ralphControlSpec{closed: false}},
		{"control not resolvable", ralphControlSpec{absent: true}},
		{"control skipped", ralphControlSpec{closed: true, outcome: beadmeta.OutcomeSkipped}},
		{"control canceled", ralphControlSpec{closed: true, outcome: beadmeta.OutcomeCanceled}},
		{"outcome absent", ralphControlSpec{closed: true}},
		{"outcome unknown", ralphControlSpec{closed: true, outcome: "weird"}},
		{"control open but carrying outcome pass", ralphControlSpec{closed: false, outcome: beadmeta.OutcomePass}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newRalphScan(t)
			_, m := s.baseline(ralphBaselineSpec{control: tc.control})
			ralphAssertTerminal(t, s, m, true)
		})
	}
}

// Beads outside the exempt set keep today's verdict even with a passing
// control in the root.
func TestTerminalAbortScopeFailureMemberUnchangedRows(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		member   ralphMetaEdit
		closed   bool // false: M stays open
		terminal bool
	}{
		{"non-ralph member", ralphMetaEdit{del: []string{"gc.ralph_step_id", "gc.attempt"}}, true, true},
		{"key missing: no attempt", ralphMetaEdit{del: []string{"gc.attempt"}}, true, true},
		{"key missing: no ralph_step_id", ralphMetaEdit{del: []string{"gc.ralph_step_id"}}, true, true},
		{"ralph control itself", ralphMetaEdit{
			set: map[string]string{"gc.kind": "ralph"},
			del: []string{"gc.attempt"},
		}, true, true},
		{"transient", ralphMetaEdit{set: map[string]string{"gc.failure_class": "transient"}}, true, false},
		{"other on_fail", ralphMetaEdit{set: map[string]string{"gc.on_fail": "continue"}}, true, false},
		{"no on_fail", ralphMetaEdit{del: []string{"gc.on_fail"}}, true, false},
		{"not closed", ralphMetaEdit{}, false, false},
		{"passed", ralphMetaEdit{set: map[string]string{"gc.outcome": beadmeta.OutcomePass}}, true, false},
		{"skipped", ralphMetaEdit{set: map[string]string{"gc.outcome": beadmeta.OutcomeSkipped}}, true, false},
		{"canceled", ralphMetaEdit{set: map[string]string{"gc.outcome": beadmeta.OutcomeCanceled}}, true, false},
	}
	run := func(name string, member ralphMetaEdit, closed bool, owner ralphControlSpec, terminal bool) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := newRalphScan(t)
			s.add(owner.closed, ralphControlMeta("wf.loop", "loop", owner.outcome))
			s.add(true, ralphScopeBodyMeta("wf.loop.iteration.1", "", "loop"))
			m := s.add(closed, member.apply(ralphBodyMemberMeta(), strings.NewReplacer()))
			ralphAssertTerminal(t, s, m, terminal)
		})
	}
	for _, tc := range cases {
		run(tc.name, tc.member, tc.closed, ralphPassedControl, tc.terminal)
	}

	// With a passing owner the ralph-owner exemption would hide a dropped
	// clause above, so each of these clauses is pinned again beside an owner
	// that is NOT closed pass: the member must stay out of the terminal set on
	// its own account.
	failedOwner := ralphControlSpec{closed: true, outcome: beadmeta.OutcomeFail}
	run("not closed, owner failed", ralphMetaEdit{}, false, failedOwner, false)
	run("passed, owner failed", ralphMetaEdit{set: map[string]string{"gc.outcome": beadmeta.OutcomePass}}, true, failedOwner, false)
	run("transient, owner failed", ralphMetaEdit{set: map[string]string{"gc.failure_class": "transient"}}, true, failedOwner, false)
}

// An attempt or iteration root carrying a logical id keeps its existing
// exemption (isRetryAttemptSubject) whatever the owning control did.
func TestTerminalAbortScopeFailureMemberAttemptRootKeepsExemption(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		control ralphControlSpec
	}{
		{"control failed", ralphControlSpec{closed: true, outcome: beadmeta.OutcomeFail}},
		{"control open", ralphControlSpec{closed: false}},
		{"control absent", ralphControlSpec{absent: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newRalphScan(t)
			s.baseline(ralphBaselineSpec{
				control: tc.control,
				member: ralphMetaEdit{set: map[string]string{
					"gc.kind":            "scope",
					"gc.attempt":         "2",
					"gc.logical_bead_id": "$C",
				}},
			})
			ralphAssertTerminal(t, s, beads.Bead{}, false)
		})
	}
}

// The per-bead failure predicates return today's values for a ralph
// body member, with and without a logical id.
func TestRalphBodyMemberAbortPredicatesUnchanged(t *testing.T) {
	t.Parallel()

	member := beads.Bead{ID: "ga-member-1", Status: "closed", Metadata: ralphBodyMemberMeta()}
	if !beadOutcomeFailed(member) {
		t.Errorf("beadOutcomeFailed(member without logical id) = false, want true")
	}
	if isRetryAttemptSubject(member) {
		t.Errorf("isRetryAttemptSubject(member without logical id) = true, want false")
	}

	withLogical := beads.Bead{ID: "ga-member-2", Status: "closed", Metadata: ralphBodyMemberMeta()}
	withLogical.Metadata["gc.logical_bead_id"] = "ga-control-1"
	if beadOutcomeFailed(withLogical) {
		t.Errorf("beadOutcomeFailed(member with logical id) = true, want false")
	}
	if !isRetryAttemptSubject(withLogical) {
		t.Errorf("isRetryAttemptSubject(member with logical id) = false, want true")
	}
}

// The verdict is repeatable and writes nothing.
func TestTerminalAbortScopeFailureMemberRalphIdempotentNoWrites(t *testing.T) {
	t.Parallel()

	s := newRalphScan(t)
	c, m := s.baseline(ralphBaselineSpec{control: ralphPassedControl})
	snapshot := func() []beads.Bead {
		all, err := s.store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
		if err != nil {
			t.Fatalf("list beads: %v", err)
		}
		return all
	}
	before := snapshot()

	_, first := s.terminal()
	_, second := s.terminal()
	if first != second {
		t.Fatalf("found changed between two calls: %t then %t", first, second)
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("terminalAbortScopeFailureMember wrote to the store:\nbefore=%+v\nafter=%+v", before, after)
	}
	if got := mustGetBead(t, s.store, m.ID).Metadata["gc.outcome"]; got != beadmeta.OutcomeFail {
		t.Errorf("M gc.outcome = %q, want %q", got, beadmeta.OutcomeFail)
	}
	if got := mustGetBead(t, s.store, c.ID).Metadata["gc.outcome"]; got != beadmeta.OutcomePass {
		t.Errorf("C gc.outcome = %q, want %q", got, beadmeta.OutcomePass)
	}
}

// With an exempted ralph member M and an unrelated failed non-ralph member X,
// the finalize outcome is fail, the culprit is X (never M), and the verdict and
// the culprit agree, whichever of M or X was created first. In row A, M comes
// first, so a scan that stops at the first failed member would name M.
func TestFinalizeCulpritSkipsExemptRalphMemberToNonRalphFailure(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name          string
		xBeforeMOrder bool
	}{
		{"A M created before X", false},
		{"B X created before M", true},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newRalphScan(t)
			c := s.add(true, ralphControlMeta("wf.loop", "loop", beadmeta.OutcomePass))
			s.add(true, ralphScopeBodyMeta("wf.loop.iteration.1", "", "loop"))
			addM := func() beads.Bead { return s.add(true, ralphBodyMemberMeta()) }
			addX := func() beads.Bead {
				return s.add(true, map[string]string{
					"gc.on_fail":  "abort_scope",
					"gc.outcome":  "fail",
					"gc.step_ref": "wf.verify",
				})
			}
			var m, x beads.Bead
			if tc.xBeforeMOrder {
				x = addX()
				m = addM()
			} else {
				m = addM()
				x = addX()
			}
			// Every direct blocker passed, so the outcome comes from the scan.
			mustDepAdd(t, s.store, s.finalizer.ID, c.ID, "blocks")

			members, err := rootViewMembers(s.store, s.root.ID, ProcessOptions{})
			if err != nil {
				t.Fatalf("rootViewMembers: %v", err)
			}
			var order []string
			for _, member := range members {
				order = append(order, member.Metadata["gc.step_ref"])
			}
			t.Logf("rootViewMembers order: %q (M=%s X=%s)", order, m.ID, x.ID)

			outcome, err := resolveFinalizeOutcome(s.store, s.finalizer, ProcessOptions{})
			if err != nil {
				t.Fatalf("resolveFinalizeOutcome: %v", err)
			}
			if outcome != beadmeta.OutcomeFail {
				t.Errorf("finalize outcome = %q, want %q", outcome, beadmeta.OutcomeFail)
			}
			diag := resolveFinalizeFailureDiagnostics(s.store, s.finalizer, ProcessOptions{})
			if got := diag[beadmeta.FailureSubjectMetadataKey]; got != x.ID {
				t.Errorf("failure culprit = %q, want X %q (M is %q)", got, x.ID, m.ID)
			}
			if got, found := s.terminal(); !found || got.ID != x.ID {
				t.Errorf("terminalAbortScopeFailureMember = (%q, %t), want X %q", got.ID, found, x.ID)
			}
		})
	}
}
