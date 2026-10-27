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

// A compiled ralph iteration must drain before its control decides anything
// (gastownhall/gascity#2519). On the pass path the iteration body cannot close
// while a member is in flight: each member's scope-check holds it open. On the
// fail path it can: a failed member under gc.on_fail=abort_scope aborts the
// iteration, the abort skips only unclaimed (open) members, and the body
// closes failed beside a claimed sibling that is still running. The ralph
// control then saw a closed iteration and minted the next one, which workers
// claimed while the sibling ran.

// compiledRalphDrainFormula is the #2519 shape: a ralph with max_attempts >= 2
// over two independent children, both ready at once.
const compiledRalphDrainFormula = `formula = "ralph-iteration-drain"
version = 2
contract = "graph.v2"

[[steps]]
id = "loop"
title = "Loop"

[steps.ralph]
max_attempts = 3

[steps.ralph.check]
mode = "exec"
path = "` + ralphHardFailCheckScript + `"
timeout = "30s"

[[steps.children]]
id = "alpha"
title = "Alpha"

[[steps.children]]
id = "beta"
title = "Beta"
`

const ralphDrainClaimant = "worker-beta"

// ralphDrainStore names a store the drain fixture is cooked on.
type ralphDrainStore struct {
	name string
	open func(t *testing.T) beads.Store
}

func memDrainStore() ralphDrainStore {
	return ralphDrainStore{name: "mem", open: func(*testing.T) beads.Store { return beads.NewMemStore() }}
}

func sqliteDrainStore() ralphDrainStore {
	return ralphDrainStore{name: "sqlite", open: func(t *testing.T) beads.Store {
		t.Helper()
		store, err := beads.OpenSQLiteStore(t.TempDir())
		if err != nil {
			t.Fatalf("OpenSQLiteStore: %v", err)
		}
		if closer, ok := store.(interface{ CloseStore() error }); ok {
			t.Cleanup(func() { _ = closer.CloseStore() })
		}
		return store
	}}
}

// ralphDrainFixture is a cooked drain formula with no agents: the test plays
// every worker by hand, so nothing closes unless the test closes it.
type ralphDrainFixture struct {
	crashFixture
	store  beads.Store
	rootID string
}

func buildRalphDrainFixture(t *testing.T, store beads.Store, formulaText string) ralphDrainFixture {
	t.Helper()
	city := newCompiledRalphCity(t, "ralph-iteration-drain", formulaText, 0)
	cooked, err := molecule.Cook(context.Background(), store, city.name, []string{city.formulaDir}, molecule.Options{})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}
	return ralphDrainFixture{
		crashFixture: crashFixture{
			dispatch: store,
			opts:     ProcessOptions{CityPath: city.cityPath, FormulaSearchPaths: []string{city.formulaDir}},
		},
		store:  store,
		rootID: cooked.RootID,
	}
}

func (fx ralphDrainFixture) serve(t *testing.T) {
	t.Helper()
	if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
		t.Fatalf("serve loop did not converge: %v", errs)
	}
}

func (fx ralphDrainFixture) ledger(t *testing.T) []ledgerBead {
	t.Helper()
	return snapshotLedger(t, []beads.Store{fx.store})
}

// claim marks the bead with step-ref suffix in_progress for the drain
// claimant, as a worker's claim does.
func (fx ralphDrainFixture) claim(t *testing.T, suffix string) ledgerBead {
	t.Helper()
	b := ledgerBeadByRefSuffix(t, fx.ledger(t), suffix)
	inProgress, assignee := "in_progress", ralphDrainClaimant
	if err := fx.store.Update(b.rawID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("claiming %s: %v", b.ID, err)
	}
	return b
}

// finish closes the bead with step-ref suffix as its worker would: with the
// given metadata, or bare when meta is nil.
func (fx ralphDrainFixture) finish(t *testing.T, suffix string, meta map[string]string) {
	t.Helper()
	b := ledgerBeadByRefSuffix(t, fx.ledger(t), suffix)
	var err error
	if meta == nil {
		err = fx.store.Close(b.rawID)
	} else {
		err = updateMetadataAndClose(fx.store, b.rawID, meta)
	}
	if err != nil {
		t.Fatalf("closing %s: %v", b.ID, err)
	}
}

// requireNoIteration fails when any bead of the loop's iteration n exists.
func requireNoIteration(t *testing.T, ledger []ledgerBead, n int) {
	t.Helper()
	fragment := "loop.iteration." + strconv.Itoa(n)
	for _, b := range ledger {
		if strings.Contains(b.Metadata[beadmeta.StepRefMetadataKey], fragment) {
			t.Errorf("iteration %d bead %s (%s, status %s) exists before iteration %d drained",
				n, b.ID, b.Metadata[beadmeta.StepRefMetadataKey], b.Status, n-1)
		}
	}
}

// ralphDrainFailures are the ways member alpha ends the iteration failed.
var ralphDrainFailures = []struct {
	name string
	meta map[string]string
}{
	{name: "transient", meta: workerFailure(beadmeta.FailureClassTransient, "rate_limited")},
	{name: "bare", meta: nil},
}

// TestCompiledRalphIterationDrainNoNextIterationWhileSiblingRuns pins AC-1: a
// failed member does not let the loop mint the next iteration while a claimed
// sibling of the same iteration is still running.
func TestCompiledRalphIterationDrainNoNextIterationWhileSiblingRuns(t *testing.T) {
	t.Parallel()
	for _, st := range []ralphDrainStore{memDrainStore(), sqliteDrainStore()} {
		for _, failure := range ralphDrainFailures {
			t.Run(st.name+"/"+failure.name, func(t *testing.T) {
				t.Parallel()
				fx := buildRalphDrainFixture(t, st.open(t), compiledRalphDrainFormula)
				fx.claim(t, "loop.iteration.1.beta")
				fx.finish(t, "loop.iteration.1.alpha", failure.meta)
				fx.serve(t)
				requireNoIteration(t, fx.ledger(t), 2)
			})
		}
	}
}
