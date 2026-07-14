package scripts_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type goTestShardFixture struct {
	repoRoot        string
	binDir          string
	homeDir         string
	tmpDir          string
	productArgsFile string
	productEnvFile  string
	probeFile       string
}

func newGoTestShardFixture(t *testing.T) goTestShardFixture {
	t.Helper()

	repoRoot := repoRoot(t)
	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	productArgsFile := filepath.Join(tmpDir, "product-args")
	productEnvFile := filepath.Join(tmpDir, "product-env")
	probeFile := filepath.Join(tmpDir, "metadata-probes")
	fakeGo := fmt.Sprintf(`#!/bin/sh
set -eu
case "${1:-}" in
  env)
    case "${2:-}" in
      GOPATH) printf '%%s\n' %q ;;
      GOCACHE) printf '%%s\n' %q ;;
      GOMODCACHE) printf '%%s\n' %q ;;
      GOTMPDIR) printf '%%s\n' %q ;;
      GOROOT) printf '%%s\n' %q ;;
      *) exit 99 ;;
    esac
    ;;
  list)
    [ "${2:-}" = "-m" ] || exit 99
    printf 'go-list-module\n' >> %q
    printf '%%s\n' 'github.com/gastownhall/gascity'
    ;;
  test)
    is_list=0
    is_json=0
    for arg in "$@"; do
      [ "$arg" != "-list" ] || is_list=1
      [ "$arg" != "-json" ] || is_json=1
    done
    if [ "$is_list" = 1 ]; then
      printf '%%s\n' TestAlpha TestBeta TestGamma 'ok  github.com/gastownhall/gascity/example  0.001s'
      exit 0
    fi
    printf '%%s\n' "$@" > %q
    env | LC_ALL=C sort > %q
    if [ "$is_json" = 1 ]; then
      printf '%%s\n' \
        '{"Action":"run","Package":"github.com/gastownhall/gascity/example","Test":"TestAlpha"}' \
        '{"Action":"fail","Package":"github.com/gastownhall/gascity/example","Test":"TestAlpha","Elapsed":0.25}' \
        '{"Action":"run","Package":"github.com/gastownhall/gascity/example","Test":"TestGamma"}' \
        '{"Action":"pass","Package":"github.com/gastownhall/gascity/example","Test":"TestGamma","Elapsed":0.125}' \
        '{"Action":"fail","Package":"github.com/gastownhall/gascity/example","Elapsed":0.3}'
    fi
    exit %d
    ;;
  *) exit 99 ;;
esac
`, filepath.Join(tmpDir, "gopath"), filepath.Join(tmpDir, "gocache"), filepath.Join(tmpDir, "gomodcache"), filepath.Join(tmpDir, "gotmp"), filepath.Join(tmpDir, "goroot"), probeFile, productArgsFile, productEnvFile, 23)
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "uname"), []byte("#!/bin/sh\n[ \"$#\" -eq 0 ] || exit 99\nprintf 'Linux\\n'\n"), 0o755); err != nil {
		t.Fatalf("write fake uname: %v", err)
	}
	fakeGetconf := fmt.Sprintf("#!/bin/sh\n[ \"${1:-}\" = '_NPROCESSORS_ONLN' ] || exit 99\nprintf 'getconf\\n' >> %q\nprintf '16\\n'\n", probeFile)
	if err := os.WriteFile(filepath.Join(binDir, "getconf"), []byte(fakeGetconf), 0o755); err != nil {
		t.Fatalf("write fake getconf: %v", err)
	}

	return goTestShardFixture{
		repoRoot:        repoRoot,
		binDir:          binDir,
		homeDir:         filepath.Join(tmpDir, "home"),
		tmpDir:          tmpDir,
		productArgsFile: productArgsFile,
		productEnvFile:  productEnvFile,
		probeFile:       probeFile,
	}
}

func (f goTestShardFixture) command(extraEnv ...string) *exec.Cmd {
	cmd := goTestShardCommand(f.repoRoot, "./example", "1", "2")
	cmd.Dir = f.repoRoot
	cmd.Env = append([]string{
		"PATH=" + f.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + f.homeDir,
		"SHELL=/bin/sh",
		"TMPDIR=" + f.tmpDir,
		"GO_TEST_TIMEOUT=1m",
		"GC_TEST_NO_SLICE=1",
		"SYS_USR_CGO_FALLBACK=0",
	}, extraEnv...)
	return cmd
}

func goTestShardCommand(repoRoot string, args ...string) *exec.Cmd {
	return exec.Command(filepath.Join(repoRoot, "scripts", "test-go-test-shard"), args...)
}

func runShardCommand(t *testing.T, cmd *exec.Cmd) (int, []byte) {
	t.Helper()
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, out
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run test-go-test-shard: %v\n%s", err, out)
	}
	return exitErr.ExitCode(), out
}

func readFixtureFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func fixtureEnvironment(t *testing.T, data string) map[string]string {
	t.Helper()
	environment := make(map[string]string)
	for _, entry := range strings.Split(strings.TrimSpace(data), "\n") {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed environment entry %q", entry)
		}
		environment[name] = value
	}
	for _, shellOwned := range []string{"PWD", "SHLVL", "_"} {
		delete(environment, shellOwned)
	}
	return environment
}

func TestGoTestShardWithoutTimingPreservesDirectProductContract(t *testing.T) {
	t.Parallel()

	fixture := newGoTestShardFixture(t)
	cmd := fixture.command(
		"GO_TEST_TIMING_NAME=ignored-control",
		"GO_TEST_TIMING_VARIANT=ignored-control",
		"GO_TEST_RUNNER_LABEL=ignored-control",
		"GITHUB_SHA=ignored-control",
		"RUNNER_OS=ignored-control",
		"SHOULD_NOT_LEAK=ignored-control",
	)
	status, output := runShardCommand(t, cmd)
	if status != 23 {
		t.Fatalf("shard exit = %d, want product exit 23\n%s", status, output)
	}

	wantArgs := "test\n-timeout\n1m\n./example\n-run\n^(TestAlpha|TestGamma)$\n"
	if got := readFixtureFile(t, fixture.productArgsFile); got != wantArgs {
		t.Fatalf("direct product argv:\n%s\nwant:\n%s", got, wantArgs)
	}
	wantEnv := map[string]string{
		"PATH": fixture.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME": fixture.homeDir, "USER": "", "LOGNAME": "", "SHELL": "/bin/sh",
		"LANG": "C.UTF-8", "TMPDIR": fixture.tmpDir, "XDG_RUNTIME_DIR": "",
		"GOPATH": filepath.Join(fixture.tmpDir, "gopath"), "GOCACHE": filepath.Join(fixture.tmpDir, "gocache"),
		"GOMODCACHE": filepath.Join(fixture.tmpDir, "gomodcache"), "GOTMPDIR": filepath.Join(fixture.tmpDir, "gotmp"),
		"GOROOT": filepath.Join(fixture.tmpDir, "goroot"), "GOENV": "", "GOFLAGS": "", "GO111MODULE": "",
		"GOEXPERIMENT": "", "GOPROXY": "", "GOPRIVATE": "", "GONOPROXY": "", "GONOSUMDB": "",
		"GOSUMDB": "", "GOINSECURE": "", "GOVCS": "", "GOWORK": "", "GC_FAST_UNIT": "0",
		"CGO_CPPFLAGS": "", "CGO_LDFLAGS": "", "GC_TEST_SHARD_INDEX": "1", "GC_TEST_SHARD_TOTAL": "2",
	}
	if got := fixtureEnvironment(t, readFixtureFile(t, fixture.productEnvFile)); !maps.Equal(got, wantEnv) {
		t.Fatalf("direct product environment = %#v, want %#v", got, wantEnv)
	}
	if probes, err := os.ReadFile(fixture.probeFile); err == nil {
		t.Fatalf("timing-disabled shard ran metadata probes:\n%s", probes)
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect timing-disabled metadata probes: %v", err)
	}
}

func TestGoTestShardTimingUsesObservableMetadataWithoutChangingProductStatus(t *testing.T) {
	t.Parallel()

	fixture := newGoTestShardFixture(t)
	timingDir := filepath.Join(fixture.tmpDir, "timing artifacts")
	if err := os.Mkdir(timingDir, 0o755); err != nil {
		t.Fatalf("create timing directory: %v", err)
	}
	timingFile := filepath.Join(timingDir, "shard timing.json")
	cmd := fixture.command(
		"GO_TEST_TIMING_FILE="+timingFile,
		"GO_TEST_TIMING_NAME=cmd-gc-process-1-of-2",
		"GO_TEST_TIMING_VARIANT=linux-default",
		"GO_TEST_RUNNER_LABEL=blacksmith-32vcpu",
		"GO_TEST_RUNNER_CPU_COUNT=32",
		"GITHUB_SHA=abc123",
		"GITHUB_WORKFLOW=CI",
		"GITHUB_RUN_ID=77",
		"GITHUB_RUN_ATTEMPT=2",
		"GITHUB_JOB=cmd-gc-process",
		"RUNNER_NAME=runner-9",
		"RUNNER_OS=Linux",
		"RUNNER_ARCH=X64",
		"OBSERVABLE_VARIANT=must-not-leak",
	)
	status, output := runShardCommand(t, cmd)
	if status != 23 {
		t.Fatalf("shard exit = %d, want product exit 23\n%s", status, output)
	}

	wantArgs := "test\n-json\n-timeout\n1m\n./example\n-run\n^(TestAlpha|TestGamma)$\n"
	if got := readFixtureFile(t, fixture.productArgsFile); got != wantArgs {
		t.Fatalf("observable product argv:\n%s\nwant:\n%s", got, wantArgs)
	}
	productEnv := readFixtureFile(t, fixture.productEnvFile)
	if !strings.Contains(productEnv, "GC_TEST_NO_SLICE=1\n") {
		t.Fatalf("observable wrapper lost explicit slice opt-out:\n%s", productEnv)
	}
	for _, forbidden := range []string{
		"GO_TEST_TIMING_", "GO_TEST_RUNNER_", "GITHUB_", "RUNNER_", "OBSERVABLE_",
	} {
		for _, entry := range strings.Split(productEnv, "\n") {
			if strings.HasPrefix(entry, forbidden) {
				t.Errorf("observable product environment leaked %q via %q", forbidden, entry)
			}
		}
	}

	data, err := os.ReadFile(timingFile)
	if err != nil {
		t.Fatalf("read timing artifact: %v\n%s", err, output)
	}
	var artifact observableTimingArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode timing artifact: %v\n%s", err, data)
	}
	if artifact.ShardID != "cmd-gc-process-1-of-2" || artifact.Variant != "linux-default" {
		t.Fatalf("timing identity = shard %q variant %q", artifact.ShardID, artifact.Variant)
	}
	if artifact.CommitSHA != "abc123" || artifact.Workflow != "CI" || artifact.RunID != "77" || artifact.RunAttempt != "2" || artifact.Job != "cmd-gc-process" {
		t.Fatalf("timing run metadata = %+v", artifact)
	}
	wantRunner := (observableTimingRunner{Label: "blacksmith-32vcpu", Name: "runner-9", OS: "Linux", Arch: "X64", CPUCount: 32})
	if artifact.Runner != wantRunner {
		t.Fatalf("timing runner = %+v, want %+v", artifact.Runner, wantRunner)
	}
	wantUnits := []observableTimingUnit{
		{
			UnitID: "example:TestAlpha", Kind: "test", Package: "github.com/gastownhall/gascity/example",
			Test: "TestAlpha", Outcome: "fail", DurationSeconds: 0.25,
		},
		{
			UnitID: "example:TestGamma", Kind: "test", Package: "github.com/gastownhall/gascity/example",
			Test: "TestGamma", Outcome: "pass", DurationSeconds: 0.125,
		},
	}
	found := make(map[string]bool, len(wantUnits))
	for _, unit := range artifact.Units {
		if unit.Test == "TestBeta" {
			t.Fatalf("timing artifact included unselected test: %+v", artifact.Units)
		}
		for _, want := range wantUnits {
			if unit == want {
				found[want.Test] = true
			}
		}
	}
	for _, want := range wantUnits {
		if !found[want.Test] {
			t.Errorf("timing units do not contain %+v: %+v", want, artifact.Units)
		}
	}
	if got := readFixtureFile(t, fixture.probeFile); got != "go-list-module\n" {
		t.Fatalf("timing metadata probes = %q, want only module discovery", got)
	}
}

func TestGoTestShardTimingDefaultsMetadataFromSelectedShard(t *testing.T) {
	t.Parallel()

	fixture := newGoTestShardFixture(t)
	timingFile := filepath.Join(fixture.tmpDir, "timing.json")
	status, output := runShardCommand(t, fixture.command("GO_TEST_TIMING_FILE="+timingFile))
	if status != 23 {
		t.Fatalf("shard exit = %d, want product exit 23\n%s", status, output)
	}

	data, err := os.ReadFile(timingFile)
	if err != nil {
		t.Fatalf("read timing artifact: %v\n%s", err, output)
	}
	var artifact observableTimingArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode timing artifact: %v\n%s", err, data)
	}
	if artifact.ShardID != "example-shard-1-of-2" || artifact.Variant != "default" {
		t.Fatalf("default timing identity = shard %q variant %q", artifact.ShardID, artifact.Variant)
	}
	if artifact.CommitSHA != "" || artifact.Workflow != "" || artifact.RunID != "" || artifact.RunAttempt != "" || artifact.Job != "" {
		t.Fatalf("default timing run metadata = %+v", artifact)
	}
	wantRunner := (observableTimingRunner{CPUCount: 16})
	if artifact.Runner != wantRunner {
		t.Fatalf("default timing runner = %+v, want %+v", artifact.Runner, wantRunner)
	}
	if got := readFixtureFile(t, fixture.probeFile); got != "getconf\ngo-list-module\n" {
		t.Fatalf("default timing metadata probes = %q", got)
	}
}

func TestGoTestShardTimingArtifactFailureIsAdvisory(t *testing.T) {
	t.Parallel()

	fixture := newGoTestShardFixture(t)
	timingFile := filepath.Join(fixture.tmpDir, "missing", "timing.json")
	status, output := runShardCommand(t, fixture.command(
		"GO_TEST_TIMING_FILE="+timingFile,
		"GO_TEST_RUNNER_CPU_COUNT=8",
	))
	if status != 23 {
		t.Fatalf("shard exit = %d, want product exit 23\n%s", status, output)
	}
	if _, err := os.Stat(timingFile); !os.IsNotExist(err) {
		t.Fatalf("unwritable timing path produced an artifact: err=%v", err)
	}
	if !strings.Contains(string(output), "timing directory does not exist") {
		t.Fatalf("shard did not report advisory timing failure:\n%s", output)
	}
	wantArgs := "test\n-json\n-timeout\n1m\n./example\n-run\n^(TestAlpha|TestGamma)$\n"
	if got := readFixtureFile(t, fixture.productArgsFile); got != wantArgs {
		t.Fatalf("advisory failure changed product argv:\n%s\nwant:\n%s", got, wantArgs)
	}
}

func TestGoTestShardPreservesAcceptanceAuthEnv(t *testing.T) {
	repoRoot := filepath.Dir(t.TempDir())
	if wd, err := os.Getwd(); err == nil {
		repoRoot = filepath.Dir(wd)
	}

	cmd := exec.Command(
		filepath.Join(repoRoot, "scripts", "test-go-test-shard"),
		"./scripts/testdata/test-go-test-shard/env_required",
		"1",
		"1",
	)
	cmd.Dir = repoRoot
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GO_TEST_TIMEOUT=1m",
		"ANTHROPIC_AUTH_TOKEN=synthetic-token",
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-go-test-shard failed: %v\n%s", err, out)
	}
}

func TestGoTestShardRunsWithoutPreservedProviderEnv(t *testing.T) {
	repoRoot := filepath.Dir(t.TempDir())
	if wd, err := os.Getwd(); err == nil {
		repoRoot = filepath.Dir(wd)
	}

	cmd := goTestShardCommand(
		repoRoot,
		"./scripts/testdata/test-go-test-shard/no_extra_env",
		"1",
		"1",
	)
	cmd.Dir = repoRoot
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GO_TEST_TIMEOUT=1m",
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-go-test-shard failed without preserved provider env: %v\n%s", err, out)
	}
}
