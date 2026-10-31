package dolt_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const restartScript = "commands/restart/run.sh"

// writeFakeBeadsBDForRestart writes a stub gc-beads-bd that records each
// invocation's first argument and exits with the code specified for that
// op. ops that aren't in opExitCodes exit 0.
func writeFakeBeadsBDForRestart(t *testing.T, cityPath, root string, opExitCodes map[string]int) string {
	t.Helper()
	scriptDir := filepath.Join(cityPath, ".gc", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir fake bd dir: %v", err)
	}
	logPath := filepath.Join(cityPath, "bd.log")
	var cases strings.Builder
	for op, code := range opExitCodes {
		fmt.Fprintf(&cases, "  %s) exit %d ;;\n", op, code)
	}
	body := `#!/bin/sh
printf '%s\n' "$1" >> "` + logPath + `"
case "$1" in
` + cases.String() + `  *) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(scriptDir, "gc-beads-bd.sh"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake bd script: %v", err)
	}
	enospcHelper, err := os.ReadFile(filepath.Join(root, "..", "assets", "scripts", "dolt-enospc.sh"))
	if err != nil {
		t.Fatalf("read production enospc helper: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptDir, "dolt-enospc.sh"), enospcHelper, 0o644); err != nil {
		t.Fatalf("write fake enospc helper: %v", err)
	}

	stateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir fake dolt state dir: %v", err)
	}
	// Bind the live-PID fixture to a recorded launch so the guard does not
	// depend on the test binary's process name or kernel start time.
	writeDoltLaunchState(t, cityPath, os.Getpid(), time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	return logPath
}

func writeRestartLog(t *testing.T, cityPath, body string) {
	t.Helper()
	logPath := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir dolt log dir: %v", err)
	}
	if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write dolt log: %v", err)
	}
}

func writeDeadDoltLaunchState(t *testing.T, cityPath, startedAt string) {
	t.Helper()
	const deadPID = 2147483647
	writeDoltLaunchState(t, cityPath, deadPID, startedAt)
}

func writeDoltPIDFile(t *testing.T, cityPath string, pid int) {
	t.Helper()
	stateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir dolt state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "dolt.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write dolt pid: %v", err)
	}
}

func writeDoltLaunchState(t *testing.T, cityPath string, pid int, startedAt string) {
	t.Helper()
	writeDoltPIDFile(t, cityPath, pid)
	stateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	state := fmt.Sprintf(
		`{"running":true,"pid":%d,"port":3307,"data_dir":%q,"started_at":%q}`+"\n",
		pid, filepath.Join(cityPath, ".beads", "dolt"), startedAt,
	)
	if err := os.WriteFile(filepath.Join(stateDir, "dolt-provider-state.json"), []byte(state), 0o644); err != nil {
		t.Fatalf("write dolt provider state: %v", err)
	}
}

func runRestart(t *testing.T, cityPath, root string, port int) ([]byte, error) {
	t.Helper()
	return runRestartWithEnv(t, cityPath, root, []string{fmt.Sprintf("GC_DOLT_PORT=%d", port)})
}

func runRestartWithEnv(t *testing.T, cityPath, root string, extraEnv []string, args ...string) ([]byte, error) {
	t.Helper()
	script := filepath.Join(root, restartScript)
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
		"GC_CITY_RUNTIME_DIR", "GC_PACK_STATE_DIR", "GC_DOLT_LOG_FILE",
		"GC_DOLT_STATE_FILE", "GC_DOLT_PID_FILE", "GC_BEADS_BD_SCRIPT",
	),
		"PATH="+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd.CombinedOutput()
}

func TestRestartCallsStopThenStart_HappyPath(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 0, "start": 0})

	out, err := runRestart(t, cityPath, root, port)
	if err != nil {
		t.Fatalf("gc dolt restart failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected ops in order 'stop start', got %q\noutput:\n%s", got, out)
	}
}

func TestRestartCallsStartWhenStopReportsNothingRunning(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	// op_stop exits 2 when no managed dolt PID is found. restart must
	// treat that as success and still invoke start.
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 2, "start": 0})

	out, err := runRestart(t, cityPath, root, port)
	if err != nil {
		t.Fatalf("gc dolt restart failed when stop reported nothing-running: %v\n%s", err, out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected ops in order 'stop start' (exit 2 on stop is recoverable), got %q\noutput:\n%s", got, out)
	}
}

func TestRestartDoesNotRequirePortWhenStopReportsNothingRunning(t *testing.T) {
	root := repoRoot(t)
	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 2, "start": 0})

	out, err := runRestartWithEnv(t, cityPath, root, nil)
	if err != nil {
		t.Fatalf("gc dolt restart failed without a resolved runtime port: %v\n%s", err, out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected ops in order 'stop start' without a runtime port, got %q\noutput:\n%s", got, out)
	}
}

func TestRestartAbortsAndDoesNotStartWhenStopFails(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	// op_stop exit code 1 is the genuine-failure path (e.g., couldn't kill
	// the managed PID). restart must abort without calling start so the
	// operator can investigate.
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 1, "start": 0})

	out, err := runRestart(t, cityPath, root, port)
	if err == nil {
		t.Fatalf("gc dolt restart unexpectedly succeeded when stop failed:\n%s", out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	if strings.Contains(string(data), "start") {
		t.Fatalf("restart called start after stop failed; ops log:\n%s\noutput:\n%s", data, out)
	}
}

func TestRestartPropagatesStartFailureWithDiagnostic(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 0, "start": 1})

	out, err := runRestart(t, cityPath, root, port)
	if err == nil {
		t.Fatalf("gc dolt restart unexpectedly succeeded when start failed:\n%s", out)
	}
	if !strings.Contains(string(out), "gc dolt restart: start failed (exit 1)") {
		t.Fatalf("restart did not report start failure; output:\n%s", out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected restart to attempt stop and start before reporting start failure, got %q\noutput:\n%s", got, out)
	}
}

func TestRestartAllowsStaleENOSPCBeforeCurrentProcessStart(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 0, "start": 0})
	writeRestartLog(t, cityPath, "time=\"2000-01-01T00:00:00Z\" level=warning msg=\"conjoin failed\"\n"+
		"fatal error: write .beads/dolt/hq/.dolt/noms/vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv: no space left on device: error writing to database journal file\n")

	out, err := runRestart(t, cityPath, root, port)
	if err != nil {
		t.Fatalf("gc dolt restart blocked a stale ENOSPC from before the current process start: %v\n%s", err, out)
	}
	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	if got := strings.Join(strings.Fields(string(data)), " "); got != "stop start" {
		t.Fatalf("expected stale ENOSPC to allow stop then start, got %q\noutput:\n%s", got, out)
	}
}

func TestRestartAllowsStaleENOSPCBeforeDeadProcessLaunch(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 0, "start": 0})
	writeDeadDoltLaunchState(t, cityPath, "2026-09-01T00:00:00Z")
	writeRestartLog(t, cityPath, "time=\"2026-08-31T23:59:59Z\" level=error msg=\"ENOSPC\"\n")

	out, err := runRestart(t, cityPath, root, port)
	if err != nil {
		t.Fatalf("gc dolt restart blocked stale ENOSPC after the managed process died: %v\n%s", err, out)
	}
	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	if got := strings.Join(strings.Fields(string(data)), " "); got != "stop start" {
		t.Fatalf("expected stale ENOSPC to allow stop then start for dead PID, got %q\noutput:\n%s", got, out)
	}
}

func TestRestartRefusesCurrentENOSPCAfterDeadProcessLaunch(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 0, "start": 0})
	writeDeadDoltLaunchState(t, cityPath, "2026-09-01T00:00:00Z")
	writeRestartLog(t, cityPath, "time=\"2026-09-01T00:00:00Z\" level=error msg=\"ENOSPC\"\n")

	out, err := runRestart(t, cityPath, root, port)
	if err == nil {
		t.Fatalf("gc dolt restart ignored ENOSPC from the dead process's launch interval:\n%s", out)
	}
	if !strings.Contains(string(out), "ENOSPC at or after managed Dolt launch") {
		t.Fatalf("restart did not explain current-launch ENOSPC refusal; output:\n%s", out)
	}
	if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("restart invoked gc-beads-bd despite dead-PID ENOSPC refusal; ops log:\n%s\noutput:\n%s", data, out)
	}
}

func TestRestartRefusesUnparseableENOSPCWithDeadProcess(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 0, "start": 0})
	writeDeadDoltLaunchState(t, cityPath, "2026-09-01T00:00:00Z")
	writeRestartLog(t, cityPath, "time=\"not-a-timestamp\" level=error msg=\"ENOSPC\"\n")

	out, err := runRestart(t, cityPath, root, port)
	if err == nil {
		t.Fatalf("gc dolt restart allowed unparseable ENOSPC after the managed process died:\n%s", out)
	}
	if !strings.Contains(string(out), "cannot parse ENOSPC log timestamp") {
		t.Fatalf("restart did not fail closed on dead-PID unparseable ENOSPC; output:\n%s", out)
	}
	if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("restart invoked gc-beads-bd after dead-PID timestamp failure; ops log:\n%s\noutput:\n%s", data, out)
	}
}

func TestRestartRefusesFreshENOSPCUnlessForced(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 0, "start": 0})
	freshTimestamp := time.Now().UTC().Format(time.RFC3339)
	writeRestartLog(t, cityPath, fmt.Sprintf("time=\"%s\" level=warning msg=\"conjoin failed\"\n", freshTimestamp)+
		"fatal error: write .beads/dolt/hq/.dolt/noms/vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv: no space left on device: error writing to database journal file\n")

	out, err := runRestart(t, cityPath, root, port)
	if err == nil {
		t.Fatalf("gc dolt restart unexpectedly ignored ENOSPC after the current process start:\n%s", out)
	}
	if !strings.Contains(string(out), "ENOSPC at or after managed Dolt launch") {
		t.Fatalf("restart did not explain ENOSPC refusal; output:\n%s", out)
	}
	if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("restart invoked gc-beads-bd despite ENOSPC refusal; ops log:\n%s\noutput:\n%s", data, out)
	}

	out, err = runRestartWithEnv(t, cityPath, root, []string{fmt.Sprintf("GC_DOLT_PORT=%d", port)}, "--force")
	if err != nil {
		t.Fatalf("gc dolt restart --force failed despite fake stop/start success: %v\n%s", err, out)
	}
	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected forced restart to call stop then start, got %q\noutput:\n%s", got, out)
	}
}

func TestRestartRefusesUnparseableFreshENOSPCWithDiagnostic(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 0, "start": 0})
	writeRestartLog(t, cityPath, "time=\"not-a-timestamp\" level=error msg=\"ENOSPC\"\n")

	out, err := runRestart(t, cityPath, root, port)
	if err == nil {
		t.Fatalf("gc dolt restart unexpectedly allowed an unparseable ENOSPC line:\n%s", out)
	}
	if !strings.Contains(string(out), "cannot parse ENOSPC log timestamp") {
		t.Fatalf("restart did not diagnose the unparseable ENOSPC timestamp; output:\n%s", out)
	}
	if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("restart invoked gc-beads-bd after fail-closed timestamp parsing; ops log:\n%s\noutput:\n%s", data, out)
	}
}

// TestRestartRefusesCurrentENOSPCWhenRetainedPIDWasReused pins the PID-reuse
// fail-closed contract: a crashed Dolt leaves dolt.pid behind, and when an
// unrelated process later takes that PID its start time must not become the
// launch boundary that reclassifies current-launch ENOSPC as stale.
func TestRestartRefusesCurrentENOSPCWhenRetainedPIDWasReused(t *testing.T) {
	root := repoRoot(t)
	for _, tc := range []struct {
		name       string
		writeState bool
		wantReason string
	}{
		{name: "with provider state", writeState: true, wantReason: "ENOSPC at or after managed Dolt launch"},
		{name: "without provider state", writeState: false, wantReason: "cannot determine managed Dolt launch boundary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, cleanup := startReachableTCPListener(t)
			defer cleanup()

			reused := exec.Command("sleep", "60")
			if err := reused.Start(); err != nil {
				t.Fatalf("start non-Dolt process: %v", err)
			}
			t.Cleanup(func() {
				_ = reused.Process.Kill()
				_ = reused.Wait()
			})

			cityPath := t.TempDir()
			bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 0, "start": 0})
			stateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
			if err := os.Remove(filepath.Join(stateDir, "dolt-provider-state.json")); err != nil {
				t.Fatalf("remove default provider state: %v", err)
			}
			now := time.Now().UTC()
			if tc.writeState {
				writeDoltLaunchState(t, cityPath, reused.Process.Pid, now.Add(-time.Hour).Format(time.RFC3339))
			} else {
				writeDoltPIDFile(t, cityPath, reused.Process.Pid)
			}
			// After the recorded launch, but before the reused PID's process started.
			writeRestartLog(t, cityPath, fmt.Sprintf("time=\"%s\" level=error msg=\"ENOSPC\"\n",
				now.Add(-30*time.Minute).Format(time.RFC3339)))

			out, err := runRestart(t, cityPath, root, port)
			if err == nil {
				t.Fatalf("gc dolt restart trusted a reused non-Dolt PID's start time:\n%s", out)
			}
			if !strings.Contains(string(out), tc.wantReason) {
				t.Fatalf("restart output missing %q; output:\n%s", tc.wantReason, out)
			}
			if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
				t.Fatalf("restart invoked gc-beads-bd despite reused-PID refusal; ops log:\n%s\noutput:\n%s", data, out)
			}
		})
	}
}

// TestRestartTreatsLoopbackAndWildcardHostsAsLocalManaged pins the P0.5
// host-classification contract: the managed-server bind default is
// 127.0.0.1, and 0.0.0.0 remains the explicit wildcard opt-out — both must
// be treated as a GC-managed local server (restart proceeds), exactly like
// an unset GC_DOLT_HOST. Without 127.0.0.1 in the local set, adopting the
// loopback bind default would break managed-server detection.
func TestRestartTreatsLoopbackAndWildcardHostsAsLocalManaged(t *testing.T) {
	root := repoRoot(t)

	for _, host := range []string{"127.0.0.1", "0.0.0.0", "localhost", "::1"} {
		t.Run(host, func(t *testing.T) {
			port, cleanup := startReachableTCPListener(t)
			defer cleanup()

			cityPath := t.TempDir()
			bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 0, "start": 0})

			out, err := runRestartWithEnv(t, cityPath, root, []string{
				fmt.Sprintf("GC_DOLT_PORT=%d", port),
				"GC_DOLT_HOST=" + host,
			})
			if err != nil {
				t.Fatalf("gc dolt restart refused GC_DOLT_HOST=%s as if remote: %v\n%s", host, err, out)
			}

			data, err := os.ReadFile(bdLog)
			if err != nil {
				t.Fatalf("read fake bd log: %v", err)
			}
			got := strings.Join(strings.Fields(string(data)), " ")
			if got != "stop start" {
				t.Fatalf("expected ops in order 'stop start' for local host %s, got %q\noutput:\n%s", host, got, out)
			}
		})
	}
}

func TestRestartRejectsRemoteHostWithDiagnostic(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, root, map[string]int{"stop": 0, "start": 0})

	out, err := runRestartWithEnv(t, cityPath, root, []string{
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_HOST=example.internal",
	})
	if err == nil {
		t.Fatalf("gc dolt restart unexpectedly succeeded for a remote host:\n%s", out)
	}
	if !strings.Contains(string(out), "gc dolt restart: not supported for remote dolt servers") {
		t.Fatalf("restart did not explain remote-host refusal; output:\n%s", out)
	}
	if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("restart invoked gc-beads-bd despite remote-host refusal; ops log:\n%s\noutput:\n%s", data, out)
	}
}
