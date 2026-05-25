//go:build !windows

package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	otellogglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// TestExecCommandRunnerTimesOut verifies the runner returns a "timed
// out" error when the command exceeds bdCommandTimeout. No race: we
// only check the error path, not what the child did.
func TestExecCommandRunnerTimesOut(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep unavailable")
	}

	oldTimeout := bdCommandTimeout
	bdCommandTimeout = 3 * time.Second
	t.Cleanup(func() { bdCommandTimeout = oldTimeout })

	_, err := ExecCommandRunner()(t.TempDir(), "sleep", "30")
	if err == nil {
		t.Fatal("runner unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("error = %v, want timeout", err)
	}
}

func TestBDCommandTimeoutForReadCommands(t *testing.T) {
	if got := bdCommandTimeoutFor("bd", []string{"list", "--json"}); got != bdReadCommandTimeout {
		t.Fatalf("bd list timeout = %s, want %s", got, bdReadCommandTimeout)
	}
	if got := bdCommandTimeoutFor("bd", []string{"ready", "--json"}); got != bdReadCommandTimeout {
		t.Fatalf("bd ready timeout = %s, want %s", got, bdReadCommandTimeout)
	}
	if got := bdCommandTimeoutFor("bd", []string{"query", "--json", "ephemeral=true"}); got != bdReadCommandTimeout {
		t.Fatalf("bd query timeout = %s, want %s", got, bdReadCommandTimeout)
	}
	if got := bdCommandTimeoutFor("bd", []string{"dep", "list", "gc-1", "--json"}); got != bdReadCommandTimeout {
		t.Fatalf("bd dep list timeout = %s, want %s", got, bdReadCommandTimeout)
	}
	if got := bdCommandTimeoutFor("bd", []string{"update", "gc-1", "--status", "open"}); got != bdCommandTimeout {
		t.Fatalf("bd update timeout = %s, want %s", got, bdCommandTimeout)
	}
	if got := bdCommandTimeoutFor("git", []string{"status"}); got != bdCommandTimeout {
		t.Fatalf("non-bd timeout = %s, want %s", got, bdCommandTimeout)
	}
}

func TestBDCommandTimeoutForGraphApply(t *testing.T) {
	if got := bdCommandTimeoutFor("bd", []string{"create", "--graph", "/tmp/plan.json", "--json"}); got != bdGraphApplyCommandTimeout {
		t.Fatalf("bd create --graph timeout = %s, want %s", got, bdGraphApplyCommandTimeout)
	}
}

func TestBDSubprocessEnvSuppressesSideEffectsForReadCommands(t *testing.T) {
	base := map[string]string{
		"BD_BACKUP_ENABLED":     "true",
		"BD_DOLT_AUTO_COMMIT":   "on",
		"BD_DOLT_AUTO_PUSH":     "true",
		"BD_EXPORT_AUTO":        "true",
		"BD_NO_GIT_OPS":         "false",
		"BD_NO_PUSH":            "false",
		"BD_READONLY":           "false",
		"BEADS_NO_AUTO_IMPORT":  "0",
		"GC_CITY_RUNTIME_DIR":   "/city/.gc/runtime",
		"UNRELATED_TEST_MARKER": "keep",
	}

	got := bdSubprocessEnvForCommand("bd", []string{"list", "--json"}, base)
	want := map[string]string{
		"BD_BACKUP_ENABLED":    "false",
		"BD_DOLT_AUTO_COMMIT":  "off",
		"BD_DOLT_AUTO_PUSH":    "false",
		"BD_EXPORT_AUTO":       "false",
		"BD_NO_GIT_OPS":        "true",
		"BD_NO_PUSH":           "true",
		"BD_READONLY":          "true",
		"BEADS_NO_AUTO_IMPORT": "1",
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Fatalf("%s = %q, want %q in %#v", key, got[key], wantValue, got)
		}
	}
	if got["UNRELATED_TEST_MARKER"] != "keep" {
		t.Fatalf("unrelated env marker = %q, want keep", got["UNRELATED_TEST_MARKER"])
	}
	if base["BD_EXPORT_AUTO"] != "true" {
		t.Fatalf("bdSubprocessEnvForCommand mutated input map: %#v", base)
	}
}

func TestBDSubprocessEnvLeavesMutationAutoCommitPolicyUnset(t *testing.T) {
	got := bdSubprocessEnvForCommand("bd", []string{"update", "gc-1", "--status", "closed"}, nil)
	if _, ok := got["BD_DOLT_AUTO_COMMIT"]; ok {
		t.Fatalf("BD_DOLT_AUTO_COMMIT set for mutation command: %#v", got)
	}
	if _, ok := got["BD_READONLY"]; ok {
		t.Fatalf("BD_READONLY set for mutation command: %#v", got)
	}
	if got["BD_EXPORT_AUTO"] != "false" || got["BD_DOLT_AUTO_PUSH"] != "false" || got["BD_NO_PUSH"] != "true" {
		t.Fatalf("side-effect suppression missing for mutation command: %#v", got)
	}
}

func TestExecCommandRunnerAppliesBDReadOnlyEnv(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf 'BD_EXPORT_AUTO=%s\n' "${BD_EXPORT_AUTO:-}"
printf 'BD_DOLT_AUTO_PUSH=%s\n' "${BD_DOLT_AUTO_PUSH:-}"
printf 'BD_BACKUP_ENABLED=%s\n' "${BD_BACKUP_ENABLED:-}"
printf 'BD_NO_PUSH=%s\n' "${BD_NO_PUSH:-}"
printf 'BD_NO_GIT_OPS=%s\n' "${BD_NO_GIT_OPS:-}"
printf 'BEADS_NO_AUTO_IMPORT=%s\n' "${BEADS_NO_AUTO_IMPORT:-}"
printf 'BD_DOLT_AUTO_COMMIT=%s\n' "${BD_DOLT_AUTO_COMMIT:-}"
printf 'BD_READONLY=%s\n' "${BD_READONLY:-}"
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_EXPORT_AUTO", "true")
	t.Setenv("BD_DOLT_AUTO_PUSH", "true")
	t.Setenv("BD_BACKUP_ENABLED", "true")
	t.Setenv("BD_DOLT_AUTO_COMMIT", "on")
	t.Setenv("BD_READONLY", "false")

	dir := t.TempDir()
	runner := ExecCommandRunnerWithEnv(map[string]string{
		"BEADS_DIR":           filepath.Join(dir, ".beads"),
		"GC_CITY_RUNTIME_DIR": filepath.Join(dir, ".gc", "runtime"),
	})
	out, err := runner(dir, "bd", "list", "--json")
	if err != nil {
		t.Fatalf("ExecCommandRunner bd list: %v", err)
	}
	got := parseEnvOutput(string(out))
	want := map[string]string{
		"BD_BACKUP_ENABLED":    "false",
		"BD_DOLT_AUTO_COMMIT":  "off",
		"BD_DOLT_AUTO_PUSH":    "false",
		"BD_EXPORT_AUTO":       "false",
		"BD_NO_GIT_OPS":        "true",
		"BD_NO_PUSH":           "true",
		"BD_READONLY":          "true",
		"BEADS_NO_AUTO_IMPORT": "1",
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Fatalf("%s = %q, want %q; output:\n%s", key, got[key], wantValue, out)
		}
	}
}

func TestExecCommandRunnerSerializesBDList(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	binDir := t.TempDir()
	stateDir := t.TempDir()
	activeDir := filepath.Join(stateDir, "active")
	violationPath := filepath.Join(stateDir, "overlap")
	writeExecutable(t, filepath.Join(binDir, "bd"), fmt.Sprintf(`#!/bin/sh
if [ "$1" = "list" ]; then
  if mkdir %q 2>/dev/null; then
    sleep 0.15
    rmdir %q
    printf '[]\n'
    exit 0
  fi
  printf 'overlap\n' >> %q
  printf '[]\n'
  exit 0
fi
printf '[]\n'
`, activeDir, activeDir, violationPath))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	runner := ExecCommandRunnerWithEnv(map[string]string{
		"BEADS_DIR":           filepath.Join(dir, ".beads"),
		"GC_CITY_RUNTIME_DIR": filepath.Join(dir, ".gc", "runtime"),
	})
	const workers = 4
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := runner(dir, "bd", "list", "--json")
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("bd list runner error: %v", err)
		}
	}
	if data, err := os.ReadFile(violationPath); err == nil {
		t.Fatalf("bd list subprocesses overlapped despite flock: %s", data)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read overlap marker: %v", err)
	}
}

func TestExecCommandRunnerEmitsBDSlowForLongBDCommand(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	oldThreshold := bdSlowTelemetryThreshold
	bdSlowTelemetryThreshold = 20 * time.Millisecond
	t.Cleanup(func() { bdSlowTelemetryThreshold = oldThreshold })

	exp := installBeadsRecordingLogExporter(t)
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
sleep 0.08
printf '[]\n'
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_ALIAS", "test-agent-1")

	if _, err := ExecCommandRunner()(t.TempDir(), "bd", "list", "--token", "sk-secret"); err != nil {
		t.Fatalf("ExecCommandRunner bd: %v", err)
	}

	rec := exp.waitForBody(t, "bd.slow", time.Second)
	attrs := beadsRecordAttrs(*rec)
	if got := beadsLogValueStringSlice(attrs["args"]); strings.Join(got, " ") != "list --token <redacted>" {
		t.Fatalf("bd.slow args = %#v, want token redacted", got)
	}
	if got := attrs["agent_id"].AsString(); got != "test-agent-1" {
		t.Fatalf("bd.slow agent_id = %q, want test-agent-1", got)
	}
}

func TestExecCommandRunnerStopsBDSlowTimerForFastBDCommand(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	oldThreshold := bdSlowTelemetryThreshold
	bdSlowTelemetryThreshold = 30 * time.Millisecond
	t.Cleanup(func() { bdSlowTelemetryThreshold = oldThreshold })

	exp := installBeadsRecordingLogExporter(t)
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '[]\n'
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := ExecCommandRunner()(t.TempDir(), "bd", "list"); err != nil {
		t.Fatalf("ExecCommandRunner bd: %v", err)
	}
	time.Sleep(2 * bdSlowTelemetryThreshold)
	if got := exp.countByBody("bd.slow"); got != 0 {
		t.Fatalf("bd.slow records = %d, want 0 for fast bd command", got)
	}
}

// TestKillCommandTreeKillsProcessGroup verifies killCommandTree kills
// the entire process group, not just the direct child. The script
// backgrounds a `sleep 30`; without process-group cleanup, that sleep
// would survive its parent shell's death and leak — the failure mode
// PR #1639 ("kill bd subprocess trees on timeout") fixed.
//
// No timeout involved — we wait synchronously for the script to fork
// the sleep, then call killCommandTree directly. The previous version
// of this test (TestExecCommandRunnerTimeoutKillsChildProcess) raced
// the same assertion against a 50ms timeout, which lost on macOS where
// first-exec of a new script file pays a ~150ms validation tax.
func TestKillCommandTreeKillsProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "spawn-child.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
sleep 30 &
echo "$!" > "$1"
wait
`), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cmd := exec.Command(script, pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = killCommandTree(cmd)
		_ = cmd.Wait()
	})

	childPid := waitForNonEmptyFile(t, pidFile, 5*time.Second)

	if err := killCommandTree(cmd); err != nil {
		t.Fatalf("killCommandTree: %v", err)
	}

	for range 50 {
		if err := exec.Command("kill", "-0", childPid).Run(); err != nil {
			return // child is gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = exec.Command("kill", "-KILL", childPid).Run()
	t.Fatalf("child process %s survived killCommandTree", childPid)
}

func TestKillCommandTreeHandlesNilCommand(t *testing.T) {
	if err := killCommandTree(nil); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killCommandTree(nil): %v", err)
	}
}

func waitForNonEmptyFile(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pidBytes, err := os.ReadFile(path)
		if err == nil {
			pid := strings.TrimSpace(string(pidBytes))
			if pid != "" {
				return pid
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read child pid: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child pid was not written within %s", timeout)
	return ""
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func parseEnvOutput(out string) map[string]string {
	result := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

type beadsRecordingLogExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func installBeadsRecordingLogExporter(t *testing.T) *beadsRecordingLogExporter {
	t.Helper()
	exp := &beadsRecordingLogExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	prev := otellogglobal.GetLoggerProvider()
	otellogglobal.SetLoggerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otellogglobal.SetLoggerProvider(prev)
	})
	return exp
}

func (e *beadsRecordingLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rec := range records {
		e.records = append(e.records, rec.Clone())
	}
	return nil
}

func (e *beadsRecordingLogExporter) Shutdown(context.Context) error {
	return nil
}

func (e *beadsRecordingLogExporter) ForceFlush(context.Context) error {
	return nil
}

func (e *beadsRecordingLogExporter) waitForBody(t *testing.T, body string, timeout time.Duration) *sdklog.Record {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rec := e.recordByBody(body); rec != nil {
			return rec
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("log body %q did not arrive within %s", body, timeout)
	return nil
}

func (e *beadsRecordingLogExporter) recordByBody(body string) *sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.records {
		if e.records[i].Body().AsString() == body {
			rec := e.records[i].Clone()
			return &rec
		}
	}
	return nil
}

func (e *beadsRecordingLogExporter) countByBody(body string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	var count int
	for i := range e.records {
		if e.records[i].Body().AsString() == body {
			count++
		}
	}
	return count
}

func beadsRecordAttrs(rec sdklog.Record) map[string]otellog.Value {
	attrs := make(map[string]otellog.Value)
	rec.WalkAttributes(func(kv otellog.KeyValue) bool {
		attrs[kv.Key] = kv.Value
		return true
	})
	return attrs
}

func beadsLogValueStringSlice(value otellog.Value) []string {
	values := value.AsSlice()
	out := make([]string, 0, len(values))
	for _, item := range values {
		out = append(out, item.AsString())
	}
	return out
}
