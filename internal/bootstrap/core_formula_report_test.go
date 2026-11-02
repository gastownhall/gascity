package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/formula"
)

func coreFormulaSearchPaths(t *testing.T) []string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	if root := bazeltest.OverrideRoot(); root != "" {
		return []string{filepath.Join(root, "internal", "bootstrap", "packs", "core", "formulas")}
	}
	return []string{filepath.Join(filepath.Dir(filename), "packs", "core", "formulas")}
}

func compileCorePolecatReport(t *testing.T) *formula.Recipe {
	t.Helper()
	prev := formula.IsFormulaV2Enabled()
	formula.SetFormulaV2Enabled(true)
	t.Cleanup(func() { formula.SetFormulaV2Enabled(prev) })

	recipe, err := formula.Compile(context.Background(), "mol-polecat-report", coreFormulaSearchPaths(t), map[string]string{
		"convoy_id":   "gc-convoy",
		"base_branch": "main",
	})
	if err != nil {
		t.Fatalf("compile mol-polecat-report: %v", err)
	}
	return recipe
}

func TestCoreMolPolecatReportCompilesWriteReportTerminalStep(t *testing.T) {
	recipe := compileCorePolecatReport(t)

	root := recipe.RootStep()
	if root == nil {
		t.Fatal("root step missing")
	}
	if got := root.Metadata["gc.formula_contract"]; got != "graph.v2" {
		t.Fatalf("root gc.formula_contract = %q, want graph.v2", got)
	}

	writeReport := recipe.StepByID("mol-polecat-report.write-report")
	if writeReport == nil {
		t.Fatal("recipe missing mol-polecat-report.write-report step")
	}

	// write-report must be terminal: only the synthetic graph.v2
	// workflow-finalize step may depend on it.
	for _, dep := range recipe.Deps {
		if dep.DependsOnID != writeReport.ID || dep.Type != "blocks" {
			continue
		}
		if dep.StepID == "mol-polecat-report.workflow-finalize" {
			continue
		}
		t.Fatalf("write-report should be terminal, but %s depends on it", dep.StepID)
	}
}

func TestCoreMolPolecatReportWriteReportStepRecordsNoteAndAvoidsPRFlow(t *testing.T) {
	recipe := compileCorePolecatReport(t)
	writeReport := recipe.StepByID("mol-polecat-report.write-report")
	if writeReport == nil {
		t.Fatal("recipe missing mol-polecat-report.write-report step")
	}

	description := writeReport.Description
	for _, want := range []string{
		"gc convoy status {{convoy_id}}",
		"WORK_BEAD_ID",
		"bd update",
		"--append-notes",
		"bd close",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("write-report description missing %q:\n%s", want, description)
		}
	}

	lowerDescription := strings.ToLower(description)
	for _, forbidden := range []string{
		"gh pr create",
		"git push",
		"{{issue}}",
	} {
		if strings.Contains(lowerDescription, forbidden) {
			t.Fatalf("write-report description must not contain %q:\n%s", forbidden, description)
		}
	}
}

var (
	workBeadUpdatePattern = regexp.MustCompile(`bd update "?\$\{?WORK_BEAD_ID\}?"?(\s|$)`)
	// replaceNotesFlagPattern matches the whole --notes flag, so
	// --append-notes does not count.
	replaceNotesFlagPattern = regexp.MustCompile(`(^|\s)--notes(\s|=|$)`)
)

// TestCoreFormulasNeverReplaceWorkBeadNotes guards against core formula steps
// running `bd update "$WORK_BEAD_ID" ... --notes`. --notes replaces the notes
// field and discards notes the work bead already carries (coordinator notes,
// audit lines); work-bead writes must use --append-notes.
func TestCoreFormulasNeverReplaceWorkBeadNotes(t *testing.T) {
	dir := coreFormulaSearchPaths(t)[0]
	paths, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		t.Fatalf("glob core formulas: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no core formulas found in %s", dir)
	}
	checked := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, cmd := range joinShellContinuations(string(data)) {
			if !workBeadUpdatePattern.MatchString(cmd) {
				continue
			}
			checked++
			if replaceNotesFlagPattern.MatchString(cmd) {
				t.Errorf("%s: work-bead update must use --append-notes, not --notes (it replaces existing notes):\n%s",
					filepath.Base(path), cmd)
			}
		}
	}
	if checked == 0 {
		t.Fatal(`found no 'bd update "$WORK_BEAD_ID"' commands in core formulas; guard pattern is stale`)
	}
}

var (
	workBeadClosePattern = regexp.MustCompile(`bd close "?\$\{?WORK_BEAD_ID\}?"?(\s|$)`)
	statusClosedPattern  = regexp.MustCompile(`(^|\s)(--status|-s)(=|\s+)closed(\s|$)`)
	closeReasonPattern   = regexp.MustCompile(`(^|\s)(--reason|-r|--reason-file)(\s|=)`)
)

// TestCoreFormulasCloseWorkBeadWithReason guards against core formula steps
// closing the work bead without a close reason. bd records only the reason
// given to `bd close --reason` as the bead's close_reason, which the supervisor
// API returns from GET /bead/{id} and the bead.closed event; `bd update` has no
// reason flag, so a work bead closed with `--status=closed` (or a bare
// `bd close`) reaches API clients with no account of how the work ended.
func TestCoreFormulasCloseWorkBeadWithReason(t *testing.T) {
	dir := coreFormulaSearchPaths(t)[0]
	paths, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		t.Fatalf("glob core formulas: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no core formulas found in %s", dir)
	}
	checked := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, cmd := range joinShellContinuations(string(data)) {
			if workBeadUpdatePattern.MatchString(cmd) && statusClosedPattern.MatchString(cmd) {
				t.Errorf("%s: close the work bead with `gc bd close \"$WORK_BEAD_ID\" --reason ...`, not `bd update --status=closed` (bd update records no close reason):\n%s",
					filepath.Base(path), cmd)
				continue
			}
			if !workBeadClosePattern.MatchString(cmd) {
				continue
			}
			checked++
			if !closeReasonPattern.MatchString(cmd) {
				t.Errorf("%s: work-bead close must carry --reason (the API's close_reason):\n%s",
					filepath.Base(path), cmd)
			}
		}
	}
	if checked == 0 {
		t.Fatal(`found no 'bd close "$WORK_BEAD_ID"' commands in core formulas; guard pattern is stale`)
	}
}

// TestCorePoolWorkerPromptClosesWorkBeadWithReason is the prompt-side twin of
// TestCoreFormulasCloseWorkBeadWithReason: the pool worker closes the bead it
// claimed itself, so its instructions must give the close a reason.
func TestCorePoolWorkerPromptClosesWorkBeadWithReason(t *testing.T) {
	path := filepath.Join(filepath.Dir(coreFormulaSearchPaths(t)[0]), "assets", "prompts", "pool-worker.template.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	checked := 0
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "gc bd close <id>") {
			continue
		}
		checked++
		if !closeReasonPattern.MatchString(line) {
			t.Errorf("pool-worker.template.md: work-bead close must carry --reason:\n%s", line)
		}
	}
	if checked == 0 {
		t.Fatal("found no 'gc bd close <id>' instructions in pool-worker.template.md; guard pattern is stale")
	}
}

// TestCoreMolScopedWorkAttemptsStateOutcomeContract checks the text of the
// attempt beads a worker actually claims: every retry-managed mol-scoped-work
// step must tell the worker how to close it with gc.outcome, and must name the
// step bead through `gc hook current --id-only`, because $GC_BEAD_ID is never
// set in a session shell. The close commands are written to be pasted as they
// are, so none may hold an unquoted < or >, and the failure command a worker
// pastes must be the retryable (transient) one.
func TestCoreMolScopedWorkAttemptsStateOutcomeContract(t *testing.T) {
	prev := formula.IsFormulaV2Enabled()
	formula.SetFormulaV2Enabled(true)
	t.Cleanup(func() { formula.SetFormulaV2Enabled(prev) })

	recipe, err := formula.Compile(context.Background(), "mol-scoped-work", coreFormulaSearchPaths(t), map[string]string{
		"convoy_id":   "gc-convoy",
		"base_branch": "main",
	})
	if err != nil {
		t.Fatalf("compile mol-scoped-work: %v", err)
	}
	steps := []string{"load-context", "workspace-setup", "preflight-tests", "implement", "self-review", "submit", "cleanup-worktree"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			id := "mol-scoped-work." + step + ".attempt.1"
			attempt := recipe.StepByID(id)
			if attempt == nil {
				t.Fatalf("compiled mol-scoped-work has no attempt bead %q; is %s still retry-managed?", id, step)
			}
			for _, want := range []string{
				"gc.outcome=pass",
				"gc.outcome=fail",
				"gc.failure_class=",
				"gc.failure_reason=",
				"--status=closed",
				"$(gc hook current --id-only) || exit 1",
			} {
				if !strings.Contains(attempt.Description, want) {
					t.Errorf("%s description does not contain %q; a close without gc.outcome fails the step as missing_outcome", id, want)
				}
			}
			if strings.Contains(attempt.Description, "GC_BEAD_ID") {
				t.Errorf("%s description names GC_BEAD_ID, which is never set in a session shell; use gc hook current --id-only", id)
			}
			closes, transientFailCloses := 0, 0
			for _, cmd := range joinShellContinuations(attempt.Description) {
				if !strings.Contains(cmd, `gc bd update "$STEP_BEAD_ID"`) {
					continue
				}
				closes++
				if hasUnquotedRedirect(cmd) {
					t.Errorf("%s: close command has an unquoted < or >, which the shell runs as a redirect, so the pasted close never reaches gc bd update; quote the placeholder:\n%s", id, cmd)
				}
				if strings.Contains(cmd, "gc.outcome=fail") && strings.Contains(cmd, "gc.failure_class=transient") {
					transientFailCloses++
				}
			}
			if closes == 0 {
				t.Errorf("%s description has no `gc bd update \"$STEP_BEAD_ID\"` close command", id)
			}
			if transientFailCloses == 0 {
				t.Errorf("%s: no failure close command uses gc.failure_class=transient; a failure pasted as hard is never retried", id)
			}
		})
	}
}

// hasUnquotedRedirect reports whether a shell command line has a < or >
// outside quotes. The shell reads one as a redirect, so a <placeholder> left
// unquoted in a command meant to be pasted breaks it.
func hasUnquotedRedirect(cmd string) bool {
	var quote rune
	for _, r := range cmd {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '<' || r == '>':
			return true
		}
	}
	return false
}

// TestHasUnquotedRedirect pins the paste-safety scan the close-command guards
// rely on: it must flag the unquoted placeholder and pass the quoted forms.
func TestHasUnquotedRedirect(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		{name: "unquoted placeholder", cmd: `gc bd update "$ID" --set-metadata gc.failure_reason=<short_reason> --status=closed`, want: true},
		{name: "double-quoted placeholder", cmd: `gc bd update "$ID" --set-metadata gc.failure_reason="<short_reason>" --status=closed`},
		{name: "single-quoted placeholder", cmd: `gc bd update "$ID" --set-metadata gc.failure_reason='<short_reason>'`},
		{name: "redirect after a closed quote", cmd: `gc bd show "$ID" >out.json`, want: true},
		{name: "no placeholder", cmd: `gc bd update "$ID" --set-metadata gc.outcome=pass --status=closed`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasUnquotedRedirect(tt.cmd); got != tt.want {
				t.Fatalf("hasUnquotedRedirect(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

// readCorePoolWorkerPrompt returns the stock pool-worker prompt template.
func readCorePoolWorkerPrompt(t *testing.T) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(coreFormulaSearchPaths(t)[0]), "assets", "prompts", "pool-worker.template.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// corePoolWorkerPromptSection returns the pool-worker prompt section whose
// "## " heading starts with heading.
func corePoolWorkerPromptSection(t *testing.T, prompt, heading string) string {
	t.Helper()
	for _, section := range strings.Split(prompt, "\n## ") {
		if strings.HasPrefix(section, heading) {
			return section
		}
	}
	t.Fatalf("pool-worker.template.md has no %q section", "## "+heading)
	return ""
}

// TestCorePoolWorkerPromptRecordsOutcomeBeforeClose guards the retry contract
// in the stock worker prompt: a retry-managed workflow step whose attempt
// closes without gc.outcome is classified missing_outcome and retried until it
// hard-fails, so every close instruction must be preceded, within its section,
// by the outcome write, and must still give the close a reason.
func TestCorePoolWorkerPromptRecordsOutcomeBeforeClose(t *testing.T) {
	const closeInstruction = "gc bd close <"
	checked := 0
	for _, section := range strings.Split(readCorePoolWorkerPrompt(t), "\n## ") {
		heading, _, _ := strings.Cut(section, "\n")
		rest := section
		for {
			at := strings.Index(rest, closeInstruction)
			if at < 0 {
				break
			}
			checked++
			lineEnd := strings.IndexByte(rest[at:], '\n')
			if lineEnd < 0 {
				lineEnd = len(rest) - at
			}
			line := rest[at : at+lineEnd]
			if !strings.Contains(rest[:at], "--set-metadata gc.outcome=") {
				t.Errorf("pool-worker.template.md section %q: close instruction is not preceded by a `gc bd update <id> --set-metadata gc.outcome=...` write (a close without gc.outcome fails a retry-managed step as missing_outcome):\n%s", heading, line)
			}
			if !closeReasonPattern.MatchString(line) {
				t.Errorf("pool-worker.template.md section %q: close must carry --reason:\n%s", heading, line)
			}
			rest = rest[at+len(closeInstruction):]
		}
	}
	if checked == 0 {
		t.Fatal("found no 'gc bd close <' instructions in pool-worker.template.md; guard pattern is stale")
	}
}

// TestCorePoolWorkerPromptStatesStepFailureContract pins the failure half of
// the retry contract: a workflow step the worker claimed (its metadata has
// gc.root_bead_id) that cannot succeed is closed with gc.outcome=fail, a
// failure class and a quoted reason so the retry evaluator can decide, and no
// success instruction carries failure metadata, which the evaluator retries as
// pass_with_failure_metadata. The failure close must not be keyed on
// gc.step_ref: v1 molecule steps carry it too, nothing evaluates their outcome,
// and closing one only unblocks the steps after it, so the molecule loop never
// records a failure.
func TestCorePoolWorkerPromptStatesStepFailureContract(t *testing.T) {
	prompt := readCorePoolWorkerPrompt(t)
	for _, want := range []string{"gc.failure_class=transient", "gc.failure_class=hard"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("pool-worker.template.md never mentions %q", want)
		}
	}

	closing := corePoolWorkerPromptSection(t, prompt, "Closing a Bead")
	failAt := strings.Index(closing, "gc.outcome=fail")
	if failAt < 0 {
		t.Fatal(`pool-worker.template.md section "Closing a Bead" never records gc.outcome=fail`)
	}
	// The bold lead-in above the failure command names the beads it is for.
	leadAt := strings.LastIndex(closing[:failAt], "\n**")
	if leadAt < 0 {
		t.Fatal(`pool-worker.template.md section "Closing a Bead" has no bold lead-in naming the beads the failure close is for`)
	}
	lead := closing[leadAt:failAt]
	for _, want := range []string{"gc hook --claim", "gc.root_bead_id"} {
		if !strings.Contains(lead, want) {
			t.Errorf("pool-worker.template.md: the failure close must be for the workflow step the worker claimed (its metadata has gc.root_bead_id, which only graph.v2 steps carry), but its lead-in never mentions %q:\n%s", want, lead)
		}
	}
	if strings.Contains(lead, "gc.step_ref") {
		t.Errorf("pool-worker.template.md: the failure close is keyed on gc.step_ref, which v1 molecule steps carry too; closing one of those unblocks the steps after it:\n%s", lead)
	}
	if molecules := corePoolWorkerPromptSection(t, prompt, "Molecules"); strings.Contains(molecules, "gc.outcome=fail") {
		t.Error(`pool-worker.template.md section "Molecules" records gc.outcome=fail; nothing evaluates a molecule step's outcome, so closing a failed step only unblocks the steps after it`)
	}

	failWrites := 0
	closeAfterFail := false
	for _, cmd := range joinShellContinuations(prompt) {
		if failWrites > 0 && strings.Contains(cmd, "gc bd close <") {
			closeAfterFail = true
		}
		if !strings.Contains(cmd, "gc bd update <") {
			continue
		}
		hasFailureMetadata := strings.Contains(cmd, "gc.failure_class=") || strings.Contains(cmd, "gc.failure_reason=")
		if strings.Contains(cmd, "gc.outcome=pass") && hasFailureMetadata {
			t.Errorf("pool-worker.template.md: a pass that carries gc.failure_class or gc.failure_reason is retried:\n%s", cmd)
		}
		if !strings.Contains(cmd, "gc.outcome=fail") {
			continue
		}
		failWrites++
		if !strings.Contains(cmd, "gc.failure_class=") || !strings.Contains(cmd, "gc.failure_reason=") {
			t.Errorf("pool-worker.template.md: a gc.outcome=fail write must also set gc.failure_class and gc.failure_reason:\n%s", cmd)
		}
		if !strings.Contains(cmd, `gc.failure_reason="`) {
			t.Errorf("pool-worker.template.md: quote the gc.failure_reason value; an unquoted <placeholder> is a shell redirect:\n%s", cmd)
		}
	}
	if failWrites == 0 {
		t.Fatal("pool-worker.template.md never tells the worker to record gc.outcome=fail on a step that cannot succeed")
	}
	if !closeAfterFail {
		t.Error("pool-worker.template.md records gc.outcome=fail but never closes the step afterwards; the retry evaluator only reads closed attempts")
	}
}

// joinShellContinuations folds backslash-continued lines into one logical
// command line so multi-line bd invocations are checked as a whole.
func joinShellContinuations(text string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		if strings.HasSuffix(trimmed, "\\") {
			cur.WriteString(strings.TrimSuffix(trimmed, "\\"))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(line)
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
