package scripts_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
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

// observableLogFixture runs the public wrapper with a fake go binary. Each
// fixture has its own TMPDIR, so retention checks cannot touch live run logs.
type observableLogFixture struct {
	root, bin, events string
	env               []string
}

func newObservableLogFixture(t *testing.T, events string, productStatus int) observableLogFixture {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	f := observableLogFixture{root: root, bin: bin, events: filepath.Join(root, "events.jsonl")}
	f.setGo(t, events, productStatus)
	f.env = replaceScriptEnv(goTestScriptEnv(t, root), "PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.env = replaceScriptEnv(f.env, "GC_TEST_NO_SLICE", "1")
	return f
}

func (f observableLogFixture) setGo(t *testing.T, events string, productStatus int) {
	t.Helper()
	if err := os.WriteFile(f.events, []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "list" ]; then printf 'github.com/gastownhall/gascity\n'; exit 0; fi
if [ "$1" = "test" ] && [ "$2" = "-json" ]; then cat %q; exit %d; fi
exit 99
`, f.events, productStatus)
	if err := os.WriteFile(filepath.Join(f.bin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f observableLogFixture) command(t *testing.T, extra map[string]string) *exec.Cmd {
	t.Helper()
	cmd := scriptCommand(repoRoot(t), "go-test-observable", "retention-test", "--", "./internal/example")
	cmd.Dir = repoRoot(t)
	cmd.Env = append([]string(nil), f.env...)
	for key, value := range extra {
		cmd.Env = replaceScriptEnv(cmd.Env, key, value)
	}
	return cmd
}

func (f observableLogFixture) run(t *testing.T, extra map[string]string, wantStatus int) (string, string) {
	t.Helper()
	cmd := f.command(t, extra)
	out, err := cmd.CombinedOutput()
	status := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run wrapper: %v\n%s", err, out)
		}
		status = exit.ExitCode()
	}
	if status != wantStatus {
		t.Fatalf("wrapper exit = %d, want product status %d:\n%s", status, wantStatus, out)
	}
	return observableLogPath(t, string(out)), string(out)
}

func observableLogPath(t *testing.T, output string) string {
	t.Helper()
	matches := regexp.MustCompile(`(?m)^observable go test: log=(.+)$`).FindAllStringSubmatch(output, -1)
	if len(matches) != 1 {
		t.Fatalf("want one log= line, got %d:\n%s", len(matches), output)
	}
	return matches[0][1]
}

func observableOwnedDir(root string) string {
	return filepath.Join(root, fmt.Sprintf("gascity-observable-logs-%d", os.Getuid()))
}

func ageObservableFile(t *testing.T, path string, minutes int) {
	t.Helper()
	when := time.Now().Add(-time.Duration(minutes) * time.Minute)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestGoTestObservableDefaultLogOwnershipAndEvidence(t *testing.T) { // T1
	events := `{"Action":"pass","Package":"example","Test":"TestEvidence","Elapsed":0.1}` + "\n"
	f := newObservableLogFixture(t, events, 0)
	path, output := f.run(t, nil, 0)
	if filepath.Dir(path) != observableOwnedDir(f.root) {
		t.Errorf("default log parent = %q, want owned directory %q", filepath.Dir(path), observableOwnedDir(f.root))
	}
	dirInfo, err := os.Lstat(observableOwnedDir(f.root))
	if err != nil {
		t.Fatalf("owned directory missing: %v", err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("owned directory mode = %v, want directory 0700", dirInfo.Mode())
	}
	if stat, ok := dirInfo.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
		t.Errorf("owned directory uid = %d, want %d", stat.Uid, os.Getuid())
	}
	if !regexp.MustCompile(`^gascity-[A-Za-z0-9._-]+\.jsonl\.[A-Za-z0-9]{6}$`).MatchString(filepath.Base(path)) {
		t.Errorf("default log basename = %q", filepath.Base(path))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("PASS evidence missing: %v\n%s", err, output)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("default log mode = %v, want 0600", info.Mode())
	}
	wider := newObservableLogFixture(t, "", 0)
	if err := os.Mkdir(observableOwnedDir(wider.root), 0o755); err != nil {
		t.Fatal(err)
	}
	wider.run(t, nil, 0)
	if info, err := os.Stat(observableOwnedDir(wider.root)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("pre-existing owned directory was not tightened: %v, %v", info, err)
	}
}

func TestGoTestObservableRetainsPassingAndFailingEvidence(t *testing.T) { // T3, T4
	events := `{"Action":"pass","Package":"example","Test":"TestEvidence","Elapsed":0.1}` + "\n"
	f := newObservableLogFixture(t, events, 0)
	path, output := f.run(t, nil, 0)
	if data, err := os.ReadFile(path); err != nil || string(data) != events {
		t.Errorf("PASS evidence bytes = %q, err %v, want %q", data, err, events)
	}
	if !strings.HasSuffix(strings.TrimSpace(output), "observable go test: PASS log="+path) {
		t.Errorf("PASS line is not terminal or points elsewhere:\n%s", output)
	}

	failureEvents := `{"Action":"fail","Package":"example","Test":"TestBroken","Elapsed":0.1}` + "\n"
	f.setGo(t, failureEvents, 17)
	failurePath, failure := f.run(t, nil, 17)
	if data, err := os.ReadFile(failurePath); err != nil || string(data) != failureEvents {
		t.Errorf("FAIL evidence bytes = %q, err %v, want %q", data, err, failureEvents)
	}
	failLine := "observable go test: FAIL status=17 log=" + failurePath
	if !strings.Contains(failure, failLine) || !strings.Contains(failure, "observable go test: failure details from "+failurePath) || strings.Index(failure, failLine) > strings.Index(failure, "observable go test: failure details from ") {
		t.Errorf("FAIL line or detail ordering changed:\n%s", failure)
	}
}

func TestGoTestObservablePrunesOnlyExpiredOwnedRegularFiles(t *testing.T) { // T5
	for _, expiredCount := range []int{1, 3, 0} {
		t.Run(fmt.Sprintf("expired-%d", expiredCount), func(t *testing.T) {
			f := newObservableLogFixture(t, "", 0)
			d := observableOwnedDir(f.root)
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < expiredCount; i++ {
				path := filepath.Join(d, fmt.Sprintf("expired-%d.jsonl", i))
				if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
				ageObservableFile(t, path, 4321)
			}
			for _, minutes := range []int{4319, 0} {
				path := filepath.Join(d, fmt.Sprintf("young-%d.jsonl", minutes))
				if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				ageObservableFile(t, path, minutes)
			}
			_, output := f.run(t, nil, 0)
			for i := 0; i < expiredCount; i++ {
				path := filepath.Join(d, fmt.Sprintf("expired-%d.jsonl", i))
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("expired file %q remains: %v", path, err)
				}
			}
			for _, path := range []string{filepath.Join(d, "young-4319.jsonl"), filepath.Join(d, "young-0.jsonl")} {
				if _, err := os.Lstat(path); err != nil {
					t.Errorf("protected path %q lost: %v", path, err)
				}
			}
			pruneLines := regexp.MustCompile(`(?m)^observable go test: pruned \d+ expired log\(s\) older than 72h from .+$`).FindAllString(output, -1)
			if expiredCount == 0 && len(pruneLines) != 0 {
				t.Errorf("unexpected prune line: %q", pruneLines)
			}
			if expiredCount > 0 && (len(pruneLines) != 1 || pruneLines[0] != fmt.Sprintf("observable go test: pruned %d expired log(s) older than 72h from %s", expiredCount, d)) {
				t.Errorf("prune lines = %q, want count %d in %s:\n%s", pruneLines, expiredCount, d, output)
			}
		})
	}
}

func TestGoTestObservableNeverTouchesOtherPaths(t *testing.T) { // T6, T7
	f := newObservableLogFixture(t, "", 0)
	d := observableOwnedDir(f.root)
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(f.root, "gascity-test.jsonl.AAAAAA")
	unrelated := filepath.Join(f.root, "unrelated")
	victim := filepath.Join(f.root, "victim")
	for _, path := range []string{legacy, unrelated, victim} {
		if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		ageObservableFile(t, path, 30*24*60)
	}
	rootLink := filepath.Join(f.root, "gascity-x.jsonl.BBBBBB")
	if err := os.Symlink(victim, rootLink); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(d, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	nestedOld := filepath.Join(nested, "old.jsonl")
	if err := os.WriteFile(nestedOld, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	ageObservableFile(t, nestedOld, 4321)
	ownedLink := filepath.Join(d, "link.jsonl")
	if err := os.Symlink(victim, ownedLink); err != nil {
		t.Fatal(err)
	}
	f.run(t, nil, 0)
	for _, path := range []string{legacy, unrelated, victim, rootLink, nestedOld, ownedLink} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("protected path %q lost: %v", path, err)
		}
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "keep" {
		t.Errorf("victim changed: %q, %v", data, err)
	}
}

func TestGoTestObservableExplicitLogIsCallerOwned(t *testing.T) { // T8
	for _, productStatus := range []int{0, 17} {
		t.Run(fmt.Sprintf("status-%d", productStatus), func(t *testing.T) {
			events := `{"Action":"pass","Package":"example","Elapsed":0.1}` + "\n"
			f := newObservableLogFixture(t, events, productStatus)
			explicit := filepath.Join(f.root, "caller.jsonl")
			path, output := f.run(t, map[string]string{"OBSERVABLE_TEST_LOG": explicit}, productStatus)
			if path != explicit {
				t.Errorf("log path = %q, want explicit %q", path, explicit)
			}
			if data, err := os.ReadFile(explicit); err != nil || string(data) != events {
				t.Errorf("explicit log = %q, %v", data, err)
			}
			if _, err := os.Lstat(observableOwnedDir(f.root)); !os.IsNotExist(err) {
				t.Errorf("explicit run created owned directory: %v", err)
			}
			if got := strings.Count(output, "observable go test: log-retention: caller-owned, never removed by the wrapper"); got != 1 {
				t.Errorf("caller-owned line count = %d:\n%s", got, output)
			}
			d := observableOwnedDir(f.root)
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			old := filepath.Join(d, "old.jsonl")
			if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			ageObservableFile(t, old, 4321)
			f.run(t, map[string]string{"OBSERVABLE_TEST_LOG": explicit}, productStatus)
			if _, err := os.Stat(old); !os.IsNotExist(err) {
				t.Errorf("expired owned file remains on explicit run: %v", err)
			}
			if data, err := os.ReadFile(explicit); err != nil || string(data) != events {
				t.Errorf("explicit log changed after prune: %q, %v", data, err)
			}
		})
	}
}

func TestGoTestObservableReplacesStaleExplicitLog(t *testing.T) { // T9
	events := `{"Action":"pass","Package":"example","Elapsed":0.1}` + "\n"
	f := newObservableLogFixture(t, events, 0)
	explicit := filepath.Join(f.root, "caller.jsonl")
	if err := os.WriteFile(explicit, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, _ := f.run(t, map[string]string{"OBSERVABLE_TEST_LOG": explicit}, 0)
	if path != explicit {
		t.Errorf("log path = %q, want %q", path, explicit)
	}
	if data, err := os.ReadFile(explicit); err != nil || string(data) != events {
		t.Errorf("explicit log = %q, %v", data, err)
	}
}

func TestGoTestObservablePruneDoesNotChangeTiming(t *testing.T) { // T10
	events := `{"Action":"pass","Package":"github.com/gastownhall/gascity/internal/example","Test":"TestAlpha","Elapsed":0.3}` + "\n"
	var artifacts [2][]byte
	for i := range artifacts {
		f := newObservableLogFixture(t, events, 0)
		timing := filepath.Join(f.root, "timing.json")
		if i == 1 {
			d := observableOwnedDir(f.root)
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			old := filepath.Join(d, "old.jsonl")
			if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			ageObservableFile(t, old, 4321)
		}
		f.run(t, map[string]string{"OBSERVABLE_TIMING_FILE": timing, "OBSERVABLE_RUNNER_CPU_COUNT": "1"}, 0)
		var err error
		artifacts[i], err = os.ReadFile(timing)
		if err != nil {
			t.Fatalf("timing artifact: %v", err)
		}
	}
	if !bytes.Equal(artifacts[0], artifacts[1]) {
		t.Errorf("pruning changed timing artifact:\n%s\n%s", artifacts[0], artifacts[1])
	}
}

func TestGoTestObservableHousekeepingFailureKeepsProductResult(t *testing.T) { // T11
	for _, productStatus := range []int{0, 17} {
		t.Run(fmt.Sprintf("status-%d", productStatus), func(t *testing.T) {
			f := newObservableLogFixture(t, `{"Action":"pass","Package":"example","Elapsed":0.1}`+"\n", productStatus)
			find := filepath.Join(f.bin, "find")
			if err := os.WriteFile(find, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			path, output := f.run(t, nil, productStatus)
			if filepath.Dir(path) != observableOwnedDir(f.root) {
				t.Errorf("housekeeping failure log = %q", path)
			}
			if _, err := os.Stat(path); err != nil {
				t.Errorf("fake product did not write log: %v", err)
			}
			if got := strings.Count(output, "observable go test: warning: log housekeeping failed"); got != 1 {
				t.Errorf("housekeeping warning count = %d:\n%s", got, output)
			}
		})
	}
}

func TestGoTestObservableRejectsUnownedDirectory(t *testing.T) { // T12
	for _, kind := range []string{"regular-file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := newObservableLogFixture(t, "", 0)
			d := observableOwnedDir(f.root)
			target := filepath.Join(f.root, "target")
			if err := os.WriteFile(target, []byte("victim"), 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "regular-file" {
				if err := os.WriteFile(d, []byte("occupier"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(target, d); err != nil {
				t.Fatal(err)
			}
			path, output := f.run(t, nil, 0)
			if filepath.Dir(path) != f.root {
				t.Errorf("fallback log = %q, want scratch root %q", path, f.root)
			}
			if !regexp.MustCompile(`^gascity-[A-Za-z0-9._-]+\.jsonl\.[A-Za-z0-9]{6}$`).MatchString(filepath.Base(path)) {
				t.Errorf("fallback basename = %q", filepath.Base(path))
			}
			if _, err := os.Stat(path); err != nil {
				t.Errorf("fallback log missing: %v", err)
			}
			if got := strings.Count(output, "observable go test: warning: cannot establish log directory"); got != 1 {
				t.Errorf("directory warning count = %d:\n%s", got, output)
			}
			if data, err := os.ReadFile(target); err != nil || string(data) != "victim" {
				t.Errorf("symlink target changed: %q, %v", data, err)
			}
			if kind == "regular-file" {
				if data, err := os.ReadFile(d); err != nil || string(data) != "occupier" {
					t.Errorf("directory occupier changed: %q, %v", data, err)
				}
			} else if info, err := os.Lstat(d); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("directory symlink changed: %v, %v", info, err)
			}
		})
	}
}

func TestGoTestObservableInterruptedLogCanBePruned(t *testing.T) { // T13
	f := newObservableLogFixture(t, "", 0)
	blockingGo := `#!/bin/sh
if [ "$1" = "list" ]; then printf 'github.com/gastownhall/gascity\n'; exit 0; fi
if [ "$1" = "test" ]; then
  printf '{"Action":"run","Package":"example","Test":"TestInterrupted"}\n'
  printf 'FAKE_GO_BLOCKED\n' >&2
  exec sleep 60
fi
exit 99
`
	if err := os.WriteFile(filepath.Join(f.bin, "go"), []byte(blockingGo), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := f.command(t, nil)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	running := true
	t.Cleanup(func() {
		if running {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	lines := make(chan string, 32)
	go func() {
		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	var path string
	ready := false
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for !ready {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("wrapper closed stderr before fake go blocked; stdout=%s", stdout.String())
			}
			if strings.HasPrefix(line, "observable go test: log=") {
				path = strings.TrimPrefix(line, "observable go test: log=")
			}
			if line == "FAKE_GO_BLOCKED" {
				ready = true
			}
		case <-deadline.C:
			t.Fatal("fake go never reached blocking point")
		}
	}
	if path == "" {
		t.Fatal("wrapper did not announce log before product began")
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Error("interrupted wrapper unexpectedly succeeded")
	}
	running = false
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "TestInterrupted") {
		t.Errorf("partial evidence missing: %q, %v", data, err)
	}
	if filepath.Dir(path) != observableOwnedDir(f.root) {
		t.Errorf("interrupted log = %q", path)
	}
	ageObservableFile(t, path, 4321)
	f.setGo(t, "", 0)
	f.run(t, nil, 0)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expired interrupted log remains: %v", err)
	}
}

func TestGoTestObservableConcurrentRunsKeepDistinctLogs(t *testing.T) { // T14
	f := newObservableLogFixture(t, `{"Action":"pass","Package":"example","Elapsed":0.1}`+"\n", 0)
	d := observableOwnedDir(f.root)
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(d, "expired.jsonl")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	ageObservableFile(t, old, 4321)
	var cmds [4]*exec.Cmd
	var outputs [4]bytes.Buffer
	for i := range cmds {
		cmds[i] = f.command(t, nil)
		cmds[i].Stdout = &outputs[i]
		cmds[i].Stderr = &outputs[i]
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	paths := make(map[string]bool)
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Errorf("wrapper %d: %v\n%s", i, err, outputs[i].String())
			continue
		}
		output := outputs[i].String()
		if strings.Contains(output, "No such file") {
			t.Errorf("wrapper %d raced on prune:\n%s", i, output)
		}
		path := observableLogPath(t, output)
		if paths[path] {
			t.Errorf("duplicate concurrent log %q", path)
		}
		paths[path] = true
		if _, err := os.Stat(path); err != nil {
			t.Errorf("concurrent log %q missing: %v", path, err)
		}
	}
	if len(paths) != 4 {
		t.Errorf("distinct logs = %d, want 4", len(paths))
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("expired file remains: %v", err)
	}
}

type observableFileState struct {
	name  string
	size  int64
	mtime int64
}

func observableDirSnapshot(t *testing.T, dir string) []observableFileState {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []observableFileState
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, observableFileState{entry.Name(), info.Size(), info.ModTime().UnixNano()})
	}
	return files
}

func TestGoTestObservableLeavesNoDetachedHousekeeping(t *testing.T) { // T15
	f := newObservableLogFixture(t, "", 0)
	d := observableOwnedDir(f.root)
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	f.run(t, nil, 0)
	before := observableDirSnapshot(t, d)
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	after := observableDirSnapshot(t, d)
	if !slices.Equal(before, after) {
		t.Errorf("owned directory changed after wrapper exit: before=%v after=%v", before, after)
	}
}

func TestGoTestObservableRetentionOutputGrammar(t *testing.T) { // T16
	f := newObservableLogFixture(t, `{"Action":"run","Package":"example","Test":"TestProgress"}`+"\n", 0)
	cmd := f.command(t, nil)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("wrapper: %v\n%s", err, stderr.String())
	}
	path := observableLogPath(t, stderr.String())
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	logIndex := -1
	for i, line := range lines {
		if line == "observable go test: log="+path {
			logIndex = i
		}
	}
	retention := "observable go test: log-retention: wrapper-owned, pruned after 72h (set OBSERVABLE_TEST_LOG to keep a log)"
	if logIndex < 0 || logIndex+1 >= len(lines) || lines[logIndex+1] != retention || strings.Count(stderr.String(), "observable go test: log-retention:") != 1 {
		t.Errorf("retention line not immediately after unique log line:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "observable go test: command=go test -json ./internal/example") || !strings.HasSuffix(strings.TrimSpace(stderr.String()), "observable go test: PASS log="+path) {
		t.Errorf("command or terminal PASS grammar changed:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "log-retention") || strings.Contains(stdout.String(), "pruned") {
		t.Errorf("housekeeping leaked to stdout:\n%s", stdout.String())
	}
}

func TestGoTestObservableRetentionSourceGuard(t *testing.T) { // T17
	source, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "go-test-observable"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if got := strings.Count(text, "4320"); got != 1 {
		t.Errorf("retention window occurrences = %d, want one named constant", got)
	}
	if !regexp.MustCompile(`(?m)^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*=4320([[:space:]]*(#.*)?)?$`).MatchString(text) {
		t.Error("retention window must be a named shell constant")
	}
	if !regexp.MustCompile(`-maxdepth[[:space:]]+1[[:space:]]+-type[[:space:]]+f[[:space:]]+-mmin`).MatchString(text) {
		t.Error("prune must select only direct regular files by modification age")
	}
}
