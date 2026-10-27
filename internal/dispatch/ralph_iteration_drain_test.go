package dispatch

import (
	"context"
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

// drainStoreCase opens the base store a drain fixture cooks on.
type drainStoreCase struct {
	name string
	open func(t *testing.T) (beads.Store, *beads.MemStore)
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
			open: func(t *testing.T) (beads.Store, *beads.MemStore) {
				store, err := beads.OpenSQLiteStore(t.TempDir())
				if err != nil {
					t.Fatalf("OpenSQLiteStore: %v", err)
				}
				t.Cleanup(func() { _ = store.(interface{ CloseStore() error }).CloseStore() })
				return store, nil
			},
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
// check never passes (checkPassFrom 0), so a drained failed iteration always
// iterates.
func buildRalphDrainFixture(t *testing.T, tc drainStoreCase, formulaText string) ralphDrainFixture {
	t.Helper()
	city := newCompiledRalphCity(t, "ralph-iteration-drain", formulaText, 0)
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
			fx := buildRalphDrainFixture(t, tc, compiledRalphDrainFormula)
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
				fx := buildRalphDrainFixture(t, tc, compiledRalphDrainFormula)
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
