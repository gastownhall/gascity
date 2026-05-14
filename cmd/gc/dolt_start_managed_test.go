package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	bdpack "github.com/gastownhall/gascity/examples/bd"
)

func TestDoltServerEnv_DoesNotInjectGCSchedulerDefault(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "HOME=/home/test"}
	out := doltServerEnv(parent)

	for _, kv := range out {
		if strings.HasPrefix(kv, "DOLT_GC_SCHEDULER=") {
			t.Fatalf("managed Dolt env should not inject GC scheduler default, got %v", out)
		}
	}
	// Original entries preserved.
	for _, kv := range parent {
		var hit bool
		for _, got := range out {
			if got == kv {
				hit = true
				break
			}
		}
		if !hit {
			t.Fatalf("parent entry %q missing from output env %v", kv, out)
		}
	}
}

func TestDoltServerEnv_RespectsUserOverride(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "DOLT_GC_SCHEDULER=LOADAVG", "HOME=/home/test"}
	out := doltServerEnv(parent)

	// User-provided value must be preserved exactly.
	count := 0
	for _, kv := range out {
		if kv == "DOLT_GC_SCHEDULER=LOADAVG" {
			count++
		}
		if kv == "DOLT_GC_SCHEDULER=NONE" {
			t.Fatalf("user override clobbered by default: %v", out)
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one DOLT_GC_SCHEDULER=LOADAVG entry, got %d in %v", count, out)
	}
}

func TestDoltServerEnv_PreservesEmptyUserValue(t *testing.T) {
	parent := []string{"DOLT_GC_SCHEDULER="}
	out := doltServerEnv(parent)
	if len(out) != 1 || out[0] != "DOLT_GC_SCHEDULER=" {
		t.Fatalf("explicit empty-value env not preserved: %v", out)
	}
}

func TestGCBeadsBDScript_DoesNotDefaultDoltGCScheduler(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	scriptPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	script := string(data)

	for _, forbidden := range []string{`DOLT_GC_SCHEDULER=NONE`, `DOLT_GC_SCHEDULER:=NONE`} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("gc-beads-bd.sh must not default DOLT_GC_SCHEDULER; found %q", forbidden)
		}
	}
}

func TestGCBeadsBDScript_UsesPortableSleepMS(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	scriptPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	script := string(data)
	embedded, err := bdpack.PackFS.ReadFile("assets/scripts/gc-beads-bd.sh")
	if err != nil {
		t.Fatalf("read embedded gc-beads-bd.sh: %v", err)
	}
	if string(embedded) != script {
		t.Fatalf("embedded gc-beads-bd.sh differs from source script")
	}

	if !strings.Contains(script, "sleep_ms()") {
		t.Fatalf("gc-beads-bd.sh must define portable sleep_ms helper")
	}
	if strings.Contains(script, `sleep "$(awk`) {
		t.Fatalf("gc-beads-bd.sh must not use awk to calculate sleep durations")
	}
	if got := strings.Count(script, `sleep_ms "$backoff_ms" 2>/dev/null || sleep 1`); got < 3 {
		t.Fatalf("gc-beads-bd.sh must use sleep_ms for retry backoff sleeps; found %d call sites", got)
	}
	if !strings.Contains(script, "for attempt in 1 2 3 4 5 6 7 8; do") {
		t.Fatalf("gc-beads-bd.sh must allow slow bd runtime schema visibility after init")
	}
}

// TestGCBeadsBDScript_DoesNotMutateDoltInternals pins gc-beads-bd.sh against
// re-introducing any mv/rm of files under a .dolt/ directory. Comments are
// permitted; only non-comment occurrences fail the test.
func TestGCBeadsBDScript_DoesNotMutateDoltInternals(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	scriptPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	script := string(data)

	forbidden := []string{
		"cleanup_stale_locks()",
		"quarantine_phantom_dbs()",
		`mv -f "$dir" "$quarantine_dir"`,
		`rm -f "$lock_file"`,
	}
	for _, bad := range forbidden {
		// Allow appearances inside comments (lines starting with `#`).
		for _, line := range strings.Split(script, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.Contains(line, bad) {
				t.Fatalf("gc-beads-bd.sh contains forbidden Dolt-internal mutator %q: %s", bad, line)
			}
		}
	}
}

// TestGCBeadsBDScript_InitForcesReinitOverPreSeededMetadata guards the
// fresh-init regression where `gc init` / `gc rig add` aborted at provider
// readiness with bd's "This workspace is already initialized" error. GC
// pre-seeds .beads/metadata.json (dolt_database/dolt_mode) before invoking
// gc-beads-bd init; bd (>= 1.0.x) treats any present metadata.json as proof
// the workspace is already initialized and bails unless `bd init` is given
// --force. op_init's "already initialized on disk" branch must therefore key
// on the metadata.json file itself (not on a project_id, which a fresh
// pre-seeded stub never has) so the schema-missing path can set --force.
func TestGCBeadsBDScript_InitForcesReinitOverPreSeededMetadata(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	scriptPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	script := string(data)

	guard := `if [ -f "$dir/.beads/metadata.json" ]; then
        if ensure_database_registered "$dolt_database"; then`
	if !strings.Contains(script, guard) {
		t.Fatalf("gc-beads-bd.sh op_init must gate the already-initialized branch on the metadata.json file, not on project_id; " +
			"gating on project_id leaves --force unset for gc-pre-seeded metadata and bd init aborts")
	}
	if strings.Contains(script, `if metadata_has_project_id "$dir/.beads/metadata.json"; then
        if ensure_database_registered`) {
		t.Fatal("gc-beads-bd.sh op_init must not gate the already-initialized branch on metadata_has_project_id (fresh-init regression)")
	}
}

func TestManagedDoltStartFields(t *testing.T) {
	report := managedDoltStartReport{
		Ready:        true,
		PID:          4321,
		Port:         3312,
		AddressInUse: false,
		Attempts:     2,
	}
	fields := managedDoltStartFields(report)
	want := []string{
		"ready\ttrue",
		"pid\t4321",
		"port\t3312",
		"address_in_use\tfalse",
		"attempts\t2",
	}
	if len(fields) != len(want) {
		t.Fatalf("got %d fields, want %d", len(fields), len(want))
	}
	for i, w := range want {
		if fields[i] != w {
			t.Errorf("fields[%d] = %q, want %q", i, fields[i], w)
		}
	}
}

func withManagedDoltTestMode(t *testing.T, enabled bool) {
	t.Helper()
	old := managedDoltTestMode
	managedDoltTestMode = func() bool { return enabled }
	t.Cleanup(func() { managedDoltTestMode = old })
}

func clearManagedDoltTestProcessRegistry(t *testing.T) {
	t.Helper()
	managedDoltTestProcessRegistry.Range(func(key, _ any) bool {
		managedDoltTestProcessRegistry.Delete(key)
		return true
	})
}

func TestManagedDoltSQLServerSysProcAttrProductionDetaches(t *testing.T) {
	withManagedDoltTestMode(t, false)
	t.Setenv(managedDoltTestModeEnv, "")

	attr := managedDoltSQLServerSysProcAttr()

	if attr == nil || !attr.Setpgid {
		t.Fatalf("production managed Dolt must keep detached process-group behavior, got %#v", attr)
	}
}

func TestManagedDoltSQLServerSysProcAttrTestModeDoesNotDetach(t *testing.T) {
	withManagedDoltTestMode(t, true)

	attr := managedDoltSQLServerSysProcAttr()

	if attr != nil {
		t.Fatalf("test-mode managed Dolt must stay in the test process group, got %#v", attr)
	}
}

func TestManagedDoltTestWatchdogCanBeDisabledByEnv(t *testing.T) {
	withManagedDoltTestMode(t, true)
	t.Setenv("GC_MANAGED_DOLT_TEST_WATCHDOG", "0")

	if managedDoltTestWatchdogEnabled() {
		t.Fatalf("managedDoltTestWatchdogEnabled() = true, want false when GC_MANAGED_DOLT_TEST_WATCHDOG=0")
	}
}

func TestManagedDoltTestWatchdogExecutableUsesOSExecutable(t *testing.T) {
	oldExecutable := managedDoltTestExecutable
	t.Cleanup(func() { managedDoltTestExecutable = oldExecutable })
	want := filepath.Join(t.TempDir(), "gc-test-binary")
	managedDoltTestExecutable = func() (string, error) {
		return want, nil
	}

	got, err := managedDoltTestWatchdogExecutable()
	if err != nil {
		t.Fatalf("managedDoltTestWatchdogExecutable: %v", err)
	}
	if got != want {
		t.Fatalf("managedDoltTestWatchdogExecutable() = %q, want %q", got, want)
	}
}

type blockingWatchdogPIDReader struct {
	started chan struct{}
	unblock chan struct{}
	done    chan struct{}
}

func newBlockingWatchdogPIDReader() *blockingWatchdogPIDReader {
	return &blockingWatchdogPIDReader{
		started: make(chan struct{}, 1),
		unblock: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (r *blockingWatchdogPIDReader) Read(_ []byte) (int, error) {
	defer close(r.done)
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-r.unblock
	return 0, io.EOF
}

func (r *blockingWatchdogPIDReader) Close() {
	close(r.unblock)
}

func TestReadManagedDoltTestWatchdogPIDTimeoutUnblocksReaderAfterClose(t *testing.T) {
	oldTimeout := managedDoltTestWatchdogPIDTimeout
	managedDoltTestWatchdogPIDTimeout = 10 * time.Millisecond
	t.Cleanup(func() { managedDoltTestWatchdogPIDTimeout = oldTimeout })

	reader := newBlockingWatchdogPIDReader()
	done := make(chan error, 1)
	go func() {
		_, err := readManagedDoltTestWatchdogPID(reader, 12345)
		done <- err
	}()

	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("reader did not start")
	}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("readManagedDoltTestWatchdogPID error = %v, want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("readManagedDoltTestWatchdogPID did not time out")
	}

	reader.Close()
	select {
	case <-reader.done:
	case <-time.After(time.Second):
		t.Fatal("watchdog PID reader goroutine stayed blocked after close")
	}
}

func TestManagedDoltTestModeEnabledHonorsEnv(t *testing.T) {
	withManagedDoltTestMode(t, false)
	t.Setenv("GC_MANAGED_DOLT_TEST_MODE", "1")

	if !managedDoltTestModeEnabled() {
		t.Fatalf("managedDoltTestModeEnabled() = false, want true when GC_MANAGED_DOLT_TEST_MODE=1")
	}
	if !managedDoltTestModeFromEnvOnly() {
		t.Fatalf("managedDoltTestModeFromEnvOnly() = false, want true for built helper test mode")
	}
}

func TestManagedDoltTestModeFromEnvOnlyFalseForTestBinary(t *testing.T) {
	withManagedDoltTestMode(t, true)
	t.Setenv("GC_MANAGED_DOLT_TEST_MODE", "1")

	if managedDoltTestModeFromEnvOnly() {
		t.Fatalf("managedDoltTestModeFromEnvOnly() = true, want false for the test binary itself")
	}
}

func TestManagedDoltTestParentPIDHonorsEnv(t *testing.T) {
	t.Setenv(managedDoltTestParentPIDEnv, "12345")

	if got := managedDoltTestParentPID(); got != 12345 {
		t.Fatalf("managedDoltTestParentPID() = %d, want 12345", got)
	}
}

func TestManagedDoltTestDisarmOnReadyStaysArmedForExternalParent(t *testing.T) {
	withManagedDoltTestMode(t, false)
	t.Setenv(managedDoltTestModeEnv, "1")
	t.Setenv(managedDoltTestParentPIDEnv, strconv.Itoa(os.Getpid()+1))

	if managedDoltTestDisarmOnReady() {
		t.Fatal("managedDoltTestDisarmOnReady() = true, want false with external parent")
	}
}

func TestManagedDoltTestDisarmOnReadyForEnvOnlyHelperWithoutParent(t *testing.T) {
	withManagedDoltTestMode(t, false)
	t.Setenv(managedDoltTestModeEnv, "1")

	if !managedDoltTestDisarmOnReady() {
		t.Fatal("managedDoltTestDisarmOnReady() = false, want true without external parent")
	}
}

func TestDisarmManagedDoltStartedProcessUnregistersReadyProcess(t *testing.T) {
	withManagedDoltTestMode(t, true)
	clearManagedDoltTestProcessRegistry(t)
	t.Cleanup(func() {
		clearManagedDoltTestProcessRegistry(t)
	})

	pid := os.Getpid()
	disarmFile := filepath.Join(t.TempDir(), "disarm-ready")
	started := managedDoltStartedProcess{
		PID:         pid,
		WatchdogPID: pid,
		DisarmFile:  disarmFile,
		DisarmReady: true,
	}
	registerManagedDoltTestProcess(started)

	disarmManagedDoltStartedProcess(started)

	data, err := os.ReadFile(disarmFile)
	if err != nil {
		t.Fatalf("read disarm file: %v", err)
	}
	if string(data) != "ready\n" {
		t.Fatalf("disarm file = %q, want ready marker", string(data))
	}
	var remaining int
	managedDoltTestProcessRegistry.Range(func(_, _ any) bool {
		remaining++
		return true
	})
	if remaining != 0 {
		t.Fatalf("registry still has %d entries after disarm", remaining)
	}
}

func TestTerminateManagedDoltStartedProcessUnregistersFailedStartup(t *testing.T) {
	withManagedDoltTestMode(t, true)
	clearManagedDoltTestProcessRegistry(t)
	t.Cleanup(func() {
		clearManagedDoltTestProcessRegistry(t)
	})

	startChild := func(name string) *exec.Cmd {
		t.Helper()
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s child: %v", name, err)
		}
		t.Cleanup(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		})
		return cmd
	}

	dolt := startChild("dolt")
	watchdog := startChild("watchdog")
	disarmFile := filepath.Join(t.TempDir(), "disarm")
	if err := os.WriteFile(disarmFile, []byte("ready\n"), 0o644); err != nil {
		t.Fatalf("write disarm file: %v", err)
	}
	started := managedDoltStartedProcess{
		PID:         dolt.Process.Pid,
		WatchdogPID: watchdog.Process.Pid,
		DisarmFile:  disarmFile,
	}
	registerManagedDoltTestProcess(started)

	terminateManagedDoltStartedProcess(started)

	var remaining int
	managedDoltTestProcessRegistry.Range(func(_, _ any) bool {
		remaining++
		return true
	})
	if remaining != 0 {
		t.Fatalf("registry still has %d entries after startup-failure terminate", remaining)
	}
	if _, err := os.Stat(disarmFile); !os.IsNotExist(err) {
		t.Fatalf("disarm file still exists after terminate: %v", err)
	}
}

func TestReapManagedDoltTestProcessesTerminatesRegisteredChildren(t *testing.T) {
	withManagedDoltTestMode(t, true)
	clearManagedDoltTestProcessRegistry(t)
	t.Cleanup(func() {
		clearManagedDoltTestProcessRegistry(t)
	})
	oldTerminate := managedDoltTestTerminateProcess
	var terminated []int
	managedDoltTestTerminateProcess = func(pid int) error {
		terminated = append(terminated, pid)
		return nil
	}
	t.Cleanup(func() { managedDoltTestTerminateProcess = oldTerminate })

	pid := os.Getpid()
	registerManagedDoltTestProcess(managedDoltStartedProcess{PID: pid, WatchdogPID: pid})
	reapManagedDoltTestProcesses()

	if len(terminated) != 2 || terminated[0] != pid || terminated[1] != pid {
		t.Fatalf("terminated = %v, want child and watchdog pid %d", terminated, pid)
	}
	var remaining int
	managedDoltTestProcessRegistry.Range(func(_, _ any) bool {
		remaining++
		return true
	})
	if remaining != 0 {
		t.Fatalf("registry still has %d entries after reap", remaining)
	}
}

func TestManagedDoltLogSize(t *testing.T) {
	t.Run("existing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dolt.log")
		if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, err := managedDoltLogSize(path)
		if err != nil {
			t.Fatalf("managedDoltLogSize: %v", err)
		}
		if got != 12 {
			t.Errorf("managedDoltLogSize = %d, want 12", got)
		}
	})

	t.Run("missing file returns zero", func(t *testing.T) {
		got, err := managedDoltLogSize(filepath.Join(t.TempDir(), "no-such.log"))
		if err != nil {
			t.Fatalf("managedDoltLogSize: %v", err)
		}
		if got != 0 {
			t.Errorf("managedDoltLogSize = %d, want 0", got)
		}
	})
}

func TestManagedDoltLogSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dolt.log")
	content := "line one\nline two\nline three\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Run("from offset", func(t *testing.T) {
		got, err := managedDoltLogSuffix(path, 9)
		if err != nil {
			t.Fatalf("managedDoltLogSuffix: %v", err)
		}
		if got != "line two\nline three\n" {
			t.Errorf("got %q, want %q", got, "line two\nline three\n")
		}
	})

	t.Run("offset past end returns empty", func(t *testing.T) {
		got, err := managedDoltLogSuffix(path, int64(len(content)+10))
		if err != nil {
			t.Fatalf("managedDoltLogSuffix: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("negative offset treated as zero", func(t *testing.T) {
		got, err := managedDoltLogSuffix(path, -5)
		if err != nil {
			t.Fatalf("managedDoltLogSuffix: %v", err)
		}
		if got != content {
			t.Errorf("got %q, want %q", got, content)
		}
	})

	t.Run("missing file returns empty", func(t *testing.T) {
		got, err := managedDoltLogSuffix(filepath.Join(dir, "no-such.log"), 0)
		if err != nil {
			t.Fatalf("managedDoltLogSuffix: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestResolveDoltArchiveLevel(t *testing.T) {
	tests := []struct {
		name     string
		explicit int
		envVal   string
		want     int
	}{
		{name: "explicit zero", explicit: 0, want: 0},
		{name: "explicit positive", explicit: 1, want: 1},
		{name: "explicit large", explicit: 42, want: 42},
		{name: "negative defaults to zero", explicit: -1, want: 0},
		{name: "negative with valid env", explicit: -1, envVal: "1", want: 1},
		{name: "negative with env zero", explicit: -1, envVal: "0", want: 0},
		{name: "negative with non-numeric env falls back", explicit: -1, envVal: "abc", want: 0},
		{name: "negative with empty env", explicit: -1, envVal: "", want: 0},
		{name: "explicit overrides env", explicit: 2, envVal: "5", want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GC_DOLT_ARCHIVE_LEVEL", tt.envVal)
			if got := resolveDoltArchiveLevel(tt.explicit); got != tt.want {
				t.Errorf("resolveDoltArchiveLevel(%d) = %d, want %d", tt.explicit, got, tt.want)
			}
		})
	}
}

// TestTerminateManagedDoltPID_HonorsSubPollGrace asserts that terminate uses
// the grace-clamped poll interval (managedDoltStopPollInterval) rather than a
// fixed sleep: a SIGTERM-ignoring process with a tiny configured grace must be
// SIGKILLed and the call must return quickly, not after a fixed ~100ms sleep
// past the deadline (gastownhall/gascity#2090, finding 6).
func TestTerminateManagedDoltPID_HonorsSubPollGrace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics required")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test"

[daemon]
dolt_stop_timeout = "5ms"

[[agent]]
name = "mayor"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	// A process that ignores SIGTERM forces the wait loop to run to the
	// deadline and escalate to SIGKILL.
	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	start := time.Now()
	if err := terminateManagedDoltPID(dir, pid); err != nil {
		t.Fatalf("terminateManagedDoltPID: %v", err)
	}
	elapsed := time.Since(start)

	// 5ms grace + the fixed 250ms post-SIGKILL settle. A fixed-100ms poll
	// could overshoot the 5ms deadline; the clamp keeps the SIGTERM wait at
	// ~5ms. Allow generous slack for scheduler jitter under CI load.
	if elapsed > 2*time.Second {
		t.Errorf("terminateManagedDoltPID took %v with a 5ms grace; sub-poll clamp not honored", elapsed)
	}
	if pidAlive(pid) {
		t.Errorf("pid %d still alive after terminateManagedDoltPID; SIGKILL escalation did not fire", pid)
	}
}
