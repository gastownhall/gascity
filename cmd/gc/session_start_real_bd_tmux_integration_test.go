//go:build integration

package main

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	runtimetmux "github.com/gastownhall/gascity/internal/runtime/tmux"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

type exactSessionStartStopDurableSample struct {
	Version                        string `json:"version"`
	SessionID                      string `json:"session_id"`
	SchemaStatus                   string `json:"schema_status"`
	StartAdmissionToFinalizationNS int64  `json:"start_admission_to_finalization_ns"`
	StopCommandToFinalizationNS    int64  `json:"stop_command_to_finalization_ns"`
	WakeCommandToFinalizationNS    int64  `json:"wake_command_to_finalization_ns"`
	StartPersistedState            string `json:"start_persisted_state"`
	StopPersistedState             string `json:"stop_persisted_state"`
	WakePersistedState             string `json:"wake_persisted_state"`
}

func testExactSessionStartSocketLiveSessionRecordsDetachedStatusShadow(t *testing.T) {
	guard := tmuxtest.NewGuard(t)
	cityPath := t.TempDir()
	sessionName := guard.SessionName("worker")
	sessionConfig := config.SessionConfig{
		Socket:         guard.SocketName(),
		SetupTimeout:   "3s",
		StartupTimeout: "10s",
	}
	baseProvider, err := newSessionProviderForCityByName(nil, "", sessionConfig, guard.CityName(), cityPath)
	if err != nil {
		t.Fatalf("construct isolated tmux provider: %v", err)
	}
	provider := &sessionLifecycleShadowJourneyProvider{Provider: baseProvider}
	if err := provider.Start(t.Context(), sessionName, runtime.Config{
		Command: "sleep 600",
		WorkDir: cityPath,
		Env:     map[string]string{"GC_PROVIDER": "codex"},
	}); err != nil {
		t.Fatalf("seed live isolated tmux session: %v", err)
	}
	beforeStarts := len(provider.snapshotStartCalls())
	if beforeStarts != 1 {
		t.Fatalf("seed provider Start calls = %d, want exactly one", beforeStarts)
	}
	beforeServerPID := guard.ServerPID()
	beforeSocket, err := os.Lstat(guard.SocketPath())
	if err != nil {
		t.Fatalf("stat isolated tmux socket before admission: %v", err)
	}
	beforeSessionIDs, err := runtimetmux.NewTmuxWithConfig(runtimetmux.Config{
		SocketName: guard.SocketName(),
	}).ListSessionIDs()
	if err != nil {
		t.Fatalf("list isolated tmux sessions before admission: %v", err)
	}
	if len(beforeSessionIDs) != 1 || strings.TrimSpace(beforeSessionIDs[sessionName]) == "" {
		t.Fatalf("isolated tmux sessions before admission = %v, want exactly live target %q with a non-empty ID", beforeSessionIDs, sessionName)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: guard.CityName()},
		Agents: []config.Agent{{
			Name:         "worker",
			StartCommand: "sleep 600",
			Env:          map[string]string{"GC_PROVIDER": "codex"},
		}},
		Session: sessionConfig,
	}
	env := newReconcilerTestEnv()
	bead := env.createSessionBead(sessionName, "worker")
	if err := env.store.SetMetadataBatch(bead.ID, map[string]string{
		"state":        string(sessionpkg.StateAwake),
		"wake_request": string(sessionpkg.WakeCauseExplicit),
	}); err != nil {
		t.Fatalf("configure exact-start-owned live row: %v", err)
	}
	trace := newSessionReconcilerTraceManager(cityPath, guard.CityName(), io.Discard)
	t.Cleanup(func() { _ = trace.Close() })
	status := make(chan exactSessionLifecycleStatusResult, 1)
	cs := coherentSessionStartControllerStateForTest(cfg, provider, env.store, rollout.Auto)
	cs.cityName = guard.CityName()
	cs.cityPath = cityPath
	cr := &CityRuntime{
		cityPath: cityPath,
		cityName: guard.CityName(),
		cs:       cs,
		trace:    trace,
		sessionStartOptions: []startExecutionOption{
			withExactSessionLifecycleStatusObserver(func(result exactSessionLifecycleStatusResult) {
				status <- result
			}),
		},
	}
	t.Cleanup(cr.stopSessionStartController)
	if err := cr.ensureSessionStartController(t.Context(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("start keyed session controller: %v", err)
	}

	beforeRow, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read live session row before socket admission: %v", err)
	}
	if beforeRow.Revision == 0 {
		t.Fatalf("live session row revision = %d, want nonzero", beforeRow.Revision)
	}
	if reply := cr.admitSessionStartSocketKey(bead.ID); reply != sessionStartSocketReplyOK {
		t.Fatalf("exact session-start admission = %q, want %q", reply, sessionStartSocketReplyOK)
	}

	var got exactSessionLifecycleStatusResult
	select {
	case got = <-status:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("socket-admitted live session did not report exact status")
	}
	if got.Admission.Source != sessionStartAdmissionSocket || !got.RuntimeLive ||
		got.Disposition != exactSessionLifecycleStatusDispositionCandidate ||
		got.Reason != exactSessionLifecycleStatusReasonCandidate || got.Plan == nil ||
		got.Plan.Outcome != sessionLifecycleStatusNoop || got.Plan.Reason != sessionLifecycleStatusReasonConverged ||
		got.EffectApplied {
		t.Fatalf("exact status = %#v, want socket candidate/noop/converged with no effect", got)
	}

	afterRow, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read live session row after socket admission: %v", err)
	}
	if afterRow.Revision != beforeRow.Revision || !reflect.DeepEqual(afterRow, beforeRow) {
		t.Fatalf("durable row changed after no-op socket admission:\nbefore=%#v\nafter=%#v", beforeRow, afterRow)
	}
	if got := len(provider.snapshotStartCalls()); got != 1 {
		t.Fatalf("provider Start calls = %d, want absolute total 1", got)
	}
	afterServerPID := guard.ServerPID()
	if afterServerPID != beforeServerPID {
		t.Fatalf("isolated tmux server PID after admission = %d, want unchanged %d", afterServerPID, beforeServerPID)
	}
	afterSocket, err := os.Lstat(guard.SocketPath())
	if err != nil {
		t.Fatalf("stat isolated tmux socket after admission: %v", err)
	}
	if !os.SameFile(beforeSocket, afterSocket) {
		t.Fatalf("isolated tmux socket changed after no-op admission: before=%s after=%s", beforeSocket.Name(), afterSocket.Name())
	}
	afterSessionIDs, err := runtimetmux.NewTmuxWithConfig(runtimetmux.Config{
		SocketName: guard.SocketName(),
	}).ListSessionIDs()
	if err != nil {
		t.Fatalf("list isolated tmux sessions after admission: %v", err)
	}
	if !reflect.DeepEqual(afterSessionIDs, beforeSessionIDs) || !provider.IsRunning(sessionName) || !guard.HasSession(sessionName) {
		t.Fatalf("isolated tmux sessions/live state = %v/%t/%t, want unchanged sessions %v and live target",
			afterSessionIDs, provider.IsRunning(sessionName), guard.HasSession(sessionName), beforeSessionIDs)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("read detached socket shadow trace: %v", err)
	}
	var witnesses []SessionReconcilerTraceRecord
	for _, record := range records {
		if record.RecordType == TraceRecordOperation && record.SiteCode == TraceSiteLifecycleStatusShadow {
			witnesses = append(witnesses, record)
		}
	}
	if len(witnesses) != 1 {
		t.Fatalf("socket status-shadow witnesses = %#v, want exactly one", witnesses)
	}
	witness := witnesses[0]
	if witness.OutcomeCode != TraceOutcomeNoChange || witness.Fields["session_id"] != bead.ID ||
		witness.Fields["admission"] != string(sessionStartAdmissionSocket) ||
		witness.Fields["status_outcome"] != "noop" ||
		witness.Fields["status_reason"] != string(sessionLifecycleStatusReasonConverged) ||
		witness.Fields["effect_applied"] != false {
		t.Fatalf("socket status-shadow witness = %#v, want detached no-effect converged witness", witness)
	}
}

// TestExactSessionStartNativeV59RealBDTmuxJourney proves exact socket admission
// against an already-live no-op and a v59 durable start, status heal, and stop.
func TestExactSessionStartNativeV59RealBDTmuxJourney(t *testing.T) {
	t.Run("live_socket_noop", testExactSessionStartSocketLiveSessionRecordsDetachedStatusShadow)
	t.Run("native_v59_start_stop", testExactSessionStartNativeV59RealBDTmuxJourney)
}

func testExactSessionStartNativeV59RealBDTmuxJourney(t *testing.T) {
	bdPath := strings.TrimSpace(os.Getenv("GC_TEST_BD_BIN"))
	if bdPath == "" {
		t.Skip("GC_TEST_BD_BIN is not set to a real bd binary")
	}
	bdPath, err := filepath.Abs(bdPath)
	if err != nil {
		t.Fatalf("resolve GC_TEST_BD_BIN: %v", err)
	}
	if info, err := os.Stat(bdPath); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("GC_TEST_BD_BIN %q is not an executable file: info=%v err=%v", bdPath, info, err)
	}
	requirePinnedJourneyBD(t, bdPath)
	shimDir := t.TempDir()
	if err := os.Symlink(bdPath, filepath.Join(shimDir, "bd")); err != nil {
		t.Fatalf("install bd PATH shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BEADS_DOLT_AUTO_START", "1")

	guard := tmuxtest.NewGuard(t)
	cityPath := t.TempDir()
	cleanupManagedDoltTestCity(t, cityPath)
	configPath := filepath.Join(t.TempDir(), "city.toml")
	unlimitedSessions := -1
	oneSession := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: guard.CityName()},
		Beads: config.BeadsConfig{
			Provider:          "bd",
			ConditionalWrites: "require",
		},
		Daemon: config.DaemonConfig{
			SessionReconciler: "auto",
			PatrolInterval:    "1h",
		},
		Session: config.SessionConfig{
			Socket:         guard.SocketName(),
			SetupTimeout:   "3s",
			StartupTimeout: "10s",
		},
		Agents: []config.Agent{
			{
				Name:              "worker",
				StartCommand:      "sleep 600",
				MaxActiveSessions: &unlimitedSessions,
				Env:               map[string]string{"GC_PROVIDER": "codex"},
			},
			{
				Name:              "database",
				StartCommand:      "sleep 600",
				MaxActiveSessions: &oneSession,
				Env:               map[string]string{"GC_PROVIDER": "codex"},
			},
			{
				Name:              "dependent",
				StartCommand:      "sleep 600",
				MaxActiveSessions: &oneSession,
				DependsOn:         []string{"database"},
				Env:               map[string]string{"GC_PROVIDER": "codex"},
			},
			{
				Name:              "reviewer",
				StartCommand:      "sleep 600",
				MaxActiveSessions: &oneSession,
				Env:               map[string]string{"GC_PROVIDER": "codex"},
			},
		},
		NamedSessions: []config.NamedSession{
			{Template: "database", Mode: "always"},
			{Name: "reviewer", Template: "reviewer", Mode: "on_demand"},
		},
	}
	configData, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("marshal exact start-stop city config: %v", err)
	}
	if err := os.WriteFile(configPath, configData, 0o644); err != nil {
		t.Fatalf("write exact start-stop city config: %v", err)
	}

	// Restrict the production scanner to live procfs views of test-owned pane
	// processes. This proves exact process absence without host-wide scanner
	// incompleteness from unrelated same-UID processes.
	processScanRoot := t.TempDir()
	processScanRoot, err = filepath.Abs(processScanRoot)
	if err != nil {
		t.Fatalf("resolve controlled process-scan root: %v", err)
	}
	info, err := os.Stat(processScanRoot)
	if err != nil || !info.IsDir() {
		t.Fatalf("controlled process-scan root %q is not a directory: info=%v err=%v", processScanRoot, info, err)
	}
	// proctable places a process in or out of an incarnation by reading the boot
	// time from <root>/stat. Without it the age proof cannot even run, so an
	// entry the scanner cannot read costs the whole scan its completeness
	// instead of costing that one entry a classification. The scanner enumerates
	// directories only, so this file is never mistaken for a PID.
	if err := os.Symlink("/proc/stat", filepath.Join(processScanRoot, "stat")); err != nil {
		t.Fatalf("link boot time into controlled process-scan root: %v", err)
	}
	buildDir := t.TempDir()
	realBinPath := filepath.Join(buildDir, "gc-real")
	gcBinary := filepath.Join(buildDir, "gc")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	const scanRootSymbol = "github.com/gastownhall/gascity/internal/runtime/proctable.scanRoot"
	buildGC := exec.Command("go", "build", "-ldflags", "-X="+scanRootSymbol+"="+processScanRoot, "-o", realBinPath, ".")
	buildGC.Dir = wd
	out, err := buildGC.CombinedOutput()
	if err != nil {
		t.Fatalf("go build controlled-process-scan gc binary: %v\n%s", err, out)
	}
	wrapper := fmt.Sprintf("#!/bin/sh\nexport %s=1\nif [ -z \"${%s:-}\" ]; then\n  export %s=$PPID\nfi\nexec %q \"$@\"\n",
		managedDoltTestModeEnv,
		managedDoltTestParentPIDEnv,
		managedDoltTestParentPIDEnv,
		realBinPath,
	)
	if err := os.WriteFile(gcBinary, []byte(wrapper), 0o755); err != nil {
		t.Fatalf("write controlled-process-scan gc wrapper: %v", err)
	}
	// A PID alone does not name a process: the host recycles PIDs, and the
	// occupant that follows a retired pane is some unrelated process whose
	// /proc files this fixture does not own and often cannot read. Recording
	// each registration's start time gives every entry in the controlled root a
	// provenance the retirement below can check, so a recycled PID is retired
	// rather than left behind as the unreadable entry that costs a whole scan
	// its completeness.
	registeredPaneStarts := map[string]string{}
	paneProcessStartTicks := func(pid string) (string, bool) {
		data, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
		if err != nil {
			return "", false
		}
		text := string(data)
		closeParen := strings.LastIndexByte(text, ')')
		if closeParen < 0 || closeParen+1 >= len(text) {
			return "", false
		}
		fields := strings.Fields(text[closeParen+1:])
		const startTicksIndexAfterComm = 19
		if len(fields) <= startTicksIndexAfterComm {
			return "", false
		}
		return fields[startTicksIndexAfterComm], true
	}
	registerLivePaneProcess := func(pid string) {
		t.Helper()
		if _, err := strconv.Atoi(pid); err != nil {
			t.Fatalf("controlled process-scan pane PID = %q, want numeric: %v", pid, err)
		}
		startTicks, ok := paneProcessStartTicks(pid)
		if !ok {
			t.Fatalf("read start time for controlled process-scan pane PID %q", pid)
		}
		procDir := filepath.Join(processScanRoot, pid)
		if err := os.MkdirAll(procDir, 0o755); err != nil {
			t.Fatalf("create controlled procfs entry for pane PID %q: %v", pid, err)
		}
		for _, name := range []string{"status", "environ", "stat"} {
			source := filepath.Join("/proc", pid, name)
			if _, err := os.Stat(source); err != nil {
				t.Fatalf("stat live procfs %s for pane PID %q: %v", name, pid, err)
			}
			// Idempotent by PID. Registration is per-PANE, but a pane can be
			// registered by more than one leg (and a poll loop can reach here on a
			// retry), so an existing link for the same PID is the same fact, not a
			// conflict. Re-linking became reachable once the configured-dependency
			// leg was un-skipped at WD.10a and started registering its own pane.
			link := filepath.Join(procDir, name)
			if _, err := os.Lstat(link); err == nil {
				continue
			}
			if err := os.Symlink(source, link); err != nil {
				t.Fatalf("link live procfs %s for pane PID %q: %v", name, pid, err)
			}
		}
		registeredPaneStarts[pid] = startTicks
	}
	// retireExitedPaneProcess reports whether the exact process registered under
	// pid is gone — exited, reaped, or replaced by a PID the host recycled — and
	// drops its controlled-root entry when it is. Absence proofs poll it so a
	// recycled PID stops poisoning the scanner on the first observation instead
	// of holding the leg until its budget expires.
	retireExitedPaneProcess := func(pid string) (bool, error) {
		if _, err := strconv.Atoi(pid); err != nil {
			// Joining a non-numeric PID names the scan root itself, and
			// removing that would delete every registration at once.
			return false, fmt.Errorf("retire controlled procfs entry: pane PID = %q, want numeric: %w", pid, err)
		}
		registered, tracked := registeredPaneStarts[pid]
		startTicks, running := paneProcessStartTicks(pid)
		if running && (!tracked || startTicks == registered) && !exactStartStopProcessExited(pid) {
			return false, nil
		}
		delete(registeredPaneStarts, pid)
		if err := os.RemoveAll(filepath.Join(processScanRoot, pid)); err != nil {
			return true, fmt.Errorf("retire controlled procfs entry for pane PID %q: %w", pid, err)
		}
		return true, nil
	}
	removeExitedPaneProcess := func(pid string) error {
		_, err := retireExitedPaneProcess(pid)
		return err
	}
	runtimeDir := t.TempDir()
	gcHome := t.TempDir()
	// gc init's author-identity gate shells out to `dolt config --get`, which reads
	// $DOLT_ROOT_PATH; without a seeded root it finds the developer's config or none.
	doltRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(doltRoot, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir dolt root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(doltRoot, ".dolt", "config_global.json"),
		[]byte(`{"user.name":"gc-test","user.email":"gc-test@test.local"}`), 0o644); err != nil {
		t.Fatalf("seed dolt identity: %v", err)
	}
	// testenv.Init scrubs DOLT_ROOT_PATH, so in-process helpers (ensureBeadsProvider)
	// need it back on the parent env; commandEnv carries it to gc subprocesses.
	t.Setenv("DOLT_ROOT_PATH", doltRoot)
	commandEnv := append([]string(nil), os.Environ()...)
	commandEnv = replaceEnvEntry(commandEnv, "PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	commandEnv = replaceEnvEntry(commandEnv, "GC_HOME", gcHome)
	commandEnv = replaceEnvEntry(commandEnv, "XDG_RUNTIME_DIR", runtimeDir)
	commandEnv = replaceEnvEntry(commandEnv, "GC_SESSION", "tmux")
	commandEnv = replaceEnvEntry(commandEnv, "GC_BEADS", "bd")
	commandEnv = replaceEnvEntry(commandEnv, "GC_BEADS_SCOPE_ROOT", cityPath)
	commandEnv = replaceEnvEntry(commandEnv, "GC_DOLT", "")
	commandEnv = replaceEnvEntry(commandEnv, "BEADS_DIR", filepath.Join(cityPath, ".beads"))
	commandEnv = replaceEnvEntry(commandEnv, "BEADS_DOLT_AUTO_START", "1")
	commandEnv = replaceEnvEntry(commandEnv, "GC_ALLOW_PROD_DOLT_PORT_IN_TESTS", "1")
	commandEnv = replaceEnvEntry(commandEnv, "DOLT_ROOT_PATH", doltRoot)

	runGC := func(timeout time.Duration, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		defer cancel()
		cmd := newExactStartStopGCCommand(ctx, commandEnv, gcBinary, args...)
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			t.Fatalf("gc %s: %v\n%s", strings.Join(args, " "), runErr, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGCAsSession := func(timeout time.Duration, sessionID, instanceToken string, args ...string) string {
		t.Helper()
		env := replaceEnvEntry(commandEnv, "GC_SESSION_ID", sessionID)
		env = replaceEnvEntry(env, "GC_INSTANCE_TOKEN", instanceToken)
		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		defer cancel()
		cmd := newExactStartStopGCCommand(ctx, env, gcBinary, args...)
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			t.Fatalf("gc %s as session %q: %v\n%s", strings.Join(args, " "), sessionID, runErr, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGC(30*time.Second,
		"init",
		"--skip-provider-readiness",
		"--no-start",
		"--name", guard.CityName(),
		"--file", configPath,
		cityPath,
	)
	loaded, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("load initialized exact start-stop city config: %v", err)
	}
	if loaded.Session.Socket != guard.SocketName() {
		t.Fatalf("initialized session socket = %q, want %q", loaded.Session.Socket, guard.SocketName())
	}

	schemaStatus := exactStartBDCommand(t, cityPath, "migrate", "schema", "--json")
	if strings.TrimSpace(schemaStatus) == "" {
		schemaStatus = exactStartBDCommand(t, cityPath, "migrate", "schema")
	}
	if !strings.Contains(schemaStatus, "v59") {
		t.Fatalf("bd schema status = %q, want v59", schemaStatus)
	}

	controllerCtx, cancelController := context.WithCancel(t.Context())
	var controllerStdout, controllerStderr synchronizedBuffer
	controllerCmd := newExactStartStopGCCommand(
		controllerCtx,
		commandEnv,
		gcBinary,
		"start",
		"--foreground",
		"--no-strict",
		cityPath,
	)
	controllerCmd.Stdout = &controllerStdout
	controllerCmd.Stderr = &controllerStderr
	if err := controllerCmd.Start(); err != nil {
		t.Fatalf("start production controller: %v", err)
	}
	controllerDone := make(chan error, 1)
	go func() {
		controllerDone <- controllerCmd.Wait()
	}()
	controllerStopped := false
	controllerWaited := false
	t.Cleanup(func() {
		if !controllerStopped {
			stopOutput, stopErr := runExactStartStopGC(commandEnv, 30*time.Second, gcBinary, "stop", "--force", cityPath)
			if stopErr != nil {
				t.Errorf("stop production controller: %v\n%s", stopErr, stopOutput)
			}
		}
		cancelController()
		if !controllerWaited {
			select {
			case <-controllerDone:
			case <-time.After(testutil.ExecRaceTimeout):
				t.Errorf("production controller did not exit; stdout=%q stderr=%q", controllerStdout.String(), controllerStderr.String())
			}
		}
	})
	waitForControllerAvailable(t, cityPath)

	var (
		traceOutput string
		traceStatus traceStatusResultJSON
	)
	if err := waitExactStartStopState(t.Context(), 15*time.Second, func() (bool, error) {
		out, runErr := runExactStartStopGC(
			commandEnv,
			10*time.Second,
			gcBinary,
			"--city", cityPath,
			"trace", "status", "--json",
		)
		traceOutput = out
		if runErr != nil {
			return false, runErr
		}
		var status traceStatusResultJSON
		if decodeErr := json.Unmarshal([]byte(exactStartStopJSONPayload(out)), &status); decodeErr != nil {
			return false, decodeErr
		}
		traceStatus = status
		return status.SessionReconciler.Available &&
			status.SessionReconciler.ConfiguredMode == "auto" &&
			status.SessionReconciler.EffectiveOwner == "keyed", nil
	}); err != nil {
		t.Fatalf("production session reconciler did not become auto/keyed: %v; status=%+v output=%q controller stdout=%q stderr=%q",
			err, traceStatus.SessionReconciler, traceOutput, controllerStdout.String(), controllerStderr.String())
	}

	backingStore := beads.NewBdStoreWithPrefix(cityPath, beads.ExecCommandRunner(), "gct")
	tmuxClient := runtimetmux.NewTmuxWithConfig(runtimetmux.Config{SocketName: guard.SocketName()})
	reviewerSpec, ok := sessionpkg.FindNamedSessionSpec(loaded, guard.CityName(), "reviewer")
	if !ok {
		t.Fatal("configured reviewer named-session spec is unavailable")
	}
	runGC(10*time.Second,
		"--city", cityPath,
		"trace", "start",
		"--template", "reviewer",
		"--for", "2m",
		"--level", string(TraceModeDetail),
	)
	pinnedAt := time.Now().UTC()
	pinOutput := runGC(10*time.Second,
		"--city", cityPath,
		"session", "pin", reviewerSpec.Identity, "--json",
	)
	var pinResult sessionActionResult
	if err := json.Unmarshal([]byte(exactStartStopJSONPayload(pinOutput)), &pinResult); err != nil {
		t.Fatalf("decode configured named pin: %v\n%s", err, pinOutput)
	}
	if pinResult.Action != "pin" || pinResult.SessionID == "" || pinResult.Pinned == nil || !*pinResult.Pinned {
		t.Fatalf("configured named pin result = %+v, want pinned session ID", pinResult)
	}
	var pinnedReviewer sessionpkg.Info
	if err := waitExactStartStopState(t.Context(), 30*time.Second, func() (bool, error) {
		bead, found, findErr := sessionpkg.FindCanonicalConfiguredNamedSessionBead(backingStore, reviewerSpec)
		if findErr != nil || !found || bead.ID != pinResult.SessionID {
			return false, findErr
		}
		candidates, listErr := backingStore.ListByMetadata(map[string]string{
			sessionpkg.NamedSessionIdentityMetadata: reviewerSpec.Identity,
		}, 0)
		if listErr != nil {
			return false, listErr
		}
		if len(candidates) != 1 || candidates[0].ID != bead.ID {
			return false, nil
		}
		info, getErr := sessionFrontDoor(backingStore).Get(bead.ID)
		if getErr != nil {
			return false, getErr
		}
		if sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(info)).BaseState != sessionpkg.BaseStateActive ||
			info.PinAwake != "true" || info.SessionName != reviewerSpec.SessionName || info.ConfiguredNamedIdentity != reviewerSpec.Identity ||
			info.ConfiguredNamedMode != "on_demand" || strings.TrimSpace(info.InstanceToken) == "" {
			return false, nil
		}
		ids, listErr := tmuxClient.ListSessionIDs()
		if listErr != nil || strings.TrimSpace(ids[info.SessionName]) == "" {
			return false, listErr
		}
		liveToken, tokenErr := tmuxClient.GetEnvironment(info.SessionName, "GC_INSTANCE_TOKEN")
		if tokenErr != nil || liveToken != info.InstanceToken {
			return false, tokenErr
		}
		pinnedReviewer = info
		return true, nil
	}); err != nil {
		t.Fatalf("configured named pin did not start within the 30s absolute budget: %v; controller stdout=%q stderr=%q", err, controllerStdout.String(), controllerStderr.String())
	}
	pinnedPanePID, err := tmuxClient.GetPanePID(pinnedReviewer.SessionName)
	if err != nil {
		t.Fatalf("read configured named pin pane PID: %v", err)
	}
	registerLivePaneProcess(pinnedPanePID)
	// The LEASE, not the admission source, is the ownership proof — the same
	// re-point ga-ij8mh ruling 3 ratified for the worker wake leg, applied to the
	// pin (ga-f7v2ft.157). `gc session pin` writes pin_awake, which fires
	// bead.updated and admits in_process; the CLI's socket admission then folds
	// onto that pending entry and the source stays in_process
	// (sessionStartController.admit's coalescing rule). Asserting
	// `admission == socket` therefore passed or failed on scheduling alone —
	// ga-f7v2ft.142's ruling — and when it lost, the leg saw zero records rather
	// than wrong-shaped ones (/var/tmp/frontier/journey/attempt-7-*.log).
	// `start_lease` names the family that actually authorized the start, and a
	// pinned configured-named row is certified by the configured-named wake
	// family (certifyConfiguredNamedWakeStartLease, cause pinned).
	var pinCommits []SessionReconcilerTraceRecord
	var pinCommitCandidates []SessionReconcilerTraceRecord
	if err := waitExactStartStopState(t.Context(), 10*time.Second, func() (bool, error) {
		records, readErr := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
			RecordType: TraceRecordOperation, SiteCode: TraceSiteLifecycleStartCommit,
			SessionName: pinnedReviewer.SessionName, TraceMode: TraceModeDetail,
		})
		if readErr != nil {
			return false, readErr
		}
		pinCommitCandidates = records
		pinCommits = pinCommits[:0]
		for _, record := range records {
			if record.SessionBeadID == pinnedReviewer.ID &&
				record.Fields["start_lease"] == configuredNamedWakeLeaseFamily &&
				record.Fields["session_id"] == pinnedReviewer.ID &&
				record.Fields["instance_token"] == pinnedReviewer.InstanceToken &&
				record.Fields["effect_applied"] == true {
				pinCommits = append(pinCommits, record)
			}
		}
		if len(pinCommits) > 1 {
			return false, fmt.Errorf("configured named pin commits = %d, want exactly one", len(pinCommits))
		}
		return len(pinCommits) == 1, nil
	}); err != nil {
		t.Fatalf("configured named pin start commit did not converge: %v; matching=%#v read=%#v pinned_token=%q controller stderr=%q",
			err, pinCommits, pinCommitCandidates, pinnedReviewer.InstanceToken, controllerStderr.String())
	}
	pinStartRecords, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
		RecordType: TraceRecordOperation, SiteCode: TraceSiteLifecycleStartRun, SessionName: pinnedReviewer.SessionName,
	})
	if err != nil {
		t.Fatalf("read configured named pin start trace: %v", err)
	}
	for _, record := range pinStartRecords {
		if record.OutcomeCode == TraceOutcomeStartEnqueued {
			t.Fatalf("configured named pin used legacy async start: %#v", pinStartRecords)
		}
	}
	if elapsed := time.Since(pinnedAt); elapsed >= 30*time.Second {
		t.Fatalf("configured named pin latency = %s, want live within the 30s absolute budget", elapsed)
	}
	killedReviewer := pinnedReviewer
	killedPanePID := pinnedPanePID
	killedTmuxIDs, err := tmuxClient.ListSessionIDs()
	if err != nil {
		t.Fatalf("read configured named pin tmux identity before kill: %v", err)
	}
	killedTmuxID := strings.TrimSpace(killedTmuxIDs[killedReviewer.SessionName])
	if killedTmuxID == "" {
		t.Fatalf("configured named pin tmux identity for %q is empty: %v", killedReviewer.SessionName, killedTmuxIDs)
	}
	killedAt := time.Now().UTC()
	pinnedKillOutput := runGC(10*time.Second,
		"--city", cityPath,
		"session", "kill", killedReviewer.ID, "--json",
	)
	var pinnedKillResult sessionActionResult
	if err := json.Unmarshal([]byte(exactStartStopJSONPayload(pinnedKillOutput)), &pinnedKillResult); err != nil {
		t.Fatalf("decode configured named pinned kill: %v\n%s", err, pinnedKillOutput)
	}
	if !pinnedKillResult.OK || pinnedKillResult.Action != "kill" || pinnedKillResult.SessionID != killedReviewer.ID {
		t.Fatalf("configured named pinned kill result = %+v, want successful kill for canonical %q", pinnedKillResult, killedReviewer.ID)
	}
	if err := waitExactStartStopState(t.Context(), 30*time.Second, func() (bool, error) {
		bead, found, findErr := sessionpkg.FindCanonicalConfiguredNamedSessionBead(backingStore, reviewerSpec)
		if findErr != nil || !found || bead.ID != killedReviewer.ID {
			return false, findErr
		}
		info, getErr := sessionFrontDoor(backingStore).Get(bead.ID)
		if getErr != nil {
			return false, getErr
		}
		if info.Closed || info.SessionName != killedReviewer.SessionName || info.PinAwake != "true" ||
			info.ConfiguredNamedIdentity != reviewerSpec.Identity || info.WakeRequest != "" ||
			sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(info)).BaseState != sessionpkg.BaseStateActive ||
			info.PendingCreateClaim || strings.TrimSpace(info.InstanceToken) == "" ||
			info.InstanceToken == killedReviewer.InstanceToken {
			return false, nil
		}
		if gone, retireErr := retireExitedPaneProcess(killedPanePID); retireErr != nil {
			return false, retireErr
		} else if !gone {
			return false, nil
		}
		ids, listErr := tmuxClient.ListSessionIDs()
		if listErr != nil {
			return false, listErr
		}
		tmuxID := strings.TrimSpace(ids[info.SessionName])
		if tmuxID == "" || tmuxID == killedTmuxID {
			return false, nil
		}
		liveToken, tokenErr := tmuxClient.GetEnvironment(info.SessionName, "GC_INSTANCE_TOKEN")
		if tokenErr != nil || liveToken != info.InstanceToken {
			return false, tokenErr
		}
		pinnedReviewer = info
		return true, nil
	}); err != nil {
		current, currentErr := sessionFrontDoor(backingStore).Get(killedReviewer.ID)
		t.Fatalf("killed pinned on-demand session did not restart the same row within 30s: %v; current=%+v current_err=%v controller stdout=%q stderr=%q",
			err, current, currentErr, controllerStdout.String(), controllerStderr.String())
	}
	// The bead AC phrases this budget as beating the held legacy debounce; per
	// DETECTOR.md sec 4 the journey asserts the equivalent absolute bound.
	if elapsed := time.Since(killedAt); elapsed >= 30*time.Second {
		t.Fatalf("killed pinned on-demand restart latency = %s, want the same row live again within 30s", elapsed)
	}
	if err := removeExitedPaneProcess(killedPanePID); err != nil {
		t.Fatalf("remove exited configured named pinned pane process: %v", err)
	}
	pinnedPanePID, err = tmuxClient.GetPanePID(pinnedReviewer.SessionName)
	if err != nil {
		t.Fatalf("read restarted configured named pinned pane PID: %v", err)
	}
	registerLivePaneProcess(pinnedPanePID)
	// Same re-point as the first pin commit above: the restart after a kill is
	// still a pinned configured-named wake, so the certified family is the proof
	// and the admission source is a scheduling artifact (ga-f7v2ft.157).
	var pinnedKillCommits []SessionReconcilerTraceRecord
	var pinnedKillCommitCandidates []SessionReconcilerTraceRecord
	if err := waitExactStartStopState(t.Context(), 10*time.Second, func() (bool, error) {
		records, readErr := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
			RecordType: TraceRecordOperation, SiteCode: TraceSiteLifecycleStartCommit,
			SessionName: pinnedReviewer.SessionName, TraceMode: TraceModeDetail,
		})
		if readErr != nil {
			return false, readErr
		}
		pinnedKillCommitCandidates = records
		pinnedKillCommits = pinnedKillCommits[:0]
		for _, record := range records {
			if record.SessionBeadID == pinnedReviewer.ID &&
				record.Fields["start_lease"] == configuredNamedWakeLeaseFamily &&
				record.Fields["session_id"] == pinnedReviewer.ID &&
				record.Fields["instance_token"] == pinnedReviewer.InstanceToken &&
				record.Fields["effect_applied"] == true {
				pinnedKillCommits = append(pinnedKillCommits, record)
			}
		}
		if len(pinnedKillCommits) > 1 {
			return false, fmt.Errorf("killed pinned restart commits = %d, want exactly one", len(pinnedKillCommits))
		}
		return len(pinnedKillCommits) == 1, nil
	}); err != nil {
		t.Fatalf("killed pinned restart start commit did not converge: %v; matching=%#v read=%#v pinned_token=%q controller stderr=%q",
			err, pinnedKillCommits, pinnedKillCommitCandidates, pinnedReviewer.InstanceToken, controllerStderr.String())
	}
	pinnedKillStartRecords, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
		RecordType: TraceRecordOperation, SiteCode: TraceSiteLifecycleStartRun, SessionName: pinnedReviewer.SessionName,
	})
	if err != nil {
		t.Fatalf("read killed pinned restart start trace: %v", err)
	}
	for _, record := range pinnedKillStartRecords {
		if record.OutcomeCode == TraceOutcomeStartEnqueued {
			t.Fatalf("killed pinned on-demand restart used legacy async start: %#v", pinnedKillStartRecords)
		}
	}
	restartedPinnedBead, err := backingStore.Get(pinnedReviewer.ID)
	if err != nil {
		t.Fatalf("read durable killed pinned restart row: %v", err)
	}
	if restartedPinnedBead.Metadata["wake_request"] != "" || restartedPinnedBead.Metadata["wake_requested_at"] != "" {
		t.Fatalf("killed pinned restart synthesized a wake marker: %+v", restartedPinnedBead.Metadata)
	}
	// The suspend leg's determinism is an ownership argument, not a quiet-window
	// one. This city runs patrol=1h and every poke ticks immediately, so a fleet
	// tick can land anywhere inside the leg; what makes the keyed stop the sole
	// converger once the suspend patch is durable is:
	//   F1 the legacy status heal is revision-fenced, so a heal computed from a
	//      pre-patch snapshot loses its CAS and cannot revert state to "awake";
	//   F2 the suspend family engages on ANY admission source, so no coalescing
	//      rule can drop the request into the ordinary path's held-blocker dead end;
	//   F4 legacyStartExcluded carries exactUserHoldSuspendCurrent, so a tick whose
	//      snapshot IS post-patch skips the row before it can begin a legacy drain.
	// The assertions below are therefore keyed-ownership evidence (canonical row,
	// tmux and pane gone, exactly one exact_session_suspend_stop, no drain-ack
	// metadata) measured against an absolute budget -- never "legacy was asleep".
	suspendAt := time.Now().UTC()
	suspendOutput := runGC(10*time.Second,
		"--city", cityPath,
		"session", "suspend", reviewerSpec.Identity, "--json",
	)
	var suspendResult sessionActionResult
	if err := json.Unmarshal([]byte(exactStartStopJSONPayload(suspendOutput)), &suspendResult); err != nil {
		t.Fatalf("decode configured named suspend: %v\n%s", err, suspendOutput)
	}
	if suspendResult.Action != "suspend" || suspendResult.SessionID != pinnedReviewer.ID || suspendResult.Mode != "managed" || suspendResult.State != "suspended" {
		t.Fatalf("configured named suspend result = %+v, want managed suspended canonical ID %q", suspendResult, pinnedReviewer.ID)
	}
	var suspendedReviewer sessionpkg.Info
	if err := waitExactStartStopState(t.Context(), 30*time.Second, func() (bool, error) {
		bead, found, findErr := sessionpkg.FindCanonicalConfiguredNamedSessionBead(backingStore, reviewerSpec)
		if findErr != nil || !found || bead.ID != pinnedReviewer.ID {
			return false, findErr
		}
		info, getErr := sessionFrontDoor(backingStore).Get(bead.ID)
		if getErr != nil {
			return false, getErr
		}
		heldUntil, parseErr := time.Parse(time.RFC3339, info.HeldUntil)
		if parseErr != nil || info.Closed || info.MetadataState != string(sessionpkg.StateSuspended) ||
			info.SleepIntent != "user-hold" || !heldUntil.After(time.Now().UTC()) || info.PinAwake != "true" ||
			info.SessionName != pinnedReviewer.SessionName || info.ConfiguredNamedIdentity != reviewerSpec.Identity {
			return false, parseErr
		}
		ids, listErr := tmuxClient.ListSessionIDs()
		if listErr != nil || strings.TrimSpace(ids[info.SessionName]) != "" {
			return false, listErr
		}
		if gone, retireErr := retireExitedPaneProcess(pinnedPanePID); retireErr != nil {
			return false, retireErr
		} else if !gone {
			return false, nil
		}
		suspendedReviewer = info
		return true, nil
	}); err != nil {
		current, currentErr := sessionFrontDoor(backingStore).Get(pinnedReviewer.ID)
		t.Fatalf("configured named suspend did not stop within the 30s absolute budget: %v; current=%+v current_err=%v controller stderr=%q", err, current, currentErr, controllerStderr.String())
	}
	if err := removeExitedPaneProcess(pinnedPanePID); err != nil {
		t.Fatalf("remove exited configured named pane process: %v", err)
	}
	suspendedBead, err := backingStore.Get(suspendedReviewer.ID)
	if err != nil {
		t.Fatalf("read durable configured named suspend row: %v", err)
	}
	for _, key := range []string{"drain_ack_source", "drain_ack_requester_session_id", "drain_ack_requester_instance_token"} {
		if suspendedBead.Metadata[key] != "" {
			t.Fatalf("configured named suspend retained legacy drain metadata %s=%q", key, suspendedBead.Metadata[key])
		}
	}
	if suspendedBead.Metadata["state"] == "drain-ack-stop-pending" {
		t.Fatalf("configured named suspend entered drain-ack stop-pending: %+v", suspendedBead.Metadata)
	}
	var exactSuspendStops []SessionReconcilerTraceRecord
	if err := waitExactStartStopState(t.Context(), 10*time.Second, func() (bool, error) {
		suspendTrace, readErr := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
			RecordType: TraceRecordOperation, SiteCode: TraceSiteLifecycleDrainAdvance,
			SessionName: suspendedReviewer.SessionName, TraceMode: TraceModeDetail,
		})
		if readErr != nil {
			return false, readErr
		}
		exactSuspendStops = exactSuspendStops[:0]
		for _, record := range suspendTrace {
			if record.SessionBeadID == suspendedReviewer.ID && record.ReasonCode == TraceReasonUserHold &&
				record.OutcomeCode == TraceOutcomeSuccess && record.OperationID != "" &&
				record.Fields["operation_name"] == "exact_session_suspend_stop" {
				exactSuspendStops = append(exactSuspendStops, record)
			}
		}
		if len(exactSuspendStops) > 1 {
			return false, fmt.Errorf("exact configured named suspend traces = %#v, want exactly one keyed suspend stop", exactSuspendStops)
		}
		return len(exactSuspendStops) == 1, nil
	}); err != nil {
		t.Fatalf("wait for exact configured named suspend trace: %v; traces=%#v controller stderr=%q", err, exactSuspendStops, controllerStderr.String())
	}
	if elapsed := time.Since(suspendAt); elapsed >= 30*time.Second {
		t.Fatalf("configured named suspend latency = %s, want stop within the 30s absolute budget", elapsed)
	}
	// RE-LANDED at WD.10a (ga-f7v2ft.116), closing ga-ij8mh. The leg used to
	// fabricate a bare origin-less target and assert the pool markers were ABSENT
	// — a pre-sync shape no production path sustains, which only survived because
	// the fixture's 30s tick debounce guaranteed no sync ran inside the leg. It
	// now runs against the shape production actually produces: the row is handed
	// to the fleet, syncSessionBeads stamps the canonical singleton identity
	// (pool_managed + ephemeral origin, no slot), and the leg asserts those
	// markers are PRESENT on the row the keyed family starts. Under debounce-0
	// the wake is requested before the fleet can see the row, which is what the
	// sweep rule and the pre-lease ownership seam exist to survive.
	// configuredDependencyWakeBudget is the leg's absolute detect-to-start bound.
	// It is wall-clock, not debounce-relative: the tick debouncer is retired, so
	// the old "beats the 30s debounce" framing named a mechanism that no longer
	// exists. The bound covers a full sync adoption plus a certified keyed start
	// under an active legacy at debounce-0.
	const configuredDependencyWakeBudget = 60 * time.Second
	t.Run("configured_dependency_wake", func(t *testing.T) {
		dependencySpec, ok := sessionpkg.FindNamedSessionSpec(loaded, guard.CityName(), "database")
		if !ok {
			t.Fatal("configured database named-session spec is unavailable")
		}
		var (
			dependencyBefore       sessionpkg.Info
			dependencyTmuxIDBefore string
		)
		if err := waitExactStartStopState(t.Context(), 30*time.Second, func() (bool, error) {
			bead, found, findErr := sessionpkg.FindCanonicalConfiguredNamedSessionBead(backingStore, dependencySpec)
			if findErr != nil || !found {
				return false, findErr
			}
			info, getErr := sessionFrontDoor(backingStore).Get(bead.ID)
			if getErr != nil {
				return false, getErr
			}
			lifecycle := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(info))
			if lifecycle.BaseState != sessionpkg.BaseStateActive || strings.TrimSpace(info.InstanceToken) == "" {
				return false, nil
			}
			ids, listErr := tmuxClient.ListSessionIDs()
			if listErr != nil {
				return false, listErr
			}
			tmuxID := strings.TrimSpace(ids[info.SessionName])
			if tmuxID == "" {
				return false, nil
			}
			liveToken, tokenErr := tmuxClient.GetEnvironment(info.SessionName, "GC_INSTANCE_TOKEN")
			if tokenErr != nil || liveToken != info.InstanceToken {
				return false, tokenErr
			}
			dependencyBefore = info
			dependencyTmuxIDBefore = tmuxID
			return true, nil
		}); err != nil {
			t.Fatalf("configured singleton dependency did not become live: %v; controller stdout=%q stderr=%q",
				err, controllerStdout.String(), controllerStderr.String())
		}

		// Detail-arm BEFORE the row exists. Arming sends trace-arm:, which pokes
		// the runtime, and post-WD.0 that poke runs a fleet tick immediately. Doing
		// it after the create would put a deliberate tick between a bare,
		// demand-less row and its wake — and a configured single-session agent
		// generates no desired-state demand of its own, so that tick's successor
		// reaps the row before the operator's wake ever lands. Arming first leaves
		// the create→wake window with no poke in it (raw bd writes are
		// event-silent), which is the same window production has.
		runGC(10*time.Second,
			"--city", cityPath,
			"trace", "start",
			"--template", "dependent",
			"--for", "2m",
			"--level", string(TraceModeDetail),
		)

		dependentSessionName := guard.SessionName("dependent")
		dependentMetadata := desiredSessionIdentity(sessionIdentityInputs{
			AgentName:         "dependent",
			SessionName:       dependentSessionName,
			State:             string(sessionpkg.StateAsleep),
			Generation:        1,
			ContinuationEpoch: 1,
			InstanceToken:     sessionpkg.NewInstanceToken(),
			ConfigResolved:    true,
		})
		dependentMetadata["template"] = "dependent"
		// The row is born carrying its explicit wake. Between a bare create and a
		// separate wake there is a window in which the row is asleep, undesired and
		// demand-less — a configured single-session agent generates no
		// desired-state demand of its own — and in that window every reaper in the
		// fleet is CORRECT to close it. That window is a fixture artifact: a
		// production row for such an agent never exists without demand. Creating it
		// wake-current removes the artifact without weakening anything the leg
		// proves; `gc session wake` below is still the CLI trigger that drives the
		// certified keyed admission.
		for key, value := range sessionpkg.RequestExplicitWakePatch(string(sessionpkg.WakeCauseExplicit), time.Now().UTC()) {
			dependentMetadata[key] = value
		}
		dependentBead, err := backingStore.Create(beads.Bead{
			Title:    "ordinary configured dependency wake target",
			Type:     sessionBeadType,
			Status:   "open",
			Labels:   []string{sessionBeadLabel},
			Metadata: dependentMetadata,
		})
		if err != nil {
			t.Fatalf("create ordinary configured wake target: %v", err)
		}
		// The row is handed over BARE. Nothing here stamps an identity: the whole
		// point of the re-land is that the production sync path, not the fixture,
		// materializes the canonical singleton shape, and the assertions below
		// require it to have done so by the time the keyed family starts the row.
		for _, key := range []string{
			"session_origin", "manual_session", "configured_named_identity", "configured_named_session",
			poolManagedMetadataKey, "pool_slot", "dependency_only", "pending_create_claim",
		} {
			if dependentBead.Metadata[key] != "" {
				t.Fatalf("ordinary configured wake target metadata %s = %q, want absent at hand-over", key, dependentBead.Metadata[key])
			}
		}
		dependentWakeAt := time.Now().UTC()
		dependentWakeOutput := runGC(10*time.Second,
			"--city", cityPath,
			"session", "wake", dependentBead.ID,
			"--json",
		)
		var dependentWakeResult sessionActionResult
		if err := json.Unmarshal([]byte(exactStartStopJSONPayload(dependentWakeOutput)), &dependentWakeResult); err != nil {
			t.Fatalf("decode configured-dependency wake: %v\n%s", err, dependentWakeOutput)
		}
		if dependentWakeResult.Action != "wake" || dependentWakeResult.SessionID != dependentBead.ID || dependentWakeResult.State != "wake_requested" {
			t.Fatalf("configured-dependency wake result = %+v, want wake_requested for %q", dependentWakeResult, dependentBead.ID)
		}
		var (
			dependentAfter  sessionpkg.Info
			dependentTmuxID string
		)
		if err := waitExactStartStopState(t.Context(), configuredDependencyWakeBudget, func() (bool, error) {
			info, getErr := sessionFrontDoor(backingStore).Get(dependentBead.ID)
			if getErr != nil {
				return false, getErr
			}
			lifecycle := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(info))
			if lifecycle.BaseState != sessionpkg.BaseStateActive || info.PendingCreateClaim || info.WakeRequest != "" || strings.TrimSpace(info.InstanceToken) == "" {
				return false, nil
			}
			ids, listErr := tmuxClient.ListSessionIDs()
			if listErr != nil {
				return false, listErr
			}
			tmuxID := strings.TrimSpace(ids[dependentSessionName])
			if tmuxID == "" {
				return false, nil
			}
			liveToken, tokenErr := tmuxClient.GetEnvironment(dependentSessionName, "GC_INSTANCE_TOKEN")
			if tokenErr != nil || liveToken != info.InstanceToken {
				return false, tokenErr
			}
			dependentAfter = info
			dependentTmuxID = tmuxID
			return true, nil
		}); err != nil {
			current, currentErr := sessionFrontDoor(backingStore).Get(dependentBead.ID)
			t.Fatalf("configured-dependency wake did not start within the 60s absolute budget: %v; current=%+v current_err=%v controller stdout=%q stderr=%q",
				err, current, currentErr, controllerStdout.String(), controllerStderr.String())
		}
		if dependentAfter.ID != dependentBead.ID || dependentAfter.SessionName != dependentSessionName {
			t.Fatalf("configured-dependency wake changed target identity: bead=%+v session=%+v", dependentBead, dependentAfter)
		}
		// The acceptance the re-land exists for (ga-ij8mh ruling 4, amendment 5):
		// the keyed family owns the shape production actually sustains, and the
		// two wake families partition on SLOT markers rather than on pool_managed.
		//
		// This asserts the CONTRACT, not one writer's timing. The re-land
		// originally demanded the sync-produced canonical singleton here, on
		// `dependentAfter` — the snapshot taken at the FIRST instant the start
		// converged. syncSessionBeads is an independent writer and nothing makes
		// it win that race: configuredDependencyWakeShapeMatches
		// (session_start_reconcile.go) accepts BOTH the canonical singleton AND
		// the origin-less row "a sync has not yet touched", by design. So a bare
		// row at this instant is the product behaving to spec, and the old
		// assertion failed on scheduling alone — 2 of 9 runs, ga-f7v2ft.157,
		// logs /var/tmp/frontier/journey/attempt-{2,4}-*.log.
		//
		// The proof that the KEYED family owned this start is the start_lease
		// assertion below, which the race cannot touch. What this block owes is
		// that the row is still an ordinary configured-dependency target, that it
		// never acquired a slot (amendment 1's partition), and that when sync HAS
		// stamped it the shape is exactly the canonical singleton ga-ij8mh was
		// filed about — the full regression guard, applied whenever the race
		// lands that way.
		dependentAgent := findAgentByTemplate(loaded, "dependent")
		if dependentAgent == nil {
			t.Fatal("configured dependent agent is unavailable")
		}
		if isNamedSessionInfo(dependentAfter) || isManualSessionInfoForAgent(dependentAfter, dependentAgent) ||
			dependentAfter.DependencyOnly || dependentAfter.PendingCreateClaim {
			t.Fatalf("configured-dependency wake target left the ordinary configured shape: %+v", dependentAfter)
		}
		if !configuredDependencyWakeShapeMatches(dependentAfter, dependentAgent) {
			t.Fatalf("configured-dependency wake target is not a shape the family owns: %+v", dependentAfter)
		}
		if isPoolManagedSessionInfo(dependentAfter) {
			if !isCanonicalPoolManagedSessionInfoForTemplate(dependentAfter, "dependent") ||
				strings.TrimSpace(dependentAfter.SessionOrigin) != "ephemeral" || !dependentAfter.PoolManaged {
				t.Fatalf("sync-stamped configured-dependency wake target is not the canonical singleton: %+v", dependentAfter)
			}
			t.Logf("configured-dependency wake target observed as the sync-produced canonical singleton")
		} else {
			t.Logf("configured-dependency wake target observed pre-sync (origin-less); the keyed family owns this shape too")
		}
		if strings.TrimSpace(dependentAfter.PoolSlot) != "" {
			t.Fatalf("configured-dependency wake target acquired a pool slot (%q); slotized rows belong to the strict-default pool family",
				dependentAfter.PoolSlot)
		}
		if dependentAfter.Closed || dependentAfter.MetadataState == "gc_swept" {
			t.Fatalf("configured-dependency wake target was reaped by the undesired-pool sweep: %+v", dependentAfter)
		}
		dependentPanePID, err := tmuxClient.GetPanePID(dependentSessionName)
		if err != nil {
			t.Fatalf("read configured-dependency pane PID: %v", err)
		}
		registerLivePaneProcess(dependentPanePID)

		var dependentCommits []SessionReconcilerTraceRecord
		if err := waitExactStartStopState(t.Context(), configuredDependencyWakeBudget, func() (bool, error) {
			records, readErr := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
				RecordType:  TraceRecordOperation,
				SiteCode:    TraceSiteLifecycleStartCommit,
				SessionName: dependentSessionName,
				TraceMode:   TraceModeDetail,
			})
			if readErr != nil {
				return false, readErr
			}
			dependentCommits = dependentCommits[:0]
			for _, record := range records {
				// The LEASE, not the admission source, is the ownership proof. A
				// certified configured-dependency certificate now reaches the handler
				// by three entry points — the CLI socket, the pre-lease ownership
				// seam, and the detector sweep's routing seam — and the source is
				// sticky to whichever admitted the key FIRST, so asserting on it
				// would prove only that some keyed admission ran. `start_lease` names
				// the family that actually authorized the start.
				if record.Fields["start_lease"] != configuredDependencyLeaseFamily {
					continue
				}
				if record.SessionBeadID == dependentBead.ID &&
					record.Fields["session_id"] == dependentBead.ID &&
					record.Fields["instance_token"] == dependentAfter.InstanceToken &&
					record.Fields["effect_applied"] == true {
					dependentCommits = append(dependentCommits, record)
				}
			}
			if len(dependentCommits) > 1 {
				return false, fmt.Errorf("configured-dependency start commits = %d, want exactly one", len(dependentCommits))
			}
			return len(dependentCommits) == 1, nil
		}); err != nil {
			t.Fatalf("configured-dependency keyed commit did not converge: %v; matching=%#v controller stderr=%q",
				err, dependentCommits, controllerStderr.String())
		}
		dependentStartRecords, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
			RecordType:  TraceRecordOperation,
			SiteCode:    TraceSiteLifecycleStartRun,
			SessionName: dependentSessionName,
		})
		if err != nil {
			t.Fatalf("read configured-dependency start trace: %v", err)
		}
		for _, record := range dependentStartRecords {
			if record.OutcomeCode == TraceOutcomeStartEnqueued {
				t.Fatalf("configured-dependency wake used legacy async start: %#v", dependentStartRecords)
			}
		}
		dependencyAfter, err := sessionFrontDoor(backingStore).Get(dependencyBefore.ID)
		if err != nil {
			t.Fatalf("read configured dependency after target wake: %v", err)
		}
		dependencyIDsAfter, err := tmuxClient.ListSessionIDs()
		if err != nil {
			t.Fatalf("read configured dependency tmux identity after target wake: %v", err)
		}
		dependencyLiveTokenAfter, err := tmuxClient.GetEnvironment(dependencyBefore.SessionName, "GC_INSTANCE_TOKEN")
		if err != nil {
			t.Fatalf("read configured dependency token after target wake: %v", err)
		}
		if dependencyAfter.ID != dependencyBefore.ID || dependencyAfter.InstanceToken != dependencyBefore.InstanceToken ||
			strings.TrimSpace(dependencyIDsAfter[dependencyBefore.SessionName]) != dependencyTmuxIDBefore ||
			dependencyLiveTokenAfter != dependencyBefore.InstanceToken {
			t.Fatalf("configured dependency changed during target wake: before=%+v after=%+v tmux_before=%q tmux_after=%q live_token_after=%q",
				dependencyBefore, dependencyAfter, dependencyTmuxIDBefore,
				strings.TrimSpace(dependencyIDsAfter[dependencyBefore.SessionName]), dependencyLiveTokenAfter)
		}
		// Absolute budget, not a debounce-relative claim: the debouncer is retired
		// (WD.0), so "beat the 30s debounce" no longer names anything. What the leg
		// asserts is that a certified wake converges within a fixed wall-clock
		// bound while legacy is active at debounce-0.
		if elapsed := time.Since(dependentWakeAt); elapsed >= configuredDependencyWakeBudget || dependentTmuxID == "" {
			t.Fatalf("configured-dependency wake latency/tmux = %s/%q, want live within the %s absolute budget",
				elapsed, dependentTmuxID, configuredDependencyWakeBudget)
		}
	})

	startAdmittedAt := time.Now().UTC()
	createdOutput := runGC(30*time.Second,
		"--city", cityPath,
		"session", "new", "worker",
		"--no-attach",
		"--json",
	)
	var created sessionNewJSON
	if err := json.Unmarshal([]byte(exactStartStopJSONPayload(createdOutput)), &created); err != nil {
		t.Fatalf("decode exact session creation: %v\n%s", err, createdOutput)
	}
	if created.SessionID == "" || created.SessionName == "" || !created.DeferredStart {
		t.Fatalf("exact session creation = %+v, want deferred ID and tmux name", created)
	}

	var (
		started          sessionpkg.Info
		lastStartInfo    sessionpkg.Info
		startFinalizedAt time.Time
	)
	if err := waitExactStartStopState(t.Context(), 30*time.Second, func() (bool, error) {
		info, getErr := sessionFrontDoor(backingStore).Get(created.SessionID)
		if getErr != nil {
			return false, getErr
		}
		lastStartInfo = info
		view := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(info))
		if view.BaseState != sessionpkg.BaseStateActive || info.PendingCreateClaim {
			return false, nil
		}
		started = info
		startFinalizedAt = time.Now().UTC()
		return true, nil
	}); err != nil {
		t.Fatalf("exact start did not converge: %v; current=%+v controller stdout=%q stderr=%q",
			err, lastStartInfo, controllerStdout.String(), controllerStderr.String())
	}
	liveToken, err := tmuxClient.GetEnvironment(created.SessionName, "GC_INSTANCE_TOKEN")
	if err != nil {
		t.Fatalf("read live isolated tmux instance token: %v; controller stdout=%q stderr=%q",
			err, controllerStdout.String(), controllerStderr.String())
	}
	if strings.TrimSpace(liveToken) == "" || liveToken != started.InstanceToken {
		t.Fatalf("live/durable instance tokens = %q/%q, want the same non-empty token", liveToken, started.InstanceToken)
	}
	sessionIDs, err := tmuxClient.ListSessionIDs()
	if err != nil {
		t.Fatalf("read exact-start tmux identity: %v", err)
	}
	startedTmuxID := strings.TrimSpace(sessionIDs[created.SessionName])
	if startedTmuxID == "" {
		t.Fatalf("exact-start tmux identity for %q is empty: %v", created.SessionName, sessionIDs)
	}
	startedPanePID, err := tmuxClient.GetPanePID(created.SessionName)
	if err != nil {
		t.Fatalf("read exact-start pane PID: %v", err)
	}
	if _, err := strconv.Atoi(startedPanePID); err != nil {
		t.Fatalf("exact-start pane PID = %q, want a numeric live process identity: %v", startedPanePID, err)
	}
	if exactStartStopProcessExited(startedPanePID) {
		t.Fatalf("exact-start pane PID %q was already dead before drain admission", startedPanePID)
	}
	startedTmuxServerPID := guard.ServerPID()
	if startedTmuxServerPID <= 0 {
		t.Fatalf("exact-start tmux server PID = %d, want positive", startedTmuxServerPID)
	}

	if err := sessionFrontDoor(backingStore).ApplyPatch(created.SessionID, sessionpkg.MetadataPatch{
		"wake_request": string(sessionpkg.WakeCauseExplicit),
	}); err != nil {
		t.Fatalf("stamp explicit wake marker on live session: %v", err)
	}
	preHeal, err := backingStore.Get(created.SessionID)
	if err != nil {
		t.Fatalf("read v59 pre-heal session: %v", err)
	}
	if preHeal.Revision == 0 || preHeal.Metadata["state"] != string(sessionpkg.StateActive) {
		t.Fatalf("v59 pre-heal revision/state = %d/%q, want nonzero/active", preHeal.Revision, preHeal.Metadata["state"])
	}
	healReply, err := sendControllerCommandWithReadTimeout(
		cityPath,
		sessionStartCommandPrefix+created.SessionID,
		10*time.Second,
	)
	if err != nil {
		t.Fatalf("submit exact status-heal key through controller socket: %v", err)
	}
	if got := strings.TrimSpace(string(healReply)); got != string(sessionStartSocketReplyOK) {
		t.Fatalf("exact status-heal socket reply = %q, want %q", got, sessionStartSocketReplyOK)
	}
	var (
		healedBead     beads.Bead
		lastHealedBead beads.Bead
	)
	if err := waitExactStartStopState(t.Context(), 30*time.Second, func() (bool, error) {
		current, getErr := backingStore.Get(created.SessionID)
		if getErr != nil {
			return false, getErr
		}
		lastHealedBead = current
		if current.Metadata["state"] != string(sessionpkg.StateAwake) {
			return false, nil
		}
		healedBead = current
		return true, nil
	}); err != nil {
		t.Fatalf("v59 status heal did not converge: %v; current=%+v controller stdout=%q stderr=%q",
			err, lastHealedBead, controllerStdout.String(), controllerStderr.String())
	}
	if healedBead.Revision == 0 || healedBead.Revision == preHeal.Revision ||
		healedBead.Metadata["wake_request"] != string(sessionpkg.WakeCauseExplicit) ||
		healedBead.Metadata["session_name"] != created.SessionName ||
		healedBead.Metadata["instance_token"] != started.InstanceToken ||
		healedBead.Metadata["pending_create_claim"] != "" {
		t.Fatalf("v59 healed row = revision %d metadata %#v, want a new revision with identity preserved",
			healedBead.Revision, healedBead.Metadata)
	}
	healedSession, err := sessionFrontDoor(backingStore).Get(created.SessionID)
	if err != nil {
		t.Fatalf("project v59 healed session: %v", err)
	}
	if healedSession.MetadataState != string(sessionpkg.StateAwake) {
		t.Fatalf("v59 healed session state = %q, want awake", healedSession.MetadataState)
	}
	sessionIDs, err = tmuxClient.ListSessionIDs()
	if err != nil || strings.TrimSpace(sessionIDs[created.SessionName]) != startedTmuxID {
		t.Fatalf("v59 status heal changed tmux identity: before=%q after=%q all=%v err=%v",
			startedTmuxID, strings.TrimSpace(sessionIDs[created.SessionName]), sessionIDs, err)
	}
	if err := sessionFrontDoor(backingStore).ApplyPatch(created.SessionID, sessionpkg.MetadataPatch{
		"wake_request":      "",
		"wake_requested_at": "",
	}); err != nil {
		t.Fatalf("clear status-heal wake marker before drain: %v", err)
	}
	clearedBeforeDrain, err := backingStore.Get(created.SessionID)
	if err != nil {
		t.Fatalf("read session after clearing status-heal wake marker: %v", err)
	}
	if clearedBeforeDrain.Metadata["wake_request"] != "" || clearedBeforeDrain.Metadata["wake_requested_at"] != "" {
		t.Fatalf("status-heal wake marker before drain = %#v, want both fields cleared", clearedBeforeDrain.Metadata)
	}
	started, err = sessionFrontDoor(backingStore).Get(created.SessionID)
	if err != nil {
		t.Fatalf("project session after clearing status-heal wake marker: %v", err)
	}

	killOutput := runGC(10*time.Second,
		"--city", cityPath,
		"session", "kill", created.SessionID,
		"--json",
	)
	var killResult sessionActionResult
	if err := json.Unmarshal([]byte(exactStartStopJSONPayload(killOutput)), &killResult); err != nil {
		t.Fatalf("decode exact session kill: %v\n%s", err, killOutput)
	}
	if !killResult.OK || killResult.Action != "kill" || killResult.SessionID != created.SessionID {
		t.Fatalf("exact session kill result = %+v, want successful kill for durable session %q", killResult, created.SessionID)
	}

	if err := waitExactStartStopState(t.Context(), 10*time.Second, func() (bool, error) {
		if !exactStartStopProcessExited(startedPanePID) {
			return false, nil
		}
		info, getErr := sessionFrontDoor(backingStore).Get(created.SessionID)
		if getErr != nil {
			return false, getErr
		}
		if info.MetadataState != string(sessionpkg.StateAsleep) {
			return false, nil
		}
		return true, nil
	}); err != nil {
		current, currentErr := sessionFrontDoor(backingStore).Get(created.SessionID)
		currentIDs, idsErr := tmuxClient.ListSessionIDs()
		t.Fatalf("exact session kill did not converge to an exited runtime and asleep durable state: %v; current=%+v current_err=%v tmux_ids=%v tmux_err=%v pane_pid=%q pane_exited=%t controller stdout=%q stderr=%q",
			err, current, currentErr, currentIDs, idsErr, startedPanePID, exactStartStopProcessExited(startedPanePID), controllerStdout.String(), controllerStderr.String())
	}
	killedBead, err := backingStore.Get(created.SessionID)
	if err != nil {
		t.Fatalf("read exact session-kill bead from real bd: %v", err)
	}
	if killedBead.Status != "open" || killedBead.Metadata["state"] != string(sessionpkg.StateAsleep) {
		t.Fatalf("exact session-kill bead = %+v, want open asleep session bead", killedBead)
	}
	afterIDs, listErr := tmuxClient.ListSessionIDs()
	if listErr != nil && !strings.Contains(strings.ToLower(listErr.Error()), "no server running") {
		t.Fatalf("list isolated tmux sessions after exact session kill: %v", listErr)
	}
	if afterID := strings.TrimSpace(afterIDs[created.SessionName]); afterID != "" {
		t.Fatalf("exact session kill left or replaced tmux target %q: before=%q after=%q all=%v",
			created.SessionName, startedTmuxID, afterID, afterIDs)
	}
	startSuccessLog := fmt.Sprintf(
		"session lifecycle: op=start wave=0 session=%s template=worker outcome=success",
		created.SessionName,
	)
	if count := strings.Count(controllerStderr.String(), startSuccessLog); count != 1 {
		t.Fatalf("successful provider starts = %d, want exactly 1; controller stderr=%q", count, controllerStderr.String())
	}

	runGC(10*time.Second,
		"--city", cityPath,
		"trace", "start",
		"--template", "worker",
		"--for", "2m",
		"--level", string(TraceModeDetail),
	)
	wakeCommandAt := time.Now().UTC()
	wakeOutput := runGC(30*time.Second,
		"--city", cityPath,
		"session", "wake", created.SessionID,
		"--json",
	)
	var wakeResult sessionActionResult
	if err := json.Unmarshal([]byte(exactStartStopJSONPayload(wakeOutput)), &wakeResult); err != nil {
		t.Fatalf("decode exact session wake: %v\n%s", err, wakeOutput)
	}
	if wakeResult.Action != "wake" || wakeResult.SessionID != created.SessionID || wakeResult.State != "wake_requested" {
		t.Fatalf("exact session wake result = %+v, want wake_requested for durable session %q", wakeResult, created.SessionID)
	}

	var (
		woken           sessionpkg.Info
		wakeFinalizedAt time.Time
	)
	if err := waitExactStartStopState(t.Context(), 30*time.Second, func() (bool, error) {
		info, getErr := sessionFrontDoor(backingStore).Get(created.SessionID)
		if getErr != nil {
			return false, getErr
		}
		// Converge on the restarted incarnation itself. The keyed wake commits
		// `active` and only a later status-heal pass makes it `awake`, while the
		// durable row can already read awake before the wake is consumed — so
		// the leg waits for the live projection with the wake marker cleared and
		// a rotated instance token instead of on a bare state string.
		if sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(info)).BaseState != sessionpkg.BaseStateActive ||
			info.PendingCreateClaim || info.WakeRequest != "" ||
			strings.TrimSpace(info.InstanceToken) == "" || info.InstanceToken == started.InstanceToken {
			return false, nil
		}
		woken = info
		wakeFinalizedAt = time.Now().UTC()
		return true, nil
	}); err != nil {
		current, currentErr := sessionFrontDoor(backingStore).Get(created.SessionID)
		t.Fatalf("exact wake did not converge: %v; current=%+v current_err=%v controller stdout=%q stderr=%q",
			err, current, currentErr, controllerStdout.String(), controllerStderr.String())
	}
	if !wakeFinalizedAt.After(wakeCommandAt) {
		t.Fatalf("exact wake finalized at %s before command at %s", wakeFinalizedAt, wakeCommandAt)
	}
	wokenBead, err := backingStore.Get(created.SessionID)
	if err != nil {
		t.Fatalf("read exact wake bead from real bd: %v", err)
	}
	if woken.ID != created.SessionID || woken.SessionName != created.SessionName ||
		wokenBead.Metadata["wake_request"] != "" || wokenBead.Metadata["wake_requested_at"] != "" ||
		strings.TrimSpace(woken.InstanceToken) == "" || woken.InstanceToken == started.InstanceToken {
		t.Fatalf("exact wake durable session/bead = %+v/%+v, want same identity, cleared wake marker, and a new non-empty instance token", woken, wokenBead)
	}
	wakeToken, err := tmuxClient.GetEnvironment(created.SessionName, "GC_INSTANCE_TOKEN")
	if err != nil || strings.TrimSpace(wakeToken) == "" || wakeToken != woken.InstanceToken {
		latest, latestErr := sessionFrontDoor(backingStore).Get(created.SessionID)
		t.Fatalf("exact wake live/durable token = %q/%q err=%v, want same new non-empty token; pre_wake_token=%q latest=%+v latest_err=%v starts=%d controller stderr=%q",
			wakeToken, woken.InstanceToken, err, started.InstanceToken, latest, latestErr,
			strings.Count(controllerStderr.String(), startSuccessLog), controllerStderr.String())
	}
	wakeIDs, err := tmuxClient.ListSessionIDs()
	if err != nil {
		t.Fatalf("read exact wake tmux identity: %v", err)
	}
	wakeTmuxID := strings.TrimSpace(wakeIDs[created.SessionName])
	wakeTmuxServerPID := guard.ServerPID()
	if wakeTmuxServerPID <= 0 {
		t.Fatalf("exact wake tmux server PID = %d, want positive", wakeTmuxServerPID)
	}
	if wakeTmuxID == "" {
		t.Fatalf("exact wake tmux identity for %q is empty: %v", created.SessionName, wakeIDs)
	}
	if wakeTmuxServerPID == startedTmuxServerPID && wakeTmuxID == startedTmuxID {
		t.Fatalf("exact wake tmux incarnation reused server/session identity: server=%d session=%q all=%v",
			wakeTmuxServerPID, wakeTmuxID, wakeIDs)
	}
	// The leg proves that DEMAND — not the anti-entropy census sweep or a
	// detector key — drove this wake to exactly one committed provider start on
	// the woken incarnation. It deliberately does NOT assert which demand key,
	// because the controller discards that: a socket admission folded onto a
	// pending in_process one keeps the in_process source (admit's coalescing
	// rule, pinned by
	// TestSessionStartControllerPreservesInProcessAdmissionAcrossAntiEntropy),
	// and this test's own ApplyPatch fires bead.updated, which admits in_process.
	// Asserting `admission == socket` therefore passed or failed on scheduling
	// alone — it did not prove what it claimed (ga-f7v2ft.142). ga-f7v2ft.125
	// already ruled the source is a hint and the durable row is the authority;
	// sessionStartAdmissionIsDemand is the part of the hint that survives
	// coalescing, and the identity/token/exactly-once assertions carry the rest.
	// The `start_lease` filter below is the same question from the other side:
	// demand membership says no sweep or detector key drove the wake, and the
	// absence of a certified lease says no other family's start was miscounted
	// as this one. Neither subsumes the other, so the leg keeps both.
	var demandWakeCommitRecords []SessionReconcilerTraceRecord
	var demandWakeCommitCandidates []SessionReconcilerTraceRecord
	if err := waitExactStartStopState(t.Context(), 30*time.Second, func() (bool, error) {
		records, readErr := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
			RecordType:  TraceRecordOperation,
			SiteCode:    TraceSiteLifecycleStartCommit,
			SessionName: created.SessionName,
			TraceMode:   TraceModeDetail,
			TraceSource: TraceSourceManual,
		})
		if readErr != nil {
			return false, readErr
		}
		demandWakeCommitCandidates = records
		demandWakeCommitRecords = demandWakeCommitRecords[:0]
		for _, record := range records {
			// `start_lease` names the certified family, and this manual worker
			// row belongs to no wake family — an ordinary keyed start owns it —
			// so a record carrying one was authorized by somebody else's lease,
			// not by this wake (ga-ij8mh ruling 3, run 13).
			if _, hijacked := record.Fields["start_lease"]; hijacked {
				continue
			}
			admissionSource, _ := record.Fields["admission"].(string)
			if record.SessionBeadID == created.SessionID &&
				sessionStartAdmissionIsDemand(sessionStartAdmissionSource(admissionSource)) &&
				record.Fields["session_id"] == created.SessionID &&
				record.Fields["instance_token"] == woken.InstanceToken &&
				record.Fields["effect_applied"] == true {
				demandWakeCommitRecords = append(demandWakeCommitRecords, record)
			}
		}
		if len(demandWakeCommitRecords) > 1 {
			return false, fmt.Errorf("demand wake commit traces = %d, want exactly one", len(demandWakeCommitRecords))
		}
		return len(demandWakeCommitRecords) == 1, nil
	}); err != nil {
		t.Fatalf("exact wake commit trace did not converge: %v; matching=%#v read=%#v woken_token=%q controller stderr=%q",
			err, demandWakeCommitRecords, demandWakeCommitCandidates, woken.InstanceToken, controllerStderr.String())
	}
	if err := waitExactStartStopState(t.Context(), 10*time.Second, func() (bool, error) {
		count := strings.Count(controllerStderr.String(), startSuccessLog)
		if count > 2 {
			return false, fmt.Errorf("successful provider starts after wake = %d, want exactly 2; controller stderr=%q", count, controllerStderr.String())
		}
		return count == 2, nil
	}); err != nil {
		t.Fatalf("successful provider starts after wake did not settle at exactly 2: %v; controller stderr=%q",
			err, controllerStderr.String())
	}
	wakeStartRecords, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
		RecordType:  TraceRecordOperation,
		SiteCode:    TraceSiteLifecycleStartRun,
		SessionName: created.SessionName,
	})
	if err != nil {
		t.Fatalf("read exact wake provider-start trace: %v", err)
	}
	for _, record := range wakeStartRecords {
		if record.OutcomeCode == TraceOutcomeStartEnqueued {
			t.Fatalf("exact wake used legacy async start for %q: %#v", created.SessionName, wakeStartRecords)
		}
	}
	traceStatusOutput := runGC(10*time.Second, "--city", cityPath, "trace", "status", "--json")
	var wakeTraceStatus traceStatusResultJSON
	if err := json.Unmarshal([]byte(exactStartStopJSONPayload(traceStatusOutput)), &wakeTraceStatus); err != nil {
		t.Fatalf("decode trace status after exact wake: %v\n%s", err, traceStatusOutput)
	}
	if !wakeTraceStatus.SessionReconciler.Available || wakeTraceStatus.SessionReconciler.ConfiguredMode != "auto" || wakeTraceStatus.SessionReconciler.EffectiveOwner != "keyed" {
		t.Fatalf("trace status after exact wake = %+v, want available auto/keyed", wakeTraceStatus.SessionReconciler)
	}

	// The keyed reset proves runtime death through the controlled process
	// table, and one unreadable entry there makes the whole absence proof
	// incomplete: the controlled root is a whitelist of fixtures the scanner may
	// see, and a retired PID that the host recycles into an unrelated process is
	// exactly the unreadable entry an exit-conditional prune leaves behind. The
	// wait below therefore retires by registered identity, not by exit, so the
	// recycled case is dropped on the first observation. The registration this
	// leg inherits comes from the configured-dependency leg, restored with that
	// leg at WD.10a.
	wokenPanePID, err := tmuxClient.GetPanePID(created.SessionName)
	if err != nil {
		t.Fatalf("read exact wake pane PID: %v", err)
	}
	registerLivePaneProcess(wokenPanePID)
	runGC(10*time.Second,
		"--city", cityPath,
		"trace", "start",
		"--template", "worker",
		"--for", "2m",
		"--level", string(TraceModeDetail),
	)
	resetAt := time.Now().UTC()
	resetOutput := runGC(10*time.Second,
		"--city", cityPath,
		"session", "reset", created.SessionID, "--json",
	)
	var resetResult sessionActionResult
	if err := json.Unmarshal([]byte(exactStartStopJSONPayload(resetOutput)), &resetResult); err != nil {
		t.Fatalf("decode exact session reset: %v\n%s", err, resetOutput)
	}
	if !resetResult.OK || resetResult.Action != "reset" || resetResult.SessionID != created.SessionID {
		t.Fatalf("exact session reset result = %+v, want successful reset for durable session %q", resetResult, created.SessionID)
	}
	var (
		restarted       sessionpkg.Info
		restartedTmuxID string
	)
	if err := waitExactStartStopState(t.Context(), 30*time.Second, func() (bool, error) {
		info, getErr := sessionFrontDoor(backingStore).Get(created.SessionID)
		if getErr != nil {
			return false, getErr
		}
		if info.Closed || info.ID != created.SessionID || info.SessionName != created.SessionName ||
			sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(info)).BaseState != sessionpkg.BaseStateActive ||
			info.PendingCreateClaim || strings.TrimSpace(info.InstanceToken) == "" || info.InstanceToken == woken.InstanceToken ||
			strings.TrimSpace(info.RestartRequested) != "" || strings.TrimSpace(info.ContinuationResetPending) != "" ||
			strings.TrimSpace(info.ResetCommittedAt) != "" {
			return false, nil
		}
		if gone, retireErr := retireExitedPaneProcess(wokenPanePID); retireErr != nil {
			return false, retireErr
		} else if !gone {
			return false, nil
		}
		ids, listErr := tmuxClient.ListSessionIDs()
		if listErr != nil {
			return false, listErr
		}
		tmuxID := strings.TrimSpace(ids[created.SessionName])
		if tmuxID == "" || tmuxID == wakeTmuxID {
			return false, nil
		}
		liveToken, tokenErr := tmuxClient.GetEnvironment(created.SessionName, "GC_INSTANCE_TOKEN")
		if tokenErr != nil || liveToken != info.InstanceToken {
			return false, tokenErr
		}
		restarted = info
		restartedTmuxID = tmuxID
		return true, nil
	}); err != nil {
		current, currentErr := sessionFrontDoor(backingStore).Get(created.SessionID)
		scanned, scanErr := os.ReadDir(processScanRoot)
		scannedPIDs := make([]string, 0, len(scanned))
		for _, entry := range scanned {
			scannedPIDs = append(scannedPIDs, entry.Name())
		}
		t.Fatalf("exact session reset did not retire the old incarnation and restart the same row within 30s: %v; current=%+v current_err=%v scan_root=%v scan_err=%v controller stdout=%q stderr=%q",
			err, current, currentErr, scannedPIDs, scanErr, controllerStdout.String(), controllerStderr.String())
	}
	// The bead AC phrases this budget as beating the held legacy debounce; per
	// DETECTOR.md sec 4 the journey asserts the equivalent absolute bound.
	if elapsed := time.Since(resetAt); elapsed >= 30*time.Second {
		t.Fatalf("exact session reset latency = %s, want the same row live again within 30s", elapsed)
	}
	if err := removeExitedPaneProcess(wokenPanePID); err != nil {
		t.Fatalf("remove exited woken pane process: %v", err)
	}
	restartedPanePID, err := tmuxClient.GetPanePID(created.SessionName)
	if err != nil {
		t.Fatalf("read exact reset pane PID: %v", err)
	}
	registerLivePaneProcess(restartedPanePID)
	restartedBead, err := backingStore.Get(created.SessionID)
	if err != nil {
		t.Fatalf("read durable exact reset row: %v", err)
	}
	for _, key := range []string{"drain_ack_source", "drain_ack_requester_session_id", "drain_ack_requester_instance_token"} {
		if restartedBead.Metadata[key] != "" {
			t.Fatalf("exact session reset retained legacy drain metadata %s=%q", key, restartedBead.Metadata[key])
		}
	}
	if restartedBead.Metadata["state"] == "drain-ack-stop-pending" {
		t.Fatalf("exact session reset entered drain-ack stop-pending: %+v", restartedBead.Metadata)
	}
	var exactResetStops []SessionReconcilerTraceRecord
	if err := waitExactStartStopState(t.Context(), 10*time.Second, func() (bool, error) {
		records, readErr := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
			RecordType: TraceRecordOperation, SiteCode: TraceSiteLifecycleDrainAdvance,
			SessionName: created.SessionName, TraceMode: TraceModeDetail,
		})
		if readErr != nil {
			return false, readErr
		}
		exactResetStops = exactResetStops[:0]
		for _, record := range records {
			if record.SessionBeadID == created.SessionID && record.ReasonCode == TraceReasonFreshCycle &&
				record.OutcomeCode == TraceOutcomeSuccess && record.OperationID != "" &&
				record.Fields["operation_name"] == "exact_session_reset_stop" {
				exactResetStops = append(exactResetStops, record)
			}
		}
		if len(exactResetStops) > 1 {
			return false, fmt.Errorf("exact reset stop traces = %#v, want exactly one keyed reset stop", exactResetStops)
		}
		return len(exactResetStops) == 1, nil
	}); err != nil {
		t.Fatalf("wait for exact reset stop trace: %v; traces=%#v controller stderr=%q", err, exactResetStops, controllerStderr.String())
	}
	var resetCommitRecords []SessionReconcilerTraceRecord
	var resetCommitCandidates []SessionReconcilerTraceRecord
	if err := waitExactStartStopState(t.Context(), 10*time.Second, func() (bool, error) {
		records, readErr := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
			RecordType: TraceRecordOperation, SiteCode: TraceSiteLifecycleStartCommit,
			SessionName: created.SessionName, TraceMode: TraceModeDetail,
		})
		if readErr != nil {
			return false, readErr
		}
		resetCommitCandidates = records
		resetCommitRecords = resetCommitRecords[:0]
		for _, record := range records {
			// A socket admission folded onto a pending in_process one keeps
			// in_process, so only demand membership survives coalescing here too.
			admissionSource, _ := record.Fields["admission"].(string)
			if record.SessionBeadID == created.SessionID &&
				sessionStartAdmissionIsDemand(sessionStartAdmissionSource(admissionSource)) &&
				record.Fields["session_id"] == created.SessionID &&
				record.Fields["instance_token"] == restarted.InstanceToken &&
				record.Fields["effect_applied"] == true {
				resetCommitRecords = append(resetCommitRecords, record)
			}
		}
		if len(resetCommitRecords) > 1 {
			return false, fmt.Errorf("exact reset start commits = %d, want exactly one", len(resetCommitRecords))
		}
		return len(resetCommitRecords) == 1, nil
	}); err != nil {
		t.Fatalf("exact reset start commit did not converge: %v; matching=%#v read=%#v restarted_token=%q controller stderr=%q",
			err, resetCommitRecords, resetCommitCandidates, restarted.InstanceToken, controllerStderr.String())
	}
	resetStartRecords, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
		RecordType: TraceRecordOperation, SiteCode: TraceSiteLifecycleStartRun, SessionName: created.SessionName,
	})
	if err != nil {
		t.Fatalf("read exact reset provider-start trace: %v", err)
	}
	for _, record := range resetStartRecords {
		if record.OutcomeCode == TraceOutcomeStartEnqueued {
			t.Fatalf("exact session reset used legacy async start for %q: %#v", created.SessionName, resetStartRecords)
		}
	}
	if err := waitExactStartStopState(t.Context(), 10*time.Second, func() (bool, error) {
		count := strings.Count(controllerStderr.String(), startSuccessLog)
		if count > 3 {
			return false, fmt.Errorf("successful provider starts after reset = %d, want exactly 3; controller stderr=%q", count, controllerStderr.String())
		}
		return count == 3, nil
	}); err != nil {
		t.Fatalf("successful provider starts after reset did not settle at exactly 3: %v; controller stderr=%q",
			err, controllerStderr.String())
	}
	if restartedTmuxID == wakeTmuxID {
		t.Fatalf("exact session reset reused the wake tmux incarnation %q", restartedTmuxID)
	}

	// Prove the real event-to-allocation-to-exact-drain path for generic
	// ephemeral sessions. The manual session above deliberately cannot
	// authorize this path; these sessions are controller-created and retain an
	// exact binding to their routed trigger work.
	sourceStore := workflowStoreRefForDir(cityPath, cityPath, loadedCityName(loaded, cityPath), loaded)
	if strings.TrimSpace(sourceStore) == "" {
		t.Fatal("routed-work source store is empty")
	}
	createRoutedWork := func(title string) beads.Bead {
		t.Helper()
		work, createErr := backingStore.Create(beads.Bead{
			Title:  title,
			Type:   "task",
			Status: "open",
			Metadata: map[string]string{
				"gc.routed_to": "worker",
			},
		})
		if createErr != nil {
			t.Fatalf("create %s: %v", title, createErr)
		}
		return work
	}
	emitRoutedWorkCreated := func(work beads.Bead) {
		t.Helper()
		output := runGC(10*time.Second,
			"--city", cityPath,
			"event", "emit", "bead.created",
			"--subject", work.ID,
			"--bead-payload", work.ID,
			"--actor", "bd-hook",
			"--json",
		)
		var emitted eventEmitJSONResult
		if decodeErr := json.Unmarshal([]byte(exactStartStopJSONPayload(output)), &emitted); decodeErr != nil {
			t.Fatalf("decode routed-work event for %s: %v\n%s", work.ID, decodeErr, output)
		}
		if !emitted.HasPayload || !emitted.Submitted {
			t.Fatalf("routed-work event for %s = %+v, want submitted bead payload; output=%q", work.ID, emitted, output)
		}
	}
	type startedPoolSession struct {
		info    sessionpkg.Info
		bead    beads.Bead
		tmuxID  string
		token   string
		panePID string
	}
	waitRoutedPoolStart := func(work beads.Bead) startedPoolSession {
		t.Helper()
		hint := routedWorkPoolAllocationHint{
			WorkID:      work.ID,
			PoolTarget:  "worker",
			SourceStore: sourceStore,
		}
		var last sessionpkg.Info
		var result startedPoolSession
		if waitErr := waitExactStartStopState(t.Context(), 30*time.Second, func() (bool, error) {
			info, found, findErr := findRoutedWorkPoolSession(backingStore, loaded, hint)
			if findErr != nil {
				return false, findErr
			}
			if !found {
				return false, nil
			}
			last = info
			lifecycle := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(info))
			if lifecycle.BaseState != sessionpkg.BaseStateActive || info.PendingCreateClaim ||
				strings.TrimSpace(info.SessionName) == "" || strings.TrimSpace(info.InstanceToken) == "" {
				return false, nil
			}
			stored, getErr := backingStore.Get(info.ID)
			if getErr != nil {
				return false, getErr
			}
			ids, listErr := tmuxClient.ListSessionIDs()
			if listErr != nil {
				return false, listErr
			}
			tmuxID := strings.TrimSpace(ids[info.SessionName])
			if tmuxID == "" {
				return false, nil
			}
			token, tokenErr := tmuxClient.GetEnvironment(info.SessionName, "GC_INSTANCE_TOKEN")
			if tokenErr != nil {
				return false, tokenErr
			}
			if token != info.InstanceToken {
				return false, nil
			}
			panePID, paneErr := tmuxClient.GetPanePID(info.SessionName)
			if paneErr != nil {
				return false, paneErr
			}
			if exactStartStopProcessExited(panePID) {
				return false, nil
			}
			registerLivePaneProcess(panePID)
			result = startedPoolSession{info: info, bead: stored, tmuxID: tmuxID, token: token, panePID: panePID}
			return true, nil
		}); waitErr != nil {
			t.Fatalf("routed-work pool session for %s did not start: %v; current=%+v controller stdout=%q stderr=%q",
				work.ID, waitErr, last, controllerStdout.String(), controllerStderr.String())
		}
		return result
	}
	// Serialized, and the first member is made GENUINELY BUSY, on purpose
	// (ga-f7v2ft.149). This leg asserts that two routed works get two members in
	// two slots, which every allocator path promises only for a member that is
	// actually occupied: legacy declines to re-point a member with assigned work
	// (reusablePoolSessionInfo → sessionBeadHasAssignedWorkInfo) and the keyed
	// reuse path marks the same member busy
	// (routedWorkPoolReuseAssignedWork → authorizeRoutedWorkPoolReuse), so the
	// second work grows instead of taking the first member. That is the promise
	// pinned at unit level by
	// TestRoutedWorkPoolAllocationBusyGenericReuseGrowsWithoutRebinding.
	//
	// The old fixture met neither precondition. It created and emitted both works
	// up front, so the second hint could arrive while the first member was still
	// unbound and idle, and its pool command is `sleep 600`, so the member never
	// claims anything and stays free forever to every "is this member occupied"
	// read there is. A real routed agent claims the bead it was routed; this
	// fixture now does the same, which is what makes the leg exercise the
	// contract rather than race it. The claim is released by the same
	// backingStore.Close(firstRoutedWork.ID) the drain leg below already performs,
	// so the drain-ack arm still sees no assigned work and does not cancel.
	//
	// This does NOT weaken the assertion: the growth expectation below IS the
	// contract, not a workaround for it, and the exactly-one-member-per-routed-bead
	// property proven at :458 is untouched.
	claimRoutedWork := func(work beads.Bead, sessionID string) {
		t.Helper()
		assignee := sessionID
		if err := backingStore.Update(work.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
			t.Fatalf("claim routed work %s for %s: %v", work.ID, sessionID, err)
		}
	}
	firstRoutedWork := createRoutedWork("first exact routed-work drain fixture")
	emitRoutedWorkCreated(firstRoutedWork)
	firstPool := waitRoutedPoolStart(firstRoutedWork)
	claimRoutedWork(firstRoutedWork, firstPool.info.ID)
	secondRoutedWork := createRoutedWork("second exact routed-work drain fixture")
	emitRoutedWorkCreated(secondRoutedWork)
	secondPool := waitRoutedPoolStart(secondRoutedWork)
	if firstPool.bead.Status != "open" || firstPool.bead.Revision == 0 ||
		secondPool.bead.Status != "open" || secondPool.bead.Revision == 0 {
		t.Fatalf("routed-work pool persisted rows = first(status=%q revision=%d) second(status=%q revision=%d), want exact open revisioned rows",
			firstPool.bead.Status, firstPool.bead.Revision, secondPool.bead.Status, secondPool.bead.Revision)
	}
	if firstPool.info.ID == secondPool.info.ID || firstPool.info.SessionName == secondPool.info.SessionName ||
		firstPool.tmuxID == secondPool.tmuxID {
		t.Fatalf("routed work shared a pool runtime: first=%+v second=%+v", firstPool, secondPool)
	}
	poolSlots := map[string]bool{
		firstPool.info.PoolSlot:  true,
		secondPool.info.PoolSlot: true,
	}
	if len(poolSlots) != 2 || !poolSlots["1"] || !poolSlots["2"] {
		t.Fatalf("routed-work pool slots = %q/%q, want distinct slots 1 and 2", firstPool.info.PoolSlot, secondPool.info.PoolSlot)
	}
	if err := backingStore.Close(firstRoutedWork.ID); err != nil {
		t.Fatalf("close first routed trigger %s: %v", firstRoutedWork.ID, err)
	}
	closedTrigger, err := backingStore.Get(firstRoutedWork.ID)
	if err != nil || closedTrigger.Status != "closed" {
		t.Fatalf("first routed trigger after close = %+v err=%v, want closed", closedTrigger, err)
	}
	traceBeforeDrain, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("read trace before routed-work drain acknowledgement: %v", err)
	}
	var traceSeqBeforeDrain uint64
	for _, record := range traceBeforeDrain {
		if record.Seq > traceSeqBeforeDrain {
			traceSeqBeforeDrain = record.Seq
		}
	}
	drainAckCommandAt := time.Now().UTC()
	drainAckOutput := runGCAsSession(10*time.Second, firstPool.info.ID, firstPool.token,
		"--city", cityPath,
		"runtime", "drain-ack", firstPool.info.ID,
		"--json",
	)
	var drainAck runtimeActionJSON
	if err := json.Unmarshal([]byte(exactStartStopJSONPayload(drainAckOutput)), &drainAck); err != nil {
		t.Fatalf("decode exact routed-work drain acknowledgement: %v\n%s", err, drainAckOutput)
	}
	// Target echoes the name the command was addressed by, which is the ID above;
	// Info.Alias is an optional create-time alias this pool fixture never sets.
	if !drainAck.OK || drainAck.Action != "drain-ack" || drainAck.Status != "acknowledged" ||
		drainAck.Session != firstPool.info.SessionName || drainAck.Target != firstPool.info.ID {
		t.Fatalf("exact routed-work drain acknowledgement = %+v, want target %q and runtime %q acknowledged",
			drainAck, firstPool.info.ID, firstPool.info.SessionName)
	}
	for key, want := range map[string]string{
		"GC_DRAIN_ACK":                    "1",
		reconcilerDrainAckSourceKey:       drainAckSourceAgentValue,
		drainAckRequesterSessionIDKey:     firstPool.info.ID,
		drainAckRequesterInstanceTokenKey: firstPool.token,
	} {
		got, getErr := tmuxClient.GetEnvironment(firstPool.info.SessionName, key)
		if getErr != nil || got != want {
			t.Fatalf("exact routed-work drain acknowledgement runtime metadata %s = %q, %v; want %q", key, got, getErr, want)
		}
	}

	var (
		firstPoolFinalInfo sessionpkg.Info
		firstPoolFinalBead beads.Bead
		drainFinalizedAt   time.Time
	)
	// Under first-creator-wins the legacy builder created this member and stamped
	// the demand collector's bare "city" trigger scope. The keyed drain-ack seam
	// canonicalizes that legacy spelling at lease construction (ga-2oboq), so it
	// can own the stop on a legacy-created row: without it, forSourceStore asked
	// agentutil.AgentReachesWorkflowStore -- which requires a "city:" prefix for a
	// rig-less agent -- reported the allocation policy unsupported, and every tick
	// parked with "recovered drain acknowledgement authorization no longer holds
	// before provenance write" forever.
	t.Run("routed_work_drain_finalize", func(t *testing.T) {
		if err := waitExactStartStopState(t.Context(), 15*time.Second, func() (bool, error) {
			if removeErr := removeExitedPaneProcess(firstPool.panePID); removeErr != nil {
				return false, removeErr
			}
			info, getErr := sessionFrontDoor(backingStore).Get(firstPool.info.ID)
			if getErr != nil {
				return false, getErr
			}
			if !info.Closed || info.MetadataState != string(sessionpkg.StateDrained) ||
				info.StateReason != "" || isDrainAckStopPendingInfo(info) {
				return false, nil
			}
			stored, getErr := backingStore.Get(firstPool.info.ID)
			if getErr != nil {
				return false, getErr
			}
			if stored.Status != "closed" {
				return false, nil
			}
			ids, listErr := tmuxClient.ListSessionIDs()
			if listErr != nil {
				return false, listErr
			}
			if strings.TrimSpace(ids[firstPool.info.SessionName]) != "" {
				return false, nil
			}
			firstPoolFinalInfo = info
			firstPoolFinalBead = stored
			drainFinalizedAt = time.Now().UTC()
			return true, nil
		}); err != nil {
			current, currentErr := sessionFrontDoor(backingStore).Get(firstPool.info.ID)
			ids, idsErr := tmuxClient.ListSessionIDs()
			censusCtx, cancelCensus := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancelCensus()
			censusCmd := exec.CommandContext(censusCtx, "tmux", "-L", guard.SocketName(), "list-panes", "-s", "-t", "="+firstPool.info.SessionName,
				"-F", "#{session_id}\\t#{session_name}\\t#{window_id}\\t#{pane_id}\\t#{session_attached}\\t#{pane_in_mode}\\t#{window_linked}")
			censusOutput, censusErr := censusCmd.CombinedOutput()
			postDrainTrace, traceErr := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
			t.Fatalf("exact routed-work drain did not finalize: %v; drain_ack_output=%q current=%+v current_err=%v tmux=%v tmux_err=%v target_census=%q target_census_err=%v post_drain_trace=%#v trace_err=%v controller stdout=%q stderr=%q",
				err, drainAckOutput, current, currentErr, ids, idsErr, censusOutput, censusErr, postDrainTrace, traceErr, controllerStdout.String(), controllerStderr.String())
		}
		if !drainFinalizedAt.After(drainAckCommandAt) ||
			firstPoolFinalBead.Metadata["close_reason"] != sessionpkg.CanonicalCloseReason("drained") ||
			firstPoolFinalBead.Revision == 0 || firstPoolFinalBead.Revision == firstPool.bead.Revision {
			t.Fatalf("exact routed-work drain final state = info %+v bead %+v at %s, want post-command closed/drained with a fresh nonzero revision token",
				firstPoolFinalInfo, firstPoolFinalBead, drainFinalizedAt)
		}
		// Purity, ROW-SCOPED (ga-f7v2ft.112 architect ruling, 2026-08-09). The
		// invariant this leg exists to prove is "no legacy drain EFFECT on the
		// drained row", not "no legacy cycle anywhere in the fleet": a
		// poke-triggered legacy cycle that runs inside the finalize window and
		// touches nothing of this row is background activity, exactly as the
		// sibling-isolation respec (ruling 3) already concluded for background
		// bookkeeping. Assert the effect, tolerate the cycle.
		postDrainTrace, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
		if err != nil {
			t.Fatalf("read trace after routed-work drain acknowledgement: %v", err)
		}
		drainedRows := map[string]bool{
			firstPool.info.ID: true, secondPool.info.ID: true,
			firstPool.info.SessionName: true, secondPool.info.SessionName: true,
		}
		for _, record := range postDrainTrace {
			if record.Seq <= traceSeqBeforeDrain || record.Ts.Before(drainAckCommandAt) || record.Ts.After(drainFinalizedAt) {
				continue
			}
			if !legacyDrainEffectRecord(record) {
				continue
			}
			if !drainedRows[strings.TrimSpace(record.SessionName)] && !drainedRows[strings.TrimSpace(record.SessionBeadID)] {
				continue
			}
			t.Fatalf("legacy applied a drain effect to the drained row during the exact routed-work drain: %+v", record)
		}
	})

	secondPoolAfter, err := backingStore.Get(secondPool.info.ID)
	if err != nil {
		t.Fatalf("read sibling pool session after exact drain: %v", err)
	}
	secondIDsAfter, err := tmuxClient.ListSessionIDs()
	if err != nil {
		t.Fatalf("read sibling tmux identity after exact drain: %v", err)
	}
	secondTokenAfter, err := tmuxClient.GetEnvironment(secondPool.info.SessionName, "GC_INSTANCE_TOKEN")
	if err != nil {
		t.Fatalf("read sibling token after exact drain: %v", err)
	}
	// Sibling isolation means the drain adds no lifecycle or incarnation effect
	// to the sibling -- not that the fleet freezes around it. Two writers
	// legitimately run inside the drain window and are allowlisted below; every
	// other key, INCLUDING instance_token, generation, continuation_epoch,
	// sleep_intent, sleep_reason, held_until, quarantined_until, wake_request,
	// session_key, the trigger binding, all drain_ack_* keys (which must remain
	// absent) and the pool markers, stays byte-equal by construction of the
	// remainder compare.
	if err := siblingPoolIsolationMetadataDiff(secondPool.bead.Metadata, secondPoolAfter.Metadata); err != nil {
		t.Fatalf("sibling pool session metadata changed during exact drain: %v\nbefore=%+v after=%+v", err, secondPool.bead, secondPoolAfter)
	}
	if secondPoolAfter.Status != secondPool.bead.Status ||
		strings.TrimSpace(secondIDsAfter[secondPool.info.SessionName]) != secondPool.tmuxID ||
		secondTokenAfter != secondPool.token {
		t.Fatalf("sibling pool session changed during exact drain: before=%+v after=%+v tmux_before=%q tmux_after=%q token_before=%q token_after=%q",
			secondPool.bead, secondPoolAfter, secondPool.tmuxID,
			strings.TrimSpace(secondIDsAfter[secondPool.info.SessionName]), secondPool.token, secondTokenAfter)
	}

	// Retire the sibling through the same user-visible path, so the later
	// legacy-shadow leg observes a real acknowledged fixture.
	if err := backingStore.Close(secondRoutedWork.ID); err != nil {
		t.Fatalf("close sibling routed trigger %s: %v", secondRoutedWork.ID, err)
	}
	secondDrainAckOutput := runGCAsSession(10*time.Second, secondPool.info.ID, secondPool.token,
		"--city", cityPath,
		"runtime", "drain-ack", secondPool.info.ID,
		"--json",
	)
	var secondDrainAck runtimeActionJSON
	if err := json.Unmarshal([]byte(exactStartStopJSONPayload(secondDrainAckOutput)), &secondDrainAck); err != nil {
		t.Fatalf("decode sibling routed-work drain acknowledgement: %v\n%s", err, secondDrainAckOutput)
	}
	if !secondDrainAck.OK || secondDrainAck.Action != "drain-ack" || secondDrainAck.Status != "acknowledged" ||
		secondDrainAck.Session != secondPool.info.SessionName || secondDrainAck.Target != secondPool.info.ID {
		t.Fatalf("sibling routed-work drain acknowledgement = %+v, want target %q and runtime %q acknowledged",
			secondDrainAck, secondPool.info.ID, secondPool.info.SessionName)
	}
	t.Run("routed_work_sibling_retirement", func(t *testing.T) {
		if err := waitExactStartStopState(t.Context(), 30*time.Second, func() (bool, error) {
			if removeErr := removeExitedPaneProcess(secondPool.panePID); removeErr != nil {
				return false, removeErr
			}
			info, getErr := sessionFrontDoor(backingStore).Get(secondPool.info.ID)
			if getErr != nil {
				return false, getErr
			}
			durablyRetired := info.Closed && info.MetadataState == string(sessionpkg.StateDrained) &&
				!isDrainAckStopPendingInfo(info)
			if !durablyRetired {
				return false, nil
			}
			ids, listErr := tmuxClient.ListSessionIDs()
			if listErr != nil {
				return false, listErr
			}
			tmuxGone := strings.TrimSpace(ids[secondPool.info.SessionName]) == ""
			return durablyRetired && tmuxGone, nil
		}); err != nil {
			current, currentErr := sessionFrontDoor(backingStore).Get(secondPool.info.ID)
			ids, idsErr := tmuxClient.ListSessionIDs()
			t.Fatalf("retire sibling routed-work pool session: %v; drain_ack_output=%q current=%+v current_err=%v tmux=%v tmux_err=%v controller stdout=%q stderr=%q",
				err, secondDrainAckOutput, current, currentErr, ids, idsErr, controllerStdout.String(), controllerStderr.String())
		}
	})

	sample := exactSessionStartStopDurableSample{
		Version:                        "exact-session-start-stop-v3",
		SessionID:                      firstPool.info.ID,
		SchemaStatus:                   schemaStatus,
		StartAdmissionToFinalizationNS: startFinalizedAt.Sub(startAdmittedAt).Nanoseconds(),
		WakeCommandToFinalizationNS:    wakeFinalizedAt.Sub(wakeCommandAt).Nanoseconds(),
		StartPersistedState:            started.MetadataState,
		WakePersistedState:             woken.MetadataState,
	}
	// The stop arm carries a measurement only when the skip-tracked drain
	// finalization above actually ran; an unproven leg must publish nothing.
	if !drainFinalizedAt.IsZero() {
		sample.StopCommandToFinalizationNS = drainFinalizedAt.Sub(drainAckCommandAt).Nanoseconds()
		sample.StopPersistedState = firstPoolFinalInfo.MetadataState
	}
	if sample.WakeCommandToFinalizationNS <= 0 {
		t.Fatalf("wake command-to-finalization latency = %d, want positive", sample.WakeCommandToFinalizationNS)
	}
	wire, err := json.Marshal(sample)
	if err != nil {
		t.Fatalf("marshal exact start-stop sample: %v", err)
	}
	t.Logf("EXACT_START_STOP_DURABLE_SAMPLE %s", wire)

	stopOutput, stopErr := runExactStartStopGC(commandEnv, 30*time.Second, gcBinary, "stop", "--force", cityPath)
	if stopErr != nil {
		t.Fatalf("stop keyed production controller: %v\n%s", stopErr, stopOutput)
	}
	controllerStopped = true
	cancelController()
	select {
	case controllerErr := <-controllerDone:
		controllerWaited = true
		if controllerErr != nil && !strings.Contains(strings.ToLower(controllerErr.Error()), "signal: killed") {
			t.Fatalf("keyed production controller exit: %v", controllerErr)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatalf("keyed production controller did not exit; stdout=%q stderr=%q",
			controllerStdout.String(), controllerStderr.String())
	}
	if count := strings.Count(controllerStderr.String(), startSuccessLog); count != 3 {
		t.Fatalf("successful provider starts after keyed controller exit = %d, want exactly 3; controller stderr=%q",
			count, controllerStderr.String())
	}
	if err := ensureBeadsProvider(cityPath); err != nil {
		t.Fatalf("restart test-owned bead provider for fixture reset: %v", err)
	}
	fixtureIDs, fixtureListErr := tmuxClient.ListSessionIDs()
	if fixtureListErr != nil && !strings.Contains(strings.ToLower(fixtureListErr.Error()), "no server running") {
		t.Fatalf("list isolated tmux sessions before fixture reset: %v", fixtureListErr)
	}
	if strings.TrimSpace(fixtureIDs[created.SessionName]) != "" {
		if err := tmuxClient.KillSession(created.SessionName); err != nil {
			fixtureIDs, fixtureListErr = tmuxClient.ListSessionIDs()
			if fixtureListErr != nil && !strings.Contains(strings.ToLower(fixtureListErr.Error()), "no server running") {
				t.Fatalf("remove original tmux session %q: %v; list error: %v", created.SessionName, err, fixtureListErr)
			}
			if strings.TrimSpace(fixtureIDs[created.SessionName]) != "" {
				t.Fatalf("remove original tmux session %q: %v; all=%v", created.SessionName, err, fixtureIDs)
			}
		}
	}
	fixtureResetPatch := sessionpkg.AcknowledgeDrainPatch(false)
	fixtureResetPatch["wake_request"] = ""
	fixtureResetPatch["wake_requested_at"] = ""
	fixtureResetPatch["pending_create_claim"] = ""
	fixtureResetPatch["pending_create_started_at"] = ""
	fixtureResetPatch["state_reason"] = ""
	fixtureResetPatch["sleep_reason"] = ""
	if err := sessionFrontDoor(backingStore).ApplyPatch(created.SessionID, fixtureResetPatch); err != nil {
		t.Fatalf("reset original session fixture to drained: %v", err)
	}
	// An open manual session remains desired under the legacy reconciler even
	// after a drain. The wake journey is complete, so retire only this fixture
	// before measuring the unrelated legacy-shadow session.
	if err := backingStore.Close(created.SessionID); err != nil {
		t.Fatalf("close original session fixture after wake journey: %v", err)
	}
	fixtureInfo, err := sessionFrontDoor(backingStore).Get(created.SessionID)
	if err != nil {
		t.Fatalf("read original session after fixture reset: %v", err)
	}
	fixtureBead, err := backingStore.Get(created.SessionID)
	if err != nil {
		t.Fatalf("read original bead after fixture reset: %v", err)
	}
	fixtureLifecycle := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(fixtureInfo))
	fixtureIDs, fixtureListErr = tmuxClient.ListSessionIDs()
	if fixtureListErr != nil && !strings.Contains(strings.ToLower(fixtureListErr.Error()), "no server running") {
		t.Fatalf("list isolated tmux sessions after fixture reset: %v", fixtureListErr)
	}
	if !fixtureInfo.Closed || fixtureBead.Status != "closed" ||
		fixtureBead.Metadata["state"] != string(sessionpkg.StateDrained) ||
		fixtureLifecycle.BaseState != sessionpkg.BaseStateClosed ||
		!fixtureLifecycle.Terminal ||
		fixtureBead.Metadata["wake_request"] != "" ||
		fixtureBead.Metadata["wake_requested_at"] != "" ||
		fixtureBead.Metadata["pending_create_claim"] != "" ||
		fixtureBead.Metadata["pending_create_started_at"] != "" ||
		fixtureBead.Metadata["state_reason"] != "" ||
		fixtureBead.Metadata["sleep_reason"] != "" ||
		strings.TrimSpace(fixtureIDs[created.SessionName]) != "" {
		t.Fatalf("original fixture after reset = session %#v bead %#v lifecycle %#v tmux=%v, want terminal closed with stored drained state, cleared markers, and no runtime",
			fixtureInfo, fixtureBead, fixtureLifecycle, fixtureIDs)
	}

	cityConfigPath := filepath.Join(cityPath, "city.toml")
	legacyConfig, err := os.ReadFile(cityConfigPath)
	if err != nil {
		t.Fatalf("read initialized config for legacy shadow: %v", err)
	}
	const (
		autoSessionReconciler   = `session_reconciler = "auto"`
		legacySessionReconciler = `session_reconciler = "off"`
	)
	if count := bytes.Count(legacyConfig, []byte(autoSessionReconciler)); count != 1 {
		t.Fatalf("initialized session_reconciler auto settings = %d, want exactly one", count)
	}
	legacyConfig = bytes.Replace(legacyConfig, []byte(autoSessionReconciler), []byte(legacySessionReconciler), 1)
	if err := fsys.WriteFileAtomic(
		fsys.OSFS{},
		cityConfigPath,
		legacyConfig,
		0o644,
	); err != nil {
		t.Fatalf("write legacy shadow config: %v", err)
	}
	beforeLegacyIDs, err := tmuxClient.ListSessionIDs()
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "no server running") {
		t.Fatalf("list isolated tmux sessions before legacy shadow: %v", err)
	}
	if originalID := strings.TrimSpace(beforeLegacyIDs[created.SessionName]); originalID != "" {
		t.Fatalf("original session resurrected before legacy shadow: %q; all=%v", originalID, beforeLegacyIDs)
	}

	legacyControllerCtx, cancelLegacyController := context.WithCancel(t.Context())
	var legacyControllerStdout, legacyControllerStderr synchronizedBuffer
	legacyControllerCmd := newExactStartStopGCCommand(
		legacyControllerCtx,
		commandEnv,
		gcBinary,
		"start",
		"--foreground",
		"--no-strict",
		cityPath,
	)
	legacyControllerCmd.Stdout = &legacyControllerStdout
	legacyControllerCmd.Stderr = &legacyControllerStderr
	if err := legacyControllerCmd.Start(); err != nil {
		t.Fatalf("start legacy shadow controller: %v", err)
	}
	legacyControllerDone := make(chan error, 1)
	go func() {
		legacyControllerDone <- legacyControllerCmd.Wait()
	}()
	t.Cleanup(func() {
		stopOutput, stopErr := runExactStartStopGC(commandEnv, 30*time.Second, gcBinary, "stop", "--force", cityPath)
		if stopErr != nil {
			t.Errorf("stop legacy shadow controller: %v\n%s", stopErr, stopOutput)
		}
		cancelLegacyController()
		select {
		case <-legacyControllerDone:
		case <-time.After(testutil.ExecRaceTimeout):
			t.Errorf("legacy shadow controller did not exit; stdout=%q stderr=%q",
				legacyControllerStdout.String(), legacyControllerStderr.String())
		}
	})
	waitForControllerAvailable(t, cityPath)

	if err := waitExactStartStopState(t.Context(), 15*time.Second, func() (bool, error) {
		out, runErr := runExactStartStopGC(
			commandEnv,
			10*time.Second,
			gcBinary,
			"--city", cityPath,
			"trace", "status", "--json",
		)
		traceOutput = out
		if runErr != nil {
			return false, runErr
		}
		var status traceStatusResultJSON
		if decodeErr := json.Unmarshal([]byte(exactStartStopJSONPayload(out)), &status); decodeErr != nil {
			return false, decodeErr
		}
		traceStatus = status
		return status.SessionReconciler.Available &&
			status.SessionReconciler.ConfiguredMode == "off" &&
			status.SessionReconciler.EffectiveOwner == "legacy", nil
	}); err != nil {
		t.Fatalf("production session reconciler did not become off/legacy: %v; status=%+v output=%q controller stdout=%q stderr=%q",
			err, traceStatus.SessionReconciler, traceOutput, legacyControllerStdout.String(), legacyControllerStderr.String())
	}
	runGC(10*time.Second,
		"--city", cityPath,
		"trace", "start",
		"--template", "worker",
		"--for", "2m",
		"--level", string(TraceModeDetail),
	)

	shadowCreatedOutput := runGC(30*time.Second,
		"--city", cityPath,
		"session", "new", "worker",
		"--no-attach",
		"--json",
	)
	var shadowCreated sessionNewJSON
	if err := json.Unmarshal([]byte(exactStartStopJSONPayload(shadowCreatedOutput)), &shadowCreated); err != nil {
		t.Fatalf("decode legacy shadow session creation: %v\n%s", err, shadowCreatedOutput)
	}
	if shadowCreated.SessionID == "" || shadowCreated.SessionName == "" || !shadowCreated.DeferredStart {
		t.Fatalf("legacy shadow session creation = %+v, want deferred ID and tmux name", shadowCreated)
	}

	// lastShadowInfo/lastShadowBead are assigned on every poll: shadowStarted is
	// only set on the success path, so reporting it on timeout prints a zero Info.
	var shadowStarted, lastShadowInfo sessionpkg.Info
	var lastShadowBead beads.Bead
	if err := waitExactStartStopState(t.Context(), 45*time.Second, func() (bool, error) {
		info, getErr := sessionFrontDoor(backingStore).Get(shadowCreated.SessionID)
		if getErr != nil {
			return false, getErr
		}
		lastShadowInfo = info
		if bead, beadErr := backingStore.Get(shadowCreated.SessionID); beadErr == nil {
			lastShadowBead = bead
		}
		view := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(info))
		if view.BaseState != sessionpkg.BaseStateActive || info.PendingCreateClaim {
			return false, nil
		}
		shadowStarted = info
		return true, nil
	}); err != nil {
		t.Fatalf("legacy shadow start did not converge: %v; current=%+v bead status=%q metadata=%#v controller stdout=%q stderr=%q",
			err, lastShadowInfo, lastShadowBead.Status, lastShadowBead.Metadata,
			legacyControllerStdout.String(), legacyControllerStderr.String())
	}
	shadowToken, err := tmuxClient.GetEnvironment(shadowCreated.SessionName, "GC_INSTANCE_TOKEN")
	if err != nil {
		t.Fatalf("read legacy shadow tmux instance token: %v; controller stdout=%q stderr=%q",
			err, legacyControllerStdout.String(), legacyControllerStderr.String())
	}
	if strings.TrimSpace(shadowToken) == "" || shadowToken != shadowStarted.InstanceToken {
		t.Fatalf("legacy shadow live/durable instance tokens = %q/%q, want the same non-empty token",
			shadowToken, shadowStarted.InstanceToken)
	}
	afterLegacyStartIDs, err := tmuxClient.ListSessionIDs()
	if err != nil {
		t.Fatalf("list isolated tmux sessions after legacy shadow start: %v", err)
	}
	if originalID := strings.TrimSpace(afterLegacyStartIDs[created.SessionName]); originalID != "" {
		t.Fatalf("legacy shadow start resurrected original session: %q; all=%v", originalID, afterLegacyStartIDs)
	}
	originalAfterLegacyStart, err := sessionFrontDoor(backingStore).Get(created.SessionID)
	if err != nil {
		t.Fatalf("read original session after legacy shadow start: %v", err)
	}
	originalBeadAfterLegacyStart, err := backingStore.Get(created.SessionID)
	if err != nil {
		t.Fatalf("read original bead after legacy shadow start: %v", err)
	}
	originalLifecycleAfterLegacyStart := sessionpkg.ProjectLifecycle(
		sessionpkg.LifecycleInputFromInfo(originalAfterLegacyStart),
	)
	if !originalAfterLegacyStart.Closed || originalBeadAfterLegacyStart.Status != "closed" ||
		originalLifecycleAfterLegacyStart.BaseState != sessionpkg.BaseStateClosed ||
		!originalLifecycleAfterLegacyStart.Terminal ||
		originalAfterLegacyStart.MetadataState != string(sessionpkg.StateDrained) ||
		originalBeadAfterLegacyStart.Metadata["wake_request"] != "" ||
		originalBeadAfterLegacyStart.Metadata["wake_requested_at"] != "" ||
		originalBeadAfterLegacyStart.Metadata["sleep_reason"] != "" {
		t.Fatalf("original session/bead after legacy shadow start = %#v/%#v (lifecycle=%#v), want terminal closed with stored drained state and cleared wake markers",
			originalAfterLegacyStart, originalBeadAfterLegacyStart, originalLifecycleAfterLegacyStart)
	}

	// Witness COUNT is not evidence here. A legacy start is dispatched
	// asynchronously (seconds before tmux is live), and every reconcile pass that
	// runs inside that window re-observes the same still-pending session and emits
	// its own shadow record — so counting witnesses measures reconcile cadence,
	// not behaviour. Nor can tick identity separate the two: each shadow
	// evaluation opens its own trace cycle, so distinct tick IDs are guaranteed
	// whatever produced the record. What the shadow owes is agreement and
	// inertness on EVERY pass that observed it; uniqueness of the EXECUTION is
	// anchored below on the single start_enqueued operation that owns the
	// dispatch boundary (ga-l7k4q).
	var shadowWitnesses []SessionReconcilerTraceRecord
	if err := waitExactStartStopState(t.Context(), 15*time.Second, func() (bool, error) {
		records, readErr := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
			SiteCode:    TraceSiteLifecycleStartSelectionShadow,
			Template:    "worker",
			TraceMode:   TraceModeDetail,
			TraceSource: TraceSourceManual,
		})
		if readErr != nil {
			return false, readErr
		}
		witnesses := make([]SessionReconcilerTraceRecord, 0, len(records))
		for _, record := range records {
			if record.Fields["session_id"] != shadowCreated.SessionID {
				continue
			}
			witnesses = append(witnesses, record)
		}
		shadowWitnesses = witnesses
		return len(witnesses) > 0, nil
	}); err != nil {
		t.Fatalf("legacy START-shadow witness did not converge: %v; witnesses=%d%s\ncontroller stdout=%q stderr=%q",
			err, len(shadowWitnesses), formatExactStartStopShadowWitnesses(shadowWitnesses),
			legacyControllerStdout.String(), legacyControllerStderr.String())
	}
	for i, witness := range shadowWitnesses {
		if witness.RecordType != TraceRecordOperation ||
			witness.OutcomeCode != TraceOutcomeNoChange ||
			witness.Fields["admitted_template"] != "worker" ||
			witness.Fields["admitted_source"] != string(TraceSourceManual) ||
			witness.Fields["comparison_outcome"] != string(sessionLifecycleStartSelectionComparisonMatched) ||
			witness.Fields["comparison_reason"] != string(sessionLifecycleStartSelectionComparisonReasonEquivalent) ||
			witness.Fields["effect_applied"] != false ||
			!benignExactStartStopShadowSelection(witness) {
			t.Fatalf("legacy START-shadow witness %d/%d = %#v, want matched legacy-owned no-effect evidence; all witnesses:%s",
				i+1, len(shadowWitnesses), witness, formatExactStartStopShadowWitnesses(shadowWitnesses))
		}
	}
	startRecords, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
		RecordType:  TraceRecordOperation,
		SiteCode:    TraceSiteLifecycleStartRun,
		Template:    "worker",
		SessionName: shadowCreated.SessionName,
	})
	if err != nil {
		t.Fatalf("read legacy provider-start trace: %v", err)
	}
	// Legacy starts execute asynchronously: this operation record owns the
	// dispatch boundary and therefore reports start_enqueued. Exactly one
	// enqueue plus the durable-active and exact-tmux-identity assertions above
	// proves one successful provider execution without depending on stderr text.
	// This is also the uniqueness anchor for the shadow witnesses above: however
	// many passes observed the pending start, only one start was ever dispatched.
	if len(startRecords) != 1 || startRecords[0].OutcomeCode != TraceOutcomeStartEnqueued {
		t.Fatalf("legacy provider-start trace = %#v, want exactly one async start enqueue", startRecords)
	}
}

// benignExactStartStopShadowSelection pins the selection shapes one legacy start
// may legitimately produce across the passes that observe it. The pass that
// selects the start reports prepare/ready and carries legacy_selected; every
// later pass inside the asynchronous start window parks on the same
// pending-create claim legacy parks on; once the runtime is up the session reads
// as already running, or as no longer owing a wake. Each of those is a
// not-selected agreement, and none of them can hide a missing start — the
// durable-active, exact-tmux-identity and single-start_enqueued assertions
// already prove the start happened. Anything else is a real signal: a degraded
// park (quarantined, failed create, open circuit, unavailable provider), a
// suppressed or terminal read, an incomparable plan, or a legacy_selected flag
// that contradicts the plan.
func benignExactStartStopShadowSelection(witness SessionReconcilerTraceRecord) bool {
	outcome, _ := witness.Fields["candidate_outcome"].(string)
	reason, _ := witness.Fields["candidate_reason"].(string)
	legacySelected, _ := witness.Fields["legacy_selected"].(bool)
	switch {
	case outcome == "prepare" && reason == string(sessionLifecycleStartSelectionReasonReady):
		return legacySelected
	case outcome == "park" && reason == string(sessionLifecycleStartSelectionReasonStartInFlight),
		outcome == "noop" && reason == string(sessionLifecycleStartSelectionReasonAlreadyRunning),
		outcome == "noop" && reason == string(sessionLifecycleStartSelectionReasonNotNeeded):
		return !legacySelected
	default:
		return false
	}
}

// formatExactStartStopShadowWitnesses renders every START-shadow witness with
// the identity a failure needs to tell one reconcile pass from several. TickID
// and RecordID are always distinct because each shadow evaluation opens its own
// trace cycle, so the discriminator is observed_at: a single pass produces
// exactly one observation per session, so two records sharing an observation
// stamp mean the same observation was recorded twice. The stamp is reconstructed
// from the record timestamp minus the observed-to-completed span the emitter
// carries; the raw span and the queue/planning latencies stay in the fields dump
// so the enqueue provenance is readable too.
func formatExactStartStopShadowWitnesses(witnesses []SessionReconcilerTraceRecord) string {
	if len(witnesses) == 0 {
		return "\n  (no witnesses)"
	}
	lines := make([]string, 0, len(witnesses))
	for i, witness := range witnesses {
		observedAt := "unknown"
		if ns, ok := exactStartStopTraceFieldInt64(witness.Fields["observed_to_completed_ns"]); ok {
			observedAt = witness.Ts.Add(-time.Duration(ns)).UTC().Format(time.RFC3339Nano)
		}
		lines = append(lines, fmt.Sprintf(
			"  [%d] tick_id=%s record_id=%s seq=%d record_type=%s outcome=%s ts=%s observed_at=%s duration_ms=%d\n      fields=%v",
			i+1, witness.TickID, witness.RecordID, witness.Seq, witness.RecordType, witness.OutcomeCode,
			witness.Ts.UTC().Format(time.RFC3339Nano), observedAt, witness.DurationMS, witness.Fields))
	}
	return "\n" + strings.Join(lines, "\n")
}

// exactStartStopTraceFieldInt64 reads a numeric trace field. Records arrive
// through JSON, so every number lands as float64.
func exactStartStopTraceFieldInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
}

func replaceEnvEntry(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, prefix+value)
}

func newExactStartStopGCCommand(ctx context.Context, env []string, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	return cmd
}

func runExactStartStopGC(env []string, timeout time.Duration, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := newExactStartStopGCCommand(ctx, env, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String() + stderr.String()), err
}

func waitExactStartStopState(ctx context.Context, timeout time.Duration, condition func() (bool, error)) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		ok, err := condition()
		if err != nil {
			lastErr = err
		} else if ok {
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("%w (last observation error: %v)", deadline.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

// requirePinnedJourneyBD refuses a GC_TEST_BD_BIN that is not the bd build
// deps.env pins this journey to (BD_CURRENT_REF, which CI clones and builds for
// the equipped cmd/gc integration shard). Any other bd on the host is a
// different beads era, and the two ways that goes wrong both surface hundreds
// of lines away from their cause: a post-BD_CURRENT_REF bd carries the
// legacy-upgrade guard, which reads gc's managed-Dolt city layout
// (.beads/dolt beside dolt_mode=server, no .local_version witness on a
// first-ever init) as a legacy Dolt server workspace and aborts `gc init`;
// and it initializes a schema past the v59 this journey speaks. Name the wrong
// binary here rather than let it masquerade as a product regression
// (ga-f7v2ft.145).
// It identifies the binary from its embedded Go build info rather than by
// running it: the stamp is what CI's `go build` of that checkout produced, and
// reading it costs no subprocess and cannot touch whatever store the test
// happens to be running inside.
func requirePinnedJourneyBD(t *testing.T, bdPath string) {
	t.Helper()
	ref := depsEnvValue(t, "BD_CURRENT_REF")
	if len(ref) < 12 {
		t.Fatalf("deps.env BD_CURRENT_REF = %q, want a beads commit SHA", ref)
	}
	info, err := buildinfo.ReadFile(bdPath)
	if err != nil {
		t.Fatalf("GC_TEST_BD_BIN %q: read Go build info: %v", bdPath, err)
	}
	// Builds record the checkout in different places: CI's plain `go build` of
	// a git clone stamps vcs.revision and a module pseudo-version, while a
	// release build passes the commit through -ldflags and leaves the module
	// at (devel). Any of them identifies the pin, so search the whole stamp
	// and report the parts an operator can act on.
	stamp := info.Main.Version
	identity := []string{"module " + info.Main.Version}
	for _, setting := range info.Settings {
		stamp += " " + setting.Value
		if setting.Key == "vcs.revision" || setting.Key == "-ldflags" {
			identity = append(identity, setting.Key+"="+setting.Value)
		}
	}
	if !strings.Contains(stamp, ref[:12]) {
		t.Fatalf("GC_TEST_BD_BIN %q is stamped [%s], want the deps.env-pinned bd built from BD_CURRENT_REF %s.\n"+
			"Build it the way CI does: git clone https://github.com/gastownhall/beads && git -C <src> checkout --detach %s && go -C <src> build -tags gms_pure_go -o <bin> ./cmd/bd",
			bdPath, strings.Join(identity, "; "), ref, ref)
	}
}

// depsEnvValue reads a single KEY=value line out of the repo-root deps.env.
func depsEnvValue(t *testing.T, key string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "deps.env"))
	if err != nil {
		t.Fatalf("read repo-root deps.env: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.TrimSpace(value)
		}
	}
	t.Fatalf("deps.env is missing %s", key)
	return ""
}

func exactStartBDCommand(t *testing.T, cityPath string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	runner := beads.ExecCommandRunnerWithEnvContext(ctx, map[string]string{
		"BEADS_DIR": filepath.Join(cityPath, ".beads"),
	})
	out, err := runner(cityPath, "bd", args...)
	if err != nil {
		t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func exactStartStopJSONPayload(raw string) string {
	data := []byte(raw)
	for i, b := range data {
		if b != '{' && b != '[' {
			continue
		}
		candidate := bytes.TrimSpace(data[i:])
		if json.Valid(candidate) {
			return string(candidate)
		}
	}
	return raw
}

// exactStartStopProcessExited observes the real procfs process identified by
// tmux before drain admission. A missing process or a zombie has exited; every
// other state remains live, so durable finalization cannot be accepted first.
func exactStartStopProcessExited(pid string) bool {
	data, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 || len(data) <= end+2 {
		return false
	}
	state := data[end+2]
	return state == 'Z' || state == 'X'
}
