package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// cookedSlingParityFormula has both member shapes member routing must fill: a
// plain work step, and a checked step that compiles to a control bead plus its
// iteration and spec sidecar.
const cookedSlingParityFormula = `
formula = "wf-parity"
version = 2
contract = "graph.v2"

[[steps]]
id = "work"
title = "Do the work"

[[steps]]
id = "gated"
title = "Do the gated work"

[steps.check]
max_attempts = 2

[steps.check.check]
mode = "exec"
path = "verify.sh"
`

// onDemandWorkerTOML declares the on-demand pool the cooked-root tests sling
// to: no worker session runs until demand wakes one.
const onDemandWorkerTOML = "\n[[agent]]\nname = \"worker\"\nmax_active_sessions = -1\nmin_active_sessions = 0\n"

// skipUnderSplitStrictGate skips a leg that pins single-store behavior when the
// split-strict gate runs this binary (GC_TEST_SPLIT_STRICT=1). The gate projects
// every city that relocates nothing as split, so no single-store city exists
// under it; the split leg beside each such leg runs under the gate as well.
func skipUnderSplitStrictGate(t *testing.T) {
	t.Helper()
	if splittest.GateEnabled() {
		t.Skip("pins single-store behavior; the split-strict gate projects every city with no [storage] as split, which the split leg covers")
	}
}

// writeCookedSlingParityCity prepares a one-shot CLI city with formula v2
// enabled in its own config, the on-demand worker, and the parity formula on
// disk in the city's formulas directory. bindingRoot is the whole-split
// binding, or "" for a city with no [storage].
func writeCookedSlingParityCity(t *testing.T, bindingRoot string) string {
	t.Helper()
	cityPath := oneShotCLICity(t, bindingRoot)
	appendCityTOML(t, cityPath, "\n[daemon]\nformula_v2 = true\n"+onDemandWorkerTOML)
	formulaDir := filepath.Join(cityPath, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatalf("mkdir formulas: %v", err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, "wf-parity.toml"), []byte(cookedSlingParityFormula), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, "verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write check script: %v", err)
	}
	// The city must import the builtin core pack: the control dispatcher its
	// formulas need is injected from it, exactly as `gc doctor --fix`
	// arranges for a city missing the import.
	if err := newBuiltinImportDoctorCheck(cityPath).Fix(nil); err != nil {
		t.Fatalf("gc doctor --fix (builtin core import): %v", err)
	}
	return cityPath
}

// cookParityFormula runs `gc formula cook wf-parity` on cityPath as a one-shot
// invocation of its own and returns the root it reports.
func cookParityFormula(t *testing.T, cityPath string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := run([]string{"--city", cityPath, "formula", "cook", "wf-parity"}, &out, &errb); code != 0 {
		t.Fatalf("gc formula cook wf-parity exit = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if id, ok := strings.CutPrefix(line, "Root: "); ok {
			return strings.TrimSpace(id)
		}
	}
	t.Fatalf("cook printed no root id; stdout:\n%s\nstderr:\n%s", out.String(), errb.String())
	return ""
}

// workflowRunMembers returns the city's expanded workflow run root — the one
// owning the "work" step; a launch also mints other workflow-shaped roots
// (ralph attempt molecules) that are not the run — and its members keyed by
// gc.step_id (beads without one, like the root, are not members and never
// compare).
func workflowRunMembers(t *testing.T, cityPath string) (root beads.Bead, members map[string]beads.Bead) {
	t.Helper()
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	roots, err := beads.LiveFor(store).List(beads.ListQuery{
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.WorkflowExpandedMetadataKey: "true",
		},
	})
	if err != nil {
		t.Fatalf("list workflow roots: %v", err)
	}
	for _, candidate := range roots {
		all, err := beads.LiveFor(store).List(beads.ListQuery{
			Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: candidate.ID},
			IncludeClosed: true,
		})
		if err != nil {
			t.Fatalf("list members of %s: %v", candidate.ID, err)
		}
		candidateMembers := make(map[string]beads.Bead, len(all))
		for _, b := range all {
			if id := b.Metadata[beadmeta.StepIDMetadataKey]; id != "" {
				candidateMembers[id] = b
			}
		}
		if _, ok := candidateMembers["wf-parity.work"]; ok {
			return candidate, candidateMembers
		}
		t.Logf("city %s: candidate root %s (%q) scope_kind=%q source=%q idem=%q convoy=%q", cityPath, candidate.ID, candidate.Title, candidate.Metadata[beadmeta.ScopeKindMetadataKey], candidate.Metadata[beadmeta.FormulaSourceMetadataKey], candidate.Metadata["idempotency_key"], candidate.Metadata[beadmeta.InputConvoyIDMetadataKey])
	}
	t.Fatalf("no expanded workflow root owns the \"work\" step (%d candidates)", len(roots))
	return root, members
}

// routingKeys are the metadata keys a route fills; parity is per member.
func routingKeys(b beads.Bead) map[string]string {
	keys := []string{
		beadmeta.RoutedToMetadataKey,
		beadmeta.ExecutionRoutedToMetadataKey,
		beadmeta.ExecutionRigContextMetadataKey,
		beadmeta.SessionNameMetadataKey,
		beadmeta.ContinuationGroupMetadataKey,
		beadmeta.SessionAffinityMetadataKey,
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = b.Metadata[k]
	}
	return out
}

// splitCityGraphBinding returns a split one-shot city's graph binding, where gc
// formula cook mints a workflow on such a city.
func splitCityGraphBinding(t *testing.T, cityPath string) beads.Store {
	t.Helper()
	graph, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassGraph)
	if !relocated {
		t.Fatal("graph class is not relocated; the fixture is not a split city")
	}
	return graph
}

// beadsByID indexes every bead store holds, closed ones included.
func beadsByID(t *testing.T, store beads.Store) map[string]beads.Bead {
	t.Helper()
	all := allBeads(t, store)
	byID := make(map[string]beads.Bead, len(all))
	for _, b := range all {
		byID[b.ID] = b
	}
	return byID
}

// assertBeadsUnchanged fails for every bead of after that before does not hold
// with the same status, assignee and metadata.
func assertBeadsUnchanged(t *testing.T, storeName string, before, after map[string]beads.Bead) {
	t.Helper()
	for id, b := range after {
		was, ok := before[id]
		switch {
		case !ok:
			t.Errorf("the refused sling created %s (%q) in the %s store", id, b.Title, storeName)
		case was.Status != b.Status || was.Assignee != b.Assignee || !maps.Equal(was.Metadata, b.Metadata):
			t.Errorf("the refused sling changed %s in the %s store:\n before %s %q %v\n after  %s %q %v", id, storeName, was.Status, was.Assignee, was.Metadata, b.Status, b.Assignee, b.Metadata)
		}
	}
}

// assertSplitCityRefusesCookedRootSling runs `gc sling worker <rootID>` on a
// split city whose graph binding holds the cooked run, and asserts the #5974
// refusal: the sling exits non-zero naming the root and the --formula launch,
// writes nothing to either store, and leaves the run unrouted.
func assertSplitCityRefusesCookedRootSling(t *testing.T, cityPath, rootID string) {
	t.Helper()
	graph := splitCityGraphBinding(t, cityPath)
	work, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("open the work store: %v", err)
	}
	graphBefore, workBefore := beadsByID(t, graph), beadsByID(t, work)
	if _, ok := graphBefore[rootID]; !ok {
		t.Fatalf("the graph binding does not hold the cooked root %s; the fixture is not the split shape", rootID)
	}
	if len(graphBefore) < 2 {
		t.Fatalf("the graph binding holds %d bead(s), want the cooked root and its members", len(graphBefore))
	}

	var stdout, stderr bytes.Buffer
	code := cmdSling(
		[]string{"worker", rootID},
		false, false, false, // isFormula, doNudge, force
		"", nil, "",
		true, false, false, "", // noConvoy, owned, reassign, onFormula
		false, false, false, // noFormula, fromStdin, dryRun
		"", "",
		&stdout, &stderr,
	)
	if code == 0 {
		t.Fatalf("gc sling worker %s on a split city exited 0, want the refusal\nstdout: %s", rootID, stdout.String())
	}
	for _, want := range []string{rootID, "infrastructure store", "--formula"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("refusal %q does not mention %q", stderr.String(), want)
		}
	}

	graphAfter := beadsByID(t, graph)
	assertBeadsUnchanged(t, "graph", graphBefore, graphAfter)
	assertBeadsUnchanged(t, "work", workBefore, beadsByID(t, work))
	for id, b := range graphAfter {
		for _, key := range []string{beadmeta.RoutedToMetadataKey, beadmeta.ExecutionRoutedToMetadataKey} {
			if b.Metadata[key] == "worker" {
				t.Errorf("%s %s = worker: the cooked run must stay unrouted on a split city", id, key)
			}
		}
	}
}

// TestCookedSlingMatchesFormulaLaunchRouting is the parity contract of #9039 on
// a city that keeps its workflows in the work store: a workflow `gc formula
// cook` expands and `gc sling <target> <root>` then routes must carry, per
// member, the same routing metadata as launching the same formula at the same
// target with `gc sling <target> <formula> --formula`. The two legs run in
// twin cities so each store holds exactly one run to compare.
//
// A split city keeps the cooked run in its infrastructure store, where gc
// sling refuses the root (#5974) and the run stays unrouted.
func TestCookedSlingMatchesFormulaLaunchRouting(t *testing.T) {
	t.Run("single-store", func(t *testing.T) {
		skipUnderSplitStrictGate(t)

		// Leg 1 — cook, then sling the cooked root by bead ID.
		cookedCity := writeCookedSlingParityCity(t, "")
		cookedRootID := cookParityFormula(t, cookedCity)
		slingOut, slingErr := runCmdSling(t, "worker", cookedRootID)
		t.Logf("cooked leg sling stdout:\n%s\nstderr:\n%s", slingOut, slingErr)

		// Leg 2 — launch the same formula at the same target, in a twin city
		// so each store holds exactly one run. The explicit --city matters:
		// leg 1's city is still the ambient one.
		launchCity := writeCookedSlingParityCity(t, "")
		var launchOut, launchErr bytes.Buffer
		if code := run([]string{"--city", launchCity, "sling", "worker", "wf-parity", "--formula", "--no-convoy"}, &launchOut, &launchErr); code != 0 {
			t.Fatalf("gc sling worker wf-parity --formula exit = %d\nstdout:\n%s\nstderr:\n%s", code, launchOut.String(), launchErr.String())
		}
		t.Logf("launch leg sling stdout:\n%s\nstderr:\n%s", launchOut.String(), launchErr.String())

		cookedRoot, cookedMembers := workflowRunMembers(t, cookedCity)
		launchRoot, launchMembers := workflowRunMembers(t, launchCity)
		if cookedRoot.ID != cookedRootID {
			t.Fatalf("cooked leg's workflow root = %s, want the cooked root %s", cookedRoot.ID, cookedRootID)
		}

		// The roots themselves route to the same target.
		if got, want := cookedRoot.Metadata[beadmeta.RoutedToMetadataKey], launchRoot.Metadata[beadmeta.RoutedToMetadataKey]; got != want || got != "worker" {
			t.Fatalf("root gc.routed_to: cooked=%q launched=%q, want worker on both", got, want)
		}

		// Per member (by step id), the routing metadata must match the launch.
		if len(cookedMembers) == 0 {
			t.Fatal("cooked run has no members carrying gc.step_id")
		}
		for stepID, cooked := range cookedMembers {
			launched, ok := launchMembers[stepID]
			if !ok {
				t.Errorf("step %q exists in the cooked run but not in the launched one", stepID)
				continue
			}
			delete(launchMembers, stepID)
			got, want := routingKeys(cooked), routingKeys(launched)
			for k := range want {
				if got[k] != want[k] {
					t.Errorf("step %q %s: cooked=%q launched=%q (cooked kind=%q)", stepID, k, got[k], want[k], cooked.Metadata[beadmeta.KindMetadataKey])
				}
			}
		}
		for stepID := range launchMembers {
			t.Errorf("step %q exists in the launched run but not in the cooked one", stepID)
		}
	})

	t.Run("split", func(t *testing.T) {
		cityPath := writeCookedSlingParityCity(t, filepath.Join(t.TempDir(), "store"))
		assertSplitCityRefusesCookedRootSling(t, cityPath, cookParityFormula(t, cityPath))
	})
}
