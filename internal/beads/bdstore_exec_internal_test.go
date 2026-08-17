//go:build !windows

package beads

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
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

// TestExecCommandRunnerWithEnvContextHonorsParentDeadline proves the
// context-aware runner binds each command to the caller's context: a parent
// deadline well below bdCommandTimeout kills a long-running child promptly
// instead of letting it run to the per-command budget. This is the seam the
// best-effort claim-time gc.current_run_id write uses so a slow or stuck bd
// update cannot outlast the claim's short mutation budget.
func TestExecCommandRunnerWithEnvContextHonorsParentDeadline(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep unavailable")
	}

	// Leave bdCommandTimeout at its default so the only thing that can return
	// the child quickly is the parent context, not the per-command timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ExecCommandRunnerWithEnvContext(ctx, nil)(t.TempDir(), "sleep", "30")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("context-bound runner unexpectedly succeeded")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("runner blocked %s; the 200ms parent deadline was ignored", elapsed)
	}
}

// TestExecCommandRunnerWithEnvContextTimeoutReportsCallerDeadline proves the
// timeout error is attributed to the caller's parent deadline when that budget
// wins the race, not to the much larger per-command bd timeout. The claim-time
// gc.current_run_id writer relies on this so a short claim-budget failure is not
// misreported as "timed out after 2m0s".
func TestExecCommandRunnerWithEnvContextTimeoutReportsCallerDeadline(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep unavailable")
	}

	// Leave the per-command timeout at its (large) default so only the short
	// parent deadline can fire; the message must then report the parent budget.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := ExecCommandRunnerWithEnvContext(ctx, nil)(t.TempDir(), "sleep", "30")
	if err == nil {
		t.Fatal("context-bound runner unexpectedly succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, "caller deadline") {
		t.Fatalf("timeout error = %q, want it attributed to the caller deadline", msg)
	}
	if perCommand := bdCommandTimeout.String(); strings.Contains(msg, perCommand) {
		t.Fatalf("timeout error = %q, must not report the %s per-command timeout when the caller deadline won", msg, perCommand)
	}
}

func TestBDCommandTimeoutForReadCommands(t *testing.T) {
	if got := bdCommandTimeoutFor("bd", []string{"list", "--json"}); got != bdReadCommandTimeout {
		t.Fatalf("bd list timeout = %s, want %s", got, bdReadCommandTimeout)
	}
	if got := bdCommandTimeoutFor("bd", []string{"ready", "--json"}); got != bdReadCommandTimeout {
		t.Fatalf("bd ready timeout = %s, want %s", got, bdReadCommandTimeout)
	}
	if got := bdCommandTimeoutFor("bd", []string{"sql", "select 1", "--json"}); got != bdReadCommandTimeout {
		t.Fatalf("bd sql timeout = %s, want %s", got, bdReadCommandTimeout)
	}
	if got := bdCommandTimeoutFor("bd", []string{"version"}); got != bdReadCommandTimeout {
		t.Fatalf("bd version timeout = %s, want %s", got, bdReadCommandTimeout)
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

// TestBDCommandTimeoutForQuery pins the dedicated, shorter bound on the
// ephemeral `bd query` subcommand (#3191). The bound must be below the general
// timeout so gc reload / gc doctor kill a slow ephemeral child and degrade to
// the durable tier instead of blocking.
func TestBDCommandTimeoutForQuery(t *testing.T) {
	if got := bdCommandTimeoutFor("bd", []string{"query", "--json", "ephemeral=true", "--limit", "1"}); got != bdQueryCommandTimeout {
		t.Fatalf("bd query timeout = %s, want %s", got, bdQueryCommandTimeout)
	}
	if bdQueryCommandTimeout >= bdCommandTimeout {
		t.Fatalf("bd query timeout %s must be below general timeout %s", bdQueryCommandTimeout, bdCommandTimeout)
	}
	if bdQueryCommandTimeout >= bdReadCommandTimeout {
		t.Fatalf("bd query timeout %s must be below read timeout %s", bdQueryCommandTimeout, bdReadCommandTimeout)
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
	// Use a large threshold (5 s) so a trivial shell script reliably
	// completes before the timer fires even on a heavily-loaded parallel
	// test runner. 30 ms was too tight and caused spurious "bd.slow" fires
	// (ga-2dd). The sleep after the call is decoupled from the threshold:
	// we only need to drain any in-flight timer goroutine, not wait for the
	// timer to expire. 100 ms is ample for the exporter to flush.
	bdSlowTelemetryThreshold = 5 * time.Second
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
	// After ExecCommandRunner returns, defer slowTimer.Stop() has already
	// been called. If Stop returned true the timer was defused; if false it
	// fired and the goroutine may still be recording. 100 ms gives that
	// goroutine time to complete before we assert.
	time.Sleep(100 * time.Millisecond)
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
	processgrouptest.RequireRealProcessSignals(t)

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
