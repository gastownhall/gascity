package dispatch

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/molecule"
)

// workerBlocked is a worker's close of a step it could not finish and on which
// it asks that no further work start: gc.outcome=fail, gc.work_outcome=blocked
// and a failure class that is neither hard nor transient.
var workerBlocked = map[string]string{
	beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
	beadmeta.WorkOutcomeMetadataKey:   beadmeta.WorkOutcomeBlocked,
	beadmeta.FailureClassMetadataKey:  "parent-candidate-not-published",
	beadmeta.FailureReasonMetadataKey: "parent_candidate_not_published",
}

// TestClassifyRetryAttemptBlockedWorkOutcomeIsHard pins that a failed attempt
// closed gc.work_outcome=blocked ends the retry whatever its failure class, so
// readiness letting the control run behind it never re-runs the attempt.
func TestClassifyRetryAttemptBlockedWorkOutcomeIsHard(t *testing.T) {
	t.Parallel()
	for _, class := range []string{"", beadmeta.FailureClassTransient, beadmeta.FailureClassHard, "scope-mismatch"} {
		got := classifyRetryAttempt(beads.Bead{Metadata: map[string]string{
			beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
			beadmeta.WorkOutcomeMetadataKey:   beadmeta.WorkOutcomeBlocked,
			beadmeta.FailureClassMetadataKey:  class,
			beadmeta.FailureReasonMetadataKey: "scope_mismatch",
		}})
		want := retryEvalResult{Outcome: "hard", Reason: "scope_mismatch"}
		if got != want {
			t.Errorf("classifyRetryAttempt(fail, blocked, class %q) = %+v, want %+v", class, got, want)
		}
	}
	// A blocked work outcome on a passed attempt is not a failure.
	if got := classifyRetryAttempt(beads.Bead{Metadata: map[string]string{
		beadmeta.OutcomeMetadataKey:     beadmeta.OutcomePass,
		beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked,
	}}); got.Outcome != "pass" {
		t.Errorf("classifyRetryAttempt(pass, blocked) = %+v, want pass", got)
	}
}

// TestProcessRetryControlBlockedScopeMismatchHardFails is a worker's
// scope-mismatch close: gc.outcome=fail, gc.work_outcome=blocked,
// gc.failure_class=scope-mismatch. The retry settles hard and schedules no
// second attempt.
func TestProcessRetryControlBlockedScopeMismatchHardFails(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "implement",
		Metadata: map[string]string{
			"gc.kind":             "retry",
			"gc.root_bead_id":     root.ID,
			"gc.step_ref":         "mol-test.implement",
			"gc.step_id":          "implement",
			"gc.max_attempts":     "3",
			"gc.on_exhausted":     "hard_fail",
			"gc.source_step_spec": `{"id":"implement","title":"Implement","type":"task","retry":{"max_attempts":3}}`,
			"gc.control_epoch":    "1",
		},
	})
	attempt1 := mustCreate(t, store, beads.Bead{
		Title: "implement attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id":   root.ID,
			"gc.step_ref":       "mol-test.implement.attempt.1",
			"gc.attempt":        "1",
			"gc.outcome":        "fail",
			"gc.work_outcome":   "blocked",
			"gc.failure_class":  "scope-mismatch",
			"gc.failure_reason": "scope_mismatch",
		},
	})
	mustClose(t, store, attempt1.ID)
	mustDep(t, store, control.ID, attempt1.ID, "blocks")

	result, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processRetryControl: %v", err)
	}
	if result.Action != "hard-fail" {
		t.Fatalf("action = %q, want hard-fail", result.Action)
	}
	after := mustGet(t, store, control.ID)
	if after.Status != "closed" || after.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("control = status %q outcome %q, want closed/fail", after.Status, after.Metadata["gc.outcome"])
	}
	if after.Metadata["gc.final_disposition"] != "hard_fail" {
		t.Fatalf("disposition = %q, want hard_fail", after.Metadata["gc.final_disposition"])
	}
	if after.Metadata["gc.control_epoch"] != "1" {
		t.Fatalf("epoch = %q, want 1 (no second attempt)", after.Metadata["gc.control_epoch"])
	}
	deps, err := store.DepList(control.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("control deps = %d, want 1 (attempt 1 only, no attempt scheduled)", len(deps))
	}
}

// TestCompiledSimpleRalphBlockedIterationFails runs the reported shape: a ralph with
// gc.max_attempts above one whose first iteration closed gc.outcome=fail and
// gc.work_outcome=blocked with class parent-candidate-not-published. The ralph
// is offered behind the blocked step, hard-fails in iteration 1 without running
// its check, clones no iteration 2, and the run settles failed.
func TestCompiledSimpleRalphBlockedIterationFails(t *testing.T) {
	t.Parallel()
	city := newCompiledRalphCity(t, "simple-ralph-hard-fail", compiledSimpleRalphFormula, 0)
	fx := buildCompiledRalphFixture(t, identityStore, city, shapeUnlinked, func(string, int) map[string]string { return workerBlocked })
	serveCompiledRalph(t, fx)
	ledger := snapshotLedger(t, fx.ledger)

	requireRalphHardFailed(t, ledger, "implement", 1, "parent_candidate_not_published")
	requireClosedWithOutcome(t, ledger, 0, fx.rootID, beadmeta.OutcomeFail)
}

// TestCompiledRalphBlockedMemberEndsLoop covers the scoped iteration: a member
// of the iteration's scope closes blocked, in both the linked shape (its retry
// control reads the blocked attempt as hard) and the unlinked shape (the
// member itself is the culprit), and the loop stops in that iteration.
func TestCompiledRalphBlockedMemberEndsLoop(t *testing.T) {
	t.Parallel()
	for _, shape := range []ralphFixtureShape{shapeLinked, shapeUnlinked} {
		t.Run(shape.String(), func(t *testing.T) {
			policy := func(step string, iteration int) map[string]string {
				if step == "apply-fixes" && iteration == 1 {
					return workerBlocked
				}
				return workerPass
			}
			city := newCompiledRalphCity(t, "review-loop-hard-fail", compiledReviewLoopFormula, 0)
			fx := buildCompiledRalphFixture(t, identityStore, city, shape, policy)
			serveCompiledRalph(t, fx)
			ledger := snapshotLedger(t, fx.ledger)

			requireRalphHardFailed(t, ledger, "review-loop", 1, "parent_candidate_not_published")
			requireClosedWithOutcome(t, ledger, 0, fx.rootID, beadmeta.OutcomeFail)
		})
	}
}

// TestProcessRalphCheckBlockedSubjectEndsLoop is the KindCheck shape of a
// ralph: its subject closed gc.outcome=fail and gc.work_outcome=blocked with a
// class that is neither hard nor transient. The check settles the logical bead
// failed in that attempt and clones no attempt 2, though gc.max_attempts
// allows five.
func TestProcessRalphCheckBlockedSubjectEndsLoop(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "check.sh", "#!/bin/bash\nexit 0\n")
	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 5)
	if err := store.SetMetadataBatch(run1.ID, workerBlocked); err != nil {
		t.Fatalf("stamp blocked subject: %v", err)
	}
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}

	result, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	if !result.Processed || result.Action != "hard-fail" {
		t.Fatalf("result = %+v, want processed hard-fail", result)
	}
	logicalAfter := mustGetBead(t, store, logical.ID)
	if logicalAfter.Status != "closed" || logicalAfter.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
		t.Fatalf("logical = status %q outcome %q, want closed/fail", logicalAfter.Status, logicalAfter.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if got := logicalAfter.Metadata[beadmeta.FailureReasonMetadataKey]; got != "parent_candidate_not_published" {
		t.Fatalf("logical gc.failure_reason = %q, want parent_candidate_not_published", got)
	}
	all, err := beads.DirectMembers(store, run1.Metadata[beadmeta.RootBeadIDMetadataKey])
	if err != nil {
		t.Fatalf("beads.DirectMembers: %v", err)
	}
	for _, b := range all {
		if b.Metadata[beadmeta.AttemptMetadataKey] == "2" {
			t.Fatalf("a blocked subject must not clone another attempt; found %s (kind %q)", b.ID, b.Metadata[beadmeta.KindMetadataKey])
		}
	}
}

// compiledBlockedRepairFormula is the build-from-review-base tail that
// deadlocked: a repair-review step, a finalize step under a ralph check, and a
// publish step.
const compiledBlockedRepairFormula = `formula = "blocked-repair-review"
version = 2
contract = "graph.v2"

[[steps]]
id = "repair-review"
title = "Repair or block review findings"

[[steps]]
id = "finalize"
title = "Finalize result"
needs = ["repair-review"]

[steps.check]
max_attempts = 3

[steps.check.check]
mode = "exec"
path = "` + ralphHardFailCheckScript + `"
timeout = "30s"

[[steps]]
id = "publish"
title = "Publish result"
needs = ["finalize"]
`

// TestBlockedRepairReviewSettlesWorkflowOnEveryStore runs the reported shape
// through the stores' own Ready, the predicate the controller's demand and
// the control dispatcher read. The repair-review worker closes its step
// gc.outcome=fail and gc.work_outcome=blocked. The finalize iteration must
// still be offered, so the finalize worker writes its blocked report and the
// ralph settles failed in iteration 1; the publish step runs, blocks with its
// reason as the formula's publish does after a blocked finalize, and the
// workflow root closes failed. No hand step: when Ready offers nothing and the root is
// open, the workflow is deadlocked.
func TestBlockedRepairReviewSettlesWorkflowOnEveryStore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		open func(t *testing.T) beads.Store
	}{
		{"memory", func(*testing.T) beads.Store { return beads.NewMemStore() }},
		{"file", func(t *testing.T) beads.Store {
			s, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
			if err != nil {
				t.Fatalf("OpenFileStore: %v", err)
			}
			return s
		}},
		{"sqlite", func(t *testing.T) beads.Store {
			s, err := beads.OpenSQLiteStore(t.TempDir())
			if err != nil {
				t.Fatalf("OpenSQLiteStore: %v", err)
			}
			t.Cleanup(func() { _ = s.(*beads.SQLiteStore).CloseStore() })
			return s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			city := newCompiledRalphCity(t, "blocked-repair-review", compiledBlockedRepairFormula, 0)
			store := tc.open(t)
			cooked, err := molecule.Cook(context.Background(), store, city.name, []string{city.formulaDir}, molecule.Options{})
			if err != nil {
				t.Fatalf("Cook: %v", err)
			}
			rootID := cooked.RootID
			closeAs := map[string]map[string]string{
				"repair-review": workerBlocked,
				"finalize":      workerBlocked,
				"publish":       workerBlocked,
			}
			opts := ProcessOptions{CityPath: city.cityPath}

			for turn := 0; ; turn++ {
				if turn == 40 {
					t.Fatal("workflow did not settle in 40 turns")
				}
				ready, err := store.Ready()
				if err != nil {
					t.Fatalf("Ready: %v", err)
				}
				slices.SortFunc(ready, func(a, b beads.Bead) int { return strings.Compare(a.ID, b.ID) })
				acted := false
				for _, b := range ready {
					kind := b.Metadata[beadmeta.KindMetadataKey]
					switch {
					case b.ID == rootID || kind == beadmeta.KindSpec || kind == beadmeta.KindScope:
						continue
					case beadmeta.IsControlKind(kind):
						result, err := ProcessControl(store, b, opts)
						if errors.Is(err, ErrControlPending) {
							continue
						}
						if err != nil {
							t.Fatalf("ProcessControl(%s kind=%s): %v", b.ID, kind, err)
						}
						acted = acted || result.Processed
					default:
						// "blocked-repair-review.publish" or "finalize.iteration.1".
						step, _, _ := strings.Cut(strings.TrimPrefix(b.Metadata[beadmeta.StepRefMetadataKey], city.name+"."), ".")
						meta, ok := closeAs[step]
						if !ok {
							t.Fatalf("Ready offered %s (step %q, step_ref %q), which no worker plays", b.ID, step, b.Metadata[beadmeta.StepRefMetadataKey])
						}
						if err := updateMetadataAndClose(store, b.ID, meta); err != nil {
							t.Fatalf("worker close %s: %v", b.ID, err)
						}
						acted = true
					}
					if acted {
						break
					}
				}
				root, err := store.Get(rootID)
				if err != nil {
					t.Fatalf("Get root: %v", err)
				}
				if root.Status == "closed" {
					if got := root.Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomeFail {
						t.Fatalf("root gc.outcome = %q, want fail", got)
					}
					break
				}
				if !acted {
					t.Fatalf("deadlock: root %s is open and Ready offers nothing to run (ready: %v)", rootID, readyRefs(ready))
				}
			}

			members, err := store.List(beads.ListQuery{IncludeClosed: true, Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID}})
			if err != nil {
				t.Fatalf("listing members: %v", err)
			}
			var ralph, iteration *beads.Bead
			for i := range members {
				b := &members[i]
				ref := b.Metadata[beadmeta.StepRefMetadataKey]
				switch {
				case b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindRalph:
					ralph = b
				case ref == "finalize.iteration.1":
					iteration = b
				case strings.Contains(ref, "finalize.iteration.2"):
					t.Fatalf("a blocked finalize iteration must not clone iteration 2; found %s", b.ID)
				}
			}
			if iteration == nil || iteration.Status != "closed" || iteration.Metadata[beadmeta.WorkOutcomeMetadataKey] != beadmeta.WorkOutcomeBlocked {
				t.Fatalf("finalize iteration 1 = %+v, want closed by its worker with gc.work_outcome=blocked", iteration)
			}
			if ralph == nil || ralph.Status != "closed" || ralph.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail ||
				ralph.Metadata[beadmeta.FinalDispositionMetadataKey] != beadmeta.DispositionHardFail {
				t.Fatalf("ralph = %+v, want closed fail with a hard_fail disposition", ralph)
			}
		})
	}
}

func readyRefs(ready []beads.Bead) []string {
	refs := make([]string, 0, len(ready))
	for _, b := range ready {
		refs = append(refs, b.ID+"="+b.Metadata[beadmeta.StepRefMetadataKey])
	}
	return refs
}
