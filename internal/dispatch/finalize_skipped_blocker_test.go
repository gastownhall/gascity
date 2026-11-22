package dispatch

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// skippedBlockerFixture is a graph.v2 workflow root, its finalizer, and the
// finalizer's direct blocks blockers, all in one MemStore.
type skippedBlockerFixture struct {
	store     beads.Store
	root      beads.Bead
	finalizer beads.Bead
	blockers  []beads.Bead
}

// newSkippedBlockerFixture creates a workflow root and one closed step bead per
// entry of blockerMeta (each entry merged over the step's root membership),
// then a finalizer blocked by every step. Blockers are created in slice order
// and their blocks edges added in edgeOrder (indexes into blockerMeta; nil
// means slice order).
func newSkippedBlockerFixture(t *testing.T, store beads.Store, blockerMeta []map[string]string, edgeOrder []int) skippedBlockerFixture {
	t.Helper()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	f := skippedBlockerFixture{store: store, root: root}
	for _, extra := range blockerMeta {
		meta := map[string]string{"gc.root_bead_id": root.ID}
		maps.Copy(meta, extra)
		f.blockers = append(f.blockers, mustCreateWorkflowBead(t, store, beads.Bead{
			Title:    "step",
			Type:     "task",
			Status:   "closed",
			Metadata: meta,
		}))
	}
	f.finalizer = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": root.ID,
		},
	})
	if edgeOrder == nil {
		for i := range f.blockers {
			edgeOrder = append(edgeOrder, i)
		}
	}
	for _, i := range edgeOrder {
		mustDepAdd(t, store, f.finalizer.ID, f.blockers[i].ID, "blocks")
	}
	mustDepAdd(t, store, root.ID, f.finalizer.ID, "blocks")
	return f
}

// finalize processes the fixture's finalizer and returns the control result
// and the root as stored afterwards.
func (f skippedBlockerFixture) finalize(t *testing.T) (ControlResult, beads.Bead) {
	t.Helper()
	result, err := ProcessControl(beadstest.Graph(f.store), f.finalizer, ProcessOptions{Work: beadstest.Work(f.store)})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	return result, mustGetBead(t, f.store, f.root.ID)
}

// closeRootPassed stands in for a first finalize attempt that closed the root
// pass with gc.skipped_blockers=marker and then failed before closing the
// finalizer, which stays open for the retry.
func (f skippedBlockerFixture) closeRootPassed(t *testing.T, marker string) {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.root.ID, map[string]string{
		"gc.outcome":          "pass",
		"gc.skipped_blockers": marker,
	}); err != nil {
		t.Fatalf("stamp root: %v", err)
	}
	if err := f.store.Close(f.root.ID); err != nil {
		t.Fatalf("close root: %v", err)
	}
}

func outcomeMeta(outcome string) map[string]string {
	return map[string]string{"gc.outcome": outcome}
}

// TestProcessWorkflowFinalizeRecordsSkippedBlockerOnPass pins #6096: a
// workflow whose finalizer blocker closed gc.outcome=skipped still passes,
// and the root's pass close records that blocker in gc.skipped_blockers.
func TestProcessWorkflowFinalizeRecordsSkippedBlockerOnPass(t *testing.T) {
	t.Parallel()

	f := newSkippedBlockerFixture(t, beads.NewMemStore(), []map[string]string{
		outcomeMeta("pass"),
		outcomeMeta("skipped"),
	}, nil)
	result, root := f.finalize(t)

	if result.Action != "workflow-pass" {
		t.Fatalf("action = %q, want workflow-pass", result.Action)
	}
	if root.Status != "closed" || root.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("root status=%q gc.outcome=%q, want closed/pass", root.Status, root.Metadata["gc.outcome"])
	}
	if got, want := root.Metadata["gc.skipped_blockers"], f.blockers[1].ID; got != want {
		t.Fatalf("root gc.skipped_blockers = %q, want %q", got, want)
	}
}

// TestProcessWorkflowFinalizeSkippedNativeStepIsDistinguishableFromPass is the
// triage repro for #6096: a native step dependency force-closed via the skip
// directive must not finalize the root in a state identical to one whose step
// actually completed.
func TestProcessWorkflowFinalizeSkippedNativeStepIsDistinguishableFromPass(t *testing.T) {
	t.Parallel()

	step := func(meta map[string]string) map[string]string {
		meta["gc.step_ref"] = "build-from-plan.publish"
		return meta
	}
	_, passed := newSkippedBlockerFixture(t, beads.NewMemStore(), []map[string]string{
		step(outcomeMeta("pass")),
	}, nil).finalize(t)
	_, skipped := newSkippedBlockerFixture(t, beads.NewMemStore(), []map[string]string{
		step(map[string]string{
			"gc.outcome":   "skipped",
			"close_reason": sourceworkflow.WorkflowSkippedCloseReason,
		}),
	}, nil).finalize(t)

	if maps.Equal(passed.Metadata, skipped.Metadata) {
		t.Fatalf("root after a skip-directive-closed native step is indistinguishable from a real pass: both closed with metadata %v", skipped.Metadata)
	}
}

// TestProcessWorkflowFinalizeRecordsSkippedBlockersSorted pins the encoding of
// gc.skipped_blockers: every skipped blocker's ID, sorted ascending and
// comma-joined, whatever order the blocks edges were added in, and no passed
// blocker's ID.
func TestProcessWorkflowFinalizeRecordsSkippedBlockersSorted(t *testing.T) {
	t.Parallel()

	f := newSkippedBlockerFixture(t, beads.NewMemStore(), []map[string]string{
		outcomeMeta("skipped"),
		outcomeMeta("pass"),
		outcomeMeta("skipped"),
		outcomeMeta("skipped"),
	}, []int{3, 1, 0, 2})
	result, root := f.finalize(t)

	if result.Action != "workflow-pass" {
		t.Fatalf("action = %q, want workflow-pass", result.Action)
	}
	want := []string{f.blockers[0].ID, f.blockers[2].ID, f.blockers[3].ID}
	slices.Sort(want)
	if got := root.Metadata["gc.skipped_blockers"]; got != strings.Join(want, ",") {
		t.Fatalf("root gc.skipped_blockers = %q, want %q", got, strings.Join(want, ","))
	}
	if strings.Contains(root.Metadata["gc.skipped_blockers"], f.blockers[1].ID) {
		t.Fatalf("root gc.skipped_blockers = %q contains passed blocker %s", root.Metadata["gc.skipped_blockers"], f.blockers[1].ID)
	}
}

// TestProcessWorkflowFinalizeAllPassLeavesSkippedBlockersAbsent pins that only
// the finalizer's direct blockers feed gc.skipped_blockers: when every blocker
// passed, the root closes pass without the key, even though the finalize
// sweep then closes a never-reached non-blocker member gc.outcome=skipped.
func TestProcessWorkflowFinalizeAllPassLeavesSkippedBlockersAbsent(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	f := newSkippedBlockerFixture(t, store, []map[string]string{
		outcomeMeta("pass"),
		outcomeMeta("pass"),
	}, nil)
	unreached := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "never-reached member",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": f.root.ID,
			"gc.step_ref":     "unused",
		},
	})
	result, root := f.finalize(t)

	if result.Action != "workflow-pass" {
		t.Fatalf("action = %q, want workflow-pass", result.Action)
	}
	if root.Status != "closed" || root.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("root status=%q gc.outcome=%q, want closed/pass", root.Status, root.Metadata["gc.outcome"])
	}
	if got, ok := root.Metadata["gc.skipped_blockers"]; ok {
		t.Fatalf("root gc.skipped_blockers = %q, want the key absent", got)
	}
	member := mustGetBead(t, store, unreached.ID)
	if member.Status != "closed" || member.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("never-reached member status=%q gc.outcome=%q, want the sweep's closed/skipped", member.Status, member.Metadata["gc.outcome"])
	}
}

// TestProcessWorkflowFinalizeFailOmitsSkippedBlockers pins that only a pass
// close writes gc.skipped_blockers: a failed, canceled or scope-aborted
// workflow with a skipped blocker closes its root with gc.outcome alone.
func TestProcessWorkflowFinalizeFailOmitsSkippedBlockers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		rootMeta    map[string]string
		blockerMeta []map[string]string
		// members adds non-blocker workflow members once the fixture exists.
		members     func(t *testing.T, f skippedBlockerFixture)
		wantAction  string
		wantOutcome string
	}{{
		name:        "failed blocker",
		blockerMeta: []map[string]string{outcomeMeta("fail"), outcomeMeta("skipped")},
		wantAction:  "workflow-fail",
		wantOutcome: "fail",
	}, {
		name:        "canceled run",
		rootMeta:    map[string]string{"gc.cancel_requested": "2026-11-22T00:00:00Z"},
		blockerMeta: []map[string]string{outcomeMeta("pass"), outcomeMeta("skipped")},
		wantAction:  "workflow-canceled",
		wantOutcome: "canceled",
	}, {
		name: "scope-abort-skipped sink in a failed abort_scope scope",
		blockerMeta: []map[string]string{{
			"gc.outcome":    "skipped",
			"gc.scope_ref":  "body",
			"gc.scope_role": "member",
		}},
		members: func(t *testing.T, f skippedBlockerFixture) {
			t.Helper()
			mustCreateWorkflowBead(t, f.store, beads.Bead{
				Title:  "body",
				Type:   "task",
				Status: "closed",
				Metadata: map[string]string{
					"gc.kind":         "scope",
					"gc.scope_role":   "body",
					"gc.root_bead_id": f.root.ID,
					"gc.step_ref":     "demo.body",
					"gc.outcome":      "fail",
				},
			})
			mustCreateWorkflowBead(t, f.store, beads.Bead{
				Title:  "preflight",
				Type:   "task",
				Status: "closed",
				Metadata: map[string]string{
					"gc.root_bead_id":  f.root.ID,
					"gc.scope_ref":     "body",
					"gc.scope_role":    "member",
					"gc.outcome":       "fail",
					"gc.failure_class": "hard",
					"gc.on_fail":       "abort_scope",
				},
			})
		},
		wantAction:  "workflow-fail",
		wantOutcome: "fail",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := beads.NewMemStore()
			f := newSkippedBlockerFixture(t, store, tc.blockerMeta, nil)
			for k, v := range tc.rootMeta {
				if err := store.SetMetadata(f.root.ID, k, v); err != nil {
					t.Fatalf("set root %s: %v", k, err)
				}
			}
			if tc.members != nil {
				tc.members(t, f)
			}
			result, root := f.finalize(t)

			if result.Action != tc.wantAction {
				t.Fatalf("action = %q, want %q", result.Action, tc.wantAction)
			}
			if root.Status != "closed" || root.Metadata["gc.outcome"] != tc.wantOutcome {
				t.Fatalf("root status=%q gc.outcome=%q, want closed/%s", root.Status, root.Metadata["gc.outcome"], tc.wantOutcome)
			}
			if got, ok := root.Metadata["gc.skipped_blockers"]; ok {
				t.Fatalf("root gc.skipped_blockers = %q, want the key absent on a %s close", got, tc.wantOutcome)
			}
		})
	}
}

// TestCloseWorkflowRootQuarantineRefusalWritesNoSkippedBlockers pins that a
// root quarantined after the outcome resolved refuses the pass close whole:
// the marker rides on that close, so it is not written either.
func TestCloseWorkflowRootQuarantineRefusalWritesNoSkippedBlockers(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	f := newSkippedBlockerFixture(t, store, []map[string]string{outcomeMeta("skipped")}, nil)
	quarantineWorkflowRoot(t, store, f.root.ID)

	err := closeWorkflowRoot(beadstest.Graph(store), f.root, f.root.ID, "pass", []string{f.blockers[0].ID})
	if !errors.Is(err, errRootQuarantined) {
		t.Fatalf("closeWorkflowRoot err = %v, want errRootQuarantined", err)
	}
	root := mustGetBead(t, store, f.root.ID)
	if got, ok := root.Metadata["gc.skipped_blockers"]; ok {
		t.Fatalf("quarantined root gc.skipped_blockers = %q, want the key absent", got)
	}
}

// TestProcessWorkflowFinalizeMissingRootWritesNoSkippedBlockers pins that a
// finalizer whose root is gone settles as missing_root and records its
// skipped blockers nowhere.
func TestProcessWorkflowFinalizeMissingRootWritesNoSkippedBlockers(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	blocker := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "step",
		Type:     "task",
		Status:   "closed",
		Metadata: map[string]string{"gc.root_bead_id": "gc-missing", "gc.outcome": "skipped"},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": "gc-missing",
		},
	})
	mustDepAdd(t, store, finalizer.ID, blocker.ID, "blocks")

	result, err := ProcessControl(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if result.Action != "workflow-missing_root" {
		t.Fatalf("action = %q, want workflow-missing_root", result.Action)
	}
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list beads: %v", err)
	}
	for _, b := range all {
		if got, ok := b.Metadata["gc.skipped_blockers"]; ok {
			t.Fatalf("bead %s gc.skipped_blockers = %q, want the key nowhere", b.ID, got)
		}
	}
}

// TestProcessFanoutSkippedChildCarriesNoSkippedBlockers pins that the shared
// blocker scan writes gc.skipped_blockers only from workflow finalize: a
// spawned fanout control with a skipped child still closes fanout-pass with
// gc.outcome alone.
func TestProcessFanoutSkippedChildCarriesNoSkippedBlockers(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "fanout",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.fanout_state": "spawned",
			"gc.root_bead_id": root.ID,
		},
	})
	for _, outcome := range []string{"pass", "skipped"} {
		child := mustCreateWorkflowBead(t, store, beads.Bead{
			Title:    "fanout child",
			Type:     "task",
			Status:   "closed",
			Metadata: map[string]string{"gc.root_bead_id": root.ID, "gc.outcome": outcome},
		})
		mustDepAdd(t, store, fanout.ID, child.ID, "blocks")
	}

	result, err := ProcessControl(beadstest.Graph(store), fanout, ProcessOptions{Work: beadstest.Work(store)})
	if err != nil {
		t.Fatalf("ProcessControl(fanout): %v", err)
	}
	if result.Action != "fanout-pass" {
		t.Fatalf("action = %q, want fanout-pass", result.Action)
	}
	after := mustGetBead(t, store, fanout.ID)
	if after.Status != "closed" || after.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("fanout status=%q gc.outcome=%q, want closed/pass", after.Status, after.Metadata["gc.outcome"])
	}
	if got, ok := after.Metadata["gc.skipped_blockers"]; ok {
		t.Fatalf("fanout gc.skipped_blockers = %q, want the key absent", got)
	}
}

// TestProcessWorkflowFinalizeSkippedBlockerAddsNoStoreCalls pins that the
// marker costs nothing: the skipped IDs come from the blocker rows the scan
// already read, and the key rides on the root's existing pass close, so a
// finalize with a skipped blocker makes exactly the store calls an all-pass
// one does.
func TestProcessWorkflowFinalizeSkippedBlockerAddsNoStoreCalls(t *testing.T) {
	t.Parallel()

	count := func(secondOutcome string) *callCountingStore {
		t.Helper()
		f := newSkippedBlockerFixture(t, beads.NewMemStore(), []map[string]string{
			outcomeMeta("pass"),
			outcomeMeta(secondOutcome),
		}, nil)
		counting := &callCountingStore{Store: f.store}
		result, err := ProcessControl(beadstest.Graph(counting), f.finalizer, ProcessOptions{Work: beadstest.Work(counting)})
		if err != nil {
			t.Fatalf("ProcessControl(workflow-finalize, %s blocker): %v", secondOutcome, err)
		}
		if result.Action != "workflow-pass" {
			t.Fatalf("action with a %s blocker = %q, want workflow-pass", secondOutcome, result.Action)
		}
		return counting
	}
	allPass := count("pass")
	withSkip := count("skipped")

	if withSkip.reads != allPass.reads || withSkip.writes != allPass.writes {
		t.Fatalf("finalize with a skipped blocker made %d reads/%d writes, all-pass made %d/%d; want equal\nskipped: %v\nall-pass: %v",
			withSkip.reads, withSkip.writes, allPass.reads, allPass.writes, withSkip.ops, allPass.ops)
	}
}

// TestProcessWorkflowFinalizeSkippedBlockerEdges covers the pass-path edges of
// gc.skipped_blockers: a padded outcome is still a skip, a crash retry over a
// root already closed with the marker leaves the same value, and the
// finalizer itself does not close with the skip-directive close_reason.
func TestProcessWorkflowFinalizeSkippedBlockerEdges(t *testing.T) {
	t.Parallel()

	t.Run("padded skipped outcome is recorded", func(t *testing.T) {
		t.Parallel()

		f := newSkippedBlockerFixture(t, beads.NewMemStore(), []map[string]string{
			outcomeMeta("pass"),
			outcomeMeta(" skipped "),
		}, nil)
		_, root := f.finalize(t)
		if got, want := root.Metadata["gc.skipped_blockers"], f.blockers[1].ID; got != want {
			t.Fatalf("root gc.skipped_blockers = %q, want %q", got, want)
		}
	})

	t.Run("crash retry keeps the marker", func(t *testing.T) {
		t.Parallel()

		f := newSkippedBlockerFixture(t, beads.NewMemStore(), []map[string]string{
			outcomeMeta("pass"),
			outcomeMeta("skipped"),
		}, nil)
		f.closeRootPassed(t, f.blockers[1].ID)
		result, root := f.finalize(t)
		if result.Action != "workflow-pass" {
			t.Fatalf("action = %q, want workflow-pass", result.Action)
		}
		if got, want := root.Metadata["gc.skipped_blockers"], f.blockers[1].ID; got != want {
			t.Fatalf("root gc.skipped_blockers after retry = %q, want %q", got, want)
		}
	})

	t.Run("finalizer close_reason is not the skip directive", func(t *testing.T) {
		t.Parallel()

		store := beads.NewMemStore()
		f := newSkippedBlockerFixture(t, store, []map[string]string{
			outcomeMeta("pass"),
			{"gc.outcome": "skipped", "close_reason": sourceworkflow.WorkflowSkippedCloseReason},
		}, nil)
		f.finalize(t)
		finalizer := mustGetBead(t, store, f.finalizer.ID)
		if finalizer.Status != "closed" || finalizer.Metadata["gc.outcome"] != "pass" {
			t.Fatalf("finalizer status=%q gc.outcome=%q, want closed/pass", finalizer.Status, finalizer.Metadata["gc.outcome"])
		}
		for field, got := range map[string]string{
			"metadata close_reason": finalizer.Metadata["close_reason"],
			"CloseReason":           finalizer.CloseReason,
		} {
			if got == sourceworkflow.WorkflowSkippedCloseReason {
				t.Fatalf("finalizer %s = %q, want anything but the skip directive", field, got)
			}
		}
	})
}

// TestProcessWorkflowFinalizeRetryRegradeEmptiesSkippedBlockers pins that
// gc.skipped_blockers never outlives the pass it describes. The root closes
// before its finalizer, so a finalize retry can re-settle a root the first
// attempt closed pass with the marker; when the retry settles it any other way
// than a pass with skips, the same close writes the marker empty. The fail
// row's culprit is an ordinary abort_scope member rather than a teardown step,
// because a teardown's outcome must never re-grade the root
// (formula-spec-v2.md, "Teardown is post-settlement").
func TestProcessWorkflowFinalizeRetryRegradeEmptiesSkippedBlockers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// regrade is what changes between the first attempt's root close and
		// the retry.
		regrade     func(t *testing.T, f skippedBlockerFixture)
		wantAction  string
		wantOutcome string
	}{{
		name: "run above the drain item canceled",
		regrade: func(t *testing.T, f skippedBlockerFixture) {
			t.Helper()
			run := mustCreateWorkflowBead(t, f.store, beads.Bead{
				Title: "run",
				Type:  "task",
				Metadata: map[string]string{
					"gc.kind":             "workflow",
					"gc.formula_contract": "graph.v2",
				},
			})
			drain := mustCreateWorkflowBead(t, f.store, beads.Bead{
				Title:    "drain",
				Type:     "task",
				Metadata: map[string]string{"gc.kind": "drain", "gc.root_bead_id": run.ID},
			})
			if err := f.store.SetMetadata(f.root.ID, "gc.drain_control_id", drain.ID); err != nil {
				t.Fatalf("link the item root to its drain: %v", err)
			}
			// Run cancel skips the closed item root and marks the open run.
			if err := f.store.SetMetadata(run.ID, "gc.cancel_requested", "true"); err != nil {
				t.Fatalf("cancel the run: %v", err)
			}
		},
		wantAction:  "workflow-canceled",
		wantOutcome: "canceled",
	}, {
		name: "abort_scope member failed hard",
		regrade: func(t *testing.T, f skippedBlockerFixture) {
			t.Helper()
			mustCreateWorkflowBead(t, f.store, beads.Bead{
				Title:  "preflight",
				Type:   "task",
				Status: "closed",
				Metadata: map[string]string{
					"gc.root_bead_id":  f.root.ID,
					"gc.scope_ref":     "body",
					"gc.scope_role":    "member",
					"gc.outcome":       "fail",
					"gc.failure_class": "hard",
					"gc.on_fail":       "abort_scope",
				},
			})
		},
		wantAction:  "workflow-fail",
		wantOutcome: "fail",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newSkippedBlockerFixture(t, beads.NewMemStore(), []map[string]string{
				outcomeMeta("pass"),
				outcomeMeta("skipped"),
			}, nil)
			f.closeRootPassed(t, f.blockers[1].ID)
			tc.regrade(t, f)
			result, root := f.finalize(t)

			if result.Action != tc.wantAction {
				t.Fatalf("action = %q, want %q", result.Action, tc.wantAction)
			}
			if root.Status != "closed" || root.Metadata["gc.outcome"] != tc.wantOutcome {
				t.Fatalf("root status=%q gc.outcome=%q, want closed/%s", root.Status, root.Metadata["gc.outcome"], tc.wantOutcome)
			}
			if got := root.Metadata["gc.skipped_blockers"]; got != "" {
				t.Fatalf("root gc.skipped_blockers = %q after the retry closed it %s, want it empty", got, tc.wantOutcome)
			}
		})
	}
}
