package k8s

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestControllerScriptDeployProjectsOnlyExplicitCanonicalDoltTarget(t *testing.T) {
	result := runControllerScriptDeploy(t, controllerScriptDeployOptions{
		Env: map[string]string{
			"GC_DOLT_HOST":     "canonical-dolt.example.com",
			"GC_DOLT_PORT":     "4406",
			"GC_K8S_DOLT_HOST": "legacy-dolt.example.com",
			"GC_K8S_DOLT_PORT": "3308",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-controller-k8s deploy error = %v\noutput:\n%s", result.err, result.output)
	}
	if got := result.manifestEnv["GC_DOLT_HOST"]; got != "canonical-dolt.example.com" {
		t.Fatalf("manifest GC_DOLT_HOST = %q, want canonical-dolt.example.com", got)
	}
	if got := result.manifestEnv["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("manifest GC_DOLT_PORT = %q, want 4406", got)
	}
	if strings.Contains(result.callLog, "legacy-dolt.example.com") || strings.Contains(result.callLog, "3308") {
		t.Fatalf("controller bootstrap leaked deprecated K8s Dolt target into bootstrap path:\n%s", result.callLog)
	}
	assertCallContains(t, result.callLog, "gc bd init -p 'gc' --skip-hooks")
	assertCallContains(t, result.callLog, "gc bd --rig 'frontend' init -p 'fe' --skip-hooks")
}

func TestControllerScriptDeployDoesNotProjectDeprecatedK8sDoltTarget(t *testing.T) {
	clearDoltAndCityEnv(t)
	result := runControllerScriptDeploy(t, controllerScriptDeployOptions{
		Env: map[string]string{
			"GC_K8S_DOLT_HOST": "legacy-dolt.example.com",
			"GC_K8S_DOLT_PORT": "3308",
		},
	})
	if result.err != nil {
		t.Fatalf("gc-controller-k8s deploy error = %v\noutput:\n%s", result.err, result.output)
	}
	if _, ok := result.manifestEnv["GC_DOLT_HOST"]; ok {
		t.Fatalf("manifest projected GC_DOLT_HOST from deprecated compatibility input: %#v", result.manifestEnv)
	}
	if _, ok := result.manifestEnv["GC_DOLT_PORT"]; ok {
		t.Fatalf("manifest projected GC_DOLT_PORT from deprecated compatibility input: %#v", result.manifestEnv)
	}
	if strings.Contains(result.callLog, "legacy-dolt.example.com") || strings.Contains(result.callLog, "3308") {
		t.Fatalf("controller bootstrap used deprecated K8s Dolt target directly:\n%s", result.callLog)
	}
	assertCallContains(t, result.callLog, "gc bd init -p 'gc' --skip-hooks")
	assertCallContains(t, result.callLog, "gc bd --rig 'frontend' init -p 'fe' --skip-hooks")
}

func TestControllerScriptDeployUsesResolvedConfigPrefixesForBootstrap(t *testing.T) {
	clearDoltAndCityEnv(t)
	result := runControllerScriptDeploy(t, controllerScriptDeployOptions{
		Env: map[string]string{
			"GC_DOLT_HOST": "canonical-dolt.example.com",
			"GC_DOLT_PORT": "4406",
		},
		CityToml: `[workspace]
name = "sample-city"
includes = ["prefixes.toml"]

[[rigs]]
name = "frontend"
path = "frontend"
`,
		ResolvedConfig: `[workspace]
name = "sample-city"
prefix = "hqx"

[[rigs]]
name = "frontend"
prefix = "ui"
path = "frontend"
`,
	})
	if result.err != nil {
		t.Fatalf("gc-controller-k8s deploy error = %v\noutput:\n%s", result.err, result.output)
	}
	assertCallContains(t, result.callLog, "gc bd init -p 'hqx' --skip-hooks")
	assertCallContains(t, result.callLog, "gc bd --rig 'frontend' init -p 'ui' --skip-hooks")
	assertCallNotContains(t, result.callLog, "gc bd init -p 'sc' --skip-hooks")
	assertCallNotContains(t, result.callLog, "gc bd --rig 'frontend' init -p 'fr' --skip-hooks")
}

func TestControllerScriptDeployParsesWorkspaceSectionOnlyForBootstrapPrefix(t *testing.T) {
	clearDoltAndCityEnv(t)
	result := runControllerScriptDeploy(t, controllerScriptDeployOptions{
		CityToml: `[workspace]
name = "sample-city"
prefix = "gc"

[[rigs]]
name = "frontend"
prefix = "fe"
path = "frontend"
`,
		ResolvedConfig: strings.Join([]string{
			`name = "wrong-top-level"`,
			`prefix = "bad-top-level"`,
			``,
			`[workspace.extra]`,
			`name = "wrong-subtable"`,
			`prefix = "bad-subtable"`,
			``,
			`[workspace]   `,
			`  # Comments and whitespace in this section should not hide the real values.`,
			`  name = "sample-city"   # trailing comment`,
			`  prefix = "hqx"   `,
			`  prefix = "duplicate-ignored"`,
			``,
			`[orders]`,
			`prefix = "bad-next-section"`,
			``,
			`[[rigs]]`,
			`name = "frontend"`,
			`prefix = "ui"`,
			`path = "frontend"`,
		}, "\n") + "\n",
	})
	if result.err != nil {
		t.Fatalf("gc-controller-k8s deploy error = %v\noutput:\n%s", result.err, result.output)
	}
	assertCallContains(t, result.callLog, "gc bd init -p 'hqx' --skip-hooks")
	assertCallContains(t, result.callLog, "gc bd --rig 'frontend' init -p 'ui' --skip-hooks")
	assertCallNotContains(t, result.callLog, "bad-top-level")
	assertCallNotContains(t, result.callLog, "bad-subtable")
	assertCallNotContains(t, result.callLog, "duplicate-ignored")
	assertCallNotContains(t, result.callLog, "bad-next-section")
}

func TestControllerScriptDeployBootstrapsAfterStartSignalAndLogProbe(t *testing.T) {
	result := runControllerScriptDeploy(t, controllerScriptDeployOptions{
		LogOutputs: []string{"still starting", "City started."},
	})
	if result.err != nil {
		t.Fatalf("gc-controller-k8s deploy error = %v\noutput:\n%s", result.err, result.output)
	}
	startIdx := lineIndexContaining(result.callLog, "exec gc-controller -- touch /city/.gc-start")
	logIdx := lineIndexContaining(result.callLog, "logs gc-controller --tail=50")
	initIdx := lineIndexContaining(result.callLog, "exec gc-controller -- sh -c cd /city && gc bd init -p 'gc' --skip-hooks")
	if startIdx == -1 || logIdx == -1 || initIdx == -1 {
		t.Fatalf("missing expected startup calls in log:\n%s", result.callLog)
	}
	if startIdx >= logIdx || logIdx >= initIdx {
		t.Fatalf("bootstrap ordering violated: touch=%d logs=%d init=%d\n%s", startIdx, logIdx, initIdx, result.callLog)
	}
}

func TestControllerScriptDeployBootstrapsWhenLogsNeverMatch(t *testing.T) {
	result := runControllerScriptDeploy(t, controllerScriptDeployOptions{
		LogOutputs: []string{"still starting"},
	})
	if result.err != nil {
		t.Fatalf("gc-controller-k8s deploy error = %v\noutput:\n%s", result.err, result.output)
	}
	if !strings.Contains(result.output, "Controller logs did not confirm startup; attempting bootstrap anyway.") {
		t.Fatalf("deploy output did not report fallback bootstrap path:\n%s", result.output)
	}
	assertCallContains(t, result.callLog, "gc bd init -p 'gc' --skip-hooks")
	assertCallContains(t, result.callLog, "gc bd --rig 'frontend' init -p 'fe' --skip-hooks")
}

func TestControllerScriptDeployFailsWhenBootstrapFails(t *testing.T) {
	result := runControllerScriptDeploy(t, controllerScriptDeployOptions{
		FailExecSubstring: "gc bd init -p 'gc' --skip-hooks",
		FailExecCount:     -1,
	})
	if result.err == nil {
		t.Fatalf("gc-controller-k8s deploy error = nil, want bootstrap failure\noutput:\n%s", result.output)
	}
	if !strings.Contains(result.output, "deploy: failed to bootstrap HQ on controller after 30 attempts") {
		t.Fatalf("deploy output did not report bootstrap failure:\n%s", result.output)
	}
}

func TestControllerScriptDeployRejectsPartialCanonicalDoltTarget(t *testing.T) {
	clearDoltAndCityEnv(t)
	result := runControllerScriptDeploy(t, controllerScriptDeployOptions{
		Env: map[string]string{
			"GC_DOLT_HOST": "canonical-dolt.example.com",
		},
	})
	if result.err == nil {
		t.Fatalf("gc-controller-k8s deploy error = nil, want partial GC_DOLT_* rejection\noutput:\n%s", result.output)
	}
	if !strings.Contains(result.output, "controller bootstrap requires both GC_DOLT_HOST and GC_DOLT_PORT when either is set") {
		t.Fatalf("partial GC_DOLT_* rejection output = %q", result.output)
	}
}

type controllerScriptDeployOptions struct {
	Env               map[string]string
	CityToml          string
	ResolvedConfig    string
	LogOutputs        []string
	FailExecSubstring string
	FailExecCount     int
}

type controllerScriptDeployResult struct {
	manifestEnv map[string]string
	callLog     string
	output      string
	err         error
}

func runControllerScriptDeploy(t *testing.T, opts controllerScriptDeployOptions) controllerScriptDeployResult {
	t.Helper()

	if opts.CityToml == "" {
		opts.CityToml = `[workspace]
name = "sample-city"
prefix = "gc"

[[rigs]]
name = "frontend"
prefix = "fe"
path = "frontend"
`
	}
	if opts.ResolvedConfig == "" {
		opts.ResolvedConfig = opts.CityToml
	}
	if len(opts.LogOutputs) == 0 {
		opts.LogOutputs = []string{"City started."}
	}

	tmpDir := t.TempDir()
	cityDir := filepath.Join(tmpDir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatalf("mkdir city dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(opts.CityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	manifestPath := filepath.Join(tmpDir, "manifest.json")
	callLogPath := filepath.Join(tmpDir, "call.log")
	logsPath := filepath.Join(tmpDir, "logs.txt")
	logsIndexPath := filepath.Join(tmpDir, "logs.index")
	resolvedConfigPath := filepath.Join(tmpDir, "resolved-config.toml")
	failExecCountPath := filepath.Join(tmpDir, "fail-exec-count.txt")
	claudeDir := filepath.Join(tmpDir, "empty-claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir claude dir: %v", err)
	}
	if err := os.WriteFile(logsPath, []byte(strings.Join(opts.LogOutputs, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write logs file: %v", err)
	}
	if err := os.WriteFile(resolvedConfigPath, []byte(opts.ResolvedConfig), 0o644); err != nil {
		t.Fatalf("write resolved config: %v", err)
	}
	if err := os.WriteFile(failExecCountPath, []byte(fmt.Sprintf("%d\n", opts.FailExecCount)), 0o644); err != nil {
		t.Fatalf("write fail-exec count: %v", err)
	}

	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}

	fakeSleep := filepath.Join(binDir, "sleep")
	if err := os.WriteFile(fakeSleep, []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake sleep: %v", err)
	}

	fakeGC := filepath.Join(binDir, "gc")
	gcScript := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
resolved_config=%q
if [ "$#" -ge 3 ] && [ "$1" = "config" ] && [ "$2" = "show" ] && [ "$3" = "--city" ]; then
  cat "$resolved_config"
  exit 0
fi
printf 'unexpected gc call: %%s\n' "$*" >&2
exit 1
`, resolvedConfigPath)
	if err := os.WriteFile(fakeGC, []byte(gcScript), 0o755); err != nil {
		t.Fatalf("write fake gc: %v", err)
	}

	fakeKubectl := filepath.Join(binDir, "kubectl")
	kubectlScript := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
manifest_out=%q
call_log=%q
logs_file=%q
logs_index_file=%q
fail_exec_substring=%q
fail_exec_count_file=%q
printf '%%s\n' "$*" >> "$call_log"
joined=" $* "
if [[ "$joined" == *" delete pod gc-controller "* ]]; then
  exit 0
fi
if [[ "$joined" == *" wait --for=delete pod/gc-controller "* ]]; then
  exit 0
fi
if [[ "$joined" == *" apply -f - "* ]]; then
  payload=$(cat)
  printf '%%s' "$payload" > "$manifest_out"
  exit 0
fi
if [[ "$joined" == *" get pod gc-controller -o jsonpath={.status.containerStatuses[0].state.running} "* ]]; then
  printf 'true'
  exit 0
fi
if [[ "$joined" == *" cp "* ]]; then
  exit 0
fi
if [[ "$joined" == *" exec gc-controller -- test -f /city/.gc-init-done "* ]]; then
  exit 0
fi
if [[ "$joined" == *" exec gc-controller -- touch /city/.gc-start "* ]]; then
  exit 0
fi
if [[ "$joined" == *" logs gc-controller --tail=50 "* ]]; then
  idx=0
  if [ -f "$logs_index_file" ]; then
    idx=$(cat "$logs_index_file")
  fi
  next_line=$(sed -n "$((idx + 1))p" "$logs_file")
  if [ -z "$next_line" ]; then
    next_line=$(tail -n 1 "$logs_file")
  fi
  printf '%%s' "$next_line"
  printf '%%s' "$((idx + 1))" > "$logs_index_file"
  exit 0
fi
if [[ "$joined" == *" exec gc-controller -- sh -c "* ]]; then
  if [ -n "$fail_exec_substring" ] && [[ "$*" == *"$fail_exec_substring"* ]]; then
    remaining=$(cat "$fail_exec_count_file")
    if [ "$remaining" = "-1" ] || [ "$remaining" -gt 0 ]; then
      if [ "$remaining" != "-1" ]; then
        printf '%%s\n' "$((remaining - 1))" > "$fail_exec_count_file"
      fi
      printf 'simulated bootstrap failure for %%s\n' "$fail_exec_substring" >&2
      exit 1
    fi
  fi
  exit 0
fi
printf 'unexpected kubectl call: %%s\n' "$*" >&2
exit 1
`, manifestPath, callLogPath, logsPath, logsIndexPath, opts.FailExecSubstring, failExecCountPath)
	if err := os.WriteFile(fakeKubectl, []byte(kubectlScript), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}

	cmd := exec.Command(controllerScriptPath(t), "deploy", cityDir)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CLAUDE_DIR="+claudeDir,
		"GC_BIN="+fakeGC,
		"GC_DOLT_HOST=",
		"GC_DOLT_PORT=",
		"GC_K8S_DOLT_HOST=",
		"GC_K8S_DOLT_PORT=",
		"GC_CITY_PATH=",
	)
	for key, value := range opts.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()

	callLogBytes, readCallErr := os.ReadFile(callLogPath)
	if readCallErr != nil && !os.IsNotExist(readCallErr) {
		t.Fatalf("read call log: %v", readCallErr)
	}
	manifestEnv := map[string]string{}
	manifestBytes, readManifestErr := os.ReadFile(manifestPath)
	if readManifestErr == nil && len(manifestBytes) > 0 {
		var manifest struct {
			Spec struct {
				Containers []struct {
					Env []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"env"`
				} `json:"containers"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			t.Fatalf("parse manifest json: %v\n%s", err, string(manifestBytes))
		}
		if len(manifest.Spec.Containers) == 0 {
			t.Fatalf("manifest did not contain a container:\n%s", string(manifestBytes))
		}
		for _, item := range manifest.Spec.Containers[0].Env {
			manifestEnv[item.Name] = item.Value
		}
	} else if readManifestErr != nil && !os.IsNotExist(readManifestErr) {
		t.Fatalf("read manifest: %v", readManifestErr)
	}

	// Strip tmpDir from the call log so substring assertions (e.g. searching
	// for a port number) aren't corrupted by random digits Go inserts into
	// t.TempDir() paths — those digits leak into the log via `kubectl cp`.
	callLog := strings.ReplaceAll(string(callLogBytes), tmpDir, "<TMPDIR>")

	return controllerScriptDeployResult{
		manifestEnv: manifestEnv,
		callLog:     callLog,
		output:      string(out),
		err:         err,
	}
}

func assertCallContains(t *testing.T, callLog, substring string) {
	t.Helper()
	if !strings.Contains(callLog, substring) {
		t.Fatalf("call log did not contain %q:\n%s", substring, callLog)
	}
}

func assertCallNotContains(t *testing.T, callLog, substring string) {
	t.Helper()
	if strings.Contains(callLog, substring) {
		t.Fatalf("call log unexpectedly contained %q:\n%s", substring, callLog)
	}
}

func lineIndexContaining(log, substring string) int {
	for idx, line := range strings.Split(log, "\n") {
		if strings.Contains(line, substring) {
			return idx
		}
	}
	return -1
}

func controllerScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contrib", "session-scripts", "gc-controller-k8s"))
}
