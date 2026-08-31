package scripts_test

import (
	"fmt"
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

// TestBazelConfigBacktestRecordsPerSampleGoRowsAndRotatesScenarios protects
// the measurement contract used by the historical config replay. Go is a
// measured pseudo-scenario for every sample, while the unrecorded Go prime
// only validates the revision and warms an isolated per-ref cache.
func TestBazelConfigBacktestRecordsPerSampleGoRowsAndRotatesScenarios(t *testing.T) {
	root := repoRoot(t)
	binDir := t.TempDir()
	logDir := t.TempDir()
	bazelLog := filepath.Join(logDir, "bazel.tsv")
	goLog := filepath.Join(logDir, "go.tsv")
	writeExecutable(t, filepath.Join(binDir, "fake-bazel"), fakeBazelScript())
	writeExecutable(t, filepath.Join(binDir, "go"), fakeGoMeasurementScript())

	cmd := exec.Command(filepath.Join(root, "scripts", "bazel-config-backtest.sh"), "HEAD")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BAZEL_BIN="+filepath.Join(binDir, "fake-bazel"),
		"BACKTEST_SAMPLES=20",
		"BACKTEST_TIMEOUT=30",
		"BACKTEST_SCENARIOS=source-edit,test-edit",
		"BACKTEST_KEEP_ARTIFACTS=0",
		"FAKE_BAZEL_LOG="+bazelLog,
		"FAKE_GO_LOG="+goLog,
		"FAKE_EDIT_FILE=internal/config/session_setup_path.go",
		"TMPDIR="+t.TempDir(),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("config backtest failed: %v\n%s", err, output)
	}

	var rows []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" || strings.HasPrefix(line, "ref\t") || strings.HasPrefix(line, "summary\t") || strings.HasPrefix(line, "go-prime\t") {
			continue
		}
		rows = append(rows, line)
	}
	if got, want := len(rows), 60; got != want {
		t.Fatalf("measured rows = %d, want %d (20 samples × go/source/test)", got, want)
	}
	for sample := 1; sample <= 20; sample++ {
		start := (sample - 1) * 3
		parts := make([][]string, 3)
		for i := range parts {
			parts[i] = strings.Split(rows[start+i], "\t")
			if len(parts[i]) < 14 {
				t.Fatalf("row %d has %d fields: %q", start+i, len(parts[i]), rows[start+i])
			}
			if parts[i][1] != fmt.Sprint(sample) {
				t.Fatalf("row %d sample = %q, want %d", start+i, parts[i][1], sample)
			}
		}
		orders := [][]string{
			{"go", "source-edit", "test-edit"},
			{"go", "test-edit", "source-edit"},
			{"go", "test-edit", "source-edit"},
			{"go", "source-edit", "test-edit"},
		}
		wantOrder := orders[(sample-1)%len(orders)]
		for i, want := range wantOrder {
			if got := parts[i][2]; got != want {
				t.Fatalf("sample %d order[%d] = %q, want %q", sample, i, got, want)
			}
			if parts[i][3] != "0" {
				t.Fatalf("sample %d scenario %s status = %q, want 0", sample, want, parts[i][3])
			}
		}
	}

	cacheLines, err := os.ReadFile(goLog)
	if err != nil {
		t.Fatalf("read fake Go log: %v", err)
	}
	goInvocations := strings.Split(strings.TrimSpace(string(cacheLines)), "\n")
	if got, want := len(goInvocations), 21; got != want {
		t.Fatalf("Go invocations = %d, want %d (one prime + one per sample)", got, want)
	}
	var cache string
	for _, line := range goInvocations {
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 || fields[0] == "" {
			t.Fatalf("malformed Go measurement row %q", line)
		}
		if cache == "" {
			cache = fields[0]
		} else if fields[0] != cache {
			t.Fatalf("GOCACHE changed within ref: %q then %q", cache, fields[0])
		}
	}
	if strings.Contains(cache, "/.cache/go-build") || cache == os.Getenv("GOCACHE") {
		t.Fatalf("Go measurement used a shared/default GOCACHE: %q", cache)
	}
	if !strings.Contains(string(output), "summary\t") || !strings.Contains(string(output), "\tgo\tattempted=20\tsuccess=20\tfailure=0\tskipped=0") {
		t.Fatalf("summary does not expose Go success counts:\n%s", output)
	}
	if !strings.Contains(string(output), "\tgo\tattempted=20\tsuccess=20\tfailure=0\tskipped=0\tp50_s=") || !strings.Contains(string(output), "\tvalid=true\t") {
		t.Fatalf("healthy matrix rows are not marked valid:\n%s", output)
	}
}

// TestBazelConfigBacktestGoPrimeFailureSkipsRefAndContinues ensures a bad
// historical revision cannot cause the whole matrix to abort or invoke
// Bazel. The next revision still receives its complete measured matrix.
func TestBazelConfigBacktestGoPrimeFailureSkipsRefAndContinues(t *testing.T) {
	root := repoRoot(t)
	binDir := t.TempDir()
	logDir := t.TempDir()
	bazelLog := filepath.Join(logDir, "bazel.tsv")
	goLog := filepath.Join(logDir, "go.tsv")
	artifactDir := t.TempDir()
	counter := filepath.Join(logDir, "go-count")
	writeExecutable(t, filepath.Join(binDir, "fake-bazel"), fakeBazelScript())
	writeExecutable(t, filepath.Join(binDir, "go"), fakeGoPrimeFailureScript())

	cmd := exec.Command(filepath.Join(root, "scripts", "bazel-config-backtest.sh"), "HEAD", "HEAD^")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BAZEL_BIN="+filepath.Join(binDir, "fake-bazel"),
		"BACKTEST_SAMPLES=2",
		"BACKTEST_TIMEOUT=30",
		"BACKTEST_SCENARIOS=source-edit",
		"BACKTEST_KEEP_ARTIFACTS=0",
		"BACKTEST_ARTIFACT_DIR="+artifactDir,
		"FAKE_BAZEL_LOG="+bazelLog,
		"FAKE_GO_LOG="+goLog,
		"FAKE_GO_COUNTER="+counter,
		"FAKE_EDIT_FILE=internal/config/session_setup_path.go",
		"TMPDIR="+t.TempDir(),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("config backtest should continue after Go prime failure: %v\n%s", err, output)
	}

	var firstRef, secondRef string
	for _, line := range strings.Split(string(output), "\n") {
		if line == "" || strings.HasPrefix(line, "ref\t") || strings.HasPrefix(line, "summary\t") || strings.HasPrefix(line, "go-prime\t") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 4 {
			continue
		}
		if firstRef == "" {
			firstRef = fields[0]
		} else if fields[0] != firstRef && secondRef == "" {
			secondRef = fields[0]
		}
		if fields[0] == firstRef && fields[3] != "skip-go-failed" {
			t.Fatalf("prime-failed ref row status = %q, want skip-go-failed: %q", fields[3], line)
		}
	}
	if firstRef == "" || secondRef == "" {
		t.Fatalf("output did not contain rows for both refs:\n%s", output)
	}
	if strings.Contains(string(output), "summary\t"+firstRef+"\tsource-edit\tattempted=") &&
		!strings.Contains(string(output), "summary\t"+firstRef+"\tsource-edit\tattempted=0\tsuccess=0\tfailure=0\tskipped=2") {
		t.Fatalf("prime-failed summary is not all skipped:\n%s", output)
	}
	if !strings.Contains(string(output), secondRef+"\t1\tgo\t0\t") {
		t.Fatalf("continued ref has no successful Go rows:\n%s", output)
	}

	bazelBody, err := os.ReadFile(bazelLog)
	if err != nil {
		t.Fatalf("read fake Bazel log: %v", err)
	}
	testRows := 0
	for _, line := range strings.Split(strings.TrimSpace(string(bazelBody)), "\n") {
		if strings.HasPrefix(line, "test\t") {
			testRows++
		}
	}
	if testRows != 3 {
		t.Fatalf("Bazel test invocations = %d, want 3 for only the continued ref (prime + 2 samples)", testRows)
	}
	entries, err := os.ReadDir(artifactDir)
	if err != nil {
		t.Fatalf("read Go-prime artifact directory: %v", err)
	}
	foundFailure := false
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "go-prime") {
			foundFailure = true
			break
		}
	}
	if !foundFailure {
		t.Fatalf("Go-prime failure artifact missing in %s (output:\n%s)", artifactDir, output)
	}
}

// TestBazelConfigBacktestMeasuredGoFailureSkipsBazelRows guards the measured
// Go preflight: a failed baseline must prevent that sample's Bazel scenarios.
func TestBazelConfigBacktestMeasuredGoFailureSkipsBazelRows(t *testing.T) {
	root := repoRoot(t)
	binDir := t.TempDir()
	logDir := t.TempDir()
	bazelLog := filepath.Join(logDir, "bazel.tsv")
	goLog := filepath.Join(logDir, "go.tsv")
	goCounter := filepath.Join(logDir, "go-count")
	writeExecutable(t, filepath.Join(binDir, "fake-bazel"), fakeBazelScript())
	writeExecutable(t, filepath.Join(binDir, "go"), fakeGoMeasuredFailureScript())

	cmd := exec.Command(filepath.Join(root, "scripts", "bazel-config-backtest.sh"), "HEAD")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BAZEL_BIN="+filepath.Join(binDir, "fake-bazel"),
		"BACKTEST_SAMPLES=2",
		"BACKTEST_TIMEOUT=30",
		"BACKTEST_SCENARIOS=source-edit,test-edit",
		"BACKTEST_KEEP_ARTIFACTS=0",
		"FAKE_BAZEL_LOG="+bazelLog,
		"FAKE_GO_LOG="+goLog,
		"FAKE_GO_COUNTER="+goCounter,
		"FAKE_GO_FAIL_COUNT=3", // prime + sample 1 + sample 2
		"FAKE_EDIT_FILE=internal/config/session_setup_path.go",
		"TMPDIR="+t.TempDir(),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("config backtest failed: %v\n%s", err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 4 || fields[1] != "2" || fields[2] == "go" || fields[0] == "ref" || fields[0] == "summary" {
			continue
		}
		if fields[3] != "skip-go-failed" {
			t.Fatalf("sample 2 Bazel row remained comparable after Go failure: %q\nfull output:\n%s", line, output)
		}
	}
	if !strings.Contains(string(output), "\t2\tgo\t17\t") {
		t.Fatalf("measured Go failure row missing:\n%s", output)
	}
	if !strings.Contains(string(output), "\tsource-edit\tattempted=") || !strings.Contains(string(output), "\tvalid=false\t") {
		t.Fatalf("measured Go failure was not reported invalid:\n%s", output)
	}
}

// TestBazelConfigBacktestWarmPrimeFailureSkipsRevision ensures a failed Bazel
// warm prime is explicit and does not fall through to a scenario invocation.
func TestBazelConfigBacktestWarmPrimeFailureSkipsRevision(t *testing.T) {
	root := repoRoot(t)
	binDir := t.TempDir()
	logDir := t.TempDir()
	bazelLog := filepath.Join(logDir, "bazel.tsv")
	writeExecutable(t, filepath.Join(binDir, "fake-bazel"), fakeBazelScript())
	writeExecutable(t, filepath.Join(binDir, "go"), fakeGoScript())

	cmd := exec.Command(filepath.Join(root, "scripts", "bazel-config-backtest.sh"), "HEAD")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BAZEL_BIN="+filepath.Join(binDir, "fake-bazel"),
		"BACKTEST_SAMPLES=2",
		"BACKTEST_TIMEOUT=30",
		"BACKTEST_SCENARIOS=source-edit",
		"BACKTEST_KEEP_ARTIFACTS=0",
		"FAKE_BAZEL_LOG="+bazelLog,
		"FAKE_BAZEL_FAIL_FIRST_TEST=1",
		"FAKE_EDIT_FILE=internal/config/session_setup_path.go",
		"TMPDIR="+t.TempDir(),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("config backtest failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "\tsource-edit\tskip-bazel-prime\t") {
		t.Fatalf("warm-prime failure status missing:\n%s", output)
	}
	if !strings.Contains(string(output), "\tvalid=false\t") {
		t.Fatalf("warm-prime failure was not reported invalid:\n%s", output)
	}
	body, err := os.ReadFile(bazelLog)
	if err != nil {
		t.Fatalf("read fake Bazel log: %v", err)
	}
	if got := strings.Count(string(body), "test\t"); got != 1 {
		t.Fatalf("Bazel test invocations = %d, want only failed warm prime", got)
	}
}

func TestBazelConfigBacktestUsesUniqueEditPayloadsAndArtifacts(t *testing.T) {
	root := repoRoot(t)
	binDir := t.TempDir()
	logDir := t.TempDir()
	bazelLog := filepath.Join(logDir, "bazel.tsv")
	markerLog := filepath.Join(logDir, "markers.log")
	artifactDir := filepath.Join(t.TempDir(), "artifacts")
	writeExecutable(t, filepath.Join(binDir, "fake-bazel"), fakeBazelScript())
	writeExecutable(t, filepath.Join(binDir, "go"), fakeGoScript())

	cmd := exec.Command(filepath.Join(root, "scripts", "bazel-config-backtest.sh"), "HEAD")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BAZEL_BIN="+filepath.Join(binDir, "fake-bazel"),
		"BACKTEST_SAMPLES=2",
		"BACKTEST_TIMEOUT=30",
		"BACKTEST_SCENARIOS=source-edit",
		"BACKTEST_ARTIFACT_DIR="+artifactDir,
		"BACKTEST_KEEP_ARTIFACTS=0",
		"FAKE_BAZEL_LOG="+bazelLog,
		"FAKE_BAZEL_MARKER_LOG="+markerLog,
		"FAKE_EDIT_FILE=internal/config/session_setup_path.go",
		"TMPDIR="+t.TempDir(),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("config backtest failed: %v\n%s", err, output)
	}
	markers, err := os.ReadFile(markerLog)
	if err != nil {
		t.Fatalf("read marker log: %v", err)
	}
	if got := strings.TrimSpace(string(markers)); got != "// bazel backtest source edit sample=1\n// bazel backtest source edit sample=2" {
		t.Fatalf("edit payloads = %q, want one unique marker per sample", got)
	}
	shortRefBytes, err := exec.Command("git", "-C", root, "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		t.Fatalf("resolve HEAD short ref: %v", err)
	}
	shortRef := strings.TrimSpace(string(shortRefBytes))
	for _, name := range []string{"results.tsv", "go-prime-" + shortRef + ".log", shortRef + "-1-go.log", shortRef + "-1-source_edit.log", shortRef + "-1-source_edit.bep.json", shortRef + "-1-source_edit.bep.err"} {
		if _, err := os.Stat(filepath.Join(artifactDir, name)); err != nil {
			t.Errorf("artifact %s missing: %v", name, err)
		}
	}
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
if [[ "$mode" == test && -n "${FAKE_BAZEL_MARKER_LOG:-}" && -f "$work/$edited_file" ]]; then
  payload="$(grep 'bazel backtest source edit' "$work/$edited_file" | tail -1 || true)"
  [[ -n "$payload" ]] && printf '%s\n' "$payload" >>"$FAKE_BAZEL_MARKER_LOG"
fi
if [[ "$mode" == test && "${FAKE_BAZEL_FAIL_FIRST_TEST:-0}" == 1 && ! -f "$log.first-test-failed" ]]; then
  : >"$log.first-test-failed"
  exit 23
fi
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

func fakeGoMeasurementScript() string {
	return `#!/usr/bin/env bash
set -euo pipefail
printf '%s\t%s\n' "${GOCACHE:-}" "$*" >>"${FAKE_GO_LOG:?}"
if [[ "${1:-}" == version ]]; then
  echo 'go version go1.26.6 linux/amd64'
fi
exit 0
`
}

func fakeGoPrimeFailureScript() string {
	return `#!/usr/bin/env bash
set -euo pipefail
log="${FAKE_GO_LOG:?}"
counter="${FAKE_GO_COUNTER:?}"
{
  flock 9
  n=0
  [[ -f "$counter" ]] && n="$(cat "$counter")"
  n=$((n + 1))
  printf '%s\n' "$n" >"$counter"
  printf '%s\t%s\n' "${GOCACHE:-}" "$*" >>"$log"
  if [[ "$n" == 1 ]]; then
    echo 'simulated historical Go prime failure' >&2
    exit 17
  fi
} 9>"$counter.lock"
exit 0
`
}

func fakeGoMeasuredFailureScript() string {
	return `#!/usr/bin/env bash
set -euo pipefail
log="${FAKE_GO_LOG:?}"
counter="${FAKE_GO_COUNTER:?}"
{
  flock 9
  n=0
  [[ -f "$counter" ]] && n="$(cat "$counter")"
  n=$((n + 1))
  printf '%s\n' "$n" >"$counter"
  printf '%s\t%s\n' "${GOCACHE:-}" "$*" >>"$log"
  if [[ "$n" == "${FAKE_GO_FAIL_COUNT:?}" ]]; then
    echo 'simulated measured Go failure' >&2
    exit 17
  fi
} 9>"$counter.lock"
exit 0
`
}
