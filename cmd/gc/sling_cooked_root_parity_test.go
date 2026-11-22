package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formulatest"
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

// writeCookedSlingParityCity prepares a one-shot CLI city with an on-demand
// worker and the parity formula on disk in the city's formulas directory.
func writeCookedSlingParityCity(t *testing.T) string {
	t.Helper()
	cityPath := oneShotCLICity(t, "")
	f, err := os.OpenFile(filepath.Join(cityPath, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open city.toml: %v", err)
	}
	if _, err := f.WriteString("\n[[agent]]\nname = \"worker\"\nmax_active_sessions = -1\nmin_active_sessions = 0\n"); err != nil {
		t.Fatalf("append agent: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close city.toml: %v", err)
	}
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

// TestCookedSlingMatchesFormulaLaunchRouting is the parity contract of #9039:
// a workflow `gc formula cook` expands and `gc sling <target> <root>` then
// routes must carry, per member, the same routing metadata as launching the
// same formula at the same target with `gc sling <target> <formula>
// --formula`. The two legs run in twin cities so each store holds exactly one
// run to compare.
func TestCookedSlingMatchesFormulaLaunchRouting(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	// Leg 1 — cook, then sling the cooked root by bead ID.
	cookedCity := writeCookedSlingParityCity(t)
	var cookOut, cookErr bytes.Buffer
	cookCmd := newRootCmd(&cookOut, &cookErr)
	cookCmd.SetArgs([]string{"--city", cookedCity, "formula", "cook", "wf-parity"})
	cookCmd.SetOut(&cookOut)
	cookCmd.SetErr(&cookErr)
	if err := cookCmd.Execute(); err != nil {
		t.Fatalf("gc formula cook wf-parity: %v\nstdout:\n%s\nstderr:\n%s", err, cookOut.String(), cookErr.String())
	}
	var cookedRootID string
	for _, line := range strings.Split(cookOut.String(), "\n") {
		if id, ok := strings.CutPrefix(line, "Root: "); ok {
			cookedRootID = strings.TrimSpace(id)
		}
	}
	if cookedRootID == "" {
		t.Fatalf("cook printed no root id; stdout:\n%s\nstderr:\n%s", cookOut.String(), cookErr.String())
	}
	slingOut, slingErr := runCmdSling(t, "worker", cookedRootID)
	t.Logf("cooked leg sling stdout:\n%s\nstderr:\n%s", slingOut, slingErr)

	// Leg 2 — launch the same formula at the same target, in a twin city so
	// each store holds exactly one run. The explicit --city matters: a prior
	// command in this process may have persisted its own city as current.
	launchCity := writeCookedSlingParityCity(t)
	var launchOut, launchErr bytes.Buffer
	launchCmd := newRootCmd(&launchOut, &launchErr)
	launchCmd.SetArgs([]string{"--city", launchCity, "sling", "worker", "wf-parity", "--formula", "--no-convoy"})
	launchCmd.SetOut(&launchOut)
	launchCmd.SetErr(&launchErr)
	if err := launchCmd.Execute(); err != nil {
		t.Fatalf("gc sling worker wf-parity --formula: %v\nstdout:\n%s\nstderr:\n%s", err, launchOut.String(), launchErr.String())
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
}
