package scripts_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	allTestArgsFile string
	probeFile       string
}

func newGoTestShardFixture(t *testing.T) goTestShardFixture {
	t.Helper()
	return newGoTestShardFixtureWithExit(t, 23)
}

func newGoTestShardFixtureWithExit(t *testing.T, productExit int) goTestShardFixture {
	t.Helper()

	repoRoot := repoRoot(t)
	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	productArgsFile := filepath.Join(tmpDir, "product-args")
	productEnvFile := filepath.Join(tmpDir, "product-env")
	allTestArgsFile := filepath.Join(tmpDir, "all-test-args")
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
    printf '%%s\n' "$@" >> %q
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
    printf '%%s\n' "$@" >> %q
    env | LC_ALL=C sort >> %q
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
`, filepath.Join(tmpDir, "gopath"), filepath.Join(tmpDir, "gocache"), filepath.Join(tmpDir, "gomodcache"), filepath.Join(tmpDir, "gotmp"), filepath.Join(tmpDir, "goroot"), probeFile, allTestArgsFile, productArgsFile, productEnvFile, productExit)
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
		allTestArgsFile: allTestArgsFile,
		probeFile:       probeFile,
	}
}

func (f goTestShardFixture) command(extraEnv ...string) *exec.Cmd {
	return f.commandForShard("1", "2", extraEnv...)
}

func (f goTestShardFixture) commandForShard(shardIndex, shardTotal string, extraEnv ...string) *exec.Cmd {
	return f.commandForShardWithBash("", shardIndex, shardTotal, extraEnv...)
}

func (f goTestShardFixture) commandForShardWithBash(bashPath, shardIndex, shardTotal string, extraEnv ...string) *exec.Cmd {
	cmd := goTestShardCommand(f.repoRoot, "./example", shardIndex, shardTotal)
	if bashPath != "" {
		cmd = shardTestCommand(
			bashPath,
			filepath.Join(f.repoRoot, "scripts", "test-go-test-shard"),
			"./example",
			shardIndex,
			shardTotal,
		)
	}
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

func writeGoTestManifest(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, "tests.manifest")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write test manifest: %v", err)
	}
	return path
}

func goTestShardCommand(repoRoot string, args ...string) *exec.Cmd {
	return shardTestCommand(filepath.Join(repoRoot, "scripts", "test-go-test-shard"), args...)
}

func shardTestCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
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

func TestProviderOverridesAndSuiteContractsCrossMakeIsolation(t *testing.T) {
	t.Parallel()

	acceptanceFlags := map[string]string{"-tags": "acceptance_a"}
	bdstoreFlags := map[string]string{
		"-tags": "integration",
		"-run":  "^(TestBdStoreConformance|TestBdStoreMailWispInsert)$",
	}
	tests := []struct {
		name         string
		target       string
		envName      string
		provider     string
		exitCode     int
		wantFlags    map[string]string
		wantPackages []string
	}{
		{name: "acceptance sqlite", target: "test-acceptance", envName: "GC_ACCEPTANCE_BEADS_PROVIDER", provider: "sqlite", exitCode: 23, wantFlags: acceptanceFlags, wantPackages: []string{"./test/acceptance/..."}},
		{name: "acceptance file", target: "test-acceptance", envName: "GC_ACCEPTANCE_BEADS_PROVIDER", provider: "file", exitCode: 37, wantFlags: acceptanceFlags, wantPackages: []string{"./test/acceptance/..."}},
		{name: "acceptance default", target: "test-acceptance", envName: "GC_ACCEPTANCE_BEADS_PROVIDER", exitCode: 23, wantFlags: acceptanceFlags, wantPackages: []string{"./test/acceptance/..."}},
		{name: "integration sqlite", target: "test-integration-bdstore", envName: "GC_BEADS", provider: "sqlite", exitCode: 37, wantFlags: bdstoreFlags, wantPackages: []string{"./test/integration"}},
		{name: "integration file", target: "test-integration-bdstore", envName: "GC_BEADS", provider: "file", exitCode: 23, wantFlags: bdstoreFlags, wantPackages: []string{"./test/integration"}},
		{name: "integration default", target: "test-integration-bdstore", envName: "GC_BEADS", exitCode: 37, wantFlags: bdstoreFlags, wantPackages: []string{"./test/integration"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newGoTestShardFixtureWithExit(t, tt.exitCode)
			cmd := exec.Command("make", "--no-print-directory", "--silent", tt.target)
			cmd.Dir = fixture.repoRoot
			cmd.Env = []string{
				"PATH=" + fixture.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"HOME=" + fixture.homeDir,
				"SHELL=/bin/sh",
				"LANG=C.UTF-8",
				"TMPDIR=" + fixture.tmpDir,
				"GC_TEST_NO_SLICE=1",
				"SYS_USR_CGO_FALLBACK=0",
				"GOFLAGS=-run=^$",
				"GOENV=/host/goenv",
				"GOWORK=/host/go.work",
				"GC_CITY=host-city",
				"GC_HOME=/host/gc",
				"GC_DOLT_PORT=13306",
				"BEADS_DOLT_SERVER_PORT=13307",
			}
			if tt.provider != "" {
				cmd.Env = append(cmd.Env, tt.envName+"="+tt.provider)
			}

			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), fmt.Sprintf("Error %d", tt.exitCode)) {
				t.Fatalf("make %s did not preserve fake go exit %d: %v\n%s", tt.target, tt.exitCode, err, output)
			}
			captured := fixtureEnvironment(t, readFixtureFile(t, fixture.productEnvFile))
			if got := captured[tt.envName]; got != tt.provider {
				t.Fatalf("make %s passed %s=%q to go, want %q", tt.target, tt.envName, got, tt.provider)
			}
			for _, name := range []string{"GC_CITY", "GC_HOME", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_PORT"} {
				if value, ok := captured[name]; ok {
					t.Errorf("make %s leaked host %s=%q to go", tt.target, name, value)
				}
			}
			for name, want := range map[string]string{"GOFLAGS": "", "GOENV": "off", "GOWORK": "off"} {
				if got := captured[name]; got != want {
					t.Errorf("make %s passed %s=%q, want deterministic %q", tt.target, name, got, want)
				}
			}
			wantFastUnit := ""
			if tt.target == "test-integration-bdstore" {
				wantFastUnit = "0"
			}
			if got := captured["GC_FAST_UNIT"]; got != wantFastUnit {
				t.Errorf("make %s passed GC_FAST_UNIT=%q, want %q", tt.target, got, wantFastUnit)
			}

			productArgs := readFixtureFile(t, fixture.productArgsFile)
			if allArgs := readFixtureFile(t, fixture.allTestArgsFile); allArgs != productArgs {
				t.Fatalf("make %s ran unapproved go test discovery/decoy calls:\n%s", tt.target, allArgs)
			}
			assertGoTestInvocation(t, productArgs, tt.wantFlags, tt.wantPackages)
		})
	}
}

func assertGoTestInvocation(t *testing.T, raw string, wantFlags map[string]string, wantPackages []string) {
	t.Helper()

	args := strings.Split(strings.TrimSpace(raw), "\n")
	if len(args) == 0 || args[0] != "test" {
		t.Fatalf("go arguments = %v, want one go test invocation", args)
	}
	gotFlags := make(map[string]string, len(wantFlags))
	var gotPackages []string
	for i := 1; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			gotPackages = append(gotPackages, args[i])
			continue
		}

		flag, value, joined := strings.Cut(args[i], "=")
		if flag != "-tags" && flag != "-timeout" && flag != "-run" {
			t.Fatalf("go arguments contain unsupported flag %q: %v", flag, args)
		}
		if _, duplicate := gotFlags[flag]; duplicate {
			t.Fatalf("go arguments repeat %q: %v", flag, args)
		}
		if !joined {
			i++
			if i == len(args) {
				t.Fatalf("go argument %q has no value: %v", flag, args)
			}
			value = args[i]
		}
		gotFlags[flag] = value
	}
	if timeout := gotFlags["-timeout"]; timeout == "" {
		t.Fatalf("go invocation has no explicit timeout: %v", args)
	}
	delete(gotFlags, "-timeout")
	if !maps.Equal(gotFlags, wantFlags) || !slices.Equal(gotPackages, wantPackages) {
		t.Fatalf("go invocation flags/packages = %v / %v, want %v / %v", gotFlags, gotPackages, wantFlags, wantPackages)
	}
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

func TestGoTestShardManifestSkipsDiscoveryAndPreservesModuloSelection(t *testing.T) {
	t.Parallel()

	fixture := newGoTestShardFixture(t)
	manifest := writeGoTestManifest(t, fixture.tmpDir,
		"# Source-checked fixture inventory.",
		"",
		"TestAlpha",
		"TestBeta",
		"TestGamma",
		"TestDelta",
		"TestEpsilon",
	)
	status, output := runShardCommand(t, fixture.commandForShard("2", "3", "GO_TEST_MANIFEST="+manifest))
	if status != 23 {
		t.Fatalf("manifest shard exit = %d, want product exit 23\n%s", status, output)
	}

	wantArgs := "test\n-timeout\n1m\n./example\n-run\n^(TestBeta|TestEpsilon)$\n"
	if got := readFixtureFile(t, fixture.productArgsFile); got != wantArgs {
		t.Fatalf("manifest product argv:\n%s\nwant:\n%s", got, wantArgs)
	}
	if allArgs := readFixtureFile(t, fixture.allTestArgsFile); allArgs != wantArgs {
		t.Fatalf("manifest mode ran discovery or extra go test invocations:\n%s\nwant exactly one final invocation:\n%s", allArgs, wantArgs)
	}
}

func TestGoTestShardManifestFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		manifest  func(*testing.T, goTestShardFixture) string
		wantError string
	}{
		{
			name: "unreadable",
			manifest: func(_ *testing.T, fixture goTestShardFixture) string {
				return filepath.Join(fixture.tmpDir, "missing.manifest")
			},
			wantError: "go test manifest is not readable",
		},
		{
			name: "empty",
			manifest: func(t *testing.T, fixture goTestShardFixture) string {
				return writeGoTestManifest(t, fixture.tmpDir, "# comments are not entries", "")
			},
			wantError: "go test manifest contains no tests",
		},
		{
			name: "invalid regex syntax",
			manifest: func(t *testing.T, fixture goTestShardFixture) string {
				return writeGoTestManifest(t, fixture.tmpDir, "TestAlpha|TestBeta")
			},
			wantError: "invalid go test manifest entry",
		},
		{
			name: "malformed whitespace",
			manifest: func(t *testing.T, fixture goTestShardFixture) string {
				return writeGoTestManifest(t, fixture.tmpDir, "Test Alpha")
			},
			wantError: "invalid go test manifest entry",
		},
		{
			name: "duplicate",
			manifest: func(t *testing.T, fixture goTestShardFixture) string {
				return writeGoTestManifest(t, fixture.tmpDir, "TestAlpha", "TestAlpha")
			},
			wantError: "duplicate go test manifest entry: TestAlpha",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newGoTestShardFixtureWithExit(t, 0)
			manifest := tt.manifest(t, fixture)
			status, output := runShardCommand(t, fixture.command("GO_TEST_MANIFEST="+manifest))
			if status == 0 {
				t.Fatalf("invalid manifest unexpectedly succeeded:\n%s", output)
			}
			if !strings.Contains(string(output), tt.wantError) {
				t.Fatalf("manifest error output:\n%s\nwant substring %q", output, tt.wantError)
			}
			if allArgs, err := os.ReadFile(fixture.allTestArgsFile); err == nil {
				t.Fatalf("invalid manifest invoked go test:\n%s", allArgs)
			} else if !os.IsNotExist(err) {
				t.Fatalf("inspect invalid-manifest go test calls: %v", err)
			}
		})
	}
}

func TestGoTestShardManifestRejectsNULBytesBeforeInvokingGo(t *testing.T) {
	t.Parallel()

	fixture := newGoTestShardFixtureWithExit(t, 0)
	manifest := filepath.Join(fixture.tmpDir, "nul.manifest")
	if err := os.WriteFile(manifest, []byte("TestAl\x00pha\n"), 0o644); err != nil {
		t.Fatalf("write NUL manifest: %v", err)
	}

	status, output := runShardCommand(t, fixture.command("GO_TEST_MANIFEST="+manifest))
	if status == 0 {
		t.Fatalf("NUL manifest unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "NUL bytes or could not be validated") {
		t.Fatalf("NUL manifest error output:\n%s\nwant NUL/malformed diagnostic", output)
	}
	if allArgs, err := os.ReadFile(fixture.allTestArgsFile); err == nil {
		t.Fatalf("NUL manifest invoked go test:\n%s", allArgs)
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect NUL-manifest go test calls: %v", err)
	}
}

func TestGoTestShardManifestAcceptsFirstEntryWithBash32Nounset(t *testing.T) {
	t.Parallel()

	fixture := newGoTestShardFixture(t)
	manifest := writeGoTestManifest(t, fixture.tmpDir, "TestAlpha")
	bashPath := findBash32(t)
	if bashPath == "" {
		assertManifestDuplicateScanGuardsEmptyArray(t, filepath.Join(fixture.repoRoot, "scripts", "test-go-test-shard"))
	}

	cmd := fixture.commandForShardWithBash(bashPath, "1", "1", "GO_TEST_MANIFEST="+manifest)
	status, output := runShardCommand(t, cmd)
	if status != 23 {
		t.Fatalf("single-entry manifest shard exit = %d, want product exit 23 (bash=%q)\n%s", status, bashPath, output)
	}
	wantArgs := "test\n-timeout\n1m\n./example\n-run\n^(TestAlpha)$\n"
	if got := readFixtureFile(t, fixture.productArgsFile); got != wantArgs {
		t.Fatalf("single-entry manifest product argv:\n%s\nwant:\n%s", got, wantArgs)
	}
	if allArgs := readFixtureFile(t, fixture.allTestArgsFile); allArgs != wantArgs {
		t.Fatalf("single-entry manifest ran discovery or extra go test invocations:\n%s", allArgs)
	}
}

func findBash32(t *testing.T) string {
	t.Helper()
	candidates := []string{os.Getenv("BASH32"), "bash3.2", "bash-3.2", "bash"}
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		output, err := shardTestCommand(path, "--version").CombinedOutput()
		if err == nil && strings.Contains(string(output), "version 3.2") {
			return path
		}
	}
	return ""
}

func assertManifestDuplicateScanGuardsEmptyArray(t *testing.T, scriptPath string) {
	t.Helper()
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read test-go-test-shard source: %v", err)
	}
	source := string(content)
	if count := strings.Count(source, `for existing in "${tests[@]}"; do`); count != 1 {
		t.Fatalf("manifest duplicate scans = %d, want exactly one", count)
	}
	countIndex := strings.Index(source, "manifest_test_count=0")
	guardIndex := strings.Index(source, "if (( manifest_test_count > 0 )); then")
	loopIndex := strings.Index(source, `for existing in "${tests[@]}"; do`)
	appendIndex := strings.Index(source, `tests+=("$line")`)
	incrementIndex := strings.Index(source, "manifest_test_count=$((manifest_test_count + 1))")
	guardEnd := -1
	if loopIndex >= 0 {
		if relative := strings.Index(source[loopIndex:], "\n    fi\n"); relative >= 0 {
			guardEnd = loopIndex + relative
		}
	}
	if countIndex < 0 || countIndex >= guardIndex || guardIndex >= loopIndex || loopIndex >= guardEnd || guardEnd >= appendIndex || appendIndex >= incrementIndex {
		t.Fatalf("manifest duplicate scan is not structurally guarded before the first array append")
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
