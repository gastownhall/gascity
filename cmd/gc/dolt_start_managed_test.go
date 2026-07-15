package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	bdpack "github.com/gastownhall/gascity/examples/bd"
	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

func TestDoltServerEnv_DoesNotInjectGCSchedulerDefault(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "HOME=/home/test"}
	out := doltServerEnv("", parent)

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
	out := doltServerEnv("", parent)

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
	out := doltServerEnv("", parent)
	// The explicit empty-value parent entry must be preserved exactly, and the
	// managed-server telemetry disable must be appended.
	var hasParent, hasTelemetryDisable bool
	for _, kv := range out {
		switch kv {
		case "DOLT_GC_SCHEDULER=":
			hasParent = true
		case "DOLT_DISABLE_EVENT_FLUSH=true":
			hasTelemetryDisable = true
		}
	}
	if !hasParent {
		t.Fatalf("explicit empty-value env not preserved: %v", out)
	}
	if !hasTelemetryDisable {
		t.Fatalf("managed Dolt env should disable telemetry event flush: %v", out)
	}
}

func TestDoltServerEnv_UsesDoltConfigObjectOptOut(t *testing.T) {
	cityPath := t.TempDir()
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(beadsDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("dolt:\n  disable-event-flush: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := doltServerEnv(cityPath, []string{
		"PATH=/usr/bin",
		"DOLT_DISABLE_EVENT_FLUSH=true",
	})

	for _, kv := range out {
		if strings.HasPrefix(kv, "DOLT_DISABLE_EVENT_FLUSH=") {
			t.Fatalf("config opt-out should remove telemetry-disable env, got %v", out)
		}
	}
}

func TestDoltServerEnv_DefaultsDoltConfigObjectToDisableEventFlush(t *testing.T) {
	cityPath := t.TempDir()
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(beadsDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("issue_prefix: gc\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := doltServerEnv(cityPath, []string{
		"PATH=/usr/bin",
		"DOLT_DISABLE_EVENT_FLUSH=false",
	})

	count := 0
	for _, kv := range out {
		if kv == "DOLT_DISABLE_EVENT_FLUSH=true" {
			count++
		}
		if kv == "DOLT_DISABLE_EVENT_FLUSH=false" {
			t.Fatalf("default disable should replace inherited false env, got %v", out)
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one DOLT_DISABLE_EVENT_FLUSH=true entry, got %d in %v", count, out)
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

	guard := `if [ -f "$dir/.beads/metadata.json" ]; then`
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

func writeFakeDoltSQLServer(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake requires POSIX sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "dolt")
	content := "#!/bin/sh\n" +
		"if [ \"$1\" != \"sql-server\" ]; then\n" +
		"  echo \"unexpected dolt args: $*\" >&2\n" +
		"  exit 2\n" +
		"fi\n" +
		"exec sleep 60\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return dir
}

func readManagedDoltTestState(t *testing.T, path string) (int, int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read helper state: %v", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		t.Fatalf("helper state %q has %d fields, want 2", string(data), len(fields))
	}
	doltPID, err := strconv.Atoi(fields[0])
	if err != nil || doltPID <= 0 {
		t.Fatalf("helper dolt pid %q invalid", fields[0])
	}
	watchdogPID, err := strconv.Atoi(fields[1])
	if err != nil || watchdogPID <= 0 {
		t.Fatalf("helper watchdog pid %q invalid", fields[1])
	}
	return doltPID, watchdogPID
}

func cleanupManagedDoltTestPID(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 {
		return
	}
	_ = terminateManagedDoltTestPID(pid)
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

func TestManagedDoltWatchdogExecutableUsesOSExecutable(t *testing.T) {
	oldExecutable := managedDoltTestExecutable
	t.Cleanup(func() { managedDoltTestExecutable = oldExecutable })
	want := filepath.Join(t.TempDir(), "gc-test-binary")
	managedDoltTestExecutable = func() (string, error) {
		return want, nil
	}

	got, err := managedDoltWatchdogExecutable()
	if err != nil {
		t.Fatalf("managedDoltWatchdogExecutable: %v", err)
	}
	if got != want {
		t.Fatalf("managedDoltWatchdogExecutable() = %q, want %q", got, want)
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

func TestManagedDoltTestParentDoneClosesOnPipeEOF(t *testing.T) {
	parentPipeRead, parentPipeWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer parentPipeRead.Close() //nolint:errcheck
	parentPipeFD, err := syscall.Dup(int(parentPipeRead.Fd()))
	if err != nil {
		t.Fatalf("dup parent pipe fd: %v", err)
	}
	done, closeDone, err := managedDoltTestParentDone(strconv.Itoa(parentPipeFD))
	if err != nil {
		_ = syscall.Close(parentPipeFD)
		t.Fatalf("managedDoltTestParentDone: %v", err)
	}
	defer closeDone()

	if err := parentPipeWrite.Close(); err != nil {
		t.Fatalf("close parent pipe writer: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("parent pipe EOF did not close done channel")
	}
}

func TestManagedDoltWatchdogExternalParentSurvivesSpawnerExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics required")
	}
	dir := t.TempDir()
	fakeDoltDir := writeFakeDoltSQLServer(t)
	statePath := filepath.Join(dir, "state")
	configPath := filepath.Join(dir, "dolt-config.yaml")
	logPath := filepath.Join(dir, "dolt.log")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestManagedDoltWatchdogExternalParentHelper", "-test.v")
	cmd.Env = sanitizedBaseEnv(
		"GC_TEST_MANAGED_DOLT_HELPER=external-parent",
		"GC_TEST_MANAGED_DOLT_HELPER_PARENT_PID="+strconv.Itoa(os.Getpid()),
		"GC_TEST_MANAGED_DOLT_HELPER_STATE="+statePath,
		"GC_TEST_MANAGED_DOLT_HELPER_CONFIG="+configPath,
		"GC_TEST_MANAGED_DOLT_HELPER_LOG="+logPath,
		"GC_TEST_MANAGED_DOLT_HELPER_FAKE_DOLT_DIR="+fakeDoltDir,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, output)
	}
	doltPID, watchdogPID := readManagedDoltTestState(t, statePath)
	t.Cleanup(func() {
		cleanupManagedDoltTestPID(t, doltPID)
		cleanupManagedDoltTestPID(t, watchdogPID)
	})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(doltPID) {
			logData, _ := os.ReadFile(logPath)
			t.Fatalf("fake dolt pid %d exited after short-lived spawner exit; helper output:\n%s\nwatchdog log:\n%s", doltPID, output, logData)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestManagedDoltWatchdogExternalParentHelper(t *testing.T) {
	if os.Getenv("GC_TEST_MANAGED_DOLT_HELPER") != "external-parent" {
		t.Skip("helper process only")
	}
	parentPID := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_PARENT_PID"))
	if parentPID == "" {
		t.Fatal("missing helper parent pid")
	}
	t.Setenv(managedDoltTestModeEnv, "1")
	t.Setenv(managedDoltTestParentPIDEnv, parentPID)
	fakeDoltDir := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_FAKE_DOLT_DIR"))
	if fakeDoltDir == "" {
		t.Fatal("missing fake dolt dir")
	}
	t.Setenv("PATH", fakeDoltDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	statePath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_STATE"))
	configPath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_CONFIG"))
	logPath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_LOG"))
	if statePath == "" || configPath == "" || logPath == "" {
		t.Fatal("missing helper paths")
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log file: %v", err)
	}
	defer logFile.Close() //nolint:errcheck

	started, err := startManagedDoltSQLServerWithTestWatchdog("", configPath, logPath, logFile)
	if err != nil {
		t.Fatalf("start managed dolt with watchdog: %v", err)
	}
	state := fmt.Sprintf("%d %d\n", started.PID, started.WatchdogPID)
	if err := os.WriteFile(statePath, []byte(state), 0o644); err != nil {
		t.Fatalf("write helper state: %v", err)
	}
	unregisterManagedDoltStartedProcess(started)
}

func TestManagedDoltWatchdogParentPipeEOFHonorsDisarm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics required")
	}
	withManagedDoltTestMode(t, false)
	t.Setenv(managedDoltTestModeEnv, "1")
	fakeDoltDir := writeFakeDoltSQLServer(t)
	t.Setenv("PATH", fakeDoltDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	configPath := filepath.Join(dir, "dolt-config.yaml")
	logPath := filepath.Join(dir, "dolt.log")
	disarmFile := filepath.Join(dir, "watchdog.disarm")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(disarmFile, []byte("ready\n"), 0o644); err != nil {
		t.Fatalf("write disarm file: %v", err)
	}
	parentPipeRead, parentPipeWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("create parent pipe: %v", err)
	}
	defer parentPipeRead.Close()  //nolint:errcheck
	defer parentPipeWrite.Close() //nolint:errcheck
	watchdogParentPipeFD, err := syscall.Dup(int(parentPipeRead.Fd()))
	if err != nil {
		t.Fatalf("dup parent pipe fd for watchdog: %v", err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		_ = syscall.Close(watchdogParentPipeFD)
		t.Fatalf("create stdout pipe: %v", err)
	}
	defer stdoutRead.Close()  //nolint:errcheck
	defer stdoutWrite.Close() //nolint:errcheck
	stderrPath := filepath.Join(dir, "watchdog.stderr")
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_ = syscall.Close(watchdogParentPipeFD)
		t.Fatalf("open stderr file: %v", err)
	}
	defer stderrFile.Close() //nolint:errcheck

	result := make(chan int, 1)
	args := []string{strconv.Itoa(os.Getpid()), configPath, logPath, disarmFile, strconv.Itoa(watchdogParentPipeFD)}
	go func() {
		result <- runManagedDoltTestWatchdog(args, stdoutWrite, stderrFile)
	}()

	doltPID, err := readManagedDoltTestWatchdogPID(stdoutRead, os.Getpid())
	if err != nil {
		t.Fatalf("read fake dolt pid: %v", err)
	}
	t.Cleanup(func() { cleanupManagedDoltTestPID(t, doltPID) })
	if err := parentPipeWrite.Close(); err != nil {
		t.Fatalf("close parent pipe writer: %v", err)
	}
	select {
	case code := <-result:
		if code != 0 {
			stderrData, _ := os.ReadFile(stderrPath)
			t.Fatalf("watchdog exit code = %d, want 0; stderr:\n%s", code, stderrData)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not exit after disarm file and parent pipe EOF")
	}
	if !pidAlive(doltPID) {
		t.Fatalf("fake dolt pid %d exited; disarm file should win over parent pipe EOF", doltPID)
	}
	if _, err := os.Stat(disarmFile); !os.IsNotExist(err) {
		t.Fatalf("disarm file still exists after watchdog exit: %v", err)
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

// TestTerminateManagedDoltStartedProcessSkipsReusedPID is the PR #4004 review
// follow-up regression. startManagedDoltSQLServer now reaps a failed dolt child
// with a background cmd.Wait(), which frees the numeric PID. A same-attempt
// startup-failure cleanup must therefore verify the PID's start identity before
// signaling, or it could SIGTERM/SIGKILL an unrelated process that reused the
// PID after the child exited. With a snapshot that disagrees with the re-read
// identity, the cleanup must skip the signal and leave the reused process alive.
func TestTerminateManagedDoltStartedProcessSkipsReusedPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics required")
	}

	// A real, unrelated process standing in for the one that reused the PID.
	// It must still be alive after the cleanup runs.
	victim := exec.Command("sleep", "60")
	if err := victim.Start(); err != nil {
		t.Fatalf("start victim: %v", err)
	}
	victimPID := victim.Process.Pid
	t.Cleanup(func() {
		_ = victim.Process.Kill()
		_ = victim.Wait()
	})

	// Snapshot taken at start says ticks=1111; the live PID re-reads as 2222 —
	// a different process incarnation, so the cleanup must not signal it.
	oldTicks := managedDoltTestReadStartTimeTicks
	oldIdent := managedDoltTestReadStartIdentity
	managedDoltTestReadStartTimeTicks = func(int) uint64 { return 2222 }
	managedDoltTestReadStartIdentity = func(int) string { return "" }
	t.Cleanup(func() {
		managedDoltTestReadStartTimeTicks = oldTicks
		managedDoltTestReadStartIdentity = oldIdent
	})

	terminateManagedDoltStartedProcess(managedDoltStartedProcess{PID: victimPID, StartTimeTicks: 1111})

	// Give any erroneous SIGTERM time to land before asserting survival.
	time.Sleep(200 * time.Millisecond)
	if !pidAlive(victimPID) {
		t.Fatalf("terminateManagedDoltStartedProcess signaled reused PID %d; production identity guard not enforced", victimPID)
	}
}

// TestTerminateManagedDoltStartedProcessSignalsWhenIdentityMatches asserts the
// happy-path side of the same guard: when the re-read start identity matches the
// snapshot, the dolt child we actually started is still terminated.
func TestTerminateManagedDoltStartedProcessSignalsWhenIdentityMatches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics required")
	}

	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	childPID := child.Process.Pid
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})

	oldTicks := managedDoltTestReadStartTimeTicks
	managedDoltTestReadStartTimeTicks = func(int) uint64 { return 5555 }
	t.Cleanup(func() { managedDoltTestReadStartTimeTicks = oldTicks })

	terminateManagedDoltStartedProcess(managedDoltStartedProcess{PID: childPID, StartTimeTicks: 5555})

	// terminateManagedDoltPID waits for the SIGTERM to land (sleep dies on it),
	// so the child must be gone once the cleanup returns.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(childPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("terminateManagedDoltStartedProcess did not signal matching PID %d; identity guard wrongly skipped it", childPID)
}

// TestTerminateManagedDoltStartedProcessSkipsReusedPIDBeforeSIGKILL is the
// PR #4004 attempt-4 regression: the entry identity guard is not sufficient on
// its own. terminateManagedDoltPID SIGTERMs, waits out the grace, then escalates
// to SIGKILL by bare PID, so a child that exits and has its PID reused *during*
// the grace must be re-verified before the forced kill. This drives the real
// cleanup with an identity that matches at the SIGTERM check and mismatches at
// the SIGKILL re-check, against a process that ignores SIGTERM so the escalation
// is actually reached.
func TestTerminateManagedDoltStartedProcessSkipsReusedPIDBeforeSIGKILL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics required")
	}
	city, _ := raceTestCity(t, "[workspace]\nname = \"reuse-test\"\n\n[daemon]\ndolt_stop_timeout = \"100ms\"\n")
	dataDir := filepath.Join(city, ".beads", "dolt")
	pid := startSigtermIgnoringProcess(t, dataDir)

	// The snapshot is ticks=1111. The live PID re-reads as 1111 on the first
	// check (before SIGTERM: still our child) then 2222 afterwards (before the
	// SIGKILL escalation: our child exited and the PID was reused mid-grace).
	var reads int
	oldTicks := managedDoltTestReadStartTimeTicks
	managedDoltTestReadStartTimeTicks = func(int) uint64 {
		reads++
		if reads == 1 {
			return 1111
		}
		return 2222
	}
	t.Cleanup(func() { managedDoltTestReadStartTimeTicks = oldTicks })

	oldLog := managedDoltCleanupLogf
	var logged []string
	managedDoltCleanupLogf = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { managedDoltCleanupLogf = oldLog })

	terminateManagedDoltStartedProcess(managedDoltStartedProcess{CityPath: city, PID: pid, StartTimeTicks: 1111})

	if reads < 2 {
		t.Fatalf("identity re-read %d time(s); the SIGKILL escalation must re-verify identity (want >= 2)", reads)
	}
	if !pidAlive(pid) {
		t.Fatal("startup cleanup SIGKILLed a PID reused during the SIGTERM grace; escalation guard not enforced")
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "SIGKILL") || !strings.Contains(logged[0], strconv.Itoa(pid)) {
		t.Fatalf("expected exactly one SIGKILL-skip log naming pid %d, got %v", pid, logged)
	}
}

// TestTerminateManagedDoltStartedProcessForceKillsWhenIdentityStable is the
// happy-path companion: when the start identity stays stable across the whole
// termination, the SIGKILL escalation must still fire on a process that ignores
// SIGTERM. This proves the escalation guard does not break the legacy forced
// kill for our own wedged child.
func TestTerminateManagedDoltStartedProcessForceKillsWhenIdentityStable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics required")
	}
	city, _ := raceTestCity(t, "[workspace]\nname = \"stable-test\"\n\n[daemon]\ndolt_stop_timeout = \"100ms\"\n")
	dataDir := filepath.Join(city, ".beads", "dolt")
	pid := startSigtermIgnoringProcess(t, dataDir)

	oldTicks := managedDoltTestReadStartTimeTicks
	managedDoltTestReadStartTimeTicks = func(int) uint64 { return 3333 }
	t.Cleanup(func() { managedDoltTestReadStartTimeTicks = oldTicks })

	terminateManagedDoltStartedProcess(managedDoltStartedProcess{CityPath: city, PID: pid, StartTimeTicks: 3333})

	deadline := time.Now().Add(2 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatal("startup cleanup did not SIGKILL a stable-identity child that ignored SIGTERM")
	}
}

// TestScopeWatchdogTerminateSkipsReusedWatchdogPID proves the PR #4004 attempt-4
// watchdog guard: the parent reaps the scope watchdog with a background Wait(),
// so its PID can be freed and reused too. When the watchdog's own snapshot no
// longer matches the live PID, cleanup must skip signaling it — while a dolt PID
// whose snapshot still matches is signaled normally.
func TestScopeWatchdogTerminateSkipsReusedWatchdogPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics required")
	}

	// doltChild's snapshot matches the mocked re-read (2222), so it is signaled
	// (a sleep dies on SIGTERM).
	doltChild := exec.Command("sleep", "60")
	if err := doltChild.Start(); err != nil {
		t.Fatalf("start dolt child: %v", err)
	}
	doltChildPID := doltChild.Process.Pid
	t.Cleanup(func() {
		_ = doltChild.Process.Kill()
		_ = doltChild.Wait()
	})

	// watchdogVictim stands in for an unrelated process that reused the watchdog
	// PID after the parent reaped it; its snapshot (1111) disagrees with the live
	// re-read (2222), so cleanup must leave it alone.
	watchdogVictim := exec.Command("sleep", "60")
	if err := watchdogVictim.Start(); err != nil {
		t.Fatalf("start watchdog victim: %v", err)
	}
	watchdogVictimPID := watchdogVictim.Process.Pid
	t.Cleanup(func() {
		_ = watchdogVictim.Process.Kill()
		_ = watchdogVictim.Wait()
	})

	oldTicks := managedDoltTestReadStartTimeTicks
	oldIdent := managedDoltTestReadStartIdentity
	managedDoltTestReadStartTimeTicks = func(int) uint64 { return 2222 }
	managedDoltTestReadStartIdentity = func(int) string { return "" }
	t.Cleanup(func() {
		managedDoltTestReadStartTimeTicks = oldTicks
		managedDoltTestReadStartIdentity = oldIdent
	})

	terminateManagedDoltStartedProcess(managedDoltStartedProcess{
		PID:                    doltChildPID,
		StartTimeTicks:         2222,
		WatchdogPID:            watchdogVictimPID,
		WatchdogStartTimeTicks: 1111,
	})

	// The dolt child's SIGTERM lands (a zombie reads as not-alive) once cleanup
	// returns.
	if pidAlive(doltChildPID) {
		t.Fatalf("cleanup did not signal matching dolt pid %d", doltChildPID)
	}
	// Give any erroneous watchdog SIGTERM time to land before asserting survival.
	time.Sleep(200 * time.Millisecond)
	if !pidAlive(watchdogVictimPID) {
		t.Fatalf("cleanup signaled reused watchdog pid %d; watchdog identity guard not enforced", watchdogVictimPID)
	}
}

// TestManagedDoltWatchdogStartLineRoundTrip pins the scope watchdog's stdout
// handshake protocol (PR #4004 F1 follow-up): the PID and OS start identity must
// survive format→parse so the parent can guard PID reuse. The identity is a
// `ps -o lstart=` string that contains spaces, which the tab delimiter must not
// split, and a legacy bare-PID line must still parse (zero identity).
func TestManagedDoltWatchdogStartLineRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		pid      int
		ticks    uint64
		identity string
	}{
		{"ticks only", 4321, 998877, ""},
		{"identity with spaces", 4321, 0, "Mon Jul  7 01:23:45 2026"},
		{"both present", 51, 12, "Tue Jul  8 09:00:01 2026"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := formatManagedDoltWatchdogStartLine(tc.pid, tc.ticks, tc.identity)
			if strings.ContainsAny(line, "\n") {
				t.Fatalf("handshake line must be single-line, got %q", line)
			}
			pid, ticks, identity, err := parseManagedDoltWatchdogStartLine(line)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			if pid != tc.pid || ticks != tc.ticks || identity != tc.identity {
				t.Fatalf("round-trip = (pid %d, ticks %d, identity %q), want (%d, %d, %q)",
					pid, ticks, identity, tc.pid, tc.ticks, tc.identity)
			}
		})
	}

	t.Run("legacy bare pid line", func(t *testing.T) {
		pid, ticks, identity, err := parseManagedDoltWatchdogStartLine("7788\n")
		if err != nil {
			t.Fatalf("parse legacy line: %v", err)
		}
		if pid != 7788 || ticks != 0 || identity != "" {
			t.Fatalf("legacy parse = (pid %d, ticks %d, identity %q), want (7788, 0, \"\")", pid, ticks, identity)
		}
	})

	t.Run("invalid pid errors", func(t *testing.T) {
		for _, bad := range []string{"", "  ", "0", "-3", "notapid"} {
			if _, _, _, err := parseManagedDoltWatchdogStartLine(bad); err == nil {
				t.Errorf("parseManagedDoltWatchdogStartLine(%q) = nil error, want error", bad)
			}
		}
	})
}

// TestScopeWatchdogTerminateSkipsReusedDoltPIDButSignalsWatchdog is the PR #4004
// F1 regression: the production scope-watchdog start path now reports the dolt
// child's start identity, so a startup-failure cleanup must skip a reused dolt
// PID exactly like the direct-spawn path — while still signaling the watchdog
// whose own snapshot still matches. It rebuilds the started process through the
// real handshake reader so the test exercises the scope path's identity plumbing
// end to end.
func TestScopeWatchdogTerminateSkipsReusedDoltPIDButSignalsWatchdog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics required")
	}

	// doltVictim stands in for an unrelated process that reused the dolt child's
	// numeric PID after the scope watchdog reaped it; it must survive cleanup.
	doltVictim := exec.Command("sleep", "60")
	if err := doltVictim.Start(); err != nil {
		t.Fatalf("start dolt victim: %v", err)
	}
	doltVictimPID := doltVictim.Process.Pid
	t.Cleanup(func() {
		_ = doltVictim.Process.Kill()
		_ = doltVictim.Wait()
	})

	// watchdogVictim stands in for the still-tracked scope watchdog, which
	// cleanup always signals regardless of the dolt PID-reuse guard.
	watchdogVictim := exec.Command("sleep", "60")
	if err := watchdogVictim.Start(); err != nil {
		t.Fatalf("start watchdog victim: %v", err)
	}
	watchdogVictimPID := watchdogVictim.Process.Pid
	t.Cleanup(func() {
		_ = watchdogVictim.Process.Kill()
		_ = watchdogVictim.Wait()
	})

	// Rebuild the started process exactly as startManagedDoltSQLServerWithScopeWatchdog
	// does: parse the watchdog's stdout handshake, which now carries the identity
	// (ticks=1111) snapshotted before the child could be reaped.
	line := formatManagedDoltWatchdogStartLine(doltVictimPID, 1111, "")
	pid, ticks, ident, err := readManagedDoltScopeWatchdogStart(strings.NewReader(line+"\n"), watchdogVictimPID)
	if err != nil {
		t.Fatalf("read scope watchdog start: %v", err)
	}
	if pid != doltVictimPID || ticks != 1111 {
		t.Fatalf("handshake round-trip pid=%d ticks=%d, want pid=%d ticks=1111", pid, ticks, doltVictimPID)
	}
	started := managedDoltStartedProcess{
		PID:            pid,
		WatchdogPID:    watchdogVictimPID,
		StartTimeTicks: ticks,
		StartIdentity:  ident,
		// The watchdog's own snapshot matches the mocked re-read (2222) below, so
		// its guard passes and cleanup still signals it — only the dolt PID is
		// treated as reused.
		WatchdogStartTimeTicks: 2222,
	}

	// The live PID re-reads as a different incarnation (2222 != 1111), so the
	// dolt PID must be treated as reused and left alone.
	oldTicks := managedDoltTestReadStartTimeTicks
	oldIdent := managedDoltTestReadStartIdentity
	managedDoltTestReadStartTimeTicks = func(int) uint64 { return 2222 }
	managedDoltTestReadStartIdentity = func(int) string { return "" }
	t.Cleanup(func() {
		managedDoltTestReadStartTimeTicks = oldTicks
		managedDoltTestReadStartIdentity = oldIdent
	})

	terminateManagedDoltStartedProcess(started)

	// terminateManagedDoltPID waits for the watchdog's SIGTERM to land, so it is
	// gone (a zombie reads as not-alive) once cleanup returns.
	if pidAlive(watchdogVictimPID) {
		t.Fatalf("scope cleanup did not signal watchdog pid %d", watchdogVictimPID)
	}
	// Give any erroneous SIGTERM to the dolt PID time to land before asserting.
	time.Sleep(200 * time.Millisecond)
	if !pidAlive(doltVictimPID) {
		t.Fatalf("scope cleanup signaled reused dolt pid %d; scope-path identity guard not enforced", doltVictimPID)
	}
}

// TestTerminateManagedDoltStartedProcessLogsIdentityMismatchSkip covers the
// PR #4004 F2 observability fix: when the guard declines to signal a reused PID,
// cleanup records the decision so an operator can tell the guard fired rather
// than that cleanup killed the child.
func TestTerminateManagedDoltStartedProcessLogsIdentityMismatchSkip(t *testing.T) {
	oldTicks := managedDoltTestReadStartTimeTicks
	managedDoltTestReadStartTimeTicks = func(int) uint64 { return 2222 }
	t.Cleanup(func() { managedDoltTestReadStartTimeTicks = oldTicks })

	oldLog := managedDoltCleanupLogf
	var logged []string
	managedDoltCleanupLogf = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { managedDoltCleanupLogf = oldLog })

	// Snapshot 1111 disagrees with the re-read 2222: PID reused, terminate skipped.
	terminateManagedDoltStartedProcess(managedDoltStartedProcess{PID: 424242, StartTimeTicks: 1111})

	if len(logged) != 1 {
		t.Fatalf("expected exactly one cleanup log line, got %d: %v", len(logged), logged)
	}
	if !strings.Contains(logged[0], "424242") || !strings.Contains(logged[0], "identity changed") {
		t.Fatalf("cleanup log %q missing pid or reason", logged[0])
	}
}

// TestManagedDoltStartedPIDIdentityMatchesUnconfirmedReRead documents the
// PR #4004 F3 boundary: only the identity precedence mirrors the production
// reaper, while an unconfirmed re-read (the snapshot is present but the live
// process yields no identity) intentionally keeps the legacy terminate default,
// because the ensuing signal to a vanished PID is a harmless ESRCH no-op.
func TestManagedDoltStartedPIDIdentityMatchesUnconfirmedReRead(t *testing.T) {
	oldTicks := managedDoltTestReadStartTimeTicks
	oldIdent := managedDoltTestReadStartIdentity
	t.Cleanup(func() {
		managedDoltTestReadStartTimeTicks = oldTicks
		managedDoltTestReadStartIdentity = oldIdent
	})

	cases := []struct {
		name        string
		started     managedDoltStartedProcess
		reReadTicks uint64
		reReadIdent string
		want        bool
	}{
		{"ticks match terminates", managedDoltStartedProcess{PID: 1, StartTimeTicks: 10}, 10, "", true},
		{"ticks mismatch skips", managedDoltStartedProcess{PID: 1, StartTimeTicks: 10}, 11, "", false},
		{"ticks unconfirmed terminates", managedDoltStartedProcess{PID: 1, StartTimeTicks: 10}, 0, "", true},
		{"identity match terminates", managedDoltStartedProcess{PID: 1, StartIdentity: "A"}, 0, "A", true},
		{"identity mismatch skips", managedDoltStartedProcess{PID: 1, StartIdentity: "A"}, 0, "B", false},
		{"identity unconfirmed terminates", managedDoltStartedProcess{PID: 1, StartIdentity: "A"}, 0, "", true},
		{"no snapshot terminates", managedDoltStartedProcess{PID: 1}, 0, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			managedDoltTestReadStartTimeTicks = func(int) uint64 { return tc.reReadTicks }
			managedDoltTestReadStartIdentity = func(int) string { return tc.reReadIdent }
			if got := managedDoltStartedPIDIdentityMatches(tc.started); got != tc.want {
				t.Errorf("managedDoltStartedPIDIdentityMatches = %v, want %v", got, tc.want)
			}
		})
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
	started := managedDoltStartedProcess{PID: dolt.Process.Pid, WatchdogPID: watchdog.Process.Pid}
	registerManagedDoltTestProcess(started)
	reapManagedDoltTestProcesses()

	want := []int{started.PID, started.WatchdogPID}
	if fmt.Sprint(terminated) != fmt.Sprint(want) {
		t.Fatalf("terminated = %v, want %v", terminated, want)
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

func TestResolveManagedDoltConfigForStartUsesCityListenerOverrides(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test"

[dolt]
read_timeout_millis = 300000
write_timeout_millis = 600000
max_connections = 1024
`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveManagedDoltConfigForStart(dir, -1)
	if err != nil {
		t.Fatalf("resolveManagedDoltConfigForStart: %v", err)
	}
	if got.ReadTimeoutMillis != 300000 {
		t.Fatalf("ReadTimeoutMillis = %d, want 300000", got.ReadTimeoutMillis)
	}
	if got.WriteTimeoutMillis != 600000 {
		t.Fatalf("WriteTimeoutMillis = %d, want 600000", got.WriteTimeoutMillis)
	}
	if got.MaxConnections != 1024 {
		t.Fatalf("MaxConnections = %d, want 1024", got.MaxConnections)
	}
}

func TestResolveManagedDoltConfigForStartAutoGC(t *testing.T) {
	tests := []struct {
		name     string
		cityToml string
		envVal   string
		want     bool
	}{
		{name: "defaults on", want: true},
		{name: "city toml disables", cityToml: "[dolt]\nauto_gc_enabled = false\n", want: false},
		{name: "city toml overrides env", cityToml: "[dolt]\nauto_gc_enabled = true\n", envVal: "false", want: true},
		{name: "env false disables", envVal: "false", want: false},
		{name: "env 0 disables", envVal: "0", want: false},
		{name: "env OFF disables", envVal: "OFF", want: false},
		{name: "env ON enables", envVal: "ON", want: true},
		{name: "unparseable env keeps default", envVal: "maybe", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			body := "[workspace]\nname = \"test\"\n\n" + tt.cityToml
			if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_DOLT_AUTO_GC_ENABLED", tt.envVal)
			got, err := resolveManagedDoltConfigForStart(dir, -1)
			if err != nil {
				t.Fatalf("resolveManagedDoltConfigForStart: %v", err)
			}
			if got.EffectiveAutoGCEnabled() != tt.want {
				t.Fatalf("EffectiveAutoGCEnabled() = %v, want %v", got.EffectiveAutoGCEnabled(), tt.want)
			}
		})
	}
}

func TestResolveManagedDoltConfigForStartRejectsInvalidCityDoltConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test"

[dolt]
read_timeout_millis = -1
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := resolveManagedDoltConfigForStart(dir, -1)
	if err == nil {
		t.Fatal("resolveManagedDoltConfigForStart() error = nil, want invalid city dolt config rejection")
	}
	if got := err.Error(); !strings.Contains(got, "[dolt] read_timeout_millis must not be negative") {
		t.Fatalf("error = %q, want negative read_timeout_millis rejection", got)
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

// TestReapManagedDoltTestProcessesSkipsReusedPID is the #2313 follow-up M2
// regression: when the snapshotted StartTimeTicks at registration differs from
// the value re-read at reap time, the PID has been reused — we must NOT
// signal it. Validated against the un-patched reap (no identity check) by
// flipping the seam and asserting terminate was not invoked.
func TestReapManagedDoltTestProcessesSkipsReusedPID(t *testing.T) {
	withManagedDoltTestMode(t, true)
	clearManagedDoltTestProcessRegistry(t)
	t.Cleanup(func() { clearManagedDoltTestProcessRegistry(t) })

	oldTerminate := managedDoltTestTerminateProcess
	var terminated []int
	managedDoltTestTerminateProcess = func(pid int) error {
		terminated = append(terminated, pid)
		return nil
	}
	t.Cleanup(func() { managedDoltTestTerminateProcess = oldTerminate })

	oldTicks := managedDoltTestReadStartTimeTicks
	oldIdent := managedDoltTestReadStartIdentity
	managedDoltTestReadStartTimeTicks = func(int) uint64 { return 2222 }
	managedDoltTestReadStartIdentity = func(int) string { return "" }
	t.Cleanup(func() {
		managedDoltTestReadStartTimeTicks = oldTicks
		managedDoltTestReadStartIdentity = oldIdent
	})

	// Snapshot is 1111 at registration (set explicitly so we bypass the
	// real-time reader at register-time too); the reap seam reports 2222
	// — different process, must be skipped.
	livePID := os.Getpid()
	registerManagedDoltTestProcess(managedDoltStartedProcess{PID: livePID, StartTimeTicks: 1111})

	reapManagedDoltTestProcesses()

	for _, pid := range terminated {
		if pid == livePID {
			t.Fatalf("reap signaled PID %d with mismatched start-time ticks; identity guard not enforced", livePID)
		}
	}
}

// TestReapManagedDoltTestProcessesTerminatesWhenTicksMatch asserts the
// happy-path side of the M2 identity guard: when snapshotted ticks equal
// re-read ticks, the reap proceeds as before.
func TestReapManagedDoltTestProcessesTerminatesWhenTicksMatch(t *testing.T) {
	withManagedDoltTestMode(t, true)
	clearManagedDoltTestProcessRegistry(t)
	t.Cleanup(func() { clearManagedDoltTestProcessRegistry(t) })

	oldTerminate := managedDoltTestTerminateProcess
	var terminated []int
	managedDoltTestTerminateProcess = func(pid int) error {
		terminated = append(terminated, pid)
		return nil
	}
	t.Cleanup(func() { managedDoltTestTerminateProcess = oldTerminate })

	oldTicks := managedDoltTestReadStartTimeTicks
	managedDoltTestReadStartTimeTicks = func(int) uint64 { return 5555 }
	t.Cleanup(func() { managedDoltTestReadStartTimeTicks = oldTicks })

	livePID := os.Getpid()
	registerManagedDoltTestProcess(managedDoltStartedProcess{PID: livePID, StartTimeTicks: 5555})

	reapManagedDoltTestProcesses()

	if len(terminated) == 0 || terminated[0] != livePID {
		t.Fatalf("terminated = %v, want [%d]+; identity guard wrongly skipped matching PID", terminated, livePID)
	}
}

// TestTerminateManagedDoltTestPIDKillsProcessGroup is the #2313 follow-up M3
// regression: when the target is a process-group leader, terminate must
// signal the whole group so descendant dolt workers do not survive.
// Demonstration: spawn a shell as group leader, fork a backgrounded sleep
// child, call terminateManagedDoltTestPID on the shell. Both must die.
// Without the M3 fix (leader-only kill), the child outlives the shell.
func TestTerminateManagedDoltTestPIDKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group signal semantics required")
	}
	processgrouptest.RequireRealProcessSignals(t)
	dir := t.TempDir()
	childFile := filepath.Join(dir, "child.pid")
	// Shell becomes the new process group leader (Setpgid:true). It forks
	// a backgrounded sleep that inherits that group, records the child's
	// PID, then waits.
	cmd := exec.Command("/bin/sh", "-c", `sleep 90 & echo $! > "$1"; wait`, "sh", childFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start shell: %v", err)
	}
	shellPID := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Wait for the child PID to be recorded.
	var childPID int
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		data, err := os.ReadFile(childFile)
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
				childPID = pid
				break
			}
		}
	}
	if childPID == 0 {
		t.Fatalf("child sleep never recorded its PID at %s", childFile)
	}

	if err := terminateManagedDoltTestPID(shellPID); err != nil {
		t.Fatalf("terminateManagedDoltTestPID(%d): %v", shellPID, err)
	}

	// Allow a short window for the kernel to mark both pids dead.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(shellPID) && !pidAlive(childPID) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(shellPID) {
		t.Errorf("shell pid %d still alive after pgid terminate", shellPID)
	}
	if pidAlive(childPID) {
		t.Errorf("child pid %d still alive after pgid terminate; M3 pgid-kill regression", childPID)
	}
}

// TestTerminateManagedDoltTestPIDLeaderOnlyForNonGroupLeader asserts the
// safety guard added in M3: when the target is NOT its own pgid leader (e.g.
// the watchdog inheriting the test binary's group), terminate must NOT
// signal the whole group — that would take down the test binary. We pick a
// child of the test binary that did NOT call Setpgid; it inherits the test
// binary's group. Terminate must only kill the child.
func TestTerminateManagedDoltTestPIDLeaderOnlyForNonGroupLeader(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group signal semantics required")
	}
	// Spawn a sleep WITHOUT Setpgid — it inherits the test binary's pgid.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("getpgid(%d): %v", pid, err)
	}
	if pgid == pid {
		t.Skip("sleep happens to be its own group leader; cannot exercise leader-only fallback")
	}

	if err := terminateManagedDoltTestPID(pid); err != nil {
		t.Fatalf("terminateManagedDoltTestPID(%d): %v", pid, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Errorf("sleep pid %d still alive after terminate", pid)
	}
	// Sanity: the test binary itself is still alive (we did not pgid-kill
	// our own group). If we had, the test process would have died and this
	// assertion would never run — but if it did, this guards against a
	// future regression where the fallback path forgets the leader check.
	if !pidAlive(os.Getpid()) {
		t.Fatalf("test binary signaled by terminate fallback; pgid safety check failed")
	}
}

// TestRegisterManagedDoltTestProcessSnapshotsIdentity ensures the M2
// snapshot happens at registration when caller leaves identity fields zero.
func TestRegisterManagedDoltTestProcessSnapshotsIdentity(t *testing.T) {
	withManagedDoltTestMode(t, true)
	clearManagedDoltTestProcessRegistry(t)
	t.Cleanup(func() { clearManagedDoltTestProcessRegistry(t) })

	oldTicks := managedDoltTestReadStartTimeTicks
	oldIdent := managedDoltTestReadStartIdentity
	managedDoltTestReadStartTimeTicks = func(int) uint64 { return 9876 }
	managedDoltTestReadStartIdentity = func(int) string { return "Mon Jan 1 12:34:56 2026" }
	t.Cleanup(func() {
		managedDoltTestReadStartTimeTicks = oldTicks
		managedDoltTestReadStartIdentity = oldIdent
	})

	registerManagedDoltTestProcess(managedDoltStartedProcess{PID: os.Getpid()})

	v, ok := managedDoltTestProcessRegistry.Load(os.Getpid())
	if !ok {
		t.Fatalf("registry missing entry for pid %d", os.Getpid())
	}
	got, ok := v.(managedDoltStartedProcess)
	if !ok {
		t.Fatalf("registry value type = %T, want managedDoltStartedProcess", v)
	}
	if got.StartTimeTicks != 9876 {
		t.Errorf("StartTimeTicks = %d, want 9876", got.StartTimeTicks)
	}
	if got.StartIdentity != "Mon Jan 1 12:34:56 2026" {
		t.Errorf("StartIdentity = %q, want non-empty snapshot", got.StartIdentity)
	}
}
