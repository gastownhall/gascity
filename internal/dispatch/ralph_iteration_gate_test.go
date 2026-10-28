package dispatch

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// compiledRalphGateFormula is the gastownhall/gascity#2519 shape: a ralph with
// max_attempts >= 2 over several independent children.
const compiledRalphGateFormula = `formula = "ralph-iteration-gate"
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

[[steps.children]]
id = "gamma"
title = "Gamma"
`

// TestCompiledRalphDoesNotSpawnNextIterationWhileMembersInProgress pins the
// iteration boundary from gastownhall/gascity#2519: while iteration 1 members
// are still in_progress, the control dispatcher must not run the check or
// create any iteration 2 bead.
func TestCompiledRalphDoesNotSpawnNextIterationWhileMembersInProgress(t *testing.T) {
	t.Parallel()
	city := newCompiledRalphCity(t, "ralph-iteration-gate", compiledRalphGateFormula, 0)
	fx := buildCompiledRalphFixture(t, identityStore, city, shapeUnlinked, func(string, int) map[string]string { return workerPass })
	fx.work = nil

	ledger := snapshotLedger(t, fx.ledger)
	alpha := ledgerBeadByRefSuffix(t, ledger, "loop.iteration.1.alpha")
	beta := ledgerBeadByRefSuffix(t, ledger, "loop.iteration.1.beta")
	gamma := ledgerBeadByRefSuffix(t, ledger, "loop.iteration.1.gamma")

	inProgress := "in_progress"
	for _, id := range []string{alpha.rawID, beta.rawID, gamma.rawID} {
		if err := fx.store.Update(id, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatalf("claiming %s: %v", id, err)
		}
	}
	if err := updateMetadataAndClose(fx.store, alpha.rawID, workerPass); err != nil {
		t.Fatalf("closing %s: %v", alpha.ID, err)
	}

	if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
		t.Fatalf("serve loop did not converge: %v", errs)
	}
	ledger = snapshotLedger(t, fx.ledger)

	for _, b := range ledger {
		if strings.Contains(b.Metadata[beadmeta.StepRefMetadataKey], "loop.iteration.2") {
			t.Errorf("iteration 2 bead %s (%s, status %s) exists while iteration 1 members are in_progress",
				b.ID, b.Metadata[beadmeta.StepRefMetadataKey], b.Status)
		}
	}
	if iter := ledgerBeadByRefSuffix(t, ledger, "loop.iteration.1"); iter.Status == "closed" {
		t.Errorf("iteration 1 scope closed (outcome %q) while members are in_progress", iter.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if log := ledgerBeadByRefSuffix(t, ledger, ".loop").Metadata[beadmeta.AttemptLogMetadataKey]; log != "" {
		t.Errorf("ralph check fired before iteration 1 finished: attempt log %s", log)
	}
}

// TestCompiledRalphGateFixtureSpawnsNextIterationAfterMembersClose is the
// vacuity control for the test above: once every iteration 1 member closes,
// the failing check fires and iteration 2 is created.
func TestCompiledRalphGateFixtureSpawnsNextIterationAfterMembersClose(t *testing.T) {
	t.Parallel()
	city := newCompiledRalphCity(t, "ralph-iteration-gate", compiledRalphGateFormula, 0)
	fx := buildCompiledRalphFixture(t, identityStore, city, shapeUnlinked, func(string, int) map[string]string { return workerPass })
	fx.work = nil

	ledger := snapshotLedger(t, fx.ledger)
	for _, suffix := range []string{"loop.iteration.1.alpha", "loop.iteration.1.beta", "loop.iteration.1.gamma"} {
		b := ledgerBeadByRefSuffix(t, ledger, suffix)
		if err := updateMetadataAndClose(fx.store, b.rawID, workerPass); err != nil {
			t.Fatalf("closing %s: %v", b.ID, err)
		}
	}
	if errs := serveToQuiescence(t, fx.crashFixture, &writeFaultInjector{}); len(errs) != 0 {
		t.Fatalf("serve loop did not converge: %v", errs)
	}
	if !ledgerHasRefContaining(snapshotLedger(t, fx.ledger), "loop.iteration.2") {
		t.Fatal("iteration 2 was not spawned after iteration 1 closed under a failing check")
	}
}
