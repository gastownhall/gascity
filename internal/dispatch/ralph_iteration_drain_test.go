package dispatch

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

// The ralph iteration boundary from gastownhall/gascity#2519: iteration N+1
// must not exist until iteration N's post-iteration check has fired. The
// pipeline repro held only the pass path, where every member's own
// scope-check keeps the iteration body open until the sweep finishes. The
// reported defect is the abort path: when one child fails under
// gc.on_fail=abort_scope while a claimed sibling is still running, the abort
// skips only the unclaimed members and the iteration body closes failed
// while the sibling is in_progress — the compiled ralph control then reads
// that closed body as a finished iteration and mints iteration N+1 beside
// the still-running sibling.
//
// The fix scores every ralph verdict behind a drain gate: block on each
// still-running iteration member and defer with ErrControlPending. These
// tests pin the gate from the #2519 shape: a ralph over two independent
// workers, an exec check that always fails, one worker claimed before its
// sibling fails.

// compiledRalphDrainFormula is the #2519 ralph: a loop over two workers.
const compiledRalphDrainFormula = `formula = "ralph-iteration-drain"
version = 2
contract = "graph.v2"

[[steps]]
id = "loop"
title = "Iteration loop"

[steps.ralph]
max_attempts = 3

[steps.ralph.check]
mode = "exec"
path = "` + ralphHardFailCheckScript + `"
timeout = "30s"

[[steps.children]]
id = "a"
title = "Worker A"

[[steps.children]]
id = "b"
title = "Worker B"
`

// compiledRalphDrainFanoutFormula gives worker A an on_complete fanout. A
// failing close routes the scope through the fanout's forced settle — the one
// body close every store takes, including the bd close policy that refuses
// the unforced abort — so this shape lands the closed-body-while-sibling-runs
// state on every store.
const compiledRalphDrainFanoutFormula = `formula = "ralph-iteration-drain"
version = 2
contract = "graph.v2"

[[steps]]
id = "loop"
title = "Iteration loop"

[steps.ralph]
max_attempts = 3

[steps.ralph.check]
mode = "exec"
path = "` + ralphHardFailCheckScript + `"
timeout = "30s"

[[steps.children]]
id = "a"
title = "Worker A"

[steps.children.on_complete]
for_each = "output.items"
bond = "drain-fanout-item"

[[steps.children]]
id = "b"
title = "Worker B"
`

// drainStoreCase opens the base store a drain fixture cooks on.
type drainStoreCase struct {
	name string
	open func(t *testing.T) (beads.Store, *beads.MemStore)
}

// drainSQLiteStore opens a real SQLite store on a throwaway directory.
func drainSQLiteStore(t *testing.T) (beads.Store, *beads.MemStore) {
	t.Helper()
	store, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.(interface{ CloseStore() error }).CloseStore() })
	return store, nil
}

// drainForcedSettleStores are the stores the forced-settle fixture cooks on:
// the bd close-policy double, which refuses the abort's unforced body close,
// and a real SQLite store. The fanout settle forces the body close either
// way.
func drainForcedSettleStores() []drainStoreCase {
	return []drainStoreCase{
		{
			name: "bd-close-policy",
			open: func(*testing.T) (beads.Store, *beads.MemStore) {
				store := newBdClosePolicyStore()
				return store, store.MemStore
			},
		},
		{name: "sqlite", open: drainSQLiteStore},
	}
}

// drainUnforcedAbortStores are the stores where the aborted iteration's
// unforced body close succeeds: a plain MemStore and a real SQLite store.
// The bd close-policy double refuses that close, which hides the defect, so
// it must not host the unforced-abort ACs.
func drainUnforcedAbortStores() []drainStoreCase {
	return []drainStoreCase{
		{
			name: "mem",
			open: func(*testing.T) (beads.Store, *beads.MemStore) {
				store := beads.NewMemStore()
				return store, store
			},
		},
		{
			name: "sqlite",
			open: drainSQLiteStore,
		},
	}
}

// ralphDrainFixture is a cooked drain fixture on an arbitrary base store.
// When the base store is Mem-backed its pointer feeds the crash harness's
// ledger comparisons.
type ralphDrainFixture struct {
	crashFixture
	store     beads.Store
	memLedger []*beads.MemStore
	rootID    string
}

// buildRalphDrainFixture cooks the drain formula on the store tc opens. The
// check passes from iteration checkPassFrom on (0: never), so a drained
// failed iteration iterations until the budget is spent. Each call is a
// fresh city; a caller that rebuilds the fixture for write-by-write
// comparison must create the city once (newRalphDrainCity) and use
// cookRalphDrainFixture, or the ledger's gc.formula_source paths diverge.
func buildRalphDrainFixture(t *testing.T, tc drainStoreCase, formulaText string, checkPassFrom int) ralphDrainFixture {
	t.Helper()
	city := newCompiledRalphCity(t, "ralph-iteration-drain", formulaText, checkPassFrom)
	return cookRalphDrainFixture(t, tc, city)
}

// cookRalphDrainFixture cooks one shared drain city on the store tc opens.
func cookRalphDrainFixture(t *testing.T, tc drainStoreCase, city compiledRalphCity) ralphDrainFixture {
	t.Helper()
	base, mem := tc.open(t)
	cooked, err := molecule.Cook(context.Background(), base, city.name, []string{city.formulaDir}, molecule.Options{})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}
	fx := ralphDrainFixture{
		crashFixture: crashFixture{
			dispatch: base,
			opts:     ProcessOptions{CityPath: city.cityPath},
		},
		store:  base,
		rootID: cooked.RootID,
	}
	if mem != nil {
		fx.memLedger = []*beads.MemStore{mem}
		fx.ledger = fx.memLedger
	}
	return fx
}

// drainStoreAll lists the fixture's whole ledger, closed beads included.
func drainStoreAll(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	all, err := store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("listing ledger: %v", err)
	}
	return all
}

// drainBeadsByRefSuffix returns the beads whose gc.step_ref ends in suffix,
// the same match the ledger-based drain assertions use. The scope-check step
// of an exact-ref bead does not collide: "-scope-check" guards it.
func drainBeadsByRefSuffix(all []beads.Bead, suffix string) []beads.Bead {
	var out []beads.Bead
	for _, b := range all {
		if strings.HasSuffix(b.Metadata[beadmeta.StepRefMetadataKey], suffix) {
			out = append(out, b)
		}
	}
	return out
}

func drainBeadByRefSuffix(t *testing.T, all []beads.Bead, suffix string) beads.Bead {
	t.Helper()
	matches := drainBeadsByRefSuffix(all, suffix)
	if len(matches) != 1 {
		refs := make([]string, 0, len(matches))
		for _, b := range matches {
			refs = append(refs, b.Metadata[beadmeta.StepRefMetadataKey])
		}
		t.Fatalf("beads with step_ref suffix %q = %d (%s), want 1", suffix, len(matches), strings.Join(refs, ", "))
	}
	return matches[0]
}

func drainHasRefContaining(all []beads.Bead, fragment string) bool {
	for _, b := range all {
		if strings.Contains(b.Metadata[beadmeta.StepRefMetadataKey], fragment) {
			return true
		}
	}
	return false
}

// drainRequireNoIteration asserts nothing was minted for iterationNum: not
// the iteration body, none of its children.
func drainRequireNoIteration(t *testing.T, all []beads.Bead, iterationNum int) {
	t.Helper()
	fragment := "iteration." + strconv.Itoa(iterationNum)
	if !drainHasRefContaining(all, fragment) {
		return
	}
	for _, b := range all {
		if ref := b.Metadata[beadmeta.StepRefMetadataKey]; strings.Contains(ref, fragment) {
			t.Errorf("iteration %d bead %s (status %s) exists", iterationNum, ref, b.Status)
		}
	}
}

// drainClaimed marks b claimed by a pool worker: in_progress with an
// assignee, the state a running sibling is in when the abort lands.
func drainClaimed(t *testing.T, store beads.Store, b beads.Bead) {
	t.Helper()
	inProgress := "in_progress"
	assignee := "pool-worker"
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("claiming %s: %v", b.ID, err)
	}
}

// drainServeHeld serves cycles until one makes no progress and no errors. The
// abort that closes the body is progress; the held verdict (an ErrControlPending
// pass, then the control parked behind its drain edge) is idle. The control
// stays open with no attempt spent, so the hold state is the first idle cycle.
func drainServeHeld(t *testing.T, fx crashFixture) {
	t.Helper()
	inj := &writeFaultInjector{}
	for cycle := 0; cycle < 10; cycle++ {
		out := serveCycle(t, fx, inj)
		if len(out.errs) != 0 {
			t.Fatalf("serve cycle made errors: %v", out.errs)
		}
		if !out.progress {
			return
		}
	}
	t.Fatal("serving never quiesced into the drain hold")
}

// TestCompiledRalphIterationDrainControlBlocksLiveMember pins AC-2: through
// the hold, the ralph control stays open with no attempt spent — no
// gc.attempt_log, no check has fired — parked behind a blocks edge on the
// claimed member, which keeps running with its assignee.
func TestCompiledRalphIterationDrainControlBlocksLiveMember(t *testing.T) {
	t.Parallel()
	for _, tc := range drainUnforcedAbortStores() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := buildRalphDrainFixture(t, tc, compiledRalphDrainFormula, 0)
			all := drainStoreAll(t, fx.store)
			b := drainBeadByRefSuffix(t, all, "loop.iteration.1.b")
			drainClaimed(t, fx.store, b)
			a := drainBeadByRefSuffix(t, all, "loop.iteration.1.a")
			if err := updateMetadataAndClose(fx.store, a.ID, workerFailure(beadmeta.FailureClassTransient, "rate_limited")); err != nil {
				t.Fatalf("closing worker A: %v", err)
			}

			drainServeHeld(t, fx.crashFixture)

			all = drainStoreAll(t, fx.store)
			control := drainBeadByRefSuffix(t, all, ".loop")
			if control.Status != "open" {
				t.Fatalf("ralph control status = %s, want open (no verdict yet)", control.Status)
			}
			if log := control.Metadata[beadmeta.AttemptLogMetadataKey]; log != "" {
				t.Errorf("ralph control gc.attempt_log = %s, want empty: the check must not fire before the iteration drains", log)
			}
			bAfter := drainBeadByID(t, fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.1.b").ID)
			if bAfter.Status != "in_progress" || bAfter.Assignee != "pool-worker" {
				t.Errorf("worker B = %s/%s, want still claimed in_progress by pool-worker", bAfter.Status, bAfter.Assignee)
			}
			if !drainBlocksOn(t, fx.store, control, bAfter.ID) {
				t.Errorf("ralph control %s has no blocks edge on the live member %s", control.ID, bAfter.ID)
			}
		})
	}
}

// drainBlocksOn reports whether blocker carries a blocks edge on targetID.
func drainBlocksOn(t *testing.T, store beads.Store, blocker beads.Bead, targetID string) bool {
	t.Helper()
	deps, err := store.DepList(blocker.ID, "down")
	if err != nil {
		t.Fatalf("listing deps of %s: %v", blocker.ID, err)
	}
	for _, dep := range deps {
		if dep.Type == "blocks" && dep.DependsOnID == targetID {
			return true
		}
	}
	return false
}

func drainBeadByID(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("loading %s: %v", id, err)
	}
	return b
}

// drainAttemptLog decodes the control's gc.attempt_log.
func drainAttemptLog(t *testing.T, control beads.Bead) []map[string]string {
	t.Helper()
	var log []map[string]string
	if raw := control.Metadata[beadmeta.AttemptLogMetadataKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &log); err != nil {
			t.Fatalf("decoding gc.attempt_log %q: %v", raw, err)
		}
	}
	return log
}

// TestCompiledRalphIterationDrainContinuesAfterMemberCloses pins AC-3: from
// the AC-1 hold, the claimed member closing passed releases exactly one
// verdict — one attempt-log entry, for iteration 1, carrying the aborted
// iteration's synthetic failure — and exactly one iteration-2 set, nothing
// beyond it.
func TestCompiledRalphIterationDrainContinuesAfterMemberCloses(t *testing.T) {
	t.Parallel()
	for _, tc := range drainUnforcedAbortStores() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := buildRalphDrainFixture(t, tc, compiledRalphDrainFormula, 0)
			all := drainStoreAll(t, fx.store)
			drainClaimed(t, fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.1.b"))
			if err := updateMetadataAndClose(fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.1.a").ID,
				workerFailure(beadmeta.FailureClassTransient, "rate_limited")); err != nil {
				t.Fatalf("closing worker A: %v", err)
			}
			drainServeHeld(t, fx.crashFixture)
			// This AC runs from AC-1's end state: pin the hold itself before
			// releasing it, or the run below would also pass against the
			// unminted-gate behavior whose wreckage it cleans up.
			drainRequireNoIteration(t, drainStoreAll(t, fx.store), 2)
			body := drainBeadByRefSuffix(t, drainStoreAll(t, fx.store), "loop.iteration.1")

			if err := updateMetadataAndClose(fx.store, drainBeadByRefSuffix(t, drainStoreAll(t, fx.store),
				"loop.iteration.1.b").ID, workerPass); err != nil {
				t.Fatalf("closing worker B: %v", err)
			}
			if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
				t.Fatalf("serving after the member closed did not converge: %v", errs)
			}

			all = drainStoreAll(t, fx.store)
			for _, ref := range []string{"loop.iteration.2", "loop.iteration.2.a", "loop.iteration.2.b"} {
				if got := len(drainBeadsByRefSuffix(all, ref)); got != 1 {
					t.Errorf("iteration-2 beads with step_ref suffix %q = %d, want exactly one", ref, got)
				}
			}
			drainRequireNoIteration(t, all, 3)
			control := drainBeadByRefSuffix(t, all, ".loop")
			log := drainAttemptLog(t, control)
			if len(log) != 1 {
				t.Fatalf("ralph gc.attempt_log has %d entries, want exactly one for iteration 1", len(log))
			}
			if want := "attempt subject " + body.ID + " already failed"; log[0]["reason"] != want {
				t.Errorf("attempt-log reason = %q, want %q", log[0]["reason"], want)
			}
			if log[0]["attempt"] != "1" {
				t.Errorf("attempt-log attempt = %q, want the single iteration-1 entry", log[0]["attempt"])
			}
		})
	}
}

// TestCompiledRalphIterationDrainHardFailAfterMemberCloses pins AC-4: the
// hold is what makes the sibling's own verdict visible. When the claimed
// member closes hard instead of passing, the control hard-fails on that
// member's record — terminal, no next iteration — and would have missed it
// had N+1 been minted while the member ran.
func TestCompiledRalphIterationDrainHardFailAfterMemberCloses(t *testing.T) {
	t.Parallel()
	const reason = "external_live_head_changed"
	for _, tc := range drainUnforcedAbortStores() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := buildRalphDrainFixture(t, tc, compiledRalphDrainFormula, 0)
			all := drainStoreAll(t, fx.store)
			drainClaimed(t, fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.1.b"))
			if err := updateMetadataAndClose(fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.1.a").ID,
				workerFailure(beadmeta.FailureClassTransient, "rate_limited")); err != nil {
				t.Fatalf("closing worker A: %v", err)
			}
			drainServeHeld(t, fx.crashFixture)

			if err := updateMetadataAndClose(fx.store, drainBeadByRefSuffix(t, drainStoreAll(t, fx.store),
				"loop.iteration.1.b").ID, workerFailure(beadmeta.FailureClassHard, reason)); err != nil {
				t.Fatalf("closing worker B: %v", err)
			}
			if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
				t.Fatalf("serving after the member closed hard did not converge: %v", errs)
			}

			all = drainStoreAll(t, fx.store)
			control := drainBeadByRefSuffix(t, all, ".loop")
			if control.Status != "closed" {
				t.Fatalf("ralph control status = %s, want closed hard", control.Status)
			}
			for key, want := range map[string]string{
				beadmeta.FinalDispositionMetadataKey: beadmeta.DispositionHardFail,
				beadmeta.FailureReasonMetadataKey:    reason,
				beadmeta.FailedAttemptMetadataKey:    "1",
				beadmeta.OutcomeMetadataKey:          beadmeta.OutcomeFail,
			} {
				if got := control.Metadata[key]; got != want {
					t.Errorf("ralph control gc.%s = %q, want %q", key, got, want)
				}
			}
			drainRequireNoIteration(t, all, 2)
		})
	}
}

// TestCompiledRalphIterationDrainForcedSettleHoldsNextIteration pins the
// forced-settle route to the same defect: an on_complete fanout on the failed
// worker settles the iteration scope through the forced close every store
// takes — the bd close policy included — so the body closes failed while the
// claimed sibling runs. The drain gate still holds the verdict, and exactly
// one iteration 2 exists once the sibling closes.
func TestCompiledRalphIterationDrainForcedSettleHoldsNextIteration(t *testing.T) {
	t.Parallel()
	for _, tc := range drainForcedSettleStores() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := buildRalphDrainFixture(t, tc, compiledRalphDrainFanoutFormula, 0)
			all := drainStoreAll(t, fx.store)
			drainClaimed(t, fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.1.b"))
			if err := updateMetadataAndClose(fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.1.a").ID,
				workerFailure(beadmeta.FailureClassTransient, "rate_limited")); err != nil {
				t.Fatalf("closing worker A: %v", err)
			}
			drainServeHeld(t, fx.crashFixture)

			all = drainStoreAll(t, fx.store)
			fanout := drainBeadByRefSuffix(t, all, "loop.iteration.1.a-fanout")
			if fanout.Status != "closed" || fanout.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
				t.Fatalf("worker A's fanout = %s/%s, want closed/fail: the settle this control drove is the route that closed the body", fanout.Status, fanout.Metadata[beadmeta.OutcomeMetadataKey])
			}
			body := drainBeadByRefSuffix(t, all, "loop.iteration.1")
			if body.Status != "closed" || body.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
				t.Fatalf("iteration body = %s/%s, want settled closed/fail while worker B still runs", body.Status, body.Metadata[beadmeta.OutcomeMetadataKey])
			}
			drainRequireNoIteration(t, all, 2)

			if err := updateMetadataAndClose(fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.1.b").ID, workerPass); err != nil {
				t.Fatalf("closing worker B: %v", err)
			}
			if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
				t.Fatalf("serving after the member closed did not converge: %v", errs)
			}

			all = drainStoreAll(t, fx.store)
			for _, ref := range []string{"loop.iteration.2", "loop.iteration.2.a", "loop.iteration.2.b"} {
				if got := len(drainBeadsByRefSuffix(all, ref)); got != 1 {
					t.Errorf("iteration-2 beads with step_ref suffix %q = %d, want exactly one", ref, got)
				}
			}
			drainRequireNoIteration(t, all, 3)
		})
	}
}

// TestCompiledRalphIterationDrainHoldsRuntimeSpawnedIteration pins the gate
// on iterations the loop minted itself: iteration 1 drains every member and
// fails its real check, iteration 2 is spawned, and when that runtime-spawned
// iteration then fails one member while the other is claimed, no iteration 3
// exists until the claim settles — exactly one after.
func TestCompiledRalphIterationDrainHoldsRuntimeSpawnedIteration(t *testing.T) {
	t.Parallel()
	fx := buildRalphDrainFixture(t, drainUnforcedAbortStores()[0], compiledRalphDrainFormula, 0)
	// The pool serves iteration 1 fully but passes by the runtime-spawned
	// iterations, whose members stay for the test to steer.
	fx.work = compiledRalphWorker(fx.store, fx.rootID, func(_ string, iteration int) map[string]string {
		if iteration == 1 {
			return workerPass
		}
		return nil
	})
	if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
		t.Fatalf("iteration 1 did not drain into a spawned iteration 2: %v", errs)
	}
	all := drainStoreAll(t, fx.store)
	for _, ref := range []string{"loop.iteration.2", "loop.iteration.2.a", "loop.iteration.2.b"} {
		drainBeadByRefSuffix(t, all, ref)
	}

	drainClaimed(t, fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.2.b"))
	if err := updateMetadataAndClose(fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.2.a").ID,
		workerFailure(beadmeta.FailureClassTransient, "rate_limited")); err != nil {
		t.Fatalf("closing iteration-2 worker A: %v", err)
	}
	drainServeHeld(t, fx.crashFixture)

	all = drainStoreAll(t, fx.store)
	drainRequireNoIteration(t, all, 3)
	if b := drainBeadByID(t, fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.2.b").ID); b.Status != "in_progress" || b.Assignee != "pool-worker" {
		t.Errorf("iteration-2 worker B = %s/%s, want still claimed in_progress by pool-worker", b.Status, b.Assignee)
	}

	if err := updateMetadataAndClose(fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.2.b").ID, workerPass); err != nil {
		t.Fatalf("closing iteration-2 worker B: %v", err)
	}
	if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
		t.Fatalf("serving after the member closed did not converge: %v", errs)
	}

	all = drainStoreAll(t, fx.store)
	for _, ref := range []string{"loop.iteration.3", "loop.iteration.3.a", "loop.iteration.3.b"} {
		if got := len(drainBeadsByRefSuffix(all, ref)); got != 1 {
			t.Errorf("iteration-3 beads with step_ref suffix %q = %d, want exactly one", ref, got)
		}
	}
	drainRequireNoIteration(t, all, 4)
}

// TestCompiledRalphIterationDrainDrainedIterationVerdictsUnchanged pins the
// uncontested path: an iteration whose members all closed evaluates in the
// very pass that finds the control ready — one verdict, no pending deferral,
// no drain edge on any member (the control's blockers stay the iteration
// bodies) — and its pass, iterate and exhausted verdicts match the base
// behavior, on a real store per verdict.
func TestCompiledRalphIterationDrainDrainedIterationVerdictsUnchanged(t *testing.T) {
	t.Parallel()
	// drainRequireControlBlocksOnlyIterations asserts every blocks edge of the
	// ralph control points at an iteration body: no member, no scope-check,
	// ever.
	drainRequireControlBlocksOnlyIterations := func(t *testing.T, store beads.Store, control beads.Bead) {
		t.Helper()
		deps, err := store.DepList(control.ID, "down")
		if err != nil {
			t.Fatalf("listing deps of %s: %v", control.ID, err)
		}
		for _, dep := range deps {
			target, err := store.Get(dep.DependsOnID)
			if err != nil {
				t.Fatalf("loading dep target %s: %v", dep.DependsOnID, err)
			}
			isIterationBody := target.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindScope &&
				target.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleBody &&
				strings.Contains(target.Metadata[beadmeta.StepRefMetadataKey], ".iteration.")
			if !isIterationBody {
				t.Errorf("ralph control blocks on %s (%s, kind %q, role %q), want an iteration body only",
					dep.DependsOnID, target.Metadata[beadmeta.StepRefMetadataKey],
					target.Metadata[beadmeta.KindMetadataKey], target.Metadata[beadmeta.ScopeRoleMetadataKey])
			}
		}
	}
	drainCloseAllWorkers := func(t *testing.T, store beads.Store, all []beads.Bead, iterationRef string) {
		t.Helper()
		for _, ref := range []string{iterationRef + ".a", iterationRef + ".b"} {
			if err := updateMetadataAndClose(store, drainBeadByRefSuffix(t, all, ref).ID, workerPass); err != nil {
				t.Fatalf("closing %s: %v", ref, err)
			}
		}
	}

	t.Run("iterate-verdict", func(t *testing.T) {
		t.Parallel()
		fx := buildRalphDrainFixture(t, drainUnforcedAbortStores()[0], compiledRalphDrainFormula, 0)
		all := drainStoreAll(t, fx.store)
		drainCloseAllWorkers(t, fx.store, all, "loop.iteration.1")
		if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
			t.Fatalf("a fully drained iteration did not converge in one pass: %v", errs)
		}
		all = drainStoreAll(t, fx.store)
		control := drainBeadByRefSuffix(t, all, ".loop")
		if control.Status != "open" {
			t.Fatalf("ralph control = %s, want open blocked on iteration 2", control.Status)
		}
		if log := drainAttemptLog(t, control); len(log) != 1 || log[0]["outcome"] != "fail" {
			t.Errorf("iterate verdict: gc.attempt_log = %v, want one fail entry from the real check", log)
		}
		drainBeadByRefSuffix(t, all, "loop.iteration.2")
		drainRequireControlBlocksOnlyIterations(t, fx.store, control)
	})

	t.Run("pass-verdict", func(t *testing.T) {
		t.Parallel()
		fx := buildRalphDrainFixture(t, drainUnforcedAbortStores()[0], compiledRalphDrainFormula, 1)
		all := drainStoreAll(t, fx.store)
		drainCloseAllWorkers(t, fx.store, all, "loop.iteration.1")
		if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
			t.Fatalf("a fully drained passing iteration did not converge in one pass: %v", errs)
		}
		all = drainStoreAll(t, fx.store)
		control := drainBeadByRefSuffix(t, all, ".loop")
		if control.Status != "closed" || control.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
			t.Fatalf("ralph control = %s/%s, want closed/pass on a passing check", control.Status, control.Metadata[beadmeta.OutcomeMetadataKey])
		}
		if log := drainAttemptLog(t, control); len(log) != 1 || log[0]["outcome"] != "pass" {
			t.Errorf("pass verdict: gc.attempt_log = %v, want one pass entry", log)
		}
		drainRequireNoIteration(t, all, 2)
		drainRequireControlBlocksOnlyIterations(t, fx.store, control)
	})

	t.Run("exhausted-verdict", func(t *testing.T) {
		t.Parallel()
		tc := drainUnforcedAbortStores()[0]
		fx := buildRalphDrainFixture(t, tc, compiledRalphDrainFormula, 0)
		fx.work = compiledRalphWorker(fx.store, fx.rootID, func(string, int) map[string]string { return workerPass })
		if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
			t.Fatalf("the exhausting loop did not converge: %v", errs)
		}
		all := drainStoreAll(t, fx.store)
		control := drainBeadByRefSuffix(t, all, ".loop")
		if control.Status != "closed" || control.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
			t.Fatalf("ralph control = %s/%s, want closed/fail at max_attempts", control.Status, control.Metadata[beadmeta.OutcomeMetadataKey])
		}
		if got := control.Metadata[beadmeta.FailedAttemptMetadataKey]; got != "3" {
			t.Errorf("ralph gc.failed_attempt = %q, want the 3rd (final) iteration", got)
		}
		if log := drainAttemptLog(t, control); len(log) != 3 {
			t.Errorf("exhausted verdict: gc.attempt_log has %d entries, want one per iteration", len(log))
		}
		drainRequireNoIteration(t, all, 4)
		drainRequireControlBlocksOnlyIterations(t, fx.store, control)
	})
}

// TestCompiledRalphIterationDrainHoldsNextIteration pins AC-1: when worker A
// of iteration 1 fails — with a transient failure class, or a bare close the
// abort_scope contract counts as failed — while worker B is claimed and
// running, the abort closes the iteration body, but no iteration 2 bead
// exists until the check has a definitive result.
func TestCompiledRalphIterationDrainHoldsNextIteration(t *testing.T) {
	t.Parallel()
	for _, tc := range drainUnforcedAbortStores() {
		for _, failure := range []struct {
			name string
			meta map[string]string
		}{
			{name: "transient-fail", meta: workerFailure(beadmeta.FailureClassTransient, "rate_limited")},
			{name: "bare-close", meta: map[string]string{}},
		} {
			t.Run(tc.name+"/"+failure.name, func(t *testing.T) {
				t.Parallel()
				fx := buildRalphDrainFixture(t, tc, compiledRalphDrainFormula, 0)
				all := drainStoreAll(t, fx.store)
				drainClaimed(t, fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.1.b"))
				a := drainBeadByRefSuffix(t, all, "loop.iteration.1.a")
				if err := updateMetadataAndClose(fx.store, a.ID, failure.meta); err != nil {
					t.Fatalf("closing worker A: %v", err)
				}

				drainServeHeld(t, fx.crashFixture)

				drainRequireNoIteration(t, drainStoreAll(t, fx.store), 2)
			})
		}
	}
}

// TestCrashInjectionCompiledRalphIterationDrain proves the drain gate is
// crash-safe and idempotent over its own write window: a dispatcher killed
// at, or failing, any store write of the abort that closes the body, the gate
// pass that parks the control, and the verdict that ends iteration 1
// converges on the uninterrupted end state — the aborted iteration drains,
// exactly one iteration 2 follows, the control carries its drain edge, and
// the loop exhausts as before the gate. The unforced route (MemStore) and the
// forced-settle route (the bd close-policy double) are both covered.
//
// The fault window stops at the first attach create: molecule's
// crash-recovery of a partially attached iteration does not rebuild the
// children a crash cut short (adoptExistingAttach adopts whatever exists),
// a gap that predates the drain gate and is out of scope here.
func TestCrashInjectionCompiledRalphIterationDrain(t *testing.T) {
	t.Parallel()
	for _, tc := range []crashScenario{
		iterationDrainCrashScenario(t, "unforced-abort", drainUnforcedAbortStores()[0], compiledRalphDrainFormula),
		iterationDrainCrashScenario(t, "forced-settle", drainForcedSettleStores()[0], compiledRalphDrainFanoutFormula),
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runCrashScenario(t, tc)
		})
	}
}

// iterationDrainCrashScenario builds the #2519 drain shape for the crash
// harness: worker A already failed, worker B claimed, and a pool worker that
// closes the claim — and every ready work bead after it, iterations included
// — once the dispatcher has nothing left to retry. The on_complete source must
// close with the output the fanout contract requires, so worker passes carry
// an empty items payload (the fanout of the failed worker never expands).
func iterationDrainCrashScenario(t *testing.T, name string, tc drainStoreCase, formulaText string) crashScenario {
	t.Helper()
	// One city for every rebuild: the formula source path is ledger metadata,
	// so per-build cities would differ from the uninterrupted run's.
	city := newCompiledRalphCity(t, "ralph-iteration-drain", formulaText, 0)
	return crashScenario{
		name: name,
		// The gate's fault window only: the sweep stops at the first
		// molecule.Attach create, whose partial-attach recovery is out of
		// scope (see the test comment).
		stopWritesBeforePrefix: "Create",
		build: func(t *testing.T, wrap func(beads.Store) beads.Store) crashFixture {
			fx := cookRalphDrainFixture(t, tc, city)
			all := drainStoreAll(t, fx.store)
			drainClaimed(t, fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.1.b"))
			if err := updateMetadataAndClose(fx.store, drainBeadByRefSuffix(t, all, "loop.iteration.1.a").ID,
				workerFailure(beadmeta.FailureClassTransient, "rate_limited")); err != nil {
				t.Fatalf("closing worker A: %v", err)
			}
			fx.work = compiledRalphWorker(fx.store, fx.rootID, func(string, int) map[string]string {
				return map[string]string{
					beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
					beadmeta.OutputJSONMetadataKey: `{"items":[]}`,
				}
			})
			fx.dispatch = wrap(fx.store)
			return fx.crashFixture
		},
		check: func(t *testing.T, ledger []ledgerBead) {
			control := ledgerBeadByRefSuffix(t, ledger, ".loop")
			if control.Status != "closed" || control.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
				t.Fatalf("uninterrupted run: ralph control = %s/%s, want closed/fail after exhausting its attempts", control.Status, control.Metadata[beadmeta.OutcomeMetadataKey])
			}
			if got := control.Metadata[beadmeta.FailedAttemptMetadataKey]; got != "3" {
				t.Fatalf("uninterrupted run: ralph gc.failed_attempt = %q, want 3", got)
			}
			b := ledgerBeadByRefSuffix(t, ledger, "loop.iteration.1.b")
			if !slices.Contains(control.Blocks, "blocks:"+b.ID) {
				t.Errorf("uninterrupted run: ralph control has no drain edge on %s", b.ID)
			}
			body2 := ledgerBeadByRefSuffix(t, ledger, "loop.iteration.2")
			if body2.Status != "closed" {
				t.Errorf("uninterrupted run: iteration 2 = %s, want closed", body2.Status)
			}
		},
	}
}
