package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workspaceGCShim answers the gc calls a workspace-setup step makes before it
// touches disk: the convoy reports one tracked child (ga-abc1), the bead
// carries no work_dir, and updates succeed. Anything else exits non-zero, so
// an unexpected gc call fails the row instead of passing silently.
const workspaceGCShim = `
case "$*" in
  'convoy status'*) printf '%s\n' '{"children":[{"id":"ga-abc1"}]}' ;;
  'bd show'*) printf '%s\n' '[{"metadata":{}}]' ;;
  'bd update'*) : ;;
  *) echo "unexpected gc call: gc $*" >&2; exit 64 ;;
esac
exit 0
`

// workspaceScriptEnvKeys are the inherited settings the assembled step must
// not see. GC_DIR is the variable under test; HOME and XDG_CONFIG_HOME decide
// which gitconfig git reads; the GIT_* keys would relocate the repository
// underneath the step's git calls.
var workspaceScriptEnvKeys = map[string]struct{}{
	"GC_DIR":          {},
	"HOME":            {},
	"XDG_CONFIG_HOME": {},
	"GIT_DIR":         {},
	"GIT_WORK_TREE":   {},
	"GIT_INDEX_FILE":  {},
	"GIT_COMMON_DIR":  {},
}

// stepBashBlocks returns the fenced ```bash blocks of a step description, in
// order. Workspace setup is prose plus blocks; only the blocks are runnable.
func stepBashBlocks(t *testing.T, step string) []string {
	t.Helper()
	var blocks []string
	var current []string
	inBlock := false
	for _, line := range strings.Split(step, "\n") {
		switch {
		case !inBlock && strings.HasPrefix(line, "```bash"):
			inBlock = true
			current = nil
		case inBlock && strings.HasPrefix(line, "```"):
			blocks = append(blocks, strings.Join(current, "\n")+"\n")
			current = nil
			inBlock = false
		case inBlock:
			current = append(current, line)
		}
	}
	if inBlock {
		t.Fatal("step description ends inside an open ```bash fence")
	}
	if len(blocks) == 0 {
		t.Fatal("step description contains no ```bash block")
	}
	return blocks
}

// assembleWorkspaceSetup renders the workspace-setup step of a core formula as
// one runnable script, substituting the template variables a molecule would
// supply. Blocks containing a marker in skipContaining are dropped:
// mol-polecat-commit's reuse branch (`git pull --rebase`) runs only when a
// work_dir is already recorded, which these tests never set up.
func assembleWorkspaceSetup(t *testing.T, file string, skipContaining ...string) string {
	t.Helper()
	var blocks []string
	for _, block := range stepBashBlocks(t, formulaStep(t, readFormula(t, file), "workspace-setup")) {
		dropped := false
		for _, marker := range skipContaining {
			if strings.Contains(block, marker) {
				dropped = true
				break
			}
		}
		if !dropped {
			blocks = append(blocks, block)
		}
	}
	script := strings.Join(blocks, "\n")
	script = strings.ReplaceAll(script, "{{convoy_id}}", "cv-1")
	script = strings.ReplaceAll(script, "{{base_branch}}", "main")
	script = strings.ReplaceAll(script, "{{setup_command}}", "")
	if unresolved := strings.Index(script, "{{"); unresolved >= 0 {
		t.Fatalf("%s workspace-setup renders an unresolved template variable: %q", file, script[unresolved:unresolved+30])
	}
	return script
}

// stageWorkspace builds the directory a workspace-setup step runs against: a
// bare origin with one commit on main (the commit carries a sub/ file so the
// rig clone has a tracked subdirectory to cd into), a rig clone of it, and a
// home that is either a plain directory or, when linkedHome, a linked worktree
// of the rig (the Gastown-style layout). The returned prelude leaves the shell
// cd'ed at startDir; the epilogue records where the step ended up and whether
// the rig checkout stayed clean.
func stageWorkspace(t *testing.T, startDir string, linkedHome bool) (stage, prelude, epilogue string) {
	t.Helper()
	stage = t.TempDir()
	homeSetup := "mkdir \"$STAGE/home\""
	if linkedHome {
		homeSetup = "git -C \"$STAGE/rig\" worktree add --detach \"$STAGE/home\" origin/main"
	}
	prelude = strings.Join([]string{
		"STAGE=" + stage,
		"set -eu",
		"git init --bare -b main \"$STAGE/origin.git\"",
		"git init -b main \"$STAGE/seed\"",
		"mkdir \"$STAGE/seed/sub\"",
		"echo seed > \"$STAGE/seed/sub/file\"",
		"cd \"$STAGE/seed\"",
		"git add .",
		"git commit -m \"seed origin/main\"",
		"git push \"$STAGE/origin.git\" HEAD:main",
		"git clone \"$STAGE/origin.git\" \"$STAGE/rig\"",
		homeSetup,
		"cd \"$STAGE/" + startDir + "\"",
	}, "\n")
	epilogue = strings.Join([]string{
		"pwd > \"$STAGE/final_cwd\"",
		"git -C \"$STAGE/rig\" status --porcelain --untracked-files=all > \"$STAGE/rig_status\"",
	}, "\n")
	return stage, prelude, epilogue
}

// runStagedWorkspaceSetup assembles the step against an already-built stage
// and runs it with the row's GC_DIR (nil leaves it unset), returning the
// combined output, the gc log path, and the run error.
func runStagedWorkspaceSetup(t *testing.T, stage, prelude, epilogue, file string, gcDir *string) (out, logPath string, err error) {
	t.Helper()
	binDir, logPath := fakeGCBin(t, workspaceGCShim)
	scriptPath := filepath.Join(stage, "step.sh")
	script := prelude + "\n" + assembleWorkspaceSetup(t, file, "git pull --rebase") + "\n" + epilogue
	if writeErr := os.WriteFile(scriptPath, []byte(script), 0o644); writeErr != nil {
		t.Fatalf("write step script: %v", writeErr)
	}
	env := []string{
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@gascity.invalid",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@gascity.invalid",
	}
	if gcDir != nil {
		env = append(env, "GC_DIR="+*gcDir)
	}
	out, err = runPackScript(t, scriptPath, binDir, workspaceScriptEnvKeys, env)
	return out, logPath, err
}

// TestCoreWorkspaceSetupPlacesWorktreeUnderGCDir pins issue #6014: a
// workspace-setup create branch must place the per-bead worktree under the
// session work dir ($GC_DIR), not under the process's current directory. The
// step's shell is long-lived (session affinity), and an earlier step or the
// prompt may have cd'd into the tracked rig checkout, so a path derived from
// the current directory plants an untracked worktrees/ tree inside it; the
// checkout then stays dirty and drift detection refuses to fast-forward it.
func TestCoreWorkspaceSetupPlacesWorktreeUnderGCDir(t *testing.T) {
	rows := []struct {
		name       string
		file       string
		startDir   string
		linkedHome bool
	}{
		{name: "mol-scoped-work from rig root", file: "mol-scoped-work.toml", startDir: "rig"},
		{name: "mol-scoped-work from rig subdirectory", file: "mol-scoped-work.toml", startDir: "rig/sub"},
		{name: "mol-polecat-commit from rig root", file: "mol-polecat-commit.toml", startDir: "rig"},
		{name: "mol-polecat-commit from rig subdirectory", file: "mol-polecat-commit.toml", startDir: "rig/sub"},
		// The Gastown-style layout that already works: the home is itself a
		// linked worktree of the rig and the session never left it, so the
		// anchored path and the old cwd-derived path coincide. Guards against
		// an anchor that breaks the one layout today's code serves.
		{name: "mol-scoped-work from its own worktree home", file: "mol-scoped-work.toml", startDir: "home", linkedHome: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			stage, prelude, epilogue := stageWorkspace(t, row.startDir, row.linkedHome)
			home := filepath.Join(stage, "home")
			gcDir := home
			out, logPath, err := runStagedWorkspaceSetup(t, stage, prelude, epilogue, row.file, &gcDir)
			if err != nil {
				t.Fatalf("workspace-setup failed: %v\n%s", err, out)
			}

			want := filepath.Join(home, "worktrees", "ga-abc1")
			if info, statErr := os.Stat(filepath.Join(want, ".git")); statErr != nil || info.IsDir() {
				t.Errorf("worktree missing at %s (a linked worktree's .git is a regular file): %v", want, statErr)
			}
			for _, stray := range []string{
				filepath.Join(stage, "rig", "worktrees"),
				filepath.Join(stage, "rig", "sub", "worktrees"),
			} {
				if _, statErr := os.Stat(stray); !os.IsNotExist(statErr) {
					t.Errorf("workspace-setup created a worktrees directory inside the tracked checkout at %s", stray)
				}
			}
			status, readErr := os.ReadFile(filepath.Join(stage, "rig_status"))
			if readErr != nil {
				t.Fatalf("read rig_status: %v", readErr)
			}
			if len(strings.TrimSpace(string(status))) != 0 {
				t.Errorf("rig checkout is dirty after workspace-setup:\n%s", status)
			}
			logged, readErr := os.ReadFile(logPath)
			if readErr != nil {
				t.Fatalf("read gc log: %v", readErr)
			}
			if !strings.Contains(string(logged), "gc bd update ga-abc1 --set-metadata work_dir="+want) {
				t.Errorf("work_dir metadata must record %s, got:\n%s", want, logged)
			}
			finalCwd, readErr := os.ReadFile(filepath.Join(stage, "final_cwd"))
			if readErr != nil {
				t.Fatalf("read final_cwd: %v", readErr)
			}
			if got := strings.TrimSpace(string(finalCwd)); got != want {
				t.Errorf("step ended in %s, want the new worktree %s ({{setup_command}} runs there)", got, want)
			}
		})
	}
}

// TestCoreWorkspaceSetupRefusesWithoutAbsoluteGCDir pins the fail-closed half
// of the anchor: with GC_DIR unset, empty, or relative, the create branch must
// refuse — name GC_DIR on stderr, create nothing, record no work_dir — rather
// than fall back to the current directory. The fallback is the original
// defect, not a safety net.
func TestCoreWorkspaceSetupRefusesWithoutAbsoluteGCDir(t *testing.T) {
	empty := ""
	relative := "home"
	rows := []struct {
		name  string
		file  string
		gcDir *string
	}{
		{name: "mol-scoped-work GC_DIR unset", file: "mol-scoped-work.toml", gcDir: nil},
		{name: "mol-scoped-work GC_DIR empty", file: "mol-scoped-work.toml", gcDir: &empty},
		{name: "mol-scoped-work GC_DIR relative", file: "mol-scoped-work.toml", gcDir: &relative},
		{name: "mol-polecat-commit GC_DIR unset", file: "mol-polecat-commit.toml", gcDir: nil},
		{name: "mol-polecat-commit GC_DIR empty", file: "mol-polecat-commit.toml", gcDir: &empty},
		{name: "mol-polecat-commit GC_DIR relative", file: "mol-polecat-commit.toml", gcDir: &relative},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			stage, prelude, epilogue := stageWorkspace(t, "rig", false)
			out, logPath, err := runStagedWorkspaceSetup(t, stage, prelude, epilogue, row.file, row.gcDir)
			if err == nil {
				t.Fatalf("the create branch must refuse a non-absolute GC_DIR; it succeeded:\n%s", out)
			}
			if !strings.Contains(out, "GC_DIR") {
				t.Errorf("the refusal must name GC_DIR on stderr:\n%s", out)
			}
			for _, parent := range []string{
				filepath.Join(stage, "rig"),
				filepath.Join(stage, "rig", "sub"),
				filepath.Join(stage, "home"),
			} {
				if _, statErr := os.Stat(filepath.Join(parent, "worktrees")); !os.IsNotExist(statErr) {
					t.Errorf("a refused create must build no worktree, but one exists under %s", parent)
				}
			}
			if logged, readErr := os.ReadFile(logPath); readErr != nil || strings.Contains(string(logged), "bd update") {
				t.Errorf("a refused create must record no work_dir; gc log:\n%s", logged)
			}
		})
	}
}

// TestMolPolecatCommitCleanupKeepsCwdWorktreePath is the scope guard for the
// #6014 fix: commit-and-push's cleanup runs with the session already inside
// the worktree, so WORKTREE_PATH=$(pwd) there is the intended value. After a
// relaunch GC_DIR follows the recorded work_dir (the worktree itself), so
// re-deriving $GC_DIR/worktrees/<bead> would point one level too deep. An
// over-broad anchor sweep must fail here.
func TestMolPolecatCommitCleanupKeepsCwdWorktreePath(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-commit.toml"), "commit-and-push")
	if !strings.Contains(step, "WORKTREE_PATH=$(pwd)") {
		t.Error("commit-and-push cleanup must keep deriving WORKTREE_PATH from the current directory: the step runs inside the worktree")
	}
	if strings.Contains(step, "GC_DIR") {
		t.Error("commit-and-push must not depend on GC_DIR: after a relaunch it names the worktree itself, not its parent")
	}
}
