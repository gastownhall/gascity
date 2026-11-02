package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	coreDrainAckFormulaDir = "../../internal/bootstrap/packs/core/formulas/"
	coreDrainAckPool       = "rig/polecat"
	coreDrainAckSession    = "rig--polecat-1"
)

// coreDrainAckVars satisfies every required var across the core formulas.
var coreDrainAckVars = map[string]string{
	"convoy_id":        "cv-1",
	"meta_prompt_path": "/tmp/meta.md",
	"dest_path":        "/tmp/dest.md",
	"synth_role":       "writer",
}

// coreDrainAckOwnStepClose matches a fail-closed close of a bead variable:
// `gc bd update "$X" ... --status=closed ... || exit 1` (group 1) or
// `gc bd close "$X" ... || exit 1` (group 2). The captured variable is the bead
// closed.
var coreDrainAckOwnStepClose = regexp.MustCompile(`gc bd (?:update "\$([A-Z_]+)"[^\n]*--status[= ]closed|close "\$([A-Z_]+)")[^\n]*\|\| exit 1`)

// coreDrainAckBlocks returns the body of every fenced bash block in a rendered
// step description.
func coreDrainAckBlocks(description string) []string {
	var blocks []string
	var block []string
	inBash := false
	for _, line := range strings.Split(description, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case !inBash && strings.HasPrefix(trimmed, "```bash"):
			inBash = true
			block = nil
		case inBash && trimmed == "```":
			inBash = false
			blocks = append(blocks, strings.Join(block, "\n"))
		case inBash:
			block = append(block, line)
		}
	}
	return blocks
}

// coreDrainAckViolations returns every `gc runtime drain-ack` in a rendered
// step description that is NOT preceded, inside its own fenced bash block, by a
// fail-closed close of the step bead the session is running — the bead
// `gc hook current --id-only` names. An empty result means every ack in the
// step is preceded by that close.
//
// The block is the unit because it is what an agent runs as one command: a
// close in an earlier block, or in another branch, does not guard this ack.
func coreDrainAckViolations(description string) []string {
	var violations []string
	for _, block := range coreDrainAckBlocks(description) {
		lines := strings.Split(block, "\n")
		for i, line := range lines {
			if !strings.Contains(line, "gc runtime drain-ack") {
				continue
			}
			preceding := strings.Join(lines[:i], "\n")
			closesOwnStep := false
			for _, m := range coreDrainAckOwnStepClose.FindAllStringSubmatch(preceding, -1) {
				if closed := m[1] + m[2]; closed != "WORK_BEAD_ID" {
					closesOwnStep = true
				}
			}
			if !strings.Contains(preceding, "gc hook current --id-only") || !closesOwnStep {
				violations = append(violations, strings.TrimSpace(line))
			}
		}
	}
	return violations
}

// coreDrainAckSteps compiles every core formula and returns the step refs
// (formula-qualified step IDs) whose description runs `gc runtime drain-ack`.
func coreDrainAckSteps(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(coreDrainAckFormulaDir)
	if err != nil {
		t.Fatalf("reading core formulas: %v", err)
	}
	steps := make(map[string][]string)
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".toml")
		if !ok || entry.IsDir() {
			continue
		}
		recipe, err := formula.CompileWithoutRuntimeVarValidation(context.Background(), name, []string{coreDrainAckFormulaDir}, coreDrainAckVars)
		if err != nil {
			t.Fatalf("compiling %s: %v", name, err)
		}
		for _, step := range recipe.Steps {
			if strings.Contains(step.Description, "gc runtime drain-ack") {
				steps[name] = append(steps[name], step.ID)
			}
		}
	}
	return steps
}

// coreDrainAckRun is one core workflow cooked into a store and walked by a
// pool worker up to, and including the claim of, its drain-ack step.
type coreDrainAckRun struct {
	store    *beads.MemStore
	rootID   string
	terminal beads.Bead
	work     beads.Bead
	session  beads.Bead
	// sink reports that workflow-finalize blocks directly on terminal, so the
	// workflow must finalize once terminal closes.
	sink bool
}

// startCoreDrainAckRun cooks formulaName, routes its work steps to the pool as
// gc sling does, and claims each ready step as the worker does, closing it
// through the worker protocol, until it has claimed the step with stepRef.
func startCoreDrainAckRun(t *testing.T, formulaName, stepRef string) coreDrainAckRun {
	t.Helper()
	store := beads.NewMemStore()
	result, err := molecule.Cook(context.Background(), store, formulaName, []string{coreDrainAckFormulaDir}, molecule.Options{Vars: coreDrainAckVars})
	if err != nil {
		t.Fatalf("cook %s: %v", formulaName, err)
	}
	members, err := molecule.ListSubtree(store, result.RootID)
	if err != nil {
		t.Fatalf("list %s: %v", formulaName, err)
	}
	run := coreDrainAckRun{store: store, rootID: result.RootID}
	var finalizeID string
	for _, m := range members {
		if m.Metadata[beadmeta.KindMetadataKey] == "workflow-finalize" {
			finalizeID = m.ID
		}
		if m.ID == result.RootID || beadmeta.IsControlKind(m.Metadata[beadmeta.KindMetadataKey]) {
			continue
		}
		if err := store.SetMetadata(m.ID, beadmeta.RoutedToMetadataKey, coreDrainAckPool); err != nil {
			t.Fatalf("routing %s: %v", m.ID, err)
		}
		if m.Metadata[beadmeta.StepRefMetadataKey] == stepRef {
			run.terminal = m
		}
	}
	if run.terminal.ID == "" {
		t.Fatalf("%s has no %s step", formulaName, stepRef)
	}
	finalizeDeps, err := store.DepList(finalizeID, "down")
	if err != nil {
		t.Fatalf("reading %s workflow-finalize deps: %v", formulaName, err)
	}
	run.sink = slices.ContainsFunc(finalizeDeps, func(d beads.Dep) bool { return d.Type == "blocks" && d.DependsOnID == run.terminal.ID })
	run.session, err = store.Create(beads.Bead{
		Title:    coreDrainAckSession,
		Type:     session.BeadType,
		Metadata: map[string]string{"session_name": coreDrainAckSession, "template": coreDrainAckPool},
	})
	if err != nil {
		t.Fatalf("creating session bead: %v", err)
	}
	run.work, err = store.Create(beads.Bead{Title: "the input convoy's work bead", Type: "task"})
	if err != nil {
		t.Fatalf("creating work bead: %v", err)
	}
	for {
		step, ok := run.nextReadyStep(t)
		if !ok {
			t.Fatalf("%s: no ready work step before reaching %s:\n%s", formulaName, stepRef, run.report(t))
		}
		assignee, status := coreDrainAckSession, "in_progress"
		if err := store.Update(step.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			t.Fatalf("claiming %s: %v", step.ID, err)
		}
		if step.ID == run.terminal.ID {
			return run
		}
		run.closeStep(t, step.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
	}
}

func (r coreDrainAckRun) nextReadyStep(t *testing.T) (beads.Bead, bool) {
	t.Helper()
	ready, err := r.store.Ready()
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	slices.SortFunc(ready, func(a, b beads.Bead) int { return strings.Compare(a.ID, b.ID) })
	for _, b := range ready {
		if b.Metadata[beadmeta.RootBeadIDMetadataKey] == r.rootID && b.Assignee == "" && !beadmeta.IsControlKind(b.Metadata[beadmeta.KindMetadataKey]) {
			return b, true
		}
	}
	return beads.Bead{}, false
}

// closeStep is the close a step's own text performs:
// `gc bd update <id> --set-metadata ... --status=closed`.
func (r coreDrainAckRun) closeStep(t *testing.T, id string, metadata map[string]string) {
	t.Helper()
	status := "closed"
	if err := r.store.Update(id, beads.UpdateOpts{Status: &status, Metadata: metadata}); err != nil {
		t.Fatalf("closing step %s: %v", id, err)
	}
}

// drainAck runs what `gc runtime drain-ack` does to the store — the pre-ack
// release of every in_progress claim the session still holds — and then serves
// every ready control bead of the workflow until nothing more progresses.
func (r coreDrainAckRun) drainAck(t *testing.T) {
	t.Helper()
	var stderr bytes.Buffer
	releaseUnexecutedClaimsOnDrainAck(testWorkLegs("", nil, r.store, nil), r.session, drainAckReleaseBudget, &stderr)
	for round := 0; round < 10; round++ {
		ready, err := r.store.Ready()
		if err != nil {
			t.Fatalf("ready: %v", err)
		}
		progressed := false
		for _, b := range ready {
			if b.Metadata[beadmeta.RootBeadIDMetadataKey] != r.rootID || !beadmeta.IsControlKind(b.Metadata[beadmeta.KindMetadataKey]) {
				continue
			}
			if _, err := dispatch.ProcessControl(r.store, b, dispatch.ProcessOptions{}); err != nil {
				if errors.Is(err, dispatch.ErrControlPending) {
					continue
				}
				t.Fatalf("ProcessControl(%s): %v", b.ID, err)
			}
			progressed = true
		}
		if !progressed {
			return
		}
	}
}

func (r coreDrainAckRun) get(t *testing.T, id string) beads.Bead {
	t.Helper()
	b, err := r.store.Get(id)
	if err != nil {
		t.Fatalf("reading %s: %v", id, err)
	}
	return b
}

func (r coreDrainAckRun) report(t *testing.T) string {
	t.Helper()
	members, err := molecule.ListSubtree(r.store, r.rootID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	slices.SortFunc(members, func(a, b beads.Bead) int { return strings.Compare(a.ID, b.ID) })
	var out strings.Builder
	for _, m := range members {
		fmt.Fprintf(&out, "  %s status=%s assignee=%q kind=%q step_ref=%q outcome=%q\n", m.ID, m.Status, m.Assignee,
			m.Metadata[beadmeta.KindMetadataKey], m.Metadata[beadmeta.StepRefMetadataKey], m.Metadata[beadmeta.OutcomeMetadataKey])
	}
	return out.String()
}

// TestCoreFormulaDrainAckStepsCloseTheirOwnStepFirst walks every core formula
// step that runs `gc runtime drain-ack` the way a pool worker does, and checks
// that the workflow finalizes (ga-rd3fgm).
//
// Every step bead of a v2 workflow is claimed through `gc hook --claim`, so the
// step an agent is running is in_progress and assigned to its session. drain-ack
// begins by releasing every in_progress claim the session still holds
// (releaseUnexecutedClaimsOnDrainAck, #5265): it assumes the worker closed its
// bead first. A step that acks without closing itself therefore hands itself
// back to the pool — open, unassigned, still routed, ready — so a fresh session
// runs it again, and the workflow-finalize control it blocks never fires.
//
// The step's own text decides whether it closes itself before the ack
// (coreDrainAckViolations); the release and the finalize are the real runtime.
func TestCoreFormulaDrainAckStepsCloseTheirOwnStepFirst(t *testing.T) {
	chdirToRealPackageDir(t)

	steps := coreDrainAckSteps(t)
	// Positive control: #5153 closes the drain step `|| exit 1` before acking.
	if !slices.Contains(steps["mol-do-work"], "mol-do-work.drain") {
		t.Fatalf("core drain-ack steps = %v, want mol-do-work.drain among them", steps)
	}
	for formulaName, refs := range steps {
		for _, stepRef := range refs {
			t.Run(stepRef, func(t *testing.T) {
				run := startCoreDrainAckRun(t, formulaName, stepRef)
				violations := coreDrainAckViolations(run.terminal.Description)

				// The step's writes before the ack: the work bead closes on every
				// step's success path; the step itself closes only when its text
				// does so fail-closed ahead of every ack.
				if err := run.store.Close(run.work.ID); err != nil {
					t.Fatalf("closing work bead: %v", err)
				}
				if len(violations) == 0 {
					run.closeStep(t, run.terminal.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
				}
				run.drainAck(t)

				step, root := run.get(t, run.terminal.ID), run.get(t, run.rootID)
				if len(violations) == 0 && step.Status == "closed" && (!run.sink || root.Status == "closed") {
					return
				}
				ready, err := run.store.Ready()
				if err != nil {
					t.Fatalf("ready: %v", err)
				}
				redispatchable := slices.ContainsFunc(ready, func(b beads.Bead) bool { return b.ID == step.ID })
				t.Errorf("%s: after `gc runtime drain-ack` the step %s is status=%q assignee=%q gc.routed_to=%q ready=%v, and the workflow root is %q.\n"+
					"drain-ack released the step the session was still running, so a fresh %s session runs it again and workflow-finalize never fires.\n"+
					"drain-ack lines not preceded by a fail-closed close of the session's own step (gc hook current --id-only):\n  %s\n%s",
					stepRef, step.ID, step.Status, step.Assignee, step.Metadata[beadmeta.RoutedToMetadataKey], redispatchable, root.Status,
					coreDrainAckPool, strings.Join(violations, "\n  "), run.report(t))
			})
		}
	}
}

// TestMolPolecatCommitPushConflictFinalizesFailed pins the push-failure exit of
// mol-polecat-commit (ga-rd3fgm): after three failed pushes the step must close
// itself as a hard failure before acking, so the workflow finalizes FAILED and
// the work bead stays open for the escalation target, instead of drain-ack
// handing the push back to the pool for a session in another checkout.
func TestMolPolecatCommitPushConflictFinalizesFailed(t *testing.T) {
	chdirToRealPackageDir(t)

	run := startCoreDrainAckRun(t, "mol-polecat-commit", "mol-polecat-commit.commit-and-push")
	var failureBlock string
	for _, block := range coreDrainAckBlocks(run.terminal.Description) {
		if strings.Contains(block, "Push failed after 3 attempts") {
			failureBlock = block
		}
	}
	if failureBlock == "" {
		t.Fatalf("commit-and-push has no push-failure block:\n%s", run.terminal.Description)
	}
	if v := coreDrainAckViolations("```bash\n" + failureBlock + "\n```"); len(v) != 0 {
		t.Fatalf("push-failure path acks drain without closing its own step first: %v", v)
	}
	ack := strings.Index(failureBlock, "gc runtime drain-ack")
	for _, want := range []string{"gc.outcome=fail", "gc.failure_class=hard", "gc.failure_reason=push_conflict"} {
		if !strings.Contains(failureBlock[:ack], want) {
			t.Fatalf("push-failure path must close its step with %s before acking:\n%s", want, failureBlock)
		}
	}

	// The failure path leaves the work bead open and closes the step failed.
	run.closeStep(t, run.terminal.ID, map[string]string{
		beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
		beadmeta.FailureReasonMetadataKey: "push_conflict",
	})
	run.drainAck(t)

	if step := run.get(t, run.terminal.ID); step.Status != "closed" {
		t.Fatalf("commit-and-push is %q after a push-conflict drain-ack, want closed (not handed back to the pool):\n%s", step.Status, run.report(t))
	}
	if root := run.get(t, run.rootID); root.Status != "closed" || root.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
		t.Fatalf("workflow root is status=%q gc.outcome=%q, want closed and failed:\n%s", root.Status, root.Metadata[beadmeta.OutcomeMetadataKey], run.report(t))
	}
	if work := run.get(t, run.work.ID); work.Status != "open" {
		t.Fatalf("work bead is %q after a failed push, want it left open for the escalation target", work.Status)
	}
}

// TestCoreDrainAckViolationsRecognizesTheFailClosedClose keeps the text check
// honest: it must flag a bare ack and a work-bead-only close, and accept the
// claimed-step close.
func TestCoreDrainAckViolationsRecognizesTheFailClosedClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		block  string
		wantOK bool
	}{
		{name: "bare ack", block: "gc runtime drain-ack\nexit"},
		{name: "work bead only", block: "gc bd close \"$WORK_BEAD_ID\" || exit 1\ngc runtime drain-ack"},
		{name: "close without fail-closed", block: "STEP_BEAD_ID=$(gc hook current --id-only) || exit 1\ngc bd update \"$STEP_BEAD_ID\" --status=closed\ngc runtime drain-ack"},
		{name: "close not from the claim", block: "gc bd update \"$STEP_BEAD_ID\" --status=closed || exit 1\ngc runtime drain-ack"},
		{name: "claimed step closed first", block: "STEP_BEAD_ID=$(gc hook current --id-only) || exit 1\ngc bd update \"$STEP_BEAD_ID\" --set-metadata gc.outcome=pass --status=closed || exit 1\ngc runtime drain-ack", wantOK: true},
		{name: "ack before close", block: "STEP_BEAD_ID=$(gc hook current --id-only) || exit 1\ngc runtime drain-ack\ngc bd update \"$STEP_BEAD_ID\" --status=closed || exit 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := coreDrainAckViolations("```bash\n" + tc.block + "\n```")
			if ok := len(v) == 0; ok != tc.wantOK {
				t.Fatalf("violations = %v, want ok=%v", v, tc.wantOK)
			}
		})
	}
}
