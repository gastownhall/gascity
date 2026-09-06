package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// This file covers the one-shot commands that hold a BEAD ID and read or mutate
// that bead: `gc formula version-check` and the two `gc formula cook --attach`
// arms. They resolve their store through classRoutedStoreForID
// (by_id_store_route.go), which is the shared candidate-list-and-probe.
//
// Routing by id answers WHERE, and only where. What the v1 attach arm can then
// do with a class-owned bead the graph.v2 arm cannot: a graph.v2 invocation
// also has to MINT a work-class input convoy, which has no residence for such a
// target, so that arm refuses. The two outcomes sit next to each other below on
// purpose — the resolver is the same, the expressibility is not.
//
// The OTHER residence — an attach bead in the work ledger — is refused by both
// arms, and its tests live in formula_cook_attach_class_test.go
// (TestFormulaCookAttachOnAWorkResidentBeadIsRefusedOnSplitCity and its v1
// twin). That refusal replaced this file's
// TestFormulaCookAttachOnAWorkResidentBeadIsUnchanged, which pinned the shape
// as served: a graft is graph class whatever the formula's version, so serving
// it beside a work-resident attach bead minted graph-class rows in the work
// ledger, which is a stranded write (ga-99xhy).
//
// Its siblings live in oneshot_class_routes_test.go, which covers the BIRTH
// half — where a newly minted bead lands. The distinction matters: a birth is
// routed by the CLASS of what is being created, a by-id operation by where the
// subject already IS.

// cookCityWithSplitGraph is the shared fixture: a cook city whose graph class is
// served from its own binding, plus its work store.
func cookCityWithSplitGraph(t *testing.T) (work, graph beads.Store) {
	t.Helper()
	work, graph, _ = cookCityWithSplitGraphAt(t)
	return work, graph
}

// cookCityWithSplitGraphAt is cookCityWithSplitGraph for the callers that also
// need the city path — the event stream a cook writes lives under it.
func cookCityWithSplitGraphAt(t *testing.T) (work, graph beads.Store, cityDir string) {
	t.Helper()
	cityDir = oneShotCookCity(t)
	graph = splittest.NewClassStore(t, config.BeadClassGraph)
	seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))
	work, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	return work, graph, cityDir
}

// cookFormulaErr drives the real `gc formula cook` command and returns its
// combined output and its error, for the arms whose contract is a REFUSAL.
// cookFormula is the success-path form.
func cookFormulaErr(t *testing.T, name string, extraArgs ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newFormulaCookCmd(&stdout, &stderr)
	cmd.SetArgs(append([]string{name}, extraArgs...))
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return stdout.String() + stderr.String(), err
}

// runVersionCheck drives the real `gc formula version-check <id>` command and
// returns its error, so a routing failure is read from the command rather than
// from a helper.
func runVersionCheck(t *testing.T, beadID string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newFormulaVersionCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{beadID})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return stdout.String() + stderr.String(), err
}

// TestFormulaVersionCheckReadsTheGraphResidentRoot is one of the two readers
// #5150's council named for this slice.
//
// version-check's subject is always a molecule/workflow bead — it needs
// gc.formula_hash, which only instantiation writes — and on a split city every
// such root is minted in the binding. Reading it through the scope store
// reported "not found" for a live root: an existence answer from a ledger that
// has never held the bead, which is the by-id defect this slice closes.
func TestFormulaVersionCheckReadsTheGraphResidentRoot(t *testing.T) {
	work, graph := cookCityWithSplitGraph(t)

	res := cookFormula(t, "graph-work")
	root, err := graph.Get(res.RootID)
	if err != nil {
		t.Fatalf("cooked root %s is not resident in the graph binding: %v", res.RootID, err)
	}
	if root.Metadata[beadmeta.FormulaHashMetadataKey] == "" {
		t.Fatalf("cooked root %s carries no %s; version-check has nothing to compare and this fixture proves nothing", res.RootID, beadmeta.FormulaHashMetadataKey)
	}
	if _, err := work.Get(res.RootID); err == nil {
		t.Fatalf("the work store also holds %s; the premise is that only the binding does", res.RootID)
	}

	out, err := runVersionCheck(t, res.RootID)
	if err != nil {
		t.Fatalf("gc formula version-check %s failed: %v\n%s", res.RootID, err, out)
	}
	if !strings.Contains(out, res.RootID) {
		t.Errorf("version-check output %q does not name %s", out, res.RootID)
	}
}

// TestFormulaVersionCheckLeavesWorkResidentRootsOnTheScopeStore is the other
// half: a root the work ledger holds is still read from the work ledger, so the
// probe cannot have become an unconditional route to the binding.
func TestFormulaVersionCheckLeavesWorkResidentRootsOnTheScopeStore(t *testing.T) {
	work, graph := cookCityWithSplitGraph(t)

	res := cookFormula(t, "legacy-work")
	if _, err := work.Get(res.RootID); err != nil {
		t.Fatalf("a v1 molecule root must stay in the work ledger: %v", err)
	}
	if _, err := graph.Get(res.RootID); err == nil {
		t.Fatalf("the binding holds the v1 root %s; the premise is that only the work store does", res.RootID)
	}

	if out, err := runVersionCheck(t, res.RootID); err != nil {
		t.Fatalf("gc formula version-check %s failed: %v\n%s", res.RootID, err, out)
	}
}

// TestFormulaCookGraphV2AttachOnAClassResidentBeadIsRefused pins the DEFERRED
// arm, and the residence rule that forces the deferral.
//
// A graph.v2 invocation normalizes its target into an input convoy, and a
// synthetic input convoy is a WORK bead (coordclass.Classify routes it to
// ClassWork explicitly). For an attach bead the class binding owns, neither
// placement exists:
//
//   - in the binding: a work-class bead in the infra ledger, which the
//     migration's own equality invariant says never happens
//     (internal/dispatch/drain.go drainUnitConvoyStore,
//     internal/graphv2/invocation_test.go
//     TestSyntheticInputConvoyIsWorkClassAndCoResidentWithItsTarget);
//   - in the work ledger: NormalizeInputConvoy cannot read the target there at
//     all, and the convoy's one `tracks` edge to it is cross-class, which
//     convoy.TrackItemIn refuses with ErrMemberNotCoResident — measured even
//     when the Graph class handle is named explicitly.
//
// So the graft is not expressible and the command refuses, loudly and by name,
// rather than mis-homing the convoy or emitting a run whose work association is
// silently dropped. Nobody loses a capability: on main the same invocation dies
// with "formulas v2 target <id> not found".
//
// When the cross-class membership edge lands (ga-2orlf) this test flips: the
// refusal goes, the convoy is minted work class, and the assertions below become
// the residence assertions for a served graft.
func TestFormulaCookGraphV2AttachOnAClassResidentBeadIsRefused(t *testing.T) {
	work, graph := cookCityWithSplitGraph(t)

	source, err := graph.Create(beads.Bead{Title: "a running workflow step", Type: "task"})
	if err != nil {
		t.Fatalf("create the class-resident attach bead: %v", err)
	}
	if !bdIDIsClassReserved(source.ID) {
		t.Fatalf("the binding minted %q, which carries no reserved class prefix", source.ID)
	}

	out, err := cookFormulaErr(t, "graph-work", "--attach", source.ID)
	if err == nil {
		t.Fatalf("gc formula cook graph-work --attach %s exited 0; a graft whose input convoy has nowhere to live must refuse, not serve\n%s", source.ID, out)
	}
	for _, want := range []string{source.ID, "graph.v2", "work bead", "convoy.TrackItemIn", "ga-2orlf"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal %q does not mention %q; the reason has to travel with the refusal", out, want)
		}
	}

	// The owner ruling, asserted: no work-class bead is born in the infra
	// ledger. A synthetic input convoy minted here is exactly that, and it is
	// what the by-id route did before this refusal.
	for _, b := range allBeads(t, graph) {
		if b.ID == source.ID {
			continue
		}
		t.Errorf("the graph binding holds %s (%q, type=%s, metadata=%v) after a refused graft; a refusal writes nothing, and a synthetic convoy is a WORK bead that must never be born in the infra ledger", b.ID, b.Title, b.Type, b.Metadata)
	}
	if deps, err := graph.DepList(source.ID, "down"); err != nil {
		t.Fatalf("listing attach deps: %v", err)
	} else if len(deps) > 0 {
		t.Errorf("attach bead %s gained %d dep(s) from a refused graft: %+v", source.ID, len(deps), deps)
	}
	for _, b := range allBeads(t, work) {
		if b.Type == "convoy" {
			t.Errorf("the work ledger holds convoy %s (%q) after a refused graft; the refusal must not mint a half-graft either", b.ID, b.Title)
		}
	}
}

// TestFormulaCookAttachEmitsTheWorkAssociation is the COMMAND-level statement
// that the cook's execution-fact legs are not merely equal but right.
//
// executionevent.ProjectCurrent reads the root and its steps from the graph leg
// and the input convoy's `tracks` edges from the work leg, and a work leg that
// does not hold the convoy returns an EMPTY dep list rather than an error — so a
// mis-assigned leg loses the run's link to the work it was grafted onto with no
// error, no warning and exit 0. That failure is invisible to a residence
// assertion, so it is asserted here on the fact the projection exists to
// produce, through the real cobra command.
//
// It used to run on the SPLIT topology, where the two legs can actually
// diverge. That row is gone because the shape is: the only cook that mints an
// input convoy is a graph.v2 --attach, and on a split city BOTH of its
// residences now refuse — a binding-owned attach bead because the convoy has
// nowhere to live (#5163,
// TestFormulaCookGraphV2AttachOnAClassResidentBeadIsRefused) and a work-resident
// one because the sub-DAG would be a stranded write (ga-99xhy,
// TestFormulaCookAttachOnAWorkResidentBeadIsRefusedOnSplitCity). So the
// divergence is unreachable through this command rather than unasserted, and
// the leg ASSIGNMENT keeps its own guard at
// TestEmitFormulaCookExecutionFactsReadsTheConvoyFromTheWorkLeg. When ga-2orlf
// lifts either refusal, this test moves back onto the split fixture with it.
func TestFormulaCookAttachEmitsTheWorkAssociation(t *testing.T) {
	cityPath := oneShotCookCity(t)
	seedCLIStorageRoutes(t, cityPath, nil)
	work, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}

	source, err := work.Create(beads.Bead{Title: "attach target", Type: "task"})
	if err != nil {
		t.Fatalf("create attach bead: %v", err)
	}

	res := cookFormula(t, "graph-work", "--attach", source.ID)

	recorded, err := events.ReadAll(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("read execution events: %v", err)
	}
	associated, steps := 0, 0
	for _, e := range recorded {
		switch {
		case e.Type == events.ExecutionWorkAssociated && e.RunID == res.RootID && e.Subject == source.ID:
			associated++
		case e.Type == events.ExecutionStepDefined && e.RunID == res.RootID:
			steps++
		}
	}
	if steps == 0 {
		t.Fatalf("run %s emitted no execution.step_defined at all (events=%+v); the graph leg is wrong and this fixture proves nothing about the work leg", res.RootID, recorded)
	}
	if associated != 1 {
		t.Fatalf("run %s emitted %d execution.work_associated for attach bead %s, want 1 (events=%+v); the convoy leg read a ledger the input convoy does not live in, and a DepList on a convoy a store never held comes back empty rather than failing", res.RootID, associated, source.ID, recorded)
	}
}

// TestFormulaCookLegacyAttachGraftsOntoAClassResidentBeadInOneStore is the same
// claim for the v1 arm, which runs molecule.Attach rather than the graph.v2
// pipeline. Attach reads the parent, materializes the sub-DAG and writes the
// blocking dep through ONE store, so the store it is handed has to be the one
// that holds the parent.
func TestFormulaCookLegacyAttachGraftsOntoAClassResidentBeadInOneStore(t *testing.T) {
	work, graph := cookCityWithSplitGraph(t)

	source, err := graph.Create(beads.Bead{Title: "a running workflow step", Type: "task"})
	if err != nil {
		t.Fatalf("create the class-resident attach bead: %v", err)
	}

	res := cookFormula(t, "legacy-work", "--attach", source.ID)

	if _, err := graph.Get(res.RootID); err != nil {
		t.Fatalf("v1 sub-DAG root %s is not resident in the store that holds its attach bead: %v", res.RootID, err)
	}
	if _, err := work.Get(res.RootID); err == nil {
		t.Errorf("the work ledger holds v1 sub-DAG root %s, grafted onto a bead it does not hold", res.RootID)
	}
	deps, err := graph.DepList(source.ID, "down")
	if err != nil {
		t.Fatalf("listing attach deps: %v", err)
	}
	if len(deps) == 0 {
		t.Fatalf("attach bead %s has no blocking dep after a v1 cook; the graft was never wired", source.ID)
	}
	for _, dep := range deps {
		if _, err := graph.Get(dep.DependsOnID); err != nil {
			t.Errorf("store holds dep %s -> %s (%s) whose target it cannot resolve: %v", dep.IssueID, dep.DependsOnID, dep.Type, err)
		}
	}
}

// TestFormulaCookAttachStaysOnTheOneStoreOnASingleStoreCity is the single-store
// compatibility row for the whole attach change: a city that relocates nothing
// gets the exact store its scope resolved, so both arms behave as they always
// did — and, since ga-99xhy, are not refused by the split-city class gate.
//
// Green before and after by design. Its teeth were proven by mutation: dropping
// attachGraftClassRefusal's unrelocated early return fails this test, and
// TestFormulaCookAttachEmitsTheWorkAssociation and
// TestFormulaCookAttachIsIdempotent/single-store with it, on
//
//	gc formula cook graph-work: --attach gc-1: gc-1 is work class and lives in
//	a WORK store rather than in the graph binding ...
func TestFormulaCookAttachStaysOnTheOneStoreOnASingleStoreCity(t *testing.T) {
	cityDir := oneShotCookCity(t)
	resetCLIStorageRoutes(t)
	seedCLIStorageRoutes(t, cityDir, nil)
	work, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	source, err := work.Create(beads.Bead{Title: "attach target", Type: "task"})
	if err != nil {
		t.Fatalf("create attach bead: %v", err)
	}

	for _, formulaName := range []string{"graph-work", "legacy-work"} {
		res := cookFormula(t, formulaName, "--attach", source.ID)
		if _, err := work.Get(res.RootID); err != nil {
			t.Fatalf("%s: sub-DAG root %s is not in the one store: %v", formulaName, res.RootID, err)
		}
		deps, err := work.DepList(source.ID, "down")
		if err != nil {
			t.Fatalf("%s: listing attach deps: %v", formulaName, err)
		}
		for _, dep := range deps {
			if _, err := work.Get(dep.DependsOnID); err != nil {
				t.Errorf("%s: dep %s -> %s has an unresolvable target on a single-store city: %v", formulaName, dep.IssueID, dep.DependsOnID, err)
			}
		}
	}
}
