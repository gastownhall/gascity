package core

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// formulaFile is the subset of a formula TOML these tests inspect. Steps carry
// the agent-facing instructions, so asserting on a step description is how the
// pack pins behavior that lives in prompt text rather than in Go.
type formulaFile struct {
	Formula string `toml:"formula"`
	Steps   []struct {
		ID          string `toml:"id"`
		Title       string `toml:"title"`
		Description string `toml:"description"`
		Retry       *struct {
			MaxAttempts int `toml:"max_attempts"`
		} `toml:"retry"`
	} `toml:"steps"`
}

// readFormula decodes a formula TOML from the embedded core pack.
//
// file stays parameterized even though every current caller passes
// mol-polecat-base.toml: the pack ships sibling formulas (mol-polecat-commit,
// mol-polecat-report) that inherit these steps, and the next test to pin one of
// them reads it through this same helper.
//
//nolint:unparam // see above
func readFormula(t *testing.T, file string) formulaFile {
	t.Helper()
	data, err := fs.ReadFile(PackFS, "formulas/"+file)
	if err != nil {
		t.Fatalf("reading formulas/%s: %v", file, err)
	}
	var parsed formulaFile
	if _, err := toml.Decode(string(data), &parsed); err != nil {
		t.Fatalf("decoding formulas/%s: %v", file, err)
	}
	return parsed
}

// formulaStep returns the description of the named step, failing the test when
// the step is absent.
func formulaStep(t *testing.T, f formulaFile, id string) string {
	t.Helper()
	for _, step := range f.Steps {
		if step.ID == id {
			return step.Description
		}
	}
	t.Fatalf("formula %s has no step %q", f.Formula, id)
	return ""
}

// TestMolDoWorkDrainClaimsCurrentContinuation pins the distinction between a
// process's immutable startup bead and the continuation that became ready
// while that process was running.
func TestMolDoWorkDrainClaimsCurrentContinuation(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-do-work.toml"), "drain")

	if !strings.Contains(step, "gc ready --json --limit=2") {
		t.Fatal("drain must resolve the current continuation from ready work")
	}
	if !strings.Contains(step, "--include-ephemeral") {
		t.Fatal("drain must include ephemeral rows; a wisp-tier continuation is invisible to bd ready without it")
	}
	if !strings.Contains(step, `--metadata-field "gc.root_bead_id=$ROOT_BEAD_ID"`) || !strings.Contains(step, `--metadata-field "gc.step_ref=mol-do-work.drain"`) {
		t.Fatal("drain must select the ready drain step from the current workflow root")
	}
	if strings.Contains(step, `DRAIN_BEAD_ID="${GC_BEAD_ID`) {
		t.Fatal("drain must not treat the immutable startup bead as the continuation bead")
	}
	if !strings.Contains(step, "if [ -z \"$ROOT_BEAD_ID\" ]") {
		t.Fatal("drain must fail closed when the startup bead has no workflow root")
	}
	if !strings.Contains(step, "if length == 1") || !strings.Contains(step, "could not resolve one ready continuation bead") {
		t.Fatal("drain must fail closed unless exactly one matching continuation is ready")
	}
	// The current-claim branch must stay gated on gc.step_ref. An unguarded
	// `gc hook current` re-introduces gh-5141 on the deferred path, where the
	// claim stamp still names the do-work step rather than the drain step.
	currentAt := strings.Index(step, "gc hook current --id-only")
	if currentAt < 0 {
		t.Fatal("drain must consult the current claim before falling back to a ready query")
	}
	readyAt := strings.Index(step, "gc ready --json --limit=2")
	if readyAt < currentAt {
		t.Fatal("drain must consult the current claim before the ready query, not after it")
	}
	if !strings.Contains(step[currentAt:readyAt], `.metadata["gc.step_ref"] == "mol-do-work.drain"`) {
		t.Fatal("drain must gate the current-claim branch on gc.step_ref; an unguarded claim restores the stale-identity bug")
	}
	// The startup-bead id is only needed by the ready-query fallback. Requiring
	// it up front aborts drain on any seat that is not demand-spawned:
	// GC_BEAD_ID exists only in the dispatch condition environment, never in a
	// session shell, and GC_TRIGGER_BEAD_ID is pool-seat-only (see
	// cmd/gc/cmd_hook_current.go). Same shape as ga-2q2r0.
	startupAt := strings.Index(step, `STARTUP_BEAD_ID="${GC_BEAD_ID`)
	if startupAt < 0 || startupAt < currentAt {
		t.Fatal("drain must not require a startup bead id before consulting the current claim")
	}
	// The current claim carries gc.root_bead_id, and on a warm seat it is the
	// ONLY source of it: GC_BEAD_ID never reaches a session shell and
	// GC_TRIGGER_BEAD_ID is demand-spawn-only (cmd/gc/cmd_hook_current.go).
	// Reading the root off the startup bead first strands every warm seat on
	// the deferred path — the ga-2q2r0 failure, one layer in.
	rootFromCurrent := strings.Index(step, `ROOT_BEAD_ID=$(printf '%s' "$CURRENT"`)
	if rootFromCurrent < 0 {
		t.Fatal("drain must derive the workflow root from the current claim it already fetched")
	}
	if rootFromCurrent > startupAt {
		t.Fatal("drain must try the current claim's root before falling back to startup env vars")
	}

	updateAt := strings.Index(step, "gc bd update")
	drainAckAt := strings.Index(step, "gc runtime drain-ack")
	if drainAckAt < 0 {
		t.Fatal("drain must still acknowledge runtime drain after closing its continuation bead")
	}
	if updateAt < 0 || !strings.Contains(step[updateAt:drainAckAt], "|| exit 1") {
		t.Fatal("drain must not acknowledge runtime drain after a failed bead close")
	}
}

// TestPolecatPreflightSearchesLedgerBeforeFiling pins the search-before-file
// contract in the polecat preflight step.
//
// Concurrent polecats all run a baseline against the same base branch, so they
// all observe the same pre-existing failure within seconds of each other. With
// no ledger search in front of the create, each one files its own bug: five
// duplicates inside three minutes from three polecats, and one duplicate that
// sat ready in the pool after its original had already merged, one sling away
// from dispatching a polecat to redo merged work.
func TestPolecatPreflightSearchesLedgerBeforeFiling(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-base.toml"), "preflight-tests")

	searchAt := strings.Index(step, "gc bd list --title-contains")
	if searchAt < 0 {
		t.Fatal("preflight-tests must search the ledger with `gc bd list --title-contains` before filing a pre-existing-failure bead")
	}
	createAt := strings.Index(step, "gc bd create")
	if createAt < 0 {
		t.Fatal("preflight-tests must still describe how to file a pre-existing-failure bead")
	}
	if searchAt > createAt {
		t.Error("preflight-tests searches the ledger after `gc bd create`; the search must gate the create, not follow it")
	}

	// The original report is usually already claimed by the polecat or refinery
	// fixing it, so an open-only filter misses the very bead it should match.
	searchCmd := step[searchAt:createAt]
	if !strings.Contains(searchCmd, "in_progress") {
		t.Error("the dedupe lookup must include --status in_progress; the existing bead is frequently already claimed")
	}

	// A lookup that errors is not an all-clear. The refinery's earlier attempt at
	// this check invoked a flag that does not exist (`gc bd list --search`), so it
	// errored every run and the "no duplicate found" branch filed anyway.
	if !strings.Contains(step, "LOOKUP_RC") || !strings.Contains(step, `"$LOOKUP_RC" -ne 0`) {
		t.Error("the dedupe lookup must capture its exit status in LOOKUP_RC and fail closed on a non-zero result")
	}

	// Round-trip matchability: agents write different prose for one defect, so the
	// filed title has to carry the same stable key the next agent searches for.
	if !strings.Contains(searchCmd, `--title-contains "$SYMPTOM_KEY"`) {
		t.Error("the dedupe lookup must search by the stable $SYMPTOM_KEY, not by free-text description")
	}
	if !strings.Contains(step[createAt:], "$SYMPTOM_KEY") {
		t.Error("the filed title must embed $SYMPTOM_KEY verbatim so the next polecat's lookup matches it")
	}
}

// TestPolecatPreflightKeysOnTestFunctionNotSubtest pins the two widenings that
// an exact-title match alone does not deliver.
//
// Go subtests make the reported name unstable across agents: two concurrent
// polecats often fail different subtests of the same function
// (`TestX/clean_config_with_residual_files_is_a_conflict` versus
// `TestX/peer_successor_cross-device_tree`), search different strings, and both
// file. Stripping at the first `/` keys them together.
//
// Sibling functions in one package are the second gap: three different
// `TestDisableAndPurge*` functions can share a single root cause (for example
// an environment leak), and no exact-name key groups them. Eight
// productmetrics beads landed in ~38h that way.
func TestPolecatPreflightKeysOnTestFunctionNotSubtest(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-base.toml"), "preflight-tests")

	if !strings.Contains(step, "%%/*") {
		t.Error("the symptom key must strip the Go subtest suffix (${NAME%%/*}) so sibling subtests of one function share a key")
	}

	familyAt := strings.Index(step, `--title-contains "$SYMPTOM_FAMILY"`)
	if familyAt < 0 {
		t.Error("preflight-tests must widen to a $SYMPTOM_FAMILY lookup; sibling tests in one package usually share one root cause")
	}
	createAt := strings.Index(step, "gc bd create")
	if createAt >= 0 && familyAt > createAt {
		t.Error("the family lookup must run before `gc bd create`, not after it")
	}

	// The family branch is a judgement call, so nothing auto-assigns the match —
	// but the step must still tell the agent how to hand a family hit to 3c, or
	// 3c's `gc bd comment "$EXISTING"` runs with an empty id.
	if !strings.Contains(step, `EXISTING="<the bead id you judged to be the same defect>"`) {
		t.Error("3b2 must show how to set $EXISTING for a family match; 3c cannot comment without it")
	}
	if !strings.Contains(step, "FAMILY_RC") {
		t.Error("the family lookup must capture its exit status; a failed lookup is not an all-clear")
	}
}

// TestPolecatPreflightChecksRecentlyClosedBeforeFiling pins the staleness gate.
//
// Re-filing a defect that already merged puts a ready bead in the pool, and the
// next sling dispatches a polecat to redo merged work.
func TestPolecatPreflightChecksRecentlyClosedBeforeFiling(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-base.toml"), "preflight-tests")

	closedAt := strings.Index(step, "--closed-after")
	if closedAt < 0 {
		t.Fatal("preflight-tests must check recently-closed beads before filing; a stale baseline otherwise re-files merged work")
	}
	createAt := strings.Index(step, "gc bd create")
	if createAt >= 0 && closedAt > createAt {
		t.Error("the recently-closed check must run before `gc bd create`, not after it")
	}
	if !strings.Contains(step, "CLOSED_RC") {
		t.Error("the recently-closed lookup must capture its exit status; a failed lookup is not proof the fix has not landed")
	}
}

// TestPolecatSelfReviewDefersToPreflightDedupeProtocol keeps the second
// pre-existing-failure filing path pointed at the one protocol.
//
// self-review also tells the polecat to file a bead when a failure turns out to
// be pre-existing. Restating the protocol there would let the two copies drift;
// referring to preflight-tests keeps one definition.
func TestPolecatSelfReviewDefersToPreflightDedupeProtocol(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-base.toml"), "self-review")

	if !strings.Contains(step, "preflight-tests") {
		t.Error("self-review's pre-existing-failure path must point at the preflight-tests search-first protocol instead of filing directly")
	}
	if strings.Contains(step, "gc bd create") {
		t.Error("self-review must not carry its own `gc bd create` for pre-existing failures; that bypasses the dedupe protocol")
	}
}

// TestCoreShippedAssetsAvoidNonexistentBDListSearchFlag guards the failure mode
// that made the sibling fix a no-op: `gc bd list` has no `--search` flag, so a
// dedupe lookup written against it exits 1 and returns nothing, which reads as
// "no duplicate exists" to the branch that follows. Search by --title-contains.
func TestCoreShippedAssetsAvoidNonexistentBDListSearchFlag(t *testing.T) {
	err := fs.WalkDir(PackFS, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(PackFS, path)
		if err != nil {
			return err
		}
		body := string(data)
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, "bd list") && strings.Contains(line, "--search") {
				t.Errorf("%s: `bd list --search` is not a real flag and exits 1; use --title-contains: %s", path, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded core pack: %v", err)
	}
}

// TestMolPolecatCommitResolvesRepoBeforeRemovingWorktree pins the fix for a
// stranded-worktree bug: `git worktree remove` resolves the repo from cwd,
// and this step `cd`s away from the worktree before removing it, so the bare
// form exits 128 having unregistered nothing while `rm -rf` deletes the
// directory anyway, leaving the registration behind forever. These are
// bootstrap templates, so every city seeded by `gc city init` inherited the
// defect (ga-x1u5cr; contributing cause of ga-lc9yx's 396 dead worktrees).
func TestMolPolecatCommitResolvesRepoBeforeRemovingWorktree(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-commit.toml"), "commit-and-push")

	if strings.Contains(step, `git worktree remove "$WORKTREE_PATH" --force`) {
		t.Error("commit-and-push calls bare `git worktree remove` after `cd ..`; resolve the repo via --git-common-dir first and remove via `git -C \"$REPO\" worktree remove`")
	}
	if !strings.Contains(step, "--git-common-dir") {
		t.Error("commit-and-push must resolve REPO via `git rev-parse --path-format=absolute --git-common-dir` before `cd ..`, so worktree removal does not depend on cwd")
	}
	if !strings.Contains(step, `git -C "$REPO" worktree remove`) {
		t.Error(`commit-and-push must remove the worktree via git -C "$REPO" worktree remove, not a bare invocation`)
	}

	// `git -C ""` is a no-op that silently resolves the repo from cwd, and this
	// step has already `cd ..`'d away from the worktree by then. An unresolved
	// REPO must short-circuit rather than degrade back to cwd-dependent removal.
	if !strings.Contains(step, `[ -z "$REPO" ]`) {
		t.Error(`commit-and-push must bail on an empty $REPO; git -C "" silently resolves from cwd, which is exactly the bug this step fixes`)
	}

	// WORKTREE_PATH is $(pwd), and `git worktree remove` exits 128 on a main
	// working tree. Without the guard the failure path rm -rf's the whole repo.
	guardAt := strings.Index(step, `[ -f "$WORKTREE_PATH/.git" ]`)
	if guardAt < 0 {
		t.Fatal(`commit-and-push must guard the rm -rf fallback with [ -f "$WORKTREE_PATH/.git" ]; a linked worktree's .git is a file, a main checkout's is a directory`)
	}
	// Match the delete command itself, not the word: the surrounding comment and
	// the refusal message both mention `rm -rf` and would otherwise be found first.
	if got := strings.Count(step, `rm -rf "$WORKTREE_PATH"`); got != 1 {
		t.Fatalf(`commit-and-push must delete the worktree exactly once behind the guard; found %d occurrences of rm -rf "$WORKTREE_PATH"`, got)
	}
	removeAt := strings.Index(step, `rm -rf "$WORKTREE_PATH"`)
	if guardAt > removeAt {
		t.Error("commit-and-push runs rm -rf before the linked-worktree check; the check must gate the delete, not follow it")
	}
}

// TestCoreRetryManagedStepsStateOutcomeContract requires every core-pack step
// that declares [steps.retry] to tell the worker how to close it: the retry
// evaluator classifies an attempt closed without gc.outcome as missing_outcome
// and, once attempts run out, fails the workflow even though the work succeeded.
func TestCoreRetryManagedStepsStateOutcomeContract(t *testing.T) {
	entries, err := fs.ReadDir(PackFS, "formulas")
	if err != nil {
		t.Fatalf("reading formulas: %v", err)
	}
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}
		f := readFormula(t, entry.Name())
		for _, step := range f.Steps {
			if step.Retry == nil {
				continue
			}
			checked++
			t.Run(entry.Name()+"/"+step.ID, func(t *testing.T) {
				for _, want := range []string{"gc.outcome=pass", "gc.outcome=fail", "gc.failure_class", "gc.failure_reason"} {
					if !strings.Contains(step.Description, want) {
						t.Errorf("retry-managed step %s/%s never mentions %q; a close without gc.outcome fails the step as missing_outcome", entry.Name(), step.ID, want)
					}
				}
			})
		}
	}
	if checked == 0 {
		t.Fatal("found no [steps.retry] step in the core formulas; the scan is not looking where the retry tables are")
	}
}

// TestMolScopedWorkResolvesRepoBeforeRemovingWorktree pins the same fix for
// mol-scoped-work's cleanup step, which is worse than mol-polecat-commit's:
// its `|| rm -rf` fallback makes the stranded git registration the designed
// outcome of the bare form's failure path, not just an incidental risk.
func TestMolScopedWorkResolvesRepoBeforeRemovingWorktree(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-scoped-work.toml"), "cleanup-worktree")

	if strings.Contains(step, `git worktree remove --force "$WORKTREE" || rm -rf "$WORKTREE"`) {
		t.Error("cleanup-worktree calls bare `git worktree remove --force ... || rm -rf`; resolve the repo via --git-common-dir first and remove via `git -C \"$REPO\" worktree remove`")
	}
	if !strings.Contains(step, "--git-common-dir") {
		t.Error(`cleanup-worktree must resolve REPO via git -C "$WORKTREE" rev-parse --path-format=absolute --git-common-dir; cwd is not guaranteed inside the repo at this step`)
	}
	if !strings.Contains(step, `git -C "$REPO" worktree remove`) {
		t.Error(`cleanup-worktree must remove the worktree via git -C "$REPO" worktree remove, not a bare invocation`)
	}

	// A stale directory that still passes [ -d ], or a git too old for
	// --path-format, leaves REPO empty; `git -C ""` then resolves from a cwd
	// this step explicitly does not guarantee is inside the repo.
	if !strings.Contains(step, `[ -z "$REPO" ]`) {
		t.Error(`cleanup-worktree must bail on an empty $REPO; git -C "" silently resolves from cwd, which this step cannot assume`)
	}

	guardAt := strings.Index(step, `[ -f "$WORKTREE/.git" ]`)
	if guardAt < 0 {
		t.Fatal(`cleanup-worktree must guard the rm -rf fallback with [ -f "$WORKTREE/.git" ]; a linked worktree's .git is a file, a main checkout's is a directory`)
	}
	if got := strings.Count(step, `rm -rf "$WORKTREE"`); got != 1 {
		t.Fatalf(`cleanup-worktree must delete the worktree exactly once behind the guard; found %d occurrences of rm -rf "$WORKTREE"`, got)
	}
	removeAt := strings.Index(step, `rm -rf "$WORKTREE"`)
	if guardAt > removeAt {
		t.Error("cleanup-worktree runs rm -rf before the linked-worktree check; the check must gate the delete, not follow it")
	}
}

// TestMolDoWorkWorkBeadCloseFailsClosed pins that a refused work-bead close
// (for example, the typed work-record close gate) stops the step instead of
// falling through to close the formula step over still-open work.
func TestMolDoWorkWorkBeadCloseFailsClosed(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-do-work.toml"), "do-work")
	closes := 0
	for _, line := range strings.Split(step, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `gc bd close "$WORK_BEAD_ID"`) {
			continue
		}
		closes++
		if !strings.HasSuffix(line, "|| exit 1") {
			t.Errorf("work-bead close must fail closed with `|| exit 1`: %s", line)
		}
	}
	if closes == 0 {
		t.Fatal("expected mol-do-work to close $WORK_BEAD_ID in the do-work step")
	}
}

// molDoWorkCloseStepBlock returns the bash block under the do-work step's
// "Close this formula step" heading, with the convoy variable rendered as cv-1.
func molDoWorkCloseStepBlock(t *testing.T) string {
	t.Helper()
	step := formulaStep(t, readFormula(t, "mol-do-work.toml"), "do-work")
	const marker = "**4. Close this formula step:**"
	at := strings.Index(step, marker)
	if at < 0 {
		t.Fatalf("do-work step has no %q section", marker)
	}
	section := step[at:]
	open := strings.Index(section, "```bash\n")
	if open < 0 {
		t.Fatal("close-step section has no bash block")
	}
	body := section[open+len("```bash\n"):]
	end := strings.Index(body, "```")
	if end < 0 {
		t.Fatal("close-step bash block is unterminated")
	}
	return strings.ReplaceAll(body[:end], "{{convoy_id}}", "cv-1")
}

// closeStepCase is one session shell the do-work close block runs in. The fake
// gc answers `hook current --id-only` with hookOut/hookRC, `convoy status` with
// convoy and `bd show` with show (an empty convoy or show fails the read), and
// exits updateRC for `bd update`; like gc, each failure reports on stderr. A refused case's diagnostic must name
// wantNamed.
type closeStepCase struct {
	name      string
	env       []string
	hookOut   string
	hookRC    int
	convoy    string
	show      string
	updateRC  int
	wantClose bool
	wantNamed string
}

const (
	closeStepConvoy  = `{"children":[{"id":"ga-work"}]}`
	closeStepClaimed = `[{"id":"ga-step","status":"in_progress","assignee":"gcg-session-me","metadata":{"gc.step_ref":"mol-do-work.do-work"}}]`
)

// closeStepEnvKeys are the settings each case supplies itself. A live session
// exports the identity variables, and a stale GC_BEAD_ID or a leaked block
// variable would otherwise decide the case.
var closeStepEnvKeys = map[string]struct{}{
	"GC_BEAD_ID":         {},
	"GC_TRIGGER_BEAD_ID": {},
	"GC_SESSION_ID":      {},
	"GC_SESSION_NAME":    {},
	"GC_ALIAS":           {},
	"WORK_BEAD_ID":       {},
	"STEP_BEAD_ID":       {},
	"CONVOY_STATUS":      {},
	"STEP_BEAD":          {},
}

func (c closeStepCase) fakeGC() string {
	convoy := "echo 'gc: convoy status failed' >&2; exit 1"
	if c.convoy != "" {
		convoy = fmt.Sprintf("printf '%%s\\n' '%s'", c.convoy)
	}
	show := "echo 'gc: bd show failed' >&2; exit 1"
	if c.show != "" {
		show = fmt.Sprintf("printf '%%s\\n' '%s'", c.show)
	}
	return fmt.Sprintf(`case "$*" in
"hook current --id-only") printf '%%s\n' '%s'; exit %d ;;
"convoy status cv-1 --json") %s ;;
"bd show "*" --json") %s ;;
"bd update "*) [ %[5]d -eq 0 ] || echo 'gc: bd update failed' >&2; exit %[5]d ;;
*) echo "unexpected gc command: $*" >&2; exit 64 ;;
esac
`, c.hookOut, c.hookRC, convoy, show, c.updateRC)
}

// TestMolDoWorkClosesOnlyItsVerifiedClaimedStep runs the do-work step's close
// block in a session shell, where GC_BEAD_ID is never exported (issue #7991).
// The block must close the step `gc hook current --id-only` names once it has
// verified that step is this session's in-progress do-work step and not the
// work bead, and must exit non-zero, closing nothing, whenever it cannot.
func TestMolDoWorkClosesOnlyItsVerifiedClaimedStep(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	block := molDoWorkCloseStepBlock(t)
	scriptPath := filepath.Join(t.TempDir(), "close-step.sh")
	if err := os.WriteFile(scriptPath, []byte(block), 0o644); err != nil {
		t.Fatalf("write close-step block: %v", err)
	}
	good := closeStepCase{hookOut: "ga-step", convoy: closeStepConvoy, show: closeStepClaimed, wantClose: true}
	with := func(name string, edit func(*closeStepCase)) closeStepCase {
		c := good
		c.name = name
		edit(&c)
		return c
	}
	claimed := func(fields string) string {
		return `[{"id":"ga-step",` + fields + `}]`
	}
	cases := []closeStepCase{
		with("AC-1 assignee is the session id", func(*closeStepCase) {}),
		with("AC-1 assignee is the session name", func(c *closeStepCase) {
			c.show = claimed(`"status":"in_progress","assignee":"rig--polecat-1","metadata":{"gc.step_ref":"mol-do-work.do-work"}`)
		}),
		with("AC-1 assignee is the alias", func(c *closeStepCase) {
			c.env = []string{"GC_ALIAS=rig/polecat"}
			c.show = claimed(`"status":"in_progress","assignee":"rig/polecat","metadata":{"gc.step_ref":"mol-do-work.do-work"}`)
		}),
		with("AC-1 GC_BEAD_ID names another bead", func(c *closeStepCase) {
			c.env = []string{"GC_BEAD_ID=ga-other"}
		}),
		with("AC-1 bd show prints an object", func(c *closeStepCase) {
			c.show = strings.TrimSuffix(strings.TrimPrefix(closeStepClaimed, "["), "]")
		}),
		with("AC-2 hook current fails", func(c *closeStepCase) {
			c.hookOut, c.hookRC, c.wantClose = "", 1, false
		}),
		with("AC-2 hook current prints nothing", func(c *closeStepCase) {
			c.hookOut, c.wantClose = "", false
		}),
		with("AC-2 hook current prints nothing while GC_BEAD_ID is set", func(c *closeStepCase) {
			c.hookOut, c.wantClose = "", false
			c.env = []string{"GC_BEAD_ID=ga-step"}
		}),
		with("AC-3 step is closed", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"closed","assignee":"gcg-session-me","metadata":{"gc.step_ref":"mol-do-work.do-work"}`), false, "ga-step"
		}),
		with("AC-3 step is open", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"open","assignee":"gcg-session-me","metadata":{"gc.step_ref":"mol-do-work.do-work"}`), false, "ga-step"
		}),
		with("AC-3 step belongs to another session", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"in_progress","assignee":"gcg-session-other","metadata":{"gc.step_ref":"mol-do-work.do-work"}`), false, "ga-step"
		}),
		with("AC-3 step is unassigned and no alias is set", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"in_progress","assignee":"","metadata":{"gc.step_ref":"mol-do-work.do-work"}`), false, "ga-step"
		}),
		with("AC-3 step is the drain step", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"in_progress","assignee":"gcg-session-me","metadata":{"gc.step_ref":"mol-do-work.drain"}`), false, "ga-step"
		}),
		with("AC-3 step has no step ref", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"in_progress","assignee":"gcg-session-me","metadata":{}`), false, "ga-step"
		}),
		with("AC-3 step ref only ends in the do-work ref", func(c *closeStepCase) {
			c.show, c.wantClose, c.wantNamed = claimed(`"status":"in_progress","assignee":"gcg-session-me","metadata":{"gc.step_ref":"x.mol-do-work.do-work"}`), false, "ga-step"
		}),
		with("AC-3 hook names the work bead", func(c *closeStepCase) {
			c.hookOut, c.wantClose, c.wantNamed = "ga-work", false, "ga-work"
			c.show = `[{"id":"ga-work","status":"in_progress","assignee":"gcg-session-me","metadata":{"gc.step_ref":"mol-do-work.do-work"}}]`
		}),
		with("AC-4 convoy read fails", func(c *closeStepCase) {
			c.convoy, c.wantClose = "", false
		}),
		with("AC-4 convoy has two children", func(c *closeStepCase) {
			c.convoy, c.wantClose, c.wantNamed = `{"children":[{"id":"ga-work"},{"id":"ga-more"}]}`, false, "cv-1"
		}),
		with("AC-4 convoy has no children", func(c *closeStepCase) {
			c.convoy, c.wantClose, c.wantNamed = `{"children":[]}`, false, "cv-1"
		}),
		with("AC-4 bd show fails", func(c *closeStepCase) {
			c.show, c.wantClose = "", false
		}),
		with("AC-4 closing update fails", func(c *closeStepCase) {
			c.updateRC, c.wantClose = 1, false
		}),
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir, logPath := fakeGCBin(t, tc.fakeGC())
			if err := os.WriteFile(logPath, nil, 0o644); err != nil {
				t.Fatalf("create gc log: %v", err)
			}
			env := append([]string{"GC_SESSION_ID=gcg-session-me", "GC_SESSION_NAME=rig--polecat-1"}, tc.env...)
			out, runErr := runPackScript(t, scriptPath, binDir, closeStepEnvKeys, env)
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read gc log: %v", err)
			}
			logged := string(data)
			var updates []string
			for _, line := range strings.Split(logged, "\n") {
				if strings.HasPrefix(line, "gc bd update ") {
					updates = append(updates, line)
				}
			}
			if strings.Contains(logged, "ga-other") {
				t.Errorf("block named the GC_BEAD_ID bead ga-other:\n%s", logged)
			}
			if tc.wantClose {
				if runErr != nil {
					t.Fatalf("block failed: %v\noutput:\n%s\ngc calls:\n%s", runErr, out, logged)
				}
				want := []string{"gc bd update ga-step --set-metadata gc.outcome=pass --status=closed"}
				if strings.Join(updates, "\n") != strings.Join(want, "\n") {
					t.Fatalf("updates = %q, want %q\noutput:\n%s", updates, want, out)
				}
				return
			}
			if runErr == nil {
				t.Fatalf("block exited 0 without a verified claimed step\noutput:\n%s\ngc calls:\n%s", out, logged)
			}
			if tc.updateRC == 0 && len(updates) != 0 {
				t.Fatalf("block issued updates %q without a verified claimed step\noutput:\n%s", updates, out)
			}
			if strings.TrimSpace(out) == "" {
				t.Fatal("block failed without a diagnostic")
			}
			if !strings.Contains(out, tc.wantNamed) {
				t.Fatalf("diagnostic does not name %s:\n%s", tc.wantNamed, out)
			}
		})
	}
}

// TestMolDoWorkCloseStepNeverReadsGCBeadID pins that the do-work step names
// its own bead through the hook claim: GC_BEAD_ID never reaches a session
// shell, so step text that waits on it presents a skipped close as success.
// The close block must also carry no backslash, which the TOML basic string
// would decode before the agent sees it.
func TestMolDoWorkCloseStepNeverReadsGCBeadID(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-do-work.toml"), "do-work")
	if strings.Contains(step, "GC_BEAD_ID") {
		t.Error("do-work step references GC_BEAD_ID, which a session shell never has")
	}
	if !strings.Contains(step, "gc hook current --id-only") {
		t.Error("do-work step must resolve its own bead with `gc hook current --id-only`")
	}
	if strings.Contains(step, "closed when `GC_BEAD_ID` is available") {
		t.Error("do-work exit criteria still make the step close conditional on GC_BEAD_ID")
	}
	if block := molDoWorkCloseStepBlock(t); strings.Contains(block, `\`) {
		t.Errorf("close-step block contains a backslash:\n%s", block)
	}
}

// graphBeadVars are the shell variables core formulas use for workflow
// infrastructure beads: the claimed formula step, the drain continuation and
// the workflow root. Every one carries gc.root_bead_id, so on a city that
// keeps its coordination classes in a separate store each is held by that
// store and `gc bd` serves writes to it in process.
var graphBeadVars = []string{"GC_BEAD_ID", "DRAIN_BEAD_ID", "ROOT_BEAD_ID", "STEP_BEAD_ID"}

// graphBeadRefusedShapes are the spellings the in-process door refuses on such
// a bead, because the class store has nothing to write them into (notes, a
// heartbeat lease, an audit actor). A close reason is not among them: the door
// records `close --reason` as the bead's close reason. A core formula that uses
// one of these breaks on gc's own split store.
var graphBeadRefusedShapes = []string{"--notes", "--append-notes", "--actor", "heartbeat"}

// TestCoreFormulasUseNoRefusedShapeOnGraphBeads scans every core formula for a
// `bd` write naming a graph-bead variable and fails on a refused shape, or on a
// bare `bd` (which cannot see a relocated bead at all). Continuation lines are
// joined first so a flag on the next line of a `\`-continued command counts.
func TestCoreFormulasUseNoRefusedShapeOnGraphBeads(t *testing.T) {
	entries, err := fs.ReadDir(PackFS, "formulas")
	if err != nil {
		t.Fatalf("reading formulas: %v", err)
	}
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}
		data, err := fs.ReadFile(PackFS, "formulas/"+entry.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}
		joined := strings.ReplaceAll(string(data), "\\\n", " ")
		for _, line := range strings.Split(joined, "\n") {
			bd := strings.Index(line, "bd ")
			if bd < 0 || !namesGraphBeadVar(line[bd:]) {
				continue
			}
			command := line[bd:]
			verb := strings.Fields(command)
			if len(verb) < 2 || (verb[1] != "update" && verb[1] != "close" && verb[1] != "heartbeat") {
				continue
			}
			checked++
			if !strings.HasSuffix(strings.TrimRight(line[:bd], " `"), "gc") {
				t.Errorf("%s: bare `bd` on a graph bead cannot reach a relocated class store; use `gc bd`: %s", entry.Name(), strings.TrimSpace(line))
			}
			for _, refused := range graphBeadRefusedShapes {
				if strings.Contains(command, refused) {
					t.Errorf("%s: %s is refused on a relocated graph bead: %s", entry.Name(), refused, strings.TrimSpace(line))
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no graph-bead write in the core formulas; the scan is not looking where the writes are")
	}
}

func namesGraphBeadVar(command string) bool {
	for _, name := range graphBeadVars {
		if strings.Contains(command, "$"+name) || strings.Contains(command, "${"+name) {
			return true
		}
	}
	return false
}
