//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

const sessionReconcilerColdDisableTimeout = 60 * time.Second

var sessionReconcilerColdDisableAssignment = regexp.MustCompile(`(?m)^([ \t]*session_reconciler[ \t]*=[ \t]*)"[^"]*"([ \t]*(?:#.*)?)$`)

type sessionReconcilerColdDisableStatus struct {
	ControllerRunning bool       `json:"controller_running"`
	HeadSeq           uint64     `json:"head_seq"`
	ActiveArms        []struct{} `json:"active_arms"`
	SessionReconciler struct {
		SchemaVersion  string `json:"schema_version"`
		Available      bool   `json:"available"`
		ConfiguredMode string `json:"configured_mode"`
		EffectiveOwner string `json:"effective_owner"`
		PendingKeys    int    `json:"pending_keys"`
		AuditPending   bool   `json:"audit_pending"`
	} `json:"session_reconciler"`
}

func TestSessionReconcilerColdDisableExactBinaryJourney(t *testing.T) {
	if usingSubprocess() {
		t.Skip("cold-disable journey requires an isolated named tmux server")
	}

	cityDir := setupReconcilerCityWithDaemon(t, `session_reconciler = "auto"

[[agent]]
name = "worker"
start_command = "sleep 3600"
max_active_sessions = -1
`, `patrol_interval = "10s"
`, "")
	env := commandEnvForDir(cityDir, false)
	if out, err := runGCWithEnv(env, "", "supervisor", "stop", "--wait"); err != nil {
		t.Fatalf("stop bootstrap supervisor: %v\n%s", err, out)
	}
	configPath := filepath.Join(cityDir, "city.toml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read bootstrap city config: %v", err)
	}
	if count := bytes.Count(config, []byte("max_active_sessions = -1\n")); count > 1 {
		t.Fatalf("bootstrap-only pool markers = %d, want at most 1", count)
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, configPath, bytes.Replace(config, []byte("max_active_sessions = -1\n"), nil, 1), 0o644); err != nil {
		t.Fatalf("remove bootstrap-only pool marker: %v", err)
	}
	env = replaceEnv(env, "GC_SUPERVISOR_PRESERVE_SESSIONS_ON_SIGNAL", "1")
	registerCityCommandEnv(cityDir, env)
	gcHome := parseEnvList(env)["GC_HOME"]
	startIsolatedSupervisor(t, env, gcHome)
	waitForControllerReady(t, cityDir, 15*time.Second)

	initial := sessionReconcilerColdDisableWaitForMode(t, cityDir, "auto", "keyed")
	if out, err := gc(cityDir, "trace", "start", "--template", "worker", "--for", "2m", "--level", "detail"); err != nil {
		t.Fatalf("arm worker trace: %v\n%s", err, out)
	}

	before := sessionReconcilerColdDisableNewSession(t, cityDir, "before-cutoff")
	waitForAgentRunning(t, cityDir, "before-cutoff", 45*time.Second)
	beforeIdentity := sessionWaitDependencyShadowJourneyTmuxIdentity(t, cityDir, before.SessionName)
	sessionReconcilerColdDisableEmitSessionUpdate(t, cityDir, before.SessionID)
	beforeTrace, _, err := sessionLifecycleStatusShadowJourneyWaitForWitness(
		t.Context(),
		cityDir,
		before.SessionID,
		initial.HeadSeq,
		15*time.Second,
		"enabled lifecycle-status",
		sessionLifecycleStatusShadowJourneyStatusWitnesses,
	)
	if err != nil {
		t.Fatalf("initial enabled lifecycle-status comparison did not converge: %v", err)
	}
	beforeShadows := sessionLifecycleStatusShadowJourneyStatusWitnesses(beforeTrace, before.SessionID, initial.HeadSeq)
	if len(beforeShadows) != 1 || !sessionReconcilerColdDisableIsEnabledWitness(beforeShadows[0]) || beforeShadows[0].ControllerInstanceID == "" {
		t.Fatalf("initial enabled lifecycle-status records = %+v, want exactly one converged no-effect record with controller generation", beforeShadows)
	}
	retiredGeneration := beforeShadows[0].ControllerInstanceID
	sessionReconcilerColdDisableWaitForMode(t, cityDir, "auto", "keyed")

	if out, err := gc(cityDir, "trace", "stop", "--template", "worker"); err != nil {
		t.Fatalf("disarm worker trace: %v\n%s", err, out)
	}
	disarmed := sessionReconcilerColdDisableWaitForMode(t, cityDir, "auto", "keyed")
	if len(disarmed.ActiveArms) != 0 {
		t.Fatalf("trace arms after disarm = %+v, want none", disarmed.ActiveArms)
	}
	sessionReconcilerColdDisableInstallModeConfig(t, cityDir, env, before.SessionName, "auto", "keyed", "off")

	oldPID := sessionReconcilerColdDisableSupervisorPID(t, env)
	if err := syscall.Kill(oldPID, syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM test-owned supervisor %d: %v", oldPID, err)
	}
	sessionReconcilerColdDisableWaitPIDGone(t, oldPID)
	retiredShadowCount := sessionReconcilerColdDisableShadowCount(t, cityDir, retiredGeneration)
	if got := sessionWaitDependencyShadowJourneyTmuxIdentity(t, cityDir, before.SessionName); got != beforeIdentity {
		t.Fatalf("tmux identity changed during preserve shutdown: before=%q after=%q", beforeIdentity, got)
	}

	offConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read installed off config: %v", err)
	}
	candidate := filepath.Join(cityDir, ".city.toml.cold-disable")
	releasePort := sessionReconcilerColdDisableHoldSupervisorPort(t, gcHome)
	if out, err := runGCWithEnv(env, "", "supervisor", "start"); err == nil {
		t.Fatalf("supervisor start with foreign listener succeeded:\n%s", out)
	} else if !strings.Contains(out, filepath.Join(gcHome, "supervisor.log")) {
		t.Fatalf("failed supervisor start did not point to its log:\n%s", out)
	}
	if got, err := os.ReadFile(configPath); err != nil {
		t.Fatalf("read off config after failed successor: %v", err)
	} else if !bytes.Equal(got, offConfig) {
		t.Fatalf("live off config changed after failed successor: got %q want %q", got, offConfig)
	}
	if out, err := gc(cityDir, "config", "show", "--validate", "--root-file", configPath); err != nil {
		t.Fatalf("validate preserved off config after failed successor: %v\n%s", err, out)
	}
	if _, err := os.Lstat(candidate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cold-disable candidate after failed successor: err=%v, want absent", err)
	}
	sessionReconcilerColdDisableWaitPIDGone(t, oldPID)
	if got := controllerAlive(cityDir); got != 0 {
		t.Fatalf("controller control socket PID after failed successor = %d, want none", got)
	}
	if got := sessionReconcilerColdDisableSupervisorControlSocketPID(t, gcHome); got != 0 {
		t.Fatalf("supervisor control socket PID after failed successor = %d, want none", got)
	}
	sessionReconcilerColdDisableAssertOfflineStatus(t, cityDir)
	if got := sessionWaitDependencyShadowJourneyTmuxIdentity(t, cityDir, before.SessionName); got != beforeIdentity {
		t.Fatalf("tmux identity changed after failed successor: before=%q after=%q", beforeIdentity, got)
	}

	releasePort()
	if out, err := runGCWithEnv(env, "", "supervisor", "start"); err != nil {
		t.Fatalf("retry supervisor start after port release: %v\n%s", err, out)
	}
	waitForControllerReady(t, cityDir, 15*time.Second)
	sessionReconcilerColdDisableReadStatus(t, cityDir)
	if got := sessionWaitDependencyShadowJourneyTmuxIdentity(t, cityDir, before.SessionName); got != beforeIdentity {
		t.Fatalf("tmux identity changed across cold disable: before=%q after=%q", beforeIdentity, got)
	}
	offGeneration := sessionReconcilerColdDisableProveOffSuccessor(t, cityDir, retiredGeneration, retiredShadowCount)

	// Re-enabling must start a fresh controller generation; never let an old
	// shadow observer resume after the cold-disable boundary.
	sessionReconcilerColdDisableReenableAndProveFreshGeneration(t, cityDir, env, gcHome, before, retiredGeneration, offGeneration, retiredShadowCount)
}

func sessionReconcilerColdDisableProveOffSuccessor(t *testing.T, cityDir, retiredGeneration string, retiredShadowCount int) string {
	t.Helper()
	if out, err := gc(cityDir, "trace", "start", "--template", "worker", "--for", "2m", "--level", "detail"); err != nil {
		t.Fatalf("arm worker trace under off successor: %v\n%s", err, out)
	}
	status := sessionReconcilerColdDisableReadStatus(t, cityDir)
	duringOff := sessionReconcilerColdDisableNewSession(t, cityDir, "during-off")
	waitForAgentRunning(t, cityDir, "during-off", 45*time.Second)
	if bead := sessionLifecycleStatusShadowJourneyReadBead(t, cityDir, duringOff.SessionID); bead.Metadata["state"] != "active" && bead.Metadata["state"] != "awake" {
		t.Fatalf("during-off durable session state = %q, want active/awake", bead.Metadata["state"])
	}
	if identity := sessionWaitDependencyShadowJourneyTmuxIdentity(t, cityDir, duringOff.SessionName); identity == "" {
		t.Fatal("during-off runtime identity is empty")
	}
	trace, _, err := sessionLifecycleStatusShadowJourneyWaitForWitness(t.Context(), cityDir, duringOff.SessionID, status.HeadSeq, 15*time.Second, "off-successor start-selection", sessionReconcilerColdDisableFirstShadowRecord)
	if err != nil {
		t.Fatalf("off-successor START-shadow comparison did not converge: %v", err)
	}
	shadows := sessionReconcilerColdDisableShadowRecords(trace, duringOff.SessionID, status.HeadSeq)
	if len(shadows) == 0 || shadows[0].ControllerInstanceID == "" || shadows[0].ControllerInstanceID == retiredGeneration {
		t.Fatalf("off-successor START-shadow records = %+v, want a successor generation distinct from %q", shadows, retiredGeneration)
	}
	offGeneration := shadows[0].ControllerInstanceID
	for _, shadow := range shadows[1:] {
		if shadow.ControllerInstanceID != offGeneration {
			t.Fatalf("off-successor START-shadow generations = %+v, want only %q", shadows, offGeneration)
		}
	}
	if got := sessionReconcilerColdDisableShadowCount(t, cityDir, retiredGeneration); got != retiredShadowCount {
		t.Fatalf("retired controller generation %q advanced shadow metrics after off-successor stimulus: before=%d after=%d", retiredGeneration, retiredShadowCount, got)
	}
	if out, err := gc(cityDir, "trace", "stop", "--template", "worker"); err != nil {
		t.Fatalf("disarm worker trace under off successor: %v\n%s", err, out)
	}
	if disarmed := sessionReconcilerColdDisableReadStatus(t, cityDir); len(disarmed.ActiveArms) != 0 {
		t.Fatalf("trace arms after off-successor disarm = %+v, want none", disarmed.ActiveArms)
	}
	return offGeneration
}

func sessionReconcilerColdDisableReenableAndProveFreshGeneration(t *testing.T, cityDir string, env []string, gcHome string, before sessionLifecycleStatusShadowJourneyNew, retiredGeneration, offGeneration string, retiredShadowCount int) {
	t.Helper()
	sessionReconcilerColdDisableInstallModeConfig(t, cityDir, env, before.SessionName, "off", "legacy", "auto")
	oldPID := sessionReconcilerColdDisableSupervisorPID(t, env)
	if err := syscall.Kill(oldPID, syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM test-owned off supervisor %d for re-enable: %v", oldPID, err)
	}
	sessionReconcilerColdDisableWaitPIDGone(t, oldPID)
	startIsolatedSupervisor(t, env, gcHome)
	sessionReconcilerColdDisableWaitForMode(t, cityDir, "auto", "keyed")

	if out, err := gc(cityDir, "trace", "start", "--template", "worker", "--for", "2m", "--level", "detail"); err != nil {
		t.Fatalf("arm worker trace after re-enable: %v\n%s", err, out)
	}
	status := sessionReconcilerColdDisableWaitForMode(t, cityDir, "auto", "keyed")
	after := sessionReconcilerColdDisableNewSession(t, cityDir, "after-reenable")
	waitForAgentRunning(t, cityDir, "after-reenable", 45*time.Second)
	sessionReconcilerColdDisableEmitSessionUpdate(t, cityDir, after.SessionID)
	trace, _, err := sessionLifecycleStatusShadowJourneyWaitForWitness(t.Context(), cityDir, after.SessionID, status.HeadSeq, 15*time.Second, "re-enabled lifecycle-status", sessionLifecycleStatusShadowJourneyStatusWitnesses)
	if err != nil {
		t.Fatalf("re-enabled lifecycle-status comparison did not converge: %v", err)
	}
	shadows := sessionLifecycleStatusShadowJourneyStatusWitnesses(trace, after.SessionID, status.HeadSeq)
	if len(shadows) != 1 || !sessionReconcilerColdDisableIsEnabledWitness(shadows[0]) || shadows[0].ControllerInstanceID == "" || shadows[0].ControllerInstanceID == retiredGeneration || shadows[0].ControllerInstanceID == offGeneration {
		t.Fatalf("re-enabled lifecycle-status records = %+v, want one converged no-effect fresh generation distinct from retired %q and off %q", shadows, retiredGeneration, offGeneration)
	}
	if got := sessionReconcilerColdDisableShadowCount(t, cityDir, retiredGeneration); got != retiredShadowCount {
		t.Fatalf("retired controller generation %q advanced shadow metrics after re-enable: before=%d after=%d", retiredGeneration, retiredShadowCount, got)
	}
}

func sessionReconcilerColdDisableHoldSupervisorPort(t *testing.T, gcHome string) func() {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", readSupervisorPortFromConfig(t, gcHome))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen on configured supervisor port %s: %v", addr, err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "foreign listener", http.StatusServiceUnavailable)
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()

	var once sync.Once
	release := func() {
		once.Do(func() {
			if err := server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("close foreign supervisor-port listener: %v", err)
			}
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					t.Errorf("serve foreign supervisor-port listener: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Errorf("foreign supervisor-port listener did not stop")
			}
		})
	}
	t.Cleanup(release)
	return release
}

func sessionReconcilerColdDisableSupervisorControlSocketPID(t *testing.T, gcHome string) int {
	t.Helper()
	conn, err := net.DialTimeout("unix", filepath.Join(gcHome, "supervisor.sock"), 500*time.Millisecond)
	if err != nil {
		return 0
	}
	defer conn.Close() //nolint:errcheck // best-effort test probe cleanup
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		return 0
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return 0
	}
	var pid int
	if _, err := fmt.Fscan(conn, &pid); err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func sessionReconcilerColdDisableAssertOfflineStatus(t *testing.T, cityDir string) {
	t.Helper()
	out, err := gc(cityDir, "trace", "status", "--json")
	if err != nil {
		t.Fatalf("read offline trace status: %v\n%s", err, out)
	}
	var status sessionReconcilerColdDisableStatus
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &status); err != nil {
		t.Fatalf("decode offline trace status: %v\n%s", err, out)
	}
	got := status.SessionReconciler
	if status.ControllerRunning || got.Available || got.EffectiveOwner != "unavailable" ||
		got.PendingKeys != 0 || got.AuditPending || len(status.ActiveArms) != 0 {
		t.Fatalf("offline trace status = %+v, want controller stopped, unavailable reconciler, and no arms", status)
	}
}

func sessionReconcilerColdDisableReadStatus(t *testing.T, cityDir string) sessionReconcilerColdDisableStatus {
	return sessionReconcilerColdDisableWaitForMode(t, cityDir, "off", "legacy")
}

func sessionReconcilerColdDisableWaitForMode(t *testing.T, cityDir, mode, owner string) sessionReconcilerColdDisableStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), sessionReconcilerColdDisableTimeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var status sessionReconcilerColdDisableStatus
	for {
		out, err := gc(cityDir, "trace", "status", "--json")
		if err == nil {
			err = json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &status)
		}
		got := status.SessionReconciler
		if err == nil && status.ControllerRunning && got.SchemaVersion == "1" && got.Available &&
			got.ConfiguredMode == mode && got.EffectiveOwner == owner &&
			got.PendingKeys == 0 && !got.AuditPending {
			return status
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for exact available %s/%s tuple with no keyed owner: %v; status=%+v error=%v", mode, owner, ctx.Err(), status, err)
		case <-ticker.C:
		}
	}
}

func sessionReconcilerColdDisableNewSession(t *testing.T, cityDir, alias string) sessionLifecycleStatusShadowJourneyNew {
	t.Helper()
	out, err := gc(cityDir, "session", "new", "worker", "--alias", alias, "--no-attach", "--json")
	if err != nil {
		t.Fatalf("create worker session %q: %v\n%s", alias, err, out)
	}
	var created sessionLifecycleStatusShadowJourneyNew
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &created); err != nil {
		t.Fatalf("decode worker session %q: %v\n%s", alias, err, out)
	}
	if created.SessionID == "" || created.SessionName == "" {
		t.Fatalf("worker session %q = %+v, want durable and runtime identities", alias, created)
	}
	return created
}

func sessionReconcilerColdDisableEmitSessionUpdate(t *testing.T, cityDir, sessionID string) {
	t.Helper()
	out, err := runCommand(cityDir, replaceEnv(commandEnvForDir(cityDir, false), "GC_BEADS", "file"), integrationBDCommandTimeout,
		bdBinary, "update", sessionID, "--set-metadata", "state=awake", "--set-metadata", "wake_request=explicit")
	if err != nil {
		t.Fatalf("stage exact-start ownership for %s: %v\n%s", sessionID, err, out)
	}
	out, err = gc(cityDir, "event", "emit", "bead.updated",
		"--subject", sessionID,
		"--bead-payload", sessionID,
		"--actor", "bd-hook",
		"--json")
	if err != nil {
		t.Fatalf("emit typed session update for %s: %v\n%s", sessionID, err, out)
	}
	var emitted sessionWaitDependencyShadowJourneyEventEmit
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &emitted); err != nil {
		t.Fatalf("decode typed session update for %s: %v\n%s", sessionID, err, out)
	}
	if !emitted.HasPayload || !emitted.Submitted {
		t.Fatalf("typed session update for %s = %+v, want submitted payload", sessionID, emitted)
	}
}

func sessionReconcilerColdDisableShadowRecords(trace sessionWaitDependencyShadowJourneyTraceShow, sessionID string, afterSeq uint64) []sessionWaitDependencyShadowJourneyTraceRecord {
	var matches []sessionWaitDependencyShadowJourneyTraceRecord
	for _, record := range trace.Records {
		if record.Seq > afterSeq && record.SiteCode == "lifecycle.start_selection.shadow" && record.Fields.SessionID == sessionID {
			matches = append(matches, record)
		}
	}
	return matches
}

func sessionReconcilerColdDisableFirstShadowRecord(trace sessionWaitDependencyShadowJourneyTraceShow, sessionID string, afterSeq uint64) []sessionWaitDependencyShadowJourneyTraceRecord {
	matches := sessionReconcilerColdDisableShadowRecords(trace, sessionID, afterSeq)
	if len(matches) == 0 {
		return nil
	}
	return matches[:1]
}

func sessionReconcilerColdDisableIsEnabledWitness(record sessionWaitDependencyShadowJourneyTraceRecord) bool {
	return record.SiteCode == "lifecycle.status.shadow" &&
		record.RecordType == "operation" &&
		(record.Fields.Admission == "in_process" || record.Fields.Admission == "socket") &&
		record.Fields.StatusOutcome == "noop" &&
		record.Fields.StatusReason == "converged" &&
		record.Fields.EffectApplied != nil && !*record.Fields.EffectApplied
}

func sessionReconcilerColdDisableShadowCount(t *testing.T, cityDir, controllerGeneration string) int {
	t.Helper()
	trace, err := sessionWaitDependencyShadowJourneyTrace(cityDir)
	if err != nil {
		t.Fatalf("read shadow metrics: %v", err)
	}
	count := 0
	for _, record := range trace.Records {
		if strings.Contains(record.SiteCode, ".shadow") && record.ControllerInstanceID == controllerGeneration {
			count++
		}
	}
	return count
}

func sessionReconcilerColdDisableInstallModeConfig(t *testing.T, cityDir string, env []string, sessionName, fromMode, fromOwner, mode string) {
	t.Helper()
	if mode != "off" && mode != "auto" {
		t.Fatalf("unsupported session reconciler mode %q", mode)
	}
	configPath := filepath.Join(cityDir, "city.toml")
	info, err := os.Lstat(configPath)
	if err != nil {
		t.Fatalf("inspect city config: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("refusing non-regular city config: %s", info.Mode())
	}
	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read city config: %v", err)
	}
	beforePID := sessionReconcilerColdDisableSupervisorPID(t, env)
	beforeIdentity := sessionWaitDependencyShadowJourneyTmuxIdentity(t, cityDir, sessionName)
	beforeStatus := sessionReconcilerColdDisableWaitForMode(t, cityDir, fromMode, fromOwner).SessionReconciler
	if matches := sessionReconcilerColdDisableAssignment.FindAll(current, -1); len(matches) != 1 {
		t.Fatalf("session_reconciler assignments = %d, want exactly 1", len(matches))
	}
	candidate := filepath.Join(cityDir, ".city.toml.cold-disable")
	malformed := []byte("session_reconciler = [\n")
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, candidate, malformed, info.Mode().Perm()); err != nil {
		t.Fatalf("write malformed same-directory cold-disable candidate: %v", err)
	}
	if out, err := gc(cityDir, "config", "show", "--validate", "--root-file", candidate); err == nil {
		t.Fatalf("validate malformed cold-disable candidate succeeded:\n%s", out)
	} else if !strings.Contains(out, candidate) {
		t.Fatalf("malformed candidate diagnostic = %q, want candidate path %q", out, candidate)
	}
	if got, err := os.ReadFile(configPath); err != nil {
		t.Fatalf("read live city config after malformed candidate: %v", err)
	} else if !bytes.Equal(got, current) {
		t.Fatalf("live city config changed after malformed candidate: got %q want %q", got, current)
	}
	if got, err := os.ReadFile(candidate); err != nil {
		t.Fatalf("read malformed candidate after validation: %v", err)
	} else if !bytes.Equal(got, malformed) {
		t.Fatalf("malformed candidate changed during validation: got %q want %q", got, malformed)
	}
	if got := sessionReconcilerColdDisableSupervisorPID(t, env); got != beforePID {
		t.Fatalf("supervisor PID changed after malformed candidate: before=%d after=%d", beforePID, got)
	}
	if got := sessionWaitDependencyShadowJourneyTmuxIdentity(t, cityDir, sessionName); got != beforeIdentity {
		t.Fatalf("tmux identity changed after malformed candidate: before=%q after=%q", beforeIdentity, got)
	}
	if got := sessionReconcilerColdDisableWaitForMode(t, cityDir, fromMode, fromOwner).SessionReconciler; got != beforeStatus {
		t.Fatalf("reconciler status changed after malformed candidate: before=%+v after=%+v", beforeStatus, got)
	}
	next := sessionReconcilerColdDisableAssignment.ReplaceAll(current, []byte(fmt.Sprintf(`${1}"%s"${2}`, mode)))
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, candidate, next, info.Mode().Perm()); err != nil {
		t.Fatalf("write same-directory %s candidate: %v", mode, err)
	}
	candidateInfo, err := os.Lstat(candidate)
	if err != nil || !candidateInfo.Mode().IsRegular() || candidateInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s candidate is not a regular file: info=%v err=%v", mode, candidateInfo, err)
	}
	if out, err := gc(cityDir, "config", "show", "--validate", "--root-file", candidate); err != nil {
		t.Fatalf("validate %s candidate: %v\n%s", mode, err, out)
	}
	if err := os.Rename(candidate, configPath); err != nil {
		t.Fatalf("atomically install %s candidate: %v", mode, err)
	}
}

func sessionReconcilerColdDisableSupervisorPID(t *testing.T, env []string) int {
	t.Helper()
	out, err := runCommand("", env, 5*time.Second, gcBinary, "supervisor", "status", "--json")
	if err != nil {
		t.Fatalf("read supervisor status: %v\n%s", err, out)
	}
	var status struct {
		Running bool `json:"running"`
		PID     int  `json:"pid"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &status); err != nil {
		t.Fatalf("decode supervisor status: %v\n%s", err, out)
	}
	if !status.Running || status.PID <= 0 {
		t.Fatalf("supervisor status = %+v, want running PID", status)
	}
	return status.PID
}

func sessionReconcilerColdDisableWaitPIDGone(t *testing.T, pid int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), sessionReconcilerColdDisableTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatalf("probe test-owned supervisor %d: %v", pid, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("test-owned supervisor %d did not exit: %v", pid, ctx.Err())
		case <-ticker.C:
		}
	}
}
