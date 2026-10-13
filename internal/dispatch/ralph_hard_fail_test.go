package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

// A hard-class failure inside a compiled ralph loop is terminal for the loop:
// the ralph control closes hard in the failing iteration instead of spawning
// iterations up to gc.max_attempts. That was the intent of #4035, but it only
// reached the legacy gc.kind=check processor; formulas compile to
// gc.kind=ralph, whose control looped on every non-pass. RCA ga-o6e4rl
// measured the cost: adopt-pr review loops kept re-running their whole
// reviewer fan-out after an external_live_head_changed hard failure
// (gascity#7036 10.6 h, #7049 25 h, #7084 1.6 h).
//
// The fixtures are cooked from formulas rather than hand-built because the
// defect lived in the shape the compiler emits: a gc.kind=ralph control over a
// scope body whose MEMBER carries the hard failure. Production shows two such
// shapes, and both are exercised:
//
//   - linked: graph-apply instantiation links a compile-time retry attempt to
//     its control (gc.logical_bead_id), so the retry control closes hard and
//     aborts the iteration (gascity#7084 iteration 1);
//   - unlinked: the attempt carries no link, aborts the iteration through its
//     own scope-check, and its retry control closes skipped (every
//     runtime-spawned iteration in gascity#7036, #7049 and #7084).
//
// The MemStore cooks through the sequential instantiation path, which yields
// the unlinked shape (the attempt is created before its control);
// linkCompileTimeRetryAttempts writes the link graph apply would have.

const ralphHardFailCheckScript = ".gc/scripts/review-check.sh"

// compiledReviewLoopFormula is the mol-adopt-pr-v2 review-loop shape: a ralph
// scope member whose body is a reviewer step and an apply-fixes step, each
// with its own retry, inside an outer scope body.
const compiledReviewLoopFormula = `formula = "review-loop-hard-fail"
version = 2
contract = "graph.v2"

[[steps]]
id = "body"
title = "Workflow body"
needs = ["review-loop"]
metadata = { "gc.kind" = "scope", "gc.scope_role" = "body", "gc.scope_name" = "worktree" }

[[steps]]
id = "review-loop"
title = "Review / fix loop"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.ralph]
max_attempts = 5

[steps.ralph.check]
mode = "exec"
path = "` + ralphHardFailCheckScript + `"
timeout = "30s"

[[steps.children]]
id = "review"
title = "Review"

[steps.children.retry]
max_attempts = 2
on_exhausted = "hard_fail"

[[steps.children]]
id = "apply-fixes"
title = "Apply fixes"
needs = ["review"]

[steps.children.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`

// compiledSimpleRalphFormula is a ralph with no children: each iteration is
// the work bead itself.
const compiledSimpleRalphFormula = `formula = "simple-ralph-hard-fail"
version = 2
contract = "graph.v2"

[[steps]]
id = "implement"
title = "Implement"

[steps.ralph]
max_attempts = 5

[steps.ralph.check]
mode = "exec"
path = "` + ralphHardFailCheckScript + `"
timeout = "30s"
`

// linkCompileTimeRetryAttempts gives each compile-time retry attempt the link
// to its retry control that graph-apply instantiation writes through a metadata
// reference: gc.logical_bead_id names the control whose gc.step_ref the
// attempt's gc.control_for carries.
func linkCompileTimeRetryAttempts(t *testing.T, store beads.Store, rootID string) {
	t.Helper()
	members, err := beads.DirectMembers(store, rootID)
	if err != nil {
		t.Fatalf("listing molecule members: %v", err)
	}
	controlByRef := make(map[string]string)
	for _, b := range members {
		if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindRetry {
			controlByRef[b.Metadata[beadmeta.StepRefMetadataKey]] = b.ID
		}
	}
	linked := 0
	for _, b := range members {
		if b.Metadata[beadmeta.RetryAttemptMetadataKey] == "" || b.Metadata[beadmeta.LogicalBeadIDMetadataKey] != "" {
			continue
		}
		controlID, ok := controlByRef[b.Metadata[beadmeta.ControlForMetadataKey]]
		if !ok {
			continue
		}
		if err := store.SetMetadata(b.ID, beadmeta.LogicalBeadIDMetadataKey, controlID); err != nil {
			t.Fatalf("linking attempt %s to %s: %v", b.ID, controlID, err)
		}
		linked++
	}
	if linked == 0 {
		t.Fatal("linked shape requested but the molecule has no compile-time retry attempt")
	}
}

// ralphWorkerPolicy plays the agents: the close metadata for a ready work
// bead, keyed by the bead's step and loop iteration.
type ralphWorkerPolicy func(step string, iteration int) map[string]string

var (
	workerPass       = map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}
	workerIterate    = map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, "review.verdict": "iterate"}
	workerHeadMoved  = workerFailure(beadmeta.FailureClassHard, "external_live_head_changed")
	workerRateLimits = workerFailure(beadmeta.FailureClassTransient, "rate_limited")
)

func workerFailure(class, reason string) map[string]string {
	return map[string]string{
		beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey:  class,
		beadmeta.FailureReasonMetadataKey: reason,
	}
}

// ralphFixtureShape selects how the molecule is instantiated.
type ralphFixtureShape int

const (
	shapeUnlinked ralphFixtureShape = iota
	shapeLinked
)

func (s ralphFixtureShape) String() string {
	if s == shapeLinked {
		return "linked"
	}
	return "unlinked"
}

// compiledRalphFixture is a cooked molecule ready to be served by the crash
// harness. refs resolves a step-ref suffix to the bead minted for it.
type compiledRalphFixture struct {
	crashFixture
	store  beads.Store
	rootID string
}

// compiledRalphCity is the on-disk side of a fixture: the formula to cook and
// a city whose check script passes from checkPassFrom on (0: never). It is
// built once per scenario so every crash-harness rebuild cooks the same
// formula path into the same ledger.
type compiledRalphCity struct {
	name       string
	formulaDir string
	cityPath   string
}

func newCompiledRalphCity(t *testing.T, name, formulaText string, checkPassFrom int) compiledRalphCity {
	t.Helper()
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, name+".toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}
	cityPath := t.TempDir()
	const cityToml = `[workspace]
name = "test-city"

[daemon]
formula_v2 = true

[[agent]]
name = "control-dispatcher"
max_active_sessions = 1
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
	script := "#!/bin/bash\nexit 1\n"
	if checkPassFrom > 0 {
		script = fmt.Sprintf("#!/bin/bash\nif [ \"$GC_ITERATION\" -ge %d ]; then exit 0; fi\nexit 1\n", checkPassFrom)
	}
	writeCheckScript(t, cityPath, filepath.Base(ralphHardFailCheckScript), script)
	return compiledRalphCity{name: name, formulaDir: formulaDir, cityPath: cityPath}
}

// buildCompiledRalphFixture cooks the city's formula in the requested shape and
// wires a worker that closes ready work beads by policy.
func buildCompiledRalphFixture(t *testing.T, wrap func(beads.Store) beads.Store, city compiledRalphCity, shape ralphFixtureShape, policy ralphWorkerPolicy) compiledRalphFixture {
	t.Helper()
	ledger := newBdClosePolicyStore()
	cooked, err := molecule.Cook(context.Background(), ledger, city.name, []string{city.formulaDir}, molecule.Options{})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}
	rootID := cooked.RootID
	if shape == shapeLinked {
		linkCompileTimeRetryAttempts(t, ledger, rootID)
	}
	work := func(t *testing.T) bool {
		open, err := ledger.ListOpen()
		if err != nil {
			t.Fatalf("listing open beads: %v", err)
		}
		slices.SortFunc(open, func(a, b beads.Bead) int { return strings.Compare(a.ID, b.ID) })
		for _, b := range open {
			kind := b.Metadata[beadmeta.KindMetadataKey]
			if b.ID == rootID || kind == beadmeta.KindSpec || kind == beadmeta.KindScope || beadmeta.IsControlKind(kind) {
				continue
			}
			if !allBlockersClosed(t, ledger, b.ID) {
				continue
			}
			iteration, err := strconv.Atoi(b.Metadata[beadmeta.IterationMetadataKey])
			if err != nil {
				t.Fatalf("work bead %s (%s) has no gc.iteration: %v", b.ID, b.Metadata[beadmeta.StepRefMetadataKey], b.Metadata)
			}
			// A runtime-spawned retry attempt keeps its frozen child id as its
			// step ("apply-fixes.attempt.1"), so match the step the way the pack
			// gates do: the id up to the first dot.
			step, _, _ := strings.Cut(b.Metadata[beadmeta.StepIDMetadataKey], ".")
			if err := updateMetadataAndClose(ledger, b.ID, policy(step, iteration)); err != nil {
				t.Fatalf("worker close %s: %v", b.ID, err)
			}
			return true
		}
		return false
	}
	return compiledRalphFixture{
		crashFixture: crashFixture{
			ledger:   []*beads.MemStore{ledger.MemStore},
			dispatch: wrap(ledger),
			opts:     ProcessOptions{CityPath: city.cityPath},
			work:     work,
		},
		store:  ledger,
		rootID: rootID,
	}
}

func identityStore(s beads.Store) beads.Store { return s }

// serveCompiledRalph runs the uninterrupted serve loop to quiescence.
func serveCompiledRalph(t *testing.T, fx compiledRalphFixture) {
	t.Helper()
	if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
		t.Fatalf("serve loop did not converge: %v", errs)
	}
}

// ledgerBeadsByRefSuffix returns the beads whose gc.step_ref ends in suffix.
func ledgerBeadsByRefSuffix(ledger []ledgerBead, suffix string) []ledgerBead {
	var out []ledgerBead
	for _, b := range ledger {
		if strings.HasSuffix(b.Metadata[beadmeta.StepRefMetadataKey], suffix) {
			out = append(out, b)
		}
	}
	return out
}

func ledgerBeadByRefSuffix(t *testing.T, ledger []ledgerBead, suffix string) ledgerBead {
	t.Helper()
	matches := ledgerBeadsByRefSuffix(ledger, suffix)
	if len(matches) != 1 {
		t.Fatalf("beads with step_ref suffix %q = %d, want 1", suffix, len(matches))
	}
	return matches[0]
}

func ledgerHasRefContaining(ledger []ledgerBead, fragment string) bool {
	for _, b := range ledger {
		if strings.Contains(b.Metadata[beadmeta.StepRefMetadataKey], fragment) {
			return true
		}
	}
	return false
}

// requireRalphHardFailed asserts the loop stopped in iteration with a hard
// close carrying reason, and that no later iteration was ever spawned.
func requireRalphHardFailed(t *testing.T, ledger []ledgerBead, ralphRef string, iteration int, reason string) {
	t.Helper()
	ralph := ledgerBeadByRefSuffix(t, ledger, "."+ralphRef)
	for key, want := range map[string]string{
		beadmeta.OutcomeMetadataKey:          beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey:     beadmeta.FailureClassHard,
		beadmeta.FailureReasonMetadataKey:    reason,
		beadmeta.FinalDispositionMetadataKey: beadmeta.DispositionHardFail,
		beadmeta.FailedAttemptMetadataKey:    strconv.Itoa(iteration),
	} {
		if got := ralph.Metadata[key]; got != want {
			t.Errorf("ralph %s = %q, want %q", key, got, want)
		}
	}
	if ralph.Status != "closed" {
		t.Errorf("ralph status = %s, want closed", ralph.Status)
	}
	if log := ralph.Metadata[beadmeta.AttemptLogMetadataKey]; !strings.Contains(log, `"action":"hard-fail"`) || !strings.Contains(log, reason) {
		t.Errorf("ralph attempt log = %s, want a hard-fail entry naming %s", log, reason)
	}
	next := fmt.Sprintf("%s.iteration.%d", ralphRef, iteration+1)
	if ledgerHasRefContaining(ledger, next) {
		t.Errorf("loop spawned %s after a hard failure in iteration %d", next, iteration)
	}
}

// requireHardFailureRecord asserts a closed-failed bead carries the hard
// failure stamp of the member named by subjectSuffix.
func requireHardFailureRecord(t *testing.T, ledger []ledgerBead, b ledgerBead, reason, subjectSuffix string) {
	t.Helper()
	if b.Status != "closed" || b.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
		t.Fatalf("%s status=%s outcome=%q, want closed/fail", b.ID, b.Status, b.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if got := b.Metadata[beadmeta.FailureClassMetadataKey]; got != beadmeta.FailureClassHard {
		t.Errorf("%s gc.failure_class = %q, want hard (the aborting member's record)", b.ID, got)
	}
	if got := b.Metadata[beadmeta.FailureReasonMetadataKey]; got != reason {
		t.Errorf("%s gc.failure_reason = %q, want %q", b.ID, got, reason)
	}
	subject := ledgerBeadByRefSuffix(t, ledger, subjectSuffix)
	if got := b.Metadata[beadmeta.FailureSubjectMetadataKey]; got != subject.ID {
		t.Errorf("%s gc.failure_subject = %q, want %s (%s)", b.ID, got, subject.ID, subjectSuffix)
	}
}

// TestCompiledRalphHardMemberFailureEndsLoop pins the production defect: a
// hard-failed member of a compiled ralph iteration ends the loop in that
// iteration, in both instantiation shapes, and the hard record survives the
// scope abort on the iteration body and on the enclosing scope body.
func TestCompiledRalphHardMemberFailureEndsLoop(t *testing.T) {
	t.Parallel()
	policy := func(step string, _ int) map[string]string {
		if step == "apply-fixes" {
			return workerHeadMoved
		}
		return workerPass
	}
	for _, tc := range []struct {
		shape   ralphFixtureShape
		culprit string // the member whose hard failure aborted the iteration
	}{
		{shape: shapeLinked, culprit: "review-loop.iteration.1.apply-fixes"},
		{shape: shapeUnlinked, culprit: "review-loop.iteration.1.apply-fixes.attempt.1"},
	} {
		t.Run(tc.shape.String(), func(t *testing.T) {
			city := newCompiledRalphCity(t, "review-loop-hard-fail", compiledReviewLoopFormula, 0)
			fx := buildCompiledRalphFixture(t, identityStore, city, tc.shape, policy)
			serveCompiledRalph(t, fx)
			ledger := snapshotLedger(t, fx.ledger)

			requireRalphHardFailed(t, ledger, "review-loop", 1, "external_live_head_changed")
			requireHardFailureRecord(t, ledger, ledgerBeadByRefSuffix(t, ledger, "review-loop.iteration.1"), "external_live_head_changed", tc.culprit)
			requireHardFailureRecord(t, ledger, ledgerBeadByRefSuffix(t, ledger, ".body"), "external_live_head_changed", ".review-loop")
			requireClosedWithOutcome(t, ledger, 0, fx.rootID, beadmeta.OutcomeFail)
		})
	}
}

// TestCompiledRalphHardStopKeepsMemberOutput pins that the stop is driven by
// the hard class, not by any particular reason: a pack-defined terminal close
// (here an adopt-pr maintainer-decision hold) ends the loop the same way, and
// the member's non-gc.* output still reaches the ralph control for whatever
// reads the run afterwards.
func TestCompiledRalphHardStopKeepsMemberOutput(t *testing.T) {
	t.Parallel()
	decisionHold := workerFailure(beadmeta.FailureClassHard, "needs_maintainer_decision")
	decisionHold["review.verdict"] = "needs_decision"
	policy := func(step string, _ int) map[string]string {
		if step == "apply-fixes" {
			return decisionHold
		}
		return workerPass
	}
	city := newCompiledRalphCity(t, "review-loop-hard-fail", compiledReviewLoopFormula, 0)
	fx := buildCompiledRalphFixture(t, identityStore, city, shapeLinked, policy)
	serveCompiledRalph(t, fx)
	ledger := snapshotLedger(t, fx.ledger)

	requireRalphHardFailed(t, ledger, "review-loop", 1, "needs_maintainer_decision")
	if got := ledgerBeadByRefSuffix(t, ledger, ".review-loop").Metadata["review.verdict"]; got != "needs_decision" {
		t.Fatalf("ralph review.verdict = %q, want the member's needs_decision output", got)
	}
}

// TestCompiledRalphTransientMemberFailureStillIterates is the regression
// guard: a transient member failure fails the iteration without a hard
// record, and the loop spawns the next iteration as before.
func TestCompiledRalphTransientMemberFailureStillIterates(t *testing.T) {
	t.Parallel()
	policy := func(step string, iteration int) map[string]string {
		if step == "apply-fixes" && iteration == 1 {
			return workerRateLimits
		}
		return workerPass
	}
	for _, shape := range []ralphFixtureShape{shapeLinked, shapeUnlinked} {
		t.Run(shape.String(), func(t *testing.T) {
			city := newCompiledRalphCity(t, "review-loop-hard-fail", compiledReviewLoopFormula, 2)
			fx := buildCompiledRalphFixture(t, identityStore, city, shape, policy)
			serveCompiledRalph(t, fx)
			ledger := snapshotLedger(t, fx.ledger)

			ralph := ledgerBeadByRefSuffix(t, ledger, ".review-loop")
			if ralph.Status != "closed" || ralph.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
				t.Fatalf("ralph status=%s outcome=%q, want closed/pass after iteration 2", ralph.Status, ralph.Metadata[beadmeta.OutcomeMetadataKey])
			}
			if !ledgerHasRefContaining(ledger, "review-loop.iteration.2") {
				t.Fatal("transient failure in iteration 1 did not spawn iteration 2")
			}
			first := ledgerBeadByRefSuffix(t, ledger, "review-loop.iteration.1")
			if first.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
				t.Fatalf("iteration 1 outcome = %q, want fail", first.Metadata[beadmeta.OutcomeMetadataKey])
			}
			if got := first.Metadata[beadmeta.FailureClassMetadataKey]; got != "" {
				t.Fatalf("iteration 1 gc.failure_class = %q, want none for a transient abort", got)
			}
			requireClosedWithOutcome(t, ledger, 0, fx.rootID, beadmeta.OutcomePass)
		})
	}
}

// TestCompiledSimpleRalphHardSubjectFails keeps parity with the legacy
// TestProcessRalphCheckHardSubjectFailureTerminatesWithoutRetry for a ralph
// without children, whose iteration is the work bead itself.
func TestCompiledSimpleRalphHardSubjectFails(t *testing.T) {
	t.Parallel()
	city := newCompiledRalphCity(t, "simple-ralph-hard-fail", compiledSimpleRalphFormula, 0)
	fx := buildCompiledRalphFixture(t, identityStore, city, shapeUnlinked, func(string, int) map[string]string { return workerHeadMoved })
	serveCompiledRalph(t, fx)
	ledger := snapshotLedger(t, fx.ledger)

	requireRalphHardFailed(t, ledger, "implement", 1, "external_live_head_changed")
	requireClosedWithOutcome(t, ledger, 0, fx.rootID, beadmeta.OutcomeFail)
}

// TestCompiledRalphReplaysRecordedTreadmills replays the review-loop outcome
// sequences recorded for the three adopt-pr loop exhaustions RCA ga-o6e4rl
// traced to this defect (controller event archives, 2026-10-04..11). Each kept
// spawning iterations after a hard external_live_head_changed failure; the
// loop must now stop in the iteration of the first hard failure. The runtime-
// spawned iterations take the unlinked shape, as they did in production.
func TestCompiledRalphReplaysRecordedTreadmills(t *testing.T) {
	t.Parallel()
	type recorded map[int]map[string]map[string]string // iteration -> step -> close metadata
	for _, tc := range []struct {
		name     string
		root     string
		sequence recorded
		stopAt   int
	}{
		{
			// i1 iterate; i2-i5 apply-fixes hard; ralph exhausted at 5.
			name: "gascity#7036", root: "gcg--9223372036850700448",
			sequence: recorded{1: {"apply-fixes": workerIterate}, 2: {"apply-fixes": workerHeadMoved}},
			stopAt:   2,
		},
		{
			// i1 iterate; i2-i5 apply-fixes hard; ralph exhausted at 5.
			name: "gascity#7049", root: "gcg--9223372036850497696",
			sequence: recorded{1: {"apply-fixes": workerIterate}, 2: {"apply-fixes": workerHeadMoved}},
			stopAt:   2,
		},
		{
			// i1 apply-fixes hard (its retry control closed hard: linked).
			name: "gascity#7084", root: "gcg--9223372036849724576",
			sequence: recorded{1: {"apply-fixes": workerHeadMoved}},
			stopAt:   1,
		},
		{
			// #7084 i2/i4: the reviewer, not apply-fixes, failed hard in a
			// runtime-spawned iteration.
			name: "gascity#7084-reviewer", root: "gcg--9223372036849724576",
			sequence: recorded{1: {"apply-fixes": workerIterate}, 2: {"review": workerHeadMoved}},
			stopAt:   2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := func(step string, iteration int) map[string]string {
				if meta, ok := tc.sequence[iteration][step]; ok {
					return meta
				}
				return workerPass
			}
			city := newCompiledRalphCity(t, "review-loop-hard-fail", compiledReviewLoopFormula, 0)
			fx := buildCompiledRalphFixture(t, identityStore, city, shapeLinked, policy)
			serveCompiledRalph(t, fx)
			ledger := snapshotLedger(t, fx.ledger)

			requireRalphHardFailed(t, ledger, "review-loop", tc.stopAt, "external_live_head_changed")
			requireClosedWithOutcome(t, ledger, 0, fx.rootID, beadmeta.OutcomeFail)
		})
	}
}

// TestCrashInjectionCompiledRalphHardFail proves the hard-fail close is
// crash-safe and idempotent: a dispatcher killed at, or failing, any store
// write converges on the uninterrupted end state, still stopped in iteration 1.
//
// It covers the childless ralph. A scoped iteration cannot be compared
// write-by-write yet: scope-abort settlement is serve-order dependent whenever
// the ralph closes in the cycle that aborts its iteration (a preserved
// scope-check ends skipped or pass, and a skip close_reason can bubble onto the
// bodies). Unmodified main shows the same divergence on ralph exhaustion
// (ga-o6e4rl.1); the scoped shapes are covered by the serve-loop tests above.
func TestCrashInjectionCompiledRalphHardFail(t *testing.T) {
	t.Parallel()
	city := newCompiledRalphCity(t, "simple-ralph-hard-fail", compiledSimpleRalphFormula, 0)
	runCrashScenario(t, crashScenario{
		name: "simple-hard-subject",
		build: func(t *testing.T, wrap func(beads.Store) beads.Store) crashFixture {
			return buildCompiledRalphFixture(t, wrap, city, shapeUnlinked,
				func(string, int) map[string]string { return workerHeadMoved }).crashFixture
		},
		check: func(t *testing.T, ledger []ledgerBead) {
			requireRalphHardFailed(t, ledger, "implement", 1, "external_live_head_changed")
		},
	})
}
