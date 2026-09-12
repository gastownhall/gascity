package core

import (
	"io/fs"
	"strings"
	"sync/atomic"
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

// TestMolDoWorkUsesOneExplicitClaimantAndCloseContract keeps the lightweight
// workflow from splitting ownership across bd's ambient actor fallbacks. The
// target must be claimed before heartbeat, and every terminal path has to keep
// its metadata write coupled to a reasoned bd close rather than bypassing the
// close contract with a raw status transition.
func TestMolDoWorkUsesOneExplicitClaimantAndCloseContract(t *testing.T) {
	formula := readFormula(t, "mol-do-work.toml")
	doWork := formulaStep(t, formula, "do-work")
	drain := formulaStep(t, formula, "drain")
	body := doWork + "\n" + drain

	claimActor := `CLAIM_ACTOR="${GC_AGENT:-${GC_SESSION_ID:-${GC_SESSION_NAME:-}}}"`
	claim := `gc bd --actor "$CLAIM_ACTOR" update "$WORK_BEAD_ID" --claim`
	heartbeat := `gc bd --actor "$CLAIM_ACTOR" heartbeat "$WORK_BEAD_ID"`
	if !strings.Contains(doWork, claimActor) {
		t.Fatal("mol-do-work must prefer the hook's durable agent identity for its explicit claimant")
	}
	claimAt := strings.Index(doWork, claim)
	heartbeatAt := strings.Index(doWork, heartbeat)
	if claimAt < 0 || heartbeatAt < 0 || claimAt > heartbeatAt {
		t.Fatalf("mol-do-work must claim with its explicit actor before heartbeat:\n%s", doWork)
	}
	if strings.Contains(body, "gc bd heartbeat") {
		t.Fatalf("mol-do-work must not let an ambient actor heartbeat a claimed bead:\n%s", body)
	}

	// The instructions execute as a sequence, not a transaction. Requiring the
	// claimant on every bead mutation makes a duplicate/recycled formula runner
	// fail its ownership check instead of reviving or closing another session's
	// work with the ambient actor fallback.
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		mutatesBead := strings.Contains(line, " update ") || strings.Contains(line, " heartbeat ") || strings.Contains(line, " close ")
		if strings.HasPrefix(line, "gc bd ") && mutatesBead && !strings.HasPrefix(line, `gc bd --actor "$CLAIM_ACTOR" `) {
			t.Errorf("mol-do-work bead command must use the durable claimant: %s", line)
		}
	}

	if strings.Contains(body, "--status=closed") {
		t.Fatalf("mol-do-work must not bypass bd close with --status=closed:\n%s", body)
	}
	for _, want := range []string{
		`gc bd --actor "$CLAIM_ACTOR" close "$WORK_BEAD_ID" --reason`,
		`gc bd --actor "$CLAIM_ACTOR" close "$GC_BEAD_ID" --reason`,
		`gc bd --actor "$CLAIM_ACTOR" close "$DRAIN_BEAD_ID" --reason`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mol-do-work missing explicit reasoned close %q", want)
		}
	}
	closeCount := strings.Count(body, `gc bd --actor "$CLAIM_ACTOR" close `)
	if strings.Count(body, "--notes") != closeCount {
		t.Errorf("every mol-do-work close must be coupled to its metadata/notes write; notes=%d closes=%d", strings.Count(body, "--notes"), closeCount)
	}
}

// TestMolDoWorkTargetClaimHeartbeatProtocolHasSingleRaceWinner exercises the
// target lifecycle that the formula tells an agent to run. The static contract
// above pins command spelling; this regression pins the important execution
// consequence: two formula runners racing for one open target produce one
// owner, only that owner proceeds to heartbeat, and the loser stops at the
// claim conflict instead of heartbeating work it does not own.
func TestMolDoWorkTargetClaimHeartbeatProtocolHasSingleRaceWinner(t *testing.T) {
	doWork := formulaStep(t, readFormula(t, "mol-do-work.toml"), "do-work")
	claim := `gc bd --actor "$CLAIM_ACTOR" update "$WORK_BEAD_ID" --claim`
	heartbeat := `gc bd --actor "$CLAIM_ACTOR" heartbeat "$WORK_BEAD_ID"`
	protocol := fencedBashBlockContaining(t, doWork, claim)

	claimAt := strings.Index(protocol, claim)
	heartbeatAt := strings.Index(protocol, heartbeat)
	if !strings.Contains(protocol, "if ! "+claim+"; then") {
		t.Fatalf("mol-do-work target claim must have an explicit conflict branch:\n%s", protocol)
	}
	if heartbeatAt < 0 || claimAt > heartbeatAt {
		t.Fatalf("mol-do-work target heartbeat must follow claim in the same executable block:\n%s", protocol)
	}
	claimConflictPath := protocol[claimAt:heartbeatAt]
	if !strings.Contains(claimConflictPath, "exit 1") {
		t.Fatalf("mol-do-work must stop before heartbeat when target claim loses a race:\n%s", protocol)
	}
	if !strings.Contains(protocol, "if ! "+heartbeat+"; then") || !strings.Contains(protocol[heartbeatAt:], "exit 1") {
		t.Fatalf("mol-do-work must stop when the winning actor's initial heartbeat is rejected:\n%s", protocol)
	}

	type attempt struct {
		actor              string
		claimed            bool
		heartbeatAttempted bool
		heartbeatAccepted  bool
	}
	// Model bd's atomic owner transition at the formula boundary. The bd store
	// itself owns claim concurrency; this test owns whether the prompt proceeds
	// to heartbeat after the winner/conflict result.
	var owner atomic.Pointer[string]
	var acceptedHeartbeats atomic.Int32
	start := make(chan struct{})
	results := make(chan attempt, 2)
	for _, actor := range []string{"rig/worker-a", "rig/worker-b"} {
		go func() {
			<-start
			claimant := new(string)
			*claimant = actor
			claimed := owner.CompareAndSwap(nil, claimant)
			result := attempt{actor: actor, claimed: claimed}
			if !claimed {
				// This is the formula's explicit claim-conflict exit path: the
				// losing runner never reaches the heartbeat command.
				results <- result
				return
			}
			result.heartbeatAttempted = true
			result.heartbeatAccepted = owner.Load() == claimant
			if result.heartbeatAccepted {
				acceptedHeartbeats.Add(1)
			}
			results <- result
		}()
	}
	close(start)

	var winners, conflicts int
	for range 2 {
		result := <-results
		if result.claimed {
			winners++
			if !result.heartbeatAttempted || !result.heartbeatAccepted {
				t.Errorf("winning runner %q did not heartbeat as the target owner: %+v", result.actor, result)
			}
			continue
		}
		conflicts++
		if result.heartbeatAttempted {
			t.Errorf("conflicting runner %q reached heartbeat: %+v", result.actor, result)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("claim race = %d winners, %d conflicts; want exactly one of each", winners, conflicts)
	}
	if got := acceptedHeartbeats.Load(); got != 1 {
		t.Fatalf("claim race emitted %d accepted heartbeats, want exactly one from the winning actor", got)
	}
}

func fencedBashBlockContaining(t *testing.T, body, needle string) string {
	t.Helper()
	needleAt := strings.Index(body, needle)
	if needleAt < 0 {
		t.Fatalf("formula step is missing %q", needle)
	}
	start := strings.LastIndex(body[:needleAt], "```bash\n")
	if start < 0 {
		t.Fatalf("formula command %q is not in a bash block", needle)
	}
	start += len("```bash\n")
	endOffset := strings.Index(body[needleAt:], "\n```")
	if endOffset < 0 {
		t.Fatalf("formula bash block containing %q is not terminated", needle)
	}
	return body[start : needleAt+endOffset]
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
