package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const dashboardDistPath = "internal/api/dashboardspa/dist/"

// TestDashboardDistModeGuard checks the Git-index contract of
// scripts/check-dashboard-dist-modes.sh [repo-root]. Reading os.Stat alone
// cannot enforce this: the index and working tree can disagree, and remote
// Bazel runfiles can make non-executable source files executable.
func TestDashboardDistModeGuard(t *testing.T) {
	guard := filepath.Join(repoRoot(t), "scripts", "check-dashboard-dist-modes.sh")

	for _, tc := range []struct {
		name        string
		asset       string
		indexedExec bool
		workExec    bool
		outsideExec bool
		untracked   bool
		symlink     bool
		wantReject  bool
	}{
		{name: "accept_100644", asset: "index.html"},
		{name: "reject_100755", asset: "index.html", indexedExec: true, workExec: true, wantReject: true},
		{name: "index_100755_worktree_0644", asset: "assets/app.js", indexedExec: true, wantReject: true},
		{name: "index_100644_worktree_0755", asset: "assets/app.js", workExec: true},
		{name: "ignore_executable_outside_dist", asset: "index.html", outsideExec: true},
		{name: "ignore_untracked_dist_file", asset: "index.html", untracked: true},
		{name: "reject_space_in_filename", asset: "assets/extra asset.js", indexedExec: true, wantReject: true},
		{name: "reject_newline_in_filename", asset: "assets/extra\nasset.js", indexedExec: true, wantReject: true},
		{name: "reject_git_symlink_120000", asset: "linked.html", symlink: true, wantReject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			env := []string{
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + t.TempDir(),
				"GIT_CONFIG_NOSYSTEM=1",
				"GIT_CONFIG_GLOBAL=/dev/null",
				"LC_ALL=C",
			}
			git := func(args ...string) {
				t.Helper()
				cmd := testCommand("git", args...)
				cmd.Dir, cmd.Env = repo, env
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			write := func(rel string, mode os.FileMode) {
				t.Helper()
				path := filepath.Join(repo, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("same asset bytes\n"), mode); err != nil {
					t.Fatal(err)
				}
			}
			git("init", "-q")
			git("config", "core.filemode", "true")
			rel := dashboardDistPath + tc.asset
			if tc.symlink {
				write(dashboardDistPath+"index.html", 0o644)
				if err := os.Symlink("index.html", filepath.Join(repo, filepath.FromSlash(rel))); err != nil {
					t.Fatal(err)
				}
				git("add", "--", dashboardDistPath)
			} else {
				write(rel, 0o644)
				git("add", "--", rel)
				indexMode := "--chmod=-x"
				if tc.indexedExec {
					indexMode = "--chmod=+x"
				}
				git("update-index", indexMode, "--", rel)
				workMode := os.FileMode(0o644)
				if tc.workExec {
					workMode = 0o755
				}
				if err := os.Chmod(filepath.Join(repo, filepath.FromSlash(rel)), workMode); err != nil {
					t.Fatal(err)
				}
			}
			if tc.outsideExec {
				write("scripts/tool.sh", 0o755)
				git("add", "--", "scripts/tool.sh")
				git("update-index", "--chmod=+x", "--", "scripts/tool.sh")
			}
			if tc.untracked {
				write(dashboardDistPath+"local-only.js", 0o755)
			}
			cmd := testCommand("bash", guard, repo)
			cmd.Dir, cmd.Env = t.TempDir(), env // Honor the repository argument from any cwd.
			out, err := cmd.CombinedOutput()
			if err != nil {
				var exitErr *exec.ExitError
				if !asExitError(err, &exitErr) {
					t.Fatalf("run dashboard dist mode guard: %v", err)
				}
			}
			if !tc.wantReject {
				if err != nil {
					t.Fatalf("guard rejected valid Git modes: %v\n%s", err, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("guard accepted forbidden Git mode for %q\n%s", rel, out)
			}
			wantMode := "100755"
			if tc.symlink {
				wantMode = "120000"
			}
			for _, want := range []string{dashboardDistPath, "100644", wantMode} {
				if !strings.Contains(string(out), want) {
					t.Errorf("rejection must identify path and expected/actual modes (%q):\n%s", want, out)
				}
			}
		})
	}
}

// TestDashboardDistModeGuardRunsInCheck requires a real invocation in the
// standard check recipe; mentioning a guard in a comment does not run it.
func TestDashboardDistModeGuardRunsInCheck(t *testing.T) {
	cmd := testCommand("make", "-n", "check")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expand make check: %v\n%s", err, out)
	}
	if !dashboardDistGuardInvoked(string(out)) {
		t.Fatal("make check does not invoke scripts/check-dashboard-dist-modes.sh; the content-only dist diff cannot detect Git mode drift")
	}
}

// TestDashboardDistModeGuardRunsInCI requires the real checkout's Git index
// to be checked in the required Bazel lane, after fresh-merge. Runfile modes
// and a local make-only check cannot establish the committed asset modes.
func TestDashboardDistModeGuardRunsInCI(t *testing.T) {
	lane := readMultiLaneWorkflow(t).Jobs["lane"]
	freshMerge := -1
	for i, step := range lane.Steps {
		if step.Uses == freshMergeUses {
			freshMerge = i
		}
		if !dashboardDistGuardInvoked(step.Run) {
			continue
		}
		if freshMerge < 0 || i <= freshMerge {
			t.Fatal("dashboard dist mode guard must inspect the checkout after fresh-merge")
		}
		if step.ContinueOnError != "" && step.ContinueOnError != "false" {
			t.Fatalf("dashboard dist mode guard must fail CI: continue-on-error=%q", step.ContinueOnError)
		}
		if step.If != "" && step.If != "matrix.lane == 'unit'" {
			t.Fatalf("dashboard dist mode guard must run on every unit lane: if=%q", step.If)
		}
		return
	}
	t.Fatal("required bazel.yml lane does not invoke scripts/check-dashboard-dist-modes.sh against the checkout's Git index")
}

func dashboardDistGuardInvoked(script string) bool {
	for line := range strings.SplitSeq(script, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.Contains(line, "||") {
			continue
		}
		command := strings.TrimPrefix(fields[0], "./")
		if command == "scripts/check-dashboard-dist-modes.sh" {
			return true
		}
		if (command == "bash" || command == "sh") && len(fields) > 1 &&
			strings.TrimPrefix(fields[1], "./") == "scripts/check-dashboard-dist-modes.sh" {
			return true
		}
	}
	return false
}
