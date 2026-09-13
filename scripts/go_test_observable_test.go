package scripts_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type observableTimingArtifact struct {
	Schema     int                    `json:"schema"`
	ShardID    string                 `json:"shard_id"`
	Variant    string                 `json:"variant"`
	CommitSHA  string                 `json:"commit_sha"`
	Workflow   string                 `json:"workflow"`
	RunID      string                 `json:"run_id"`
	RunAttempt string                 `json:"run_attempt"`
	Job        string                 `json:"job"`
	Runner     observableTimingRunner `json:"runner"`
	Units      []observableTimingUnit `json:"units"`
}

type observableTimingRunner struct {
	Label    string `json:"label"`
	Name     string `json:"name"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUCount int    `json:"cpu_count"`
}

type observableTimingUnit struct {
	UnitID          string  `json:"unit_id"`
	Kind            string  `json:"kind"`
	Package         string  `json:"package"`
	Test            string  `json:"test"`
	Subtest         string  `json:"subtest"`
	Outcome         string  `json:"outcome"`
	DurationSeconds float64 `json:"duration_seconds"`
}

func TestGoTestObservableDefaultLogPathIsUnique(t *testing.T) {
	repoRoot := repoRoot(t)
	tmpDir := t.TempDir()

	first := runObservableTestLogPath(t, repoRoot, tmpDir)
	second := runObservableTestLogPath(t, repoRoot, tmpDir)
	t.Cleanup(func() {
		_ = os.Remove(first)
		_ = os.Remove(second)
	})

	if first == second {
		t.Fatalf("default log paths should be unique, got %q twice", first)
	}
	for _, path := range []string{first, second} {
		if !strings.HasPrefix(path, tmpDir+string(os.PathSeparator)) {
			t.Fatalf("default log path %q should be under TMPDIR %q", path, tmpDir)
		}
		if filepath.Base(path) == "gascity-observable-log-test.jsonl" {
			t.Fatalf("default log path %q should not be a shared deterministic file", path)
		}
	}
}

func TestGoTestObservableCaptureDisabledSkipsMetadataProbes(t *testing.T) {
	repoRoot := repoRoot(t)
	tmpDir := t.TempDir()
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find go: %v", err)
	}

	fakeBin := filepath.Join(tmpDir, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	probeLog := filepath.Join(tmpDir, "metadata-probes")
	fakeGo := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "list" ]; then
  printf 'go-list\n' >> %q
fi
exec %q "$@"
`, probeLog, realGo)
	if err := os.WriteFile(filepath.Join(fakeBin, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	fakeGetconf := fmt.Sprintf("#!/bin/sh\nprintf 'getconf\\n' >> %q\nexit 1\n", probeLog)
	if err := os.WriteFile(filepath.Join(fakeBin, "getconf"), []byte(fakeGetconf), 0o755); err != nil {
		t.Fatalf("write fake getconf: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	logPath := runObservableTestLogPath(t, repoRoot, tmpDir)
	t.Cleanup(func() { _ = os.Remove(logPath) })
	if probes, err := os.ReadFile(probeLog); err == nil {
		t.Fatalf("capture-disabled wrapper ran metadata probes:\n%s", probes)
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect metadata probes: %v", err)
	}
}

func TestGoTestObservableWritesDeterministicNormalizedTiming(t *testing.T) {
	t.Parallel()

	events := strings.Join([]string{
		`{"Time":"2026-07-14T00:00:01Z","Action":"run","Package":"github.com/gastownhall/gascity/internal/example","Test":"TestZulu"}`,
		`{"Time":"2026-07-14T00:00:04Z","Action":"pass","Package":"github.com/gastownhall/gascity/internal/example","Test":"TestZulu","Elapsed":0.2}`,
		`{"Time":"2026-07-14T00:00:02Z","Action":"skip","Package":"github.com/gastownhall/gascity/internal/example","Test":"TestAlpha/case","Elapsed":0.1}`,
		`{"Time":"2026-07-14T00:00:05Z","Action":"pass","Package":"github.com/gastownhall/gascity/internal/example","Elapsed":0.5}`,
		`{"Time":"2026-07-14T00:00:03Z","Action":"pass","Package":"github.com/gastownhall/gascity/internal/example","Test":"TestAlpha","Elapsed":0.3}`,
	}, "\n") + "\n"

	first, firstOutput := runObservableWithFakeGo(t, events, 0)
	second, _ := runObservableWithFakeGo(t, events, 0)
	if !slices.Equal(first, second) {
		t.Fatalf("normalized timing is not deterministic\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	for _, want := range []string{
		"2026-07-14T00:00:01Z run TestZulu\n",
		"2026-07-14T00:00:02Z skip TestAlpha/case\n",
		"2026-07-14T00:00:05Z pass github.com/gastownhall/gascity/internal/example\n",
	} {
		if !strings.Contains(string(firstOutput), want) {
			t.Fatalf("observable output does not contain %q:\n%s", want, firstOutput)
		}
	}

	var artifact observableTimingArtifact
	if err := json.Unmarshal(first, &artifact); err != nil {
		t.Fatalf("decode timing artifact: %v\n%s", err, first)
	}
	if artifact.Schema != 1 || artifact.ShardID != "cmd-gc-process-1-of-12" || artifact.Variant != "default" {
		t.Fatalf("artifact identity = schema %d shard %q variant %q", artifact.Schema, artifact.ShardID, artifact.Variant)
	}
	if artifact.CommitSHA != "deadbeef" || artifact.Workflow != "CI" || artifact.RunID != "42" || artifact.RunAttempt != "3" || artifact.Job != "cmd-gc-process" {
		t.Fatalf("artifact run metadata = %+v", artifact)
	}
	if artifact.Runner != (observableTimingRunner{Label: "blacksmith-32vcpu", Name: "runner-7", OS: "Linux", Arch: "X64", CPUCount: 32}) {
		t.Fatalf("runner metadata = %+v", artifact.Runner)
	}
	wantUnits := []observableTimingUnit{
		{UnitID: "internal/example", Kind: "package", Package: "github.com/gastownhall/gascity/internal/example", Outcome: "pass", DurationSeconds: 0.5},
		{UnitID: "internal/example:TestAlpha", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestAlpha", Outcome: "pass", DurationSeconds: 0.3},
		{UnitID: "internal/example:TestAlpha/case", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestAlpha", Subtest: "case", Outcome: "skip", DurationSeconds: 0.1},
		{UnitID: "internal/example:TestZulu", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestZulu", Outcome: "pass", DurationSeconds: 0.2},
	}
	if !slices.Equal(artifact.Units, wantUnits) {
		t.Fatalf("timing units = %+v, want %+v", artifact.Units, wantUnits)
	}
}

func TestGoTestObservableRecordsValidFailureWithoutChangingProductStatus(t *testing.T) {
	t.Parallel()

	events := strings.Join([]string{
		`{"Action":"fail","Package":"github.com/gastownhall/gascity/internal/example","Test":"TestBroken","Elapsed":0.4}`,
		`{"Action":"fail","Package":"github.com/gastownhall/gascity/internal/example","Elapsed":0.7}`,
	}, "\n") + "\n"
	data, output := runObservableWithFakeGo(t, events, 17)
	for _, want := range []string{
		" fail TestBroken\n",
		" fail github.com/gastownhall/gascity/internal/example\n",
	} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("observable output does not contain %q:\n%s", want, output)
		}
	}

	var artifact observableTimingArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode timing artifact: %v\n%s", err, data)
	}
	wantUnits := []observableTimingUnit{
		{UnitID: "internal/example", Kind: "package", Package: "github.com/gastownhall/gascity/internal/example", Outcome: "fail", DurationSeconds: 0.7},
		{UnitID: "internal/example:TestBroken", Kind: "test", Package: "github.com/gastownhall/gascity/internal/example", Test: "TestBroken", Outcome: "fail", DurationSeconds: 0.4},
	}
	if !slices.Equal(artifact.Units, wantUnits) {
		t.Fatalf("timing units = %+v, want %+v", artifact.Units, wantUnits)
	}
}

func TestGoTestObservableSkipsTimingWhenModuleIdentityIsUnavailable(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	timingFile := filepath.Join(tmpDir, "timing.json")
	events := `{"Action":"pass","Package":"github.com/gastownhall/gascity/internal/example","Test":"TestAlpha","Elapsed":0.3}` + "\n"
	status, output := runObservableCommandWithModuleStatus(t, tmpDir, timingFile, events, 0, 23)
	if status != 0 {
		t.Fatalf("observable exit = %d, want product exit 0:\n%s", status, output)
	}
	if _, err := os.Stat(timingFile); !os.IsNotExist(err) {
		t.Fatalf("missing module identity left a timing artifact: err=%v", err)
	}
	if !strings.Contains(string(output), "module path unavailable; timing capture disabled") {
		t.Fatalf("observable output did not explain disabled timing capture:\n%s", output)
	}
}

func TestGoTestObservableCaptureFailureNeverChangesProductStatus(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name                string
		output              string
		productRun          int
		wantProgressWarning bool
	}{
		{name: "malformed passing output", output: "{not-json\n", productRun: 0, wantProgressWarning: true},
		{name: "truncated passing output", output: `{"Action":"pass"`, productRun: 0, wantProgressWarning: true},
		{name: "missing passing output", output: "", productRun: 0},
		{name: "terminal event missing elapsed", output: `{"Action":"pass","Package":"github.com/gastownhall/gascity/internal/example","Test":"TestIncomplete"}` + "\n", productRun: 0},
		{name: "malformed failing output", output: "{not-json\n", productRun: 17, wantProgressWarning: true},
		{name: "large malformed failing output", output: strings.Repeat("{not-json\n", 1<<18), productRun: 17, wantProgressWarning: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			timingFile, status, output := runObservableCaptureFailure(t, tt.output, tt.productRun)
			if status != tt.productRun {
				t.Fatalf("observable exit = %d, want product exit %d", status, tt.productRun)
			}
			if got := strings.Contains(string(output), "progress rendering failed; product result is unchanged"); got != tt.wantProgressWarning {
				t.Fatalf("progress warning present = %t, want %t:\n%s", got, tt.wantProgressWarning, output)
			}
			if _, err := os.Stat(timingFile); !os.IsNotExist(err) {
				t.Fatalf("invalid capture left a timing artifact: err=%v", err)
			}
		})
	}
}

func runObservableWithFakeGo(t *testing.T, output string, productStatus int) ([]byte, []byte) {
	t.Helper()
	tmpDir := t.TempDir()
	timingFile := filepath.Join(tmpDir, "timing.json")
	status, combined := runObservableCommand(t, tmpDir, timingFile, output, productStatus)
	if status != productStatus {
		t.Fatalf("observable exit = %d, want %d\n%s", status, productStatus, combined)
	}
	data, err := os.ReadFile(timingFile)
	if err != nil {
		t.Fatalf("read timing artifact: %v\n%s", err, combined)
	}
	return data, combined
}

func runObservableCaptureFailure(t *testing.T, output string, productStatus int) (string, int, []byte) {
	t.Helper()
	tmpDir := t.TempDir()
	timingFile := filepath.Join(tmpDir, "timing.json")
	if err := os.WriteFile(timingFile, []byte("stale"), 0o600); err != nil {
		t.Fatalf("seed stale timing artifact: %v", err)
	}
	status, combined := runObservableCommand(t, tmpDir, timingFile, output, productStatus)
	return timingFile, status, combined
}

func runObservableCommand(t *testing.T, tmpDir, timingFile, output string, productStatus int) (int, []byte) {
	t.Helper()
	return runObservableCommandWithModuleStatus(t, tmpDir, timingFile, output, productStatus, 0)
}

func runObservableCommandWithModuleStatus(t *testing.T, tmpDir, timingFile, output string, productStatus, moduleStatus int) (int, []byte) {
	t.Helper()
	repoRoot := repoRoot(t)
	fakeBin := filepath.Join(tmpDir, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	eventsFile := filepath.Join(tmpDir, "events.jsonl")
	if err := os.WriteFile(eventsFile, []byte(output), 0o600); err != nil {
		t.Fatalf("write fake events: %v", err)
	}
	fakeGo := filepath.Join(fakeBin, "go")
	fakeGoScript := fmt.Sprintf(`#!/bin/sh
set -e
if [ "$1" = "list" ] && [ "$2" = "-m" ]; then
  if [ %d -eq 0 ]; then
    printf '%%s\n' 'github.com/gastownhall/gascity'
  fi
  exit %d
fi
if [ "$1" = "test" ] && [ "$2" = "-json" ]; then
	cat %q
	exit %d
fi
exit 99
`, moduleStatus, moduleStatus, eventsFile, productStatus)
	if err := os.WriteFile(fakeGo, []byte(fakeGoScript), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}

	cmd := scriptCommand(repoRoot, "go-test-observable", "cmd-gc-process-1-of-12", "--", "./internal/example")
	cmd.Dir = repoRoot
	env := goTestScriptEnv(t, tmpDir)
	env = replaceScriptEnv(env, "PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for key, value := range map[string]string{
		"GC_TEST_NO_SLICE":            "1",
		"OBSERVABLE_TEST_LOG":         filepath.Join(tmpDir, "raw.jsonl"),
		"OBSERVABLE_TIMING_FILE":      timingFile,
		"OBSERVABLE_VARIANT":          "default",
		"OBSERVABLE_COMMIT_SHA":       "deadbeef",
		"OBSERVABLE_WORKFLOW":         "CI",
		"OBSERVABLE_RUN_ID":           "42",
		"OBSERVABLE_RUN_ATTEMPT":      "3",
		"OBSERVABLE_JOB":              "cmd-gc-process",
		"OBSERVABLE_RUNNER_LABEL":     "blacksmith-32vcpu",
		"OBSERVABLE_RUNNER_NAME":      "runner-7",
		"OBSERVABLE_RUNNER_OS":        "Linux",
		"OBSERVABLE_RUNNER_ARCH":      "X64",
		"OBSERVABLE_RUNNER_CPU_COUNT": "32",
	} {
		env = replaceScriptEnv(env, key, value)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, out
	}
	exitErr := &exec.ExitError{}
	ok := errors.As(err, &exitErr)
	if !ok {
		t.Fatalf("run observable: %v\n%s", err, out)
	}
	return exitErr.ExitCode(), out
}

func replaceScriptEnv(env []string, key, value string) []string {
	prefix := key + "="
	result := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

func scriptCommand(repoRoot, name string, args ...string) *exec.Cmd {
	return exec.Command(filepath.Join(repoRoot, "scripts", name), args...)
}

func runObservableTestLogPath(t *testing.T, repoRoot, tmpDir string) string {
	t.Helper()

	cmd := scriptCommand(
		repoRoot,
		"go-test-observable",
		"observable-log-test",
		"--",
		"./internal/shellquote",
		"-run",
		"^$",
		"-count=1",
	)
	cmd.Dir = repoRoot
	cmd.Env = goTestScriptEnv(t, tmpDir)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go-test-observable failed: %v\n%s", err, out)
	}

	match := regexp.MustCompile(`(?m)^observable go test: log=(.+)$`).FindSubmatch(out)
	if match == nil {
		t.Fatalf("go-test-observable output did not include log path:\n%s", out)
	}
	return strings.TrimSpace(string(match[1]))
}

func goTestScriptEnv(t *testing.T, tmpDir string) []string {
	t.Helper()

	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + tmpDir,
	}
	for _, key := range []string{
		"GOPATH",
		"GOCACHE",
		"GOMODCACHE",
		"GOROOT",
		"GOENV",
		"GOFLAGS",
		"GO111MODULE",
		"GOEXPERIMENT",
		"GOPROXY",
		"GOPRIVATE",
		"GONOPROXY",
		"GONOSUMDB",
		"GOSUMDB",
		"GOINSECURE",
		"GOVCS",
		"GOWORK",
	} {
		value := os.Getenv(key)
		if value == "" {
			value = goEnvValue(t, key)
		}
		if value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func goEnvValue(t *testing.T, key string) string {
	t.Helper()
	out, err := exec.Command("go", "env", key).Output()
	if err != nil {
		t.Fatalf("go env %s: %v", key, err)
	}
	return strings.TrimSpace(string(out))
}
