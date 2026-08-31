package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBazelConfigBacktestRunsBazelFromHistoricalWorktree protects the
// historical replay boundary: Bazel must discover the disposable revision,
// not the checkout that launched the harness. The fake executable also proves
// that the source-edit scenario reaches the intended worktree.
func TestBazelConfigBacktestRunsBazelFromHistoricalWorktree(t *testing.T) {
	root := repoRoot(t)
	binDir := t.TempDir()
	bazelLog := filepath.Join(t.TempDir(), "bazel.tsv")
	writeExecutable(t, filepath.Join(binDir, "fake-bazel"), fakeBazelScript())
	writeExecutable(t, filepath.Join(binDir, "go"), fakeGoScript())

	cmd := exec.Command(filepath.Join(root, "scripts", "bazel-config-backtest.sh"), "HEAD")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BAZEL_BIN="+filepath.Join(binDir, "fake-bazel"),
		"BACKTEST_SAMPLES=1",
		"BACKTEST_TIMEOUT=30",
		"BACKTEST_SCENARIOS=source-edit",
		"BACKTEST_KEEP_ARTIFACTS=0",
		"FAKE_BAZEL_LOG="+bazelLog,
		"FAKE_EDIT_FILE=internal/config/session_setup_path.go",
		"TMPDIR="+t.TempDir(),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("config backtest failed: %v\n%s", err, output)
	}
	// The config harness must prime a warm graph before the edited scenario,
	// then run the scenario itself and shut down that graph from the same
	// disposable worktree.
	assertBazelWorktreeLog(t, bazelLog, root, "internal/config/session_setup_path.go", 2)
}

// TestBazelPRBacktestRunsBazelFromHistoricalWorktree applies the same cwd
// contract to the older PR replay harness, which has an independent command
// path and must not regress when the config harness is changed.
func TestBazelPRBacktestRunsBazelFromHistoricalWorktree(t *testing.T) {
	root := repoRoot(t)
	binDir := t.TempDir()
	bazelLog := filepath.Join(t.TempDir(), "bazel.tsv")
	writeExecutable(t, filepath.Join(binDir, "fake-bazel"), fakeBazelScript())
	writeExecutable(t, filepath.Join(binDir, "go"), fakeGoScript())

	cmd := exec.Command(filepath.Join(root, "scripts", "bazel-pr-backtest.sh"), "HEAD")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BAZEL_BIN="+filepath.Join(binDir, "fake-bazel"),
		"BACKTEST_TIMEOUT=30",
		"BACKTEST_KEEP_ARTIFACTS=0",
		"FAKE_BAZEL_LOG="+bazelLog,
		"FAKE_EDIT_FILE=internal/beads/contract/metadata.go",
		"TMPDIR="+t.TempDir(),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PR backtest failed: %v\n%s", err, output)
	}
	// The PR harness has cold, forced, cached, and incremental test phases;
	// each phase and the final shutdown must stay in the historical worktree.
	assertBazelWorktreeLog(t, bazelLog, root, "internal/beads/contract/metadata.go", 4)
}

func assertBazelWorktreeLog(t *testing.T, path, callerRoot, editedFile string, minTestRows int) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fake Bazel log: %v", err)
	}
	var testRows, editedRows, shutdownRows int
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			t.Fatalf("malformed fake Bazel log row %q", line)
		}
		mode, pwd, work, marker := fields[0], fields[1], fields[2], fields[3]
		if work == "" {
			t.Fatalf("fake Bazel could not derive worktree from row %q", line)
		}
		if pwd == callerRoot {
			t.Fatalf("Bazel %s ran from caller checkout, row %q", mode, line)
		}
		if filepath.Clean(pwd) != filepath.Clean(work) {
			t.Fatalf("Bazel %s cwd %q does not match disposable worktree %q", mode, pwd, work)
		}
		if mode == "test" {
			testRows++
		}
		if mode == "shutdown" {
			shutdownRows++
		}
		if mode == "test" && marker == "present" {
			editedRows++
		}
	}
	if testRows < minTestRows {
		t.Fatalf("fake Bazel recorded %d test invocations, want at least %d (warm priming/scenario coverage)", testRows, minTestRows)
	}
	if editedRows == 0 {
		t.Fatalf("fake Bazel never observed edit to %s", editedFile)
	}
	if shutdownRows == 0 {
		t.Fatal("fake Bazel recorded no shutdown invocation")
	}
}

func fakeBazelScript() string {
	return `#!/usr/bin/env bash
set -euo pipefail
log="${FAKE_BAZEL_LOG:?}"
mode=shutdown
for arg in "$@"; do
  [[ "$arg" == test ]] && mode=test
done
output_base=""
bep=""
for arg in "$@"; do
  case "$arg" in
    --output_base=*) output_base="${arg#*=}" ;;
    --build_event_json_file=*) bep="${arg#*=}" ;;
  esac
done
work=""
if [[ -n "$output_base" ]]; then work="$(dirname "$output_base")"; fi
if [[ -n "$output_base" ]]; then mkdir -p "$output_base"; fi
marker=missing
edited_file="${FAKE_EDIT_FILE:?}"
if [[ -f "$work/$edited_file" ]] && grep -q 'bazel backtest' "$work/$edited_file"; then marker=present; fi
printf '%s\t%s\t%s\t%s\n' "$mode" "$PWD" "$work" "$marker" >>"$log"
if [[ -n "$bep" ]]; then
  label='//internal/config:config_session_setup_path_test'
  for arg in "$@"; do
    if [[ "$arg" == //internal/config:* ]]; then label="$arg"; fi
  done
  mkdir -p "$(dirname "$bep")"
  {
    printf '{"id":{"pattern":{"pattern":"%s"}},"pattern":{"pattern":"%s"}}\n' "$label" "$label"
    printf '{"id":{"targetConfigured":{"label":"%s"}},"configured":{}}\n' "$label"
    printf '{"id":{"targetCompleted":{"label":"%s"}},"completed":{}}\n' "$label"
    printf '{"buildMetrics":{"actionSummary":{"actionsCreated":1,"actionsExecuted":1,"actionCacheStatistics":{"hitCount":0,"missCount":1}}}}\n'
  } >"$bep"
fi
`
}

func fakeGoScript() string {
	return `#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == version ]]; then
  echo 'go version go1.26.6 linux/amd64'
fi
exit 0
`
}
