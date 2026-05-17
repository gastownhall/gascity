package dolt_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPortResolveOrDieEnvOverride(t *testing.T) {
	result := runPortResolveOrDie(t, portResolveCase{
		stateFile: filepath.Join(t.TempDir(), "missing-state.json"),
		dataDir:   filepath.Join(t.TempDir(), "data"),
		cityPath:  t.TempDir(),
		env:       []string{"GC_DOLT_PORT=4242"},
	})

	assertPortResolveResult(t, result, 0, "4242\n", "")
}

func TestPortResolveOrDieDiscoverySuccess(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "dolt-state.json")
	dataDir := filepath.Join(t.TempDir(), "d")
	cityPath := t.TempDir()
	if err := os.WriteFile(stateFile, []byte(fmt.Sprintf(
		`{"running":true,"pid":%d,"port":47823,"data_dir":%q}`,
		os.Getpid(),
		dataDir,
	)), 0o644); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}

	result := runPortResolveOrDie(t, portResolveCase{
		stateFile:   stateFile,
		dataDir:     dataDir,
		cityPath:    cityPath,
		managedPort: "47823",
	})

	assertPortResolveResult(t, result, 0, "47823\n", "")
}

func TestPortResolveOrDieProviderDiscoverySuccess(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "dolt-state.json")
	providerStateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")
	dataDir := filepath.Join(t.TempDir(), "d")
	cityPath := t.TempDir()
	if err := os.WriteFile(stateFile, []byte(`{"running":false}`), 0o644); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}
	if err := os.WriteFile(providerStateFile, []byte(fmt.Sprintf(
		`{"running":true,"pid":%d,"port":47824,"data_dir":%q}`,
		os.Getpid(),
		dataDir,
	)), 0o644); err != nil {
		t.Fatalf("write provider state fixture: %v", err)
	}

	result := runPortResolveOrDie(t, portResolveCase{
		stateFile:           stateFile,
		providerStateFile:   providerStateFile,
		dataDir:             dataDir,
		cityPath:            cityPath,
		providerManagedPort: "47824",
	})

	assertPortResolveResult(t, result, 0, "47824\n", "")
}

func TestPortResolveOrDieExplicitStateSkipsProviderFallback(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "dolt-state.json")
	providerStateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")
	cityPath := t.TempDir()

	result := runPortResolveOrDie(t, portResolveCase{
		stateFile:           stateFile,
		providerStateFile:   providerStateFile,
		dataDir:             filepath.Join(t.TempDir(), "data"),
		cityPath:            cityPath,
		providerManagedPort: "47824",
		env:                 []string{"GC_DOLT_STATE_FILE=" + stateFile},
	})

	assertPortResolveResult(t, result, 78, "", expectedPortResolveError(stateFile, cityPath, "missing"))
}

func TestPortResolveOrDieMissingState(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "dolt-state.json")
	cityPath := t.TempDir()

	result := runPortResolveOrDie(t, portResolveCase{
		stateFile: stateFile,
		dataDir:   filepath.Join(t.TempDir(), "data"),
		cityPath:  cityPath,
	})

	assertPortResolveResult(t, result, 78, "", expectedPortResolveError(stateFile, cityPath, "missing"))
}

func TestPortResolveOrDieStatePresentNotRunning(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "dolt-state.json")
	cityPath := t.TempDir()
	if err := os.WriteFile(stateFile, []byte(`{"running":false}`), 0o644); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}

	result := runPortResolveOrDie(t, portResolveCase{
		stateFile: stateFile,
		dataDir:   filepath.Join(t.TempDir(), "data"),
		cityPath:  cityPath,
	})

	assertPortResolveResult(t, result, 78, "", expectedPortResolveError(stateFile, cityPath, "present but not running"))
}

func TestPortResolveOrDieExit78OnEmptyState(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "dolt-state.json")
	dataDir := filepath.Join(t.TempDir(), "d")
	cityPath := t.TempDir()
	if err := os.WriteFile(stateFile, []byte(fmt.Sprintf(
		`{"running":true,"pid":99999999,"port":47823,"data_dir":%q}`,
		dataDir,
	)), 0o644); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}

	result := runPortResolveOrDie(t, portResolveCase{
		stateFile: stateFile,
		dataDir:   dataDir,
		cityPath:  cityPath,
	})

	assertPortResolveResult(t, result, 78, "", expectedPortResolveError(stateFile, cityPath, "present but not running"))
}

func TestRuntimeShUsesPortResolve(t *testing.T) {
	root := repoRoot(t)
	assertScriptSourcesPortResolveOnce(t, filepath.Join(root, "assets", "scripts", "runtime.sh"))
}

func TestDoltTargetShUsesPortResolve(t *testing.T) {
	root := filepath.Dir(repoRoot(t))
	assertScriptSourcesPortResolveOnce(t, filepath.Join(root, "gastown", "packs", "maintenance", "assets", "scripts", "dolt-target.sh"))
}

type portResolveCase struct {
	stateFile           string
	providerStateFile   string
	dataDir             string
	cityPath            string
	managedPort         string
	providerManagedPort string
	env                 []string
}

type portResolveResult struct {
	code   int
	stdout string
	stderr string
}

func runPortResolveOrDie(t *testing.T, tc portResolveCase) portResolveResult {
	t.Helper()
	root := repoRoot(t)
	driver := fmt.Sprintf(`
managed_runtime_port() {
    if [ "$1" = "$STATE_FILE" ] && [ -n "${TEST_MANAGED_PORT:-}" ]; then
        printf '%%s\n' "$TEST_MANAGED_PORT"
        return 0
    fi
    if [ -n "${PROVIDER_STATE_FILE:-}" ] && [ "$1" = "$PROVIDER_STATE_FILE" ] && [ -n "${TEST_PROVIDER_MANAGED_PORT:-}" ]; then
        printf '%%s\n' "$TEST_PROVIDER_MANAGED_PORT"
        return 0
    fi
    return 0
}
. %s
if [ -n "${PROVIDER_STATE_FILE:-}" ]; then
    resolve_dolt_port_or_die "$STATE_FILE" "$PROVIDER_STATE_FILE" "$DATA_DIR" "$CITY_PATH"
else
    resolve_dolt_port_or_die "$STATE_FILE" "$DATA_DIR" "$CITY_PATH"
fi
`, shellQuote(filepath.Join(root, "assets", "scripts", "port_resolve.sh")))

	cmd := exec.Command("sh", "-c", driver)
	cmd.Env = filteredEnv("GC_DOLT_PORT", "GC_DOLT_STATE_FILE", "STATE_FILE", "PROVIDER_STATE_FILE", "DATA_DIR", "CITY_PATH", "TEST_MANAGED_PORT", "TEST_PROVIDER_MANAGED_PORT")
	cmd.Env = append(cmd.Env,
		"STATE_FILE="+tc.stateFile,
		"PROVIDER_STATE_FILE="+tc.providerStateFile,
		"DATA_DIR="+tc.dataDir,
		"CITY_PATH="+tc.cityPath,
		"TEST_MANAGED_PORT="+tc.managedPort,
		"TEST_PROVIDER_MANAGED_PORT="+tc.providerManagedPort,
	)
	cmd.Env = append(cmd.Env, tc.env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		ok := errors.As(err, &exitErr)
		if !ok {
			t.Fatalf("port_resolve driver failed to run: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		code = exitErr.ExitCode()
	}
	return portResolveResult{
		code:   code,
		stdout: stdout.String(),
		stderr: stderr.String(),
	}
}

func assertPortResolveResult(t *testing.T, got portResolveResult, wantCode int, wantStdout, wantStderr string) {
	t.Helper()
	if got.code != wantCode {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", got.code, wantCode, got.stdout, got.stderr)
	}
	if got.stdout != wantStdout {
		t.Fatalf("stdout = %q, want %q\nstderr:\n%s", got.stdout, wantStdout, got.stderr)
	}
	if got.stderr != wantStderr {
		t.Fatalf("stderr = %q, want %q", got.stderr, wantStderr)
	}
}

func expectedPortResolveError(stateFile, cityPath, stateStatus string) string {
	return fmt.Sprintf(`gc dolt: cannot resolve runtime port
  state_file: %s (%s)
  city_path:  %s
  consulted:  GC_DOLT_PORT (unset), GC_DOLT_STATE_FILE
  remediation: run `+"`"+`gc start`+"`"+` to bring up the city, or set
               GC_DOLT_PORT explicitly to an already-running
               server.
`, stateFile, stateStatus, cityPath)
}

func expectedPortResolveErrorWithProvider(stateFile, cityPath, stateStatus string) string {
	return fmt.Sprintf(`gc dolt: cannot resolve runtime port
  state_file: %s (%s)
  city_path:  %s
  consulted:  GC_DOLT_PORT (unset), GC_DOLT_STATE_FILE, dolt-provider-state.json
  remediation: run `+"`"+`gc start`+"`"+` to bring up the city, or set
               GC_DOLT_PORT explicitly to an already-running
               server.
`, stateFile, stateStatus, cityPath)
}

func assertScriptSourcesPortResolveOnce(t *testing.T, scriptPath string) {
	t.Helper()
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	re := regexp.MustCompile(`(?m)^\.\s+.*port_resolve\.sh`)
	matches := re.FindAllString(string(data), -1)
	if len(matches) != 1 {
		t.Fatalf("%s port_resolve.sh source count = %d, want 1\nmatches: %s", scriptPath, len(matches), strings.Join(matches, "\n"))
	}
}
