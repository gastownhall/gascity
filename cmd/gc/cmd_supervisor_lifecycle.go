package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/processenv"
	"github.com/gastownhall/gascity/internal/processgroup"
	"github.com/gastownhall/gascity/internal/searchpath"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/spf13/cobra"
)

var (
	ensureSupervisorRunningHook              = ensureSupervisorRunning
	reloadSupervisorHook                     = reloadSupervisor
	supervisorAliveHook                      = supervisorAlive
	supervisorReadyTimeout                   = 15 * time.Second
	supervisorReadyPollInterval              = 100 * time.Millisecond
	supervisorSystemdWarmRefreshStopTimeout  = 5 * time.Second
	supervisorSystemdWarmRefreshPollInterval = 100 * time.Millisecond
	supervisorLaunchctlRun                   = func(args ...string) error {
		return exec.Command("launchctl", args...).Run()
	}
	supervisorLaunchdActive = func(label string) bool {
		out, err := exec.Command("launchctl", "print", supervisorLaunchdServiceTarget(label)).Output()
		return err == nil && launchdPrintReportsRunning(out)
	}
	// supervisorLaunchctlGetenv reads a value from `launchctl getenv` on
	// macOS so users can set per-domain env (e.g. GC_DOLT_LOGLEVEL) and
	// have it flow into the supervisor's launchd plist. Returns "" on
	// non-Darwin or when the key is unset / launchctl is unavailable.
	supervisorLaunchctlGetenv = func(key string) string {
		if supervisorRuntimeGOOS != "darwin" {
			return ""
		}
		out, err := exec.Command("launchctl", "getenv", key).Output()
		if err != nil {
			return ""
		}
		val := strings.TrimSuffix(string(out), "\n")
		return strings.TrimSuffix(val, "\r")
	}
	supervisorSystemctlRun = func(args ...string) error {
		return exec.Command("systemctl", args...).Run()
	}
	supervisorSystemctlActive = func(service string) bool {
		return exec.Command("systemctl", "--user", "is-active", "--quiet", service).Run() == nil
	}
	// supervisorSystemctlUserAvailable probes whether a per-user systemd
	// instance is reachable. `systemctl --user show-environment` exits
	// non-zero when there is no user manager (e.g. running as a service
	// account without `loginctl enable-linger`, or inside a minimal
	// container). The check goes through supervisorSystemctlRun so the
	// existing test seam keeps working: tests that stub
	// supervisorSystemctlRun automatically see the user manager as
	// available.
	supervisorSystemctlUserAvailable = func() bool {
		return supervisorSystemctlRun("--user", "show-environment") == nil
	}
	// supervisorLoginctlRun enables systemd user lingering via loginctl.
	// Exposed as a package var so tests stub it instead of spawning
	// loginctl. Enabling linger for one's own account is permitted without
	// root on default polkit configurations.
	supervisorLoginctlRun = func(args ...string) error {
		return exec.Command("loginctl", args...).Run()
	}
	// supervisorLingerEnabled reports whether systemd lingering is already
	// enabled for the given user, so a reinstall does not re-run
	// enable-linger or warn spuriously. Returns false when loginctl is
	// unavailable or the user manager cannot be queried.
	supervisorLingerEnabled = func(user string) bool {
		out, err := exec.Command("loginctl", "show-user", user, "--property=Linger", "--value").Output()
		if err != nil {
			return false
		}
		return strings.TrimSpace(string(out)) == "yes"
	}
	supervisorRunningPreserveSignalReady                = runningSupervisorPreserveSignalReady
	supervisorProcRoot                                  = "/proc"
	supervisorProcReadDir                               = os.ReadDir
	supervisorProcReadFile                              = os.ReadFile
	supervisorGetpgid                                   = syscall.Getpgid
	supervisorGetpgrp                                   = syscall.Getpgrp
	supervisorKill                                      = syscall.Kill
	supervisorProcessGroupPollPeriod                    = 20 * time.Millisecond
	supervisorRuntimeGOOS                               = goruntime.GOOS
	supervisorWorkspaceServiceCleanupWarnings io.Writer = os.Stderr
	// supervisorInstallForce is set true by --force on 'gc supervisor install'.
	// It permits overwriting an existing service unit that references a
	// different gc binary. Exposed as a var so tests can override it directly.
	supervisorInstallForce bool

	// supervisorServiceManagerActive reports whether the platform service
	// manager (launchd on macOS, systemd --user on Linux) considers the
	// supervisor running. Fallback liveness signal when the control-socket
	// ping fails (gascity#2984). With GC_SUPERVISOR_SYSTEMD_UNIT set, the
	// delegated unit is the only authoritative service-manager signal —
	// gc's own user unit is irrelevant, and a system-scope unit (whose
	// socket is typically unreachable from the operator's shell) must not
	// be gated on per-user manager availability; the is-active probe
	// degrades to false on its own when the manager is unreachable.
	supervisorServiceManagerActive = func() bool {
		d, delegated, err := supervisorSystemdDelegation()
		if err != nil {
			// Invalid scope: report nothing rather than probing a unit the
			// operator did not configure; lifecycle commands surface the
			// configuration error itself.
			return false
		}
		if delegated {
			return delegatedUnitActive(d)
		}
		switch supervisorRuntimeGOOS {
		case "darwin":
			return supervisorLaunchdActive(supervisorLaunchdLabel())
		case "linux":
			if !supervisorSystemctlUserAvailable() {
				return false
			}
			return supervisorSystemctlActive(supervisorSystemdServiceName())
		default:
			return false
		}
	}
	// supervisorAPIReachable reports whether the supervisor HTTP API answers
	// on its configured loopback address. Complements the service-manager
	// signal for supervisors not managed by launchd/systemd (gascity#2984).
	supervisorAPIReachable = func() bool {
		cfg, err := supervisorLoadConfig(supervisor.ConfigPath())
		if err != nil {
			return false
		}
		// Normalize wildcard binds to loopback before dialing, mirroring
		// cmd_service.go: a 0.0.0.0/:: bind is not itself a dialable address
		// (and 0.0.0.0 GETs fail on macOS).
		bind := cfg.Supervisor.BindOrDefault()
		switch bind {
		case "", "0.0.0.0":
			bind = "127.0.0.1"
		case "::", "[::]":
			bind = "::1"
		}
		url := fmt.Sprintf("http://%s/v0/cities",
			net.JoinHostPort(bind, strconv.Itoa(cfg.Supervisor.PortOrDefault())))
		client := &http.Client{Timeout: 750 * time.Millisecond}
		resp, err := client.Get(url)
		if err != nil {
			return false
		}
		defer resp.Body.Close() //nolint:errcheck
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
)

const supervisorServiceFileMode os.FileMode = 0o600

type supervisorWorkspaceServiceProcess struct {
	pid  int
	pgid int
	name string
}

type supervisorWorkspaceServiceCleanupScope struct {
	gcHome    string
	cityPaths map[string]string
}

func launchdPrintReportsRunning(out []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 3 && fields[0] == "state" && fields[1] == "=" && fields[2] == "running" {
			return true
		}
	}
	return false
}

func cleanupSupervisorWorkspaceServicesForWarmRefresh(gcHome string) error {
	scope, err := supervisorWorkspaceServiceCleanupScopeFromRegistry(gcHome)
	if err != nil {
		return err
	}
	return cleanupSupervisorWorkspaceServices(scope)
}

func cleanupSupervisorWorkspaceServicesForSupervisorStart(gcHome string) error {
	scope, err := supervisorWorkspaceServiceCleanupScopeFromRegistry(gcHome)
	if err != nil {
		return err
	}
	if supervisorRuntimeGOOS != "linux" {
		if len(scope.cityPaths) > 0 {
			warnSupervisorWorkspaceServiceCleanup("gc supervisor: workspace-service startup cleanup is not available on %s; after a non-graceful supervisor exit, stale workspace-service processes may keep sockets bound. Registered workspace-service roots: %s. Stop stale processes whose environment includes GC_SERVICE_STATE_ROOT under those roots, then restart those cities.\n", supervisorRuntimeGOOS, strings.Join(supervisorWorkspaceServiceStateRoots(scope), ", "))
		}
		return nil
	}
	if err := cleanupSupervisorWorkspaceServices(scope); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}

func warnSupervisorWorkspaceServiceCleanup(format string, args ...any) {
	if supervisorWorkspaceServiceCleanupWarnings == nil {
		return
	}
	fmt.Fprintf(supervisorWorkspaceServiceCleanupWarnings, format, args...) //nolint:errcheck // best-effort operator diagnostic
}

func supervisorWorkspaceServiceStateRoots(scope supervisorWorkspaceServiceCleanupScope) []string {
	roots := make([]string, 0, len(scope.cityPaths))
	for cityPath := range scope.cityPaths {
		roots = append(roots, citylayout.RuntimeServicesDir(cityPath))
	}
	sort.Strings(roots)
	return roots
}

// cleanupSupervisorWorkspaceServices terminates workspace-service processes
// owned by this supervisor's GC_HOME/registry scope. A second sweep with
// different matching rules (service-name + state-root + exact-argv +
// ppid 1) runs before every proxy_process spawn — see
// internal/workspacesvc/orphan_reap.go. Keep the two mechanisms in mind
// when changing either.
func cleanupSupervisorWorkspaceServices(scope supervisorWorkspaceServiceCleanupScope) error {
	procs, err := findSupervisorWorkspaceServiceProcesses(scope)
	if err != nil {
		return err
	}
	var errs []error
	for _, proc := range procs {
		if err := terminateProcessGroup(proc.pgid, 2*time.Second); err != nil {
			errs = append(errs, fmt.Errorf("stopping workspace service %q pid %d pgid %d: %w", proc.name, proc.pid, proc.pgid, err))
		}
	}
	return errors.Join(errs...)
}

func supervisorWorkspaceServiceCleanupScopeFromRegistry(gcHome string) (supervisorWorkspaceServiceCleanupScope, error) {
	scope := supervisorWorkspaceServiceCleanupScope{
		gcHome:    normalizePathForCompare(strings.TrimSpace(gcHome)),
		cityPaths: make(map[string]string),
	}
	if scope.gcHome == "" {
		return scope, errors.New("missing GC_HOME for workspace-service cleanup")
	}
	entries, err := supervisor.NewRegistry(supervisor.RegistryPath()).List()
	if err != nil {
		return scope, fmt.Errorf("reading supervisor registry for workspace-service cleanup: %w", err)
	}
	for _, entry := range entries {
		cityPath := normalizePathForCompare(strings.TrimSpace(entry.Path))
		if cityPath == "" {
			continue
		}
		scope.cityPaths[cityPath] = cityPath
	}
	return scope, nil
}

func findSupervisorWorkspaceServiceProcesses(scope supervisorWorkspaceServiceCleanupScope) ([]supervisorWorkspaceServiceProcess, error) {
	if strings.TrimSpace(scope.gcHome) == "" {
		return nil, errors.New("missing GC_HOME for workspace-service cleanup")
	}
	if len(scope.cityPaths) == 0 {
		return nil, nil
	}
	entries, err := supervisorProcReadDir(supervisorProcRoot)
	if err != nil {
		return nil, fmt.Errorf("reading /proc: %w", err)
	}
	seenPGID := make(map[int]supervisorWorkspaceServiceProcess)
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		env, err := supervisorProcReadFile(filepath.Join(supervisorProcRoot, entry.Name(), "environ"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				continue
			}
			continue
		}
		envMap := supervisorProcessEnvMap(env)
		if !supervisorWorkspaceServiceCandidateOwnedByScope(scope, envMap) {
			continue
		}
		pgid, err := supervisorGetpgid(pid)
		if err != nil {
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			errs = append(errs, fmt.Errorf("workspace service %q pid %d pgid: %w", envMap["GC_SERVICE_NAME"], pid, err))
			continue
		}
		confirmedEnv, err := supervisorProcReadFile(filepath.Join(supervisorProcRoot, entry.Name(), "environ"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				continue
			}
			continue
		}
		confirmedEnvMap := supervisorProcessEnvMap(confirmedEnv)
		if !supervisorWorkspaceServiceCandidateOwnedByScope(scope, confirmedEnvMap) ||
			!sameSupervisorWorkspaceServiceCandidate(envMap, confirmedEnvMap) {
			continue
		}
		if pgid <= 1 || pgid == supervisorGetpgrp() {
			warnSupervisorWorkspaceServiceCleanup("gc supervisor: skipping workspace service %q pid %d with unsafe process group %d; leaving it running\n", envMap["GC_SERVICE_NAME"], pid, pgid)
			continue
		}
		if _, ok := seenPGID[pgid]; !ok {
			seenPGID[pgid] = supervisorWorkspaceServiceProcess{
				pid:  pid,
				pgid: pgid,
				name: envMap["GC_SERVICE_NAME"],
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	procs := make([]supervisorWorkspaceServiceProcess, 0, len(seenPGID))
	for _, proc := range seenPGID {
		procs = append(procs, proc)
	}
	sort.Slice(procs, func(i, j int) bool {
		return procs[i].pgid < procs[j].pgid
	})
	return procs, nil
}

func supervisorWorkspaceServiceCandidateOwnedByScope(scope supervisorWorkspaceServiceCleanupScope, envMap map[string]string) bool {
	if envMap["GC_SERVICE_SOCKET"] == "" || envMap["GC_SERVICE_NAME"] == "" || envMap["GC_SERVICE_STATE_ROOT"] == "" {
		return false
	}
	return supervisorWorkspaceServiceOwnedByScope(scope, envMap)
}

func sameSupervisorWorkspaceServiceCandidate(before, after map[string]string) bool {
	for _, key := range []string{
		"GC_HOME",
		"GC_CITY_PATH",
		"GC_SERVICE_NAME",
		"GC_SERVICE_STATE_ROOT",
		"GC_SERVICE_SOCKET",
	} {
		if before[key] != after[key] {
			return false
		}
	}
	return true
}

func supervisorWorkspaceServiceOwnedByScope(scope supervisorWorkspaceServiceCleanupScope, envMap map[string]string) bool {
	envHome := normalizePathForCompare(strings.TrimSpace(envMap["GC_HOME"]))
	if envHome == "" || envHome != scope.gcHome {
		return false
	}
	cityPath := normalizePathForCompare(strings.TrimSpace(envMap["GC_CITY_PATH"]))
	if cityPath == "" {
		return false
	}
	cityPath, ok := scope.cityPaths[cityPath]
	if !ok {
		return false
	}
	stateRoot := strings.TrimSpace(envMap["GC_SERVICE_STATE_ROOT"])
	if stateRoot == "" {
		return false
	}
	return pathWithinOrSame(stateRoot, citylayout.RuntimeServicesDir(cityPath))
}

func supervisorProcessEnvMap(data []byte) map[string]string {
	env := make(map[string]string)
	for _, item := range bytes.Split(data, []byte{0}) {
		if len(item) == 0 {
			continue
		}
		key, value, ok := bytes.Cut(item, []byte("="))
		if !ok {
			continue
		}
		env[string(key)] = string(value)
	}
	return env
}

func terminateProcessGroup(pgid int, timeout time.Duration) error {
	return processgroup.Terminate(pgid, timeout, processgroup.Options{
		Kill:           supervisorKill,
		CurrentGroupID: supervisorGetpgrp,
		PollPeriod:     supervisorProcessGroupPollPeriod,
	})
}

func newSupervisorRunCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the machine-wide supervisor in the foreground",
		Long: `Run the machine-wide supervisor in the foreground.

This is the canonical long-running control loop. It reads ~/.gc/cities.toml
for registered cities, manages them from one process, and hosts the shared
API server.

Output is teed into ~/.gc/supervisor.log so 'gc supervisor logs' works
regardless of how the supervisor was invoked. Set GC_SUPERVISOR_LOG_TEE=0
in the supervisor's environment to disable the tee when the service manager
already captures output (e.g. a hand-managed systemd unit with
StandardOutput=journal).`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doSupervisorRun(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// runSupervisorFunc is the run-loop entry point invoked by
// doSupervisorRun. Indirection enables tests to substitute a no-op
// loop so pre-loop setup (defaultSupervisorBeadsActor) is observable
// without launching the real long-running supervisor.
var runSupervisorFunc = runSupervisor

func doSupervisorRun(stdout, stderr io.Writer) int {
	defaultSupervisorBeadsActor()
	return runSupervisorFunc(stdout, stderr)
}

// defaultSupervisorBeadsActor sets BEADS_ACTOR=controller in this
// process's env when the operator has not already set a value.
//
// bd hooks (.beads/hooks/on_create, on_update, on_close) are spawned
// from the supervisor process and forward events via `gc event emit`
// subprocesses that inherit this process's env. Without this default,
// eventActor() walks the GC_ALIAS → GC_AGENT → GC_SESSION_ID →
// BEADS_ACTOR chain (all unset in a fresh supervisor) and lands on the
// "human" fallback, mis-attributing every dispatcher-issued
// tracking-bead create/update/close.
//
// applyControllerBdEnv (cmd/gc/bd_env.go) covers BEADS_ACTOR for the
// env map handed to spawned bd commands; this covers the
// process-env path the hook subprocesses inherit. The two paths are
// independent and both are required for full controller attribution.
//
// Order-exec subprocesses still override BEADS_ACTOR to "order:<name>"
// via orderExecEnv (cmd/gc/order_store.go) before exec, so per-order
// attribution is preserved.
func defaultSupervisorBeadsActor() {
	if strings.TrimSpace(os.Getenv("BEADS_ACTOR")) == "" {
		_ = os.Setenv("BEADS_ACTOR", "controller")
	}
}

func doSupervisorStart(stdout, stderr io.Writer) int {
	return doSupervisorStartJSON(stdout, stderr, false)
}

func doSupervisorStartJSON(stdout, stderr io.Writer, jsonOut bool) int {
	delegation, delegated, err := supervisorSystemdDelegation()
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if delegated {
		return delegatedSupervisorStart(delegation, stdout, stderr, jsonOut)
	}
	if msg, blocked := platformSupervisorHomeOverrideError(); blocked {
		fmt.Fprintf(stderr, "gc supervisor start: %s\n", msg) //nolint:errcheck // best-effort stderr
		return 1
	}
	if pid := supervisorAlive(); pid != 0 {
		fmt.Fprintf(stderr, "gc supervisor start: supervisor already running (PID %d)\n", pid) //nolint:errcheck // best-effort stderr
		return 1
	}

	lock, err := acquireSupervisorLock()
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	lock.Close() //nolint:errcheck // release probe lock

	gcPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor start: finding executable: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	logPath := supervisorLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		fmt.Fprintf(stderr, "gc supervisor start: creating log dir: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor start: opening log: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer logFile.Close() //nolint:errcheck // best-effort cleanup

	child := exec.Command(gcPath, "supervisor", "run")
	child.SysProcAttr = backgroundSysProcAttr()
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	child.Env = os.Environ()

	if err := child.Start(); err != nil {
		fmt.Fprintf(stderr, "gc supervisor start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	deadline := time.Now().Add(supervisorReadyTimeout)
	for time.Now().Before(deadline) {
		if pid := supervisorAliveHook(); pid != 0 {
			if jsonOut {
				return writeLifecycleActionJSONOrExit(stdout, stderr, "gc supervisor start", lifecycleActionJSON{
					Command:       "supervisor start",
					Action:        "start",
					Message:       "Supervisor started.",
					SupervisorPID: pid,
				})
			}
			fmt.Fprintf(stdout, "Supervisor started (PID %d)\n", pid) //nolint:errcheck // best-effort stdout
			printDashboardStartHint(stdout)
			return 0
		}
		time.Sleep(supervisorReadyPollInterval)
	}

	fmt.Fprintf(stderr, "gc supervisor start: supervisor did not become ready; see %s\n", logPath) //nolint:errcheck // best-effort stderr
	return 1
}

func ensureSupervisorRunning(stdout, stderr io.Writer) int {
	delegation, delegated, err := supervisorSystemdDelegation()
	if err != nil {
		fmt.Fprintf(stderr, "gc: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if delegated {
		// The operator-managed unit owns install and start; never write
		// or load gc's own service files in delegated mode.
		if supervisorAliveHook() != 0 {
			return 0
		}
		// A bounded systemctl-start timeout is not terminal: fall through to
		// the same socket-then-fallback liveness check that confirms a late
		// start. Only an ordinary systemctl failure is terminal.
		if err := runDelegatedSystemctlTimeout(delegation, "start", delegatedSystemctlJobTimeout); err != nil && !isDelegatedSystemctlTimeout(err) {
			fmt.Fprintf(stderr, "gc: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if waitForSupervisorPID() != 0 {
			return 0
		}
		// The socket never answered — the expected state for a
		// system-scope unit under another uid. Trust the same fallback
		// evidence status does (unit active, then API) before calling
		// the start a failure.
		if delegatedLivenessWithoutSocket(delegation) != "" {
			return 0
		}
		// A delegated supervisor logs to the journal, not gc's fork-mode
		// log file — point readiness-timeout diagnostics at the unit.
		fmt.Fprintf(stderr, "gc: supervisor did not become ready after '%s'; check '%s'\n", delegation.commandHint("start"), delegation.commandHint("status")) //nolint:errcheck // best-effort stderr
		return 1
	}
	if msg, blocked := platformSupervisorHomeOverrideError(); blocked {
		fmt.Fprintf(stderr, "gc supervisor start: %s\n", msg) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Always regenerate the service file so upgrades pick up template
	// changes (e.g. PATH captured from the user's shell).
	if doSupervisorInstall(stdout, stderr) != 0 {
		if supervisorAlive() != 0 {
			return 0
		}
		// Fall back to bare start if install fails (e.g., unsupported OS).
		return doSupervisorStart(stdout, stderr)
	}
	if supervisorAliveHook() != 0 {
		return 0
	}
	return waitForSupervisorReady(stderr)
}

func platformSupervisorHomeOverrideError() (string, bool) {
	switch goruntime.GOOS {
	case "darwin", "linux":
	default:
		return "", false
	}
	envHome, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(envHome) == "" {
		return "", false
	}
	lookup, err := osuser.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lookup.HomeDir) == "" {
		return "", false
	}
	if filepath.Clean(envHome) == filepath.Clean(lookup.HomeDir) {
		return "", false
	}
	return fmt.Sprintf("HOME override %q differs from the user home %q; platform supervisor requires the real HOME. Keep HOME unchanged and use GC_HOME for isolated runs", envHome, lookup.HomeDir), true
}

// supervisorSystemdExecStartBinary returns the gc binary path embedded in the
// ExecStart line of a systemd unit file, or "" if the line is absent or
// cannot be parsed. The path may be quoted (strconv.Quote format) or bare.
func supervisorSystemdExecStartBinary(unit string) string {
	const prefix = "ExecStart="
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimPrefix(line, prefix)
		if len(rest) == 0 {
			return ""
		}
		if rest[0] == '"' {
			// Find the closing quote, honoring backslash escapes.
			i := 1
			for i < len(rest) {
				if rest[i] == '\\' {
					i += 2
					continue
				}
				if rest[i] == '"' {
					i++
					break
				}
				i++
			}
			unquoted, err := strconv.Unquote(rest[:i])
			if err != nil {
				return rest[:i]
			}
			return unquoted
		}
		// Unquoted: take up to the first space.
		if idx := strings.IndexByte(rest, ' '); idx >= 0 {
			return rest[:idx]
		}
		return rest
	}
	return ""
}

// supervisorLaunchdPlistGCPath returns the first ProgramArguments string
// (the gc binary path) from a launchd plist, or "" if not found.
func supervisorLaunchdPlistGCPath(plist string) string {
	idx := strings.Index(plist, "<key>ProgramArguments</key>")
	if idx < 0 {
		return ""
	}
	rest := plist[idx:]
	arrayStart := strings.Index(rest, "<array>")
	if arrayStart < 0 {
		return ""
	}
	rest = rest[arrayStart+len("<array>"):]
	strStart := strings.Index(rest, "<string>")
	if strStart < 0 {
		return ""
	}
	rest = rest[strStart+len("<string>"):]
	strEnd := strings.Index(rest, "</string>")
	if strEnd < 0 {
		return ""
	}
	raw := rest[:strEnd]
	// Reverse xmlEscape: apply multi-char entities before &amp; to avoid
	// double-unescaping sequences like &amp;lt;.
	r := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", "\"", "&apos;", "'", "&amp;", "&")
	return r.Replace(raw)
}

// supervisorSameBinary reports whether path a and path b refer to the same
// gc binary. It first compares cleaned string paths (handles both pointing
// at the same location), then falls back to inode comparison (handles one
// being a symlink to the other).
func supervisorSameBinary(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(infoA, infoB)
	}
	return false
}

func waitForSupervisorPID() int {
	deadline := time.Now().Add(supervisorReadyTimeout)
	for {
		if pid := supervisorAliveHook(); pid != 0 {
			return pid
		}
		if !time.Now().Before(deadline) {
			return 0
		}
		time.Sleep(supervisorReadyPollInterval)
	}
}

// waitForSupervisorReady polls supervisorAlive until the configured timeout.
func waitForSupervisorReady(stderr io.Writer) int {
	if waitForSupervisorPID() != 0 {
		return 0
	}
	fmt.Fprintf(stderr, "gc: supervisor did not become ready; see %s\n", supervisorLogPath()) //nolint:errcheck // best-effort stderr
	return 1
}

// unloadSupervisorService stops the platform service without removing
// the unit file, so gc start can reload it later. It is a no-op when
// the platform unit/plist is not installed — this keeps unit tests that
// invoke the stop helper hermetic on machines where the service has
// never been registered.
func unloadSupervisorService() {
	switch goruntime.GOOS {
	case "darwin":
		path := supervisorLaunchdPlistPath()
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			_ = supervisorLaunchctlRun("unload", path)
		}
		_ = unloadLegacySupervisorLaunchd(false)
	case "linux":
		service := supervisorSystemdServiceName()
		if _, err := os.Stat(supervisorSystemdServicePath()); !errors.Is(err, os.ErrNotExist) {
			_ = supervisorSystemctlRun("--user", "stop", service)
		}
		_ = unloadLegacySupervisorSystemd(false)
	}
}

func newSupervisorLogsCmd(stdout, stderr io.Writer) *cobra.Command {
	var numLines int
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Tail the supervisor log file",
		Long: `Tail the machine-wide supervisor log file.

Shows recent log output from background and service-managed supervisor runs.

When GC_SUPERVISOR_LOG_TEE=0 is set in this shell, the supervisor may be
writing only to the service manager's log: an existing log file is still
tailed (with a staleness warning), and when the file is absent the command
points at the service manager's log instead.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doSupervisorLogs(numLines, follow, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&numLines, "lines", "n", 50, "number of lines to show")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow log output")
	return cmd
}

// supervisorLogsJournalCmd builds the journalctl invocation for the
// gc-managed systemd unit, mirroring the requested -n/-f flags.
func supervisorLogsJournalCmd(numLines int, follow bool) string {
	journalCmd := fmt.Sprintf("journalctl --user -u %s -n %d", supervisorSystemdServiceName(), numLines)
	if follow {
		journalCmd += " -f"
	}
	return journalCmd
}

// supervisorLogsTeeDisabledHint builds the operator-facing pointer printed by
// `gc supervisor logs` when GC_SUPERVISOR_LOG_TEE=0 disables the supervisor
// log tee and no log file exists: there is nothing to tail, so direct the
// operator at the service manager's log instead (journalctl on linux).
func supervisorLogsTeeDisabledHint(goos string, numLines int, follow bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "gc supervisor logs: log tee is disabled (%s=0); supervisor output goes to the service manager log\n", supervisorLogTeeEnv)
	if goos == "linux" {
		fmt.Fprintf(&b, "gc supervisor logs: try: %s\n", supervisorLogsJournalCmd(numLines, follow))
	}
	return b.String()
}

// supervisorLogsTeeDisabledWarning builds the warning printed before tailing
// an existing log file while GC_SUPERVISOR_LOG_TEE=0 is set in the CLI's
// environment. The file may still be live: manual `gc supervisor start`,
// `gc start` restarts, and gc-generated service units all write it via
// fd/unit redirection regardless of the env, and a service unit's
// Environment= is invisible to this process.
func supervisorLogsTeeDisabledWarning(goos, logPath string, numLines int, follow bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "gc supervisor logs: warning: %s=0 is set in this environment; if the supervisor honors it, %s is stale\n", supervisorLogTeeEnv, logPath)
	if goos == "linux" {
		fmt.Fprintf(&b, "gc supervisor logs: service manager log: %s\n", supervisorLogsJournalCmd(numLines, follow))
	}
	return b.String()
}

func doSupervisorLogs(numLines int, follow bool, stdout, stderr io.Writer) int {
	logPath := supervisorLogPath()
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		if supervisorLogTeeDisabled() {
			// No log file and the tee is disabled in this shell: nothing to
			// tail, so point the operator at the service manager's log.
			fmt.Fprint(stderr, supervisorLogsTeeDisabledHint(goruntime.GOOS, numLines, follow)) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stderr, "gc supervisor logs: log file not found: %s\n", logPath) //nolint:errcheck // best-effort stderr
		return 1
	}
	if supervisorLogTeeDisabled() {
		// The file exists even though this shell disables the tee. Most
		// deployment shapes write it via fd/unit redirection independent of
		// the env, so the file is likely live; warn and tail instead of
		// misdirecting incident debugging away from real logs.
		fmt.Fprint(stderr, supervisorLogsTeeDisabledWarning(goruntime.GOOS, logPath, numLines, follow)) //nolint:errcheck // best-effort stderr
	}

	args := []string{"-n", fmt.Sprintf("%d", numLines)}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, logPath)

	cmd := exec.Command("tail", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "gc supervisor logs: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func newSupervisorInstallCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the supervisor as a platform service",
		Long: `Install the machine-wide supervisor as a platform service that
starts on login.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doSupervisorInstall(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&supervisorInstallForce, "force", false,
		"overwrite an existing service unit even if it references a different gc binary")
	return cmd
}

func doSupervisorInstall(stdout, stderr io.Writer) int {
	delegation, delegated, err := supervisorSystemdDelegation()
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if delegated {
		// The operator-managed unit owns the supervisor lifecycle;
		// installing gc's own service alongside it would leave two
		// service managers fighting over one supervisor.
		fmt.Fprintf(stderr, "gc supervisor install: %s is set (delegated to unit %q); gc does not install its own service files in delegated mode. Unset %s to manage gc's own service.\n", supervisorSystemdUnitEnv, delegation.Unit, supervisorSystemdUnitEnv) //nolint:errcheck // best-effort stderr
		return 1
	}
	if msg, blocked := platformSupervisorHomeOverrideError(); blocked {
		fmt.Fprintf(stderr, "gc supervisor install: %s\n", msg) //nolint:errcheck // best-effort stderr
		return 1
	}
	data, err := buildSupervisorServiceData()
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	switch goruntime.GOOS {
	case "darwin":
		return installSupervisorLaunchd(data, stdout, stderr)
	case "linux":
		return installSupervisorSystemd(data, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "gc supervisor install: not supported on %s\n", goruntime.GOOS) //nolint:errcheck // best-effort stderr
		return 1
	}
}

func newSupervisorUninstallCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the platform service",
		Long: `Remove the platform service and stop the machine-wide supervisor.

On systemd, uninstall refuses to remove an active unit when the supervisor
control socket is unavailable. Start the supervisor first so it can re-adopt
preserved sessions, then retry uninstall.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doSupervisorUninstall(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func doSupervisorUninstall(stdout, stderr io.Writer) int {
	delegation, delegated, derr := supervisorSystemdDelegation()
	if derr != nil {
		fmt.Fprintf(stderr, "gc supervisor uninstall: %v\n", derr) //nolint:errcheck // best-effort stderr
		return 1
	}
	if delegated {
		// Uninstall is a legitimate migration step (removing gc's legacy
		// unit after delegating), so warn rather than refuse: only
		// gc-owned service files are touched, never the delegated unit.
		fmt.Fprintf(stderr, "gc supervisor uninstall: warning: %s is set (delegated to unit %q); uninstall removes only gc's own service files and does not touch the delegated unit\n", supervisorSystemdUnitEnv, delegation.Unit) //nolint:errcheck // best-effort stderr
	}
	data, err := buildSupervisorServiceData()
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor uninstall: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	switch goruntime.GOOS {
	case "darwin":
		return uninstallSupervisorLaunchd(data, stdout, stderr)
	case "linux":
		return uninstallSupervisorSystemd(data, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "gc supervisor uninstall: not supported on %s\n", goruntime.GOOS) //nolint:errcheck // best-effort stderr
		return 1
	}
}

func supervisorLogPath() string {
	return filepath.Join(supervisor.DefaultHome(), "supervisor.log")
}

func ensureSupervisorServiceLogDir(logPath string) error {
	dir := filepath.Dir(logPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating supervisor log dir %s: %w", dir, err)
	}
	return nil
}

type supervisorServiceData struct {
	GCPath        string
	LogPath       string
	GCHome        string
	XDGRuntimeDir string
	LaunchdLabel  string
	SafeName      string
	Path          string
	ExtraEnv      []supervisorServiceEnvVar
}

type supervisorServiceEnvVar struct {
	Name  string
	Value string
}

func buildSupervisorServiceData() (*supervisorServiceData, error) {
	gcExe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("finding executable: %w", err)
	}
	homeDir, _ := os.UserHomeDir()
	gcPath := resolveStableSupervisorBinaryPath(homeDir, stableSupervisorBinaryGopath(homeDir), gcExe)
	home := supervisor.DefaultHome()
	xdgRuntimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if supervisor.UsesIsolatedGCHomeOverride() {
		xdgRuntimeDir = ""
	}
	return &supervisorServiceData{
		GCPath:        gcPath,
		LogPath:       supervisorLogPath(),
		GCHome:        home,
		XDGRuntimeDir: xdgRuntimeDir,
		LaunchdLabel:  supervisorLaunchdLabel(),
		SafeName:      sanitizeServiceName(filepath.Base(home)),
		Path:          searchpath.ExpandPath(homeDir, goruntime.GOOS, os.Getenv("PATH")),
		ExtraEnv:      supervisorServiceExtraEnv(),
	}, nil
}

const (
	supervisorBinaryName       = "gc"
	supervisorUserLocalBinPath = ".local/bin"
	supervisorGopathBinPath    = "bin"
)

// resolveStableSupervisorBinaryPath picks a stable install path for the
// supervisor service unit's ExecStart when one points at the same binary as
// currentExe; otherwise it returns currentExe. This prevents `gc supervisor
// install` from pinning the unit to a transient path (e.g. /tmp/gc) that
// later install paths (`make install`, gcsync) never refresh.
func resolveStableSupervisorBinaryPath(homeDir, gopath, currentExe string) string {
	if currentExe == "" {
		return currentExe
	}
	runningInfo, err := os.Stat(currentExe)
	if err != nil {
		return currentExe
	}
	for _, candidate := range stableSupervisorBinaryCandidates(homeDir, gopath) {
		if supervisorBinaryCandidateMatches(candidate, runningInfo) {
			return candidate
		}
	}
	return currentExe
}

func stableSupervisorBinaryCandidates(homeDir, gopath string) []string {
	var out []string
	if homeDir != "" {
		out = append(out, filepath.Join(homeDir, supervisorUserLocalBinPath, supervisorBinaryName))
	}
	if gopath != "" {
		out = append(out, filepath.Join(gopath, supervisorGopathBinPath, supervisorBinaryName))
	}
	return out
}

func supervisorBinaryCandidateMatches(candidate string, runningInfo os.FileInfo) bool {
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() {
		return false
	}
	if info.Mode()&0o111 == 0 {
		return false
	}
	return os.SameFile(info, runningInfo)
}

func stableSupervisorBinaryGopath(homeDir string) string {
	if v := strings.TrimSpace(os.Getenv("GOPATH")); v != "" {
		return v
	}
	if homeDir == "" {
		return ""
	}
	return filepath.Join(homeDir, "go")
}

func sanitizeServiceName(name string) string {
	name = strings.ToLower(name)
	re := regexp.MustCompile(`[^a-z0-9]+`)
	name = re.ReplaceAllString(name, "-")
	return strings.Trim(name, "-")
}

var supervisorServiceEnvNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Keep persistent service-file env narrow. Provider credentials and user
// context need to survive launchd/systemd startup; arbitrary shell state can
// be opted in with GC_SUPERVISOR_ENV.
var supervisorServiceEnvKeys = map[string]bool{
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": true,
	"CLAUDE_CODE_EFFORT_LEVEL":                 true,
	"CLAUDE_CODE_OAUTH_TOKEN":                  true,
	"CLAUDE_CODE_SUBAGENT_MODEL":               true,
	"CLAUDE_CONFIG_DIR":                        true,
	"GC_DOLT_LOGLEVEL":                         true,
	"GC_DOLT_PASSWORD":                         true,
	"GC_DOLT_USER":                             true,
	"T3_HOME":                                  true,
	"T3_WS_URL":                                true,
	"T3CODE_HOME":                              true,
	"HOME":                                     true,
	"LANG":                                     true,
	"LC_ALL":                                   true,
	"LC_CTYPE":                                 true,
	"LOGNAME":                                  true,
	"SHELL":                                    true,
	"USER":                                     true,
	"XDG_CONFIG_HOME":                          true,
	"XDG_STATE_HOME":                           true,
}

var supervisorServiceFixedEnvKeys = map[string]bool{
	"GC_HOME":                             true,
	supervisorPreserveSessionsOnSignalEnv: true,
	"PATH":                                true,
	"XDG_RUNTIME_DIR":                     true,
}

func supervisorServiceExtraEnv() []supervisorServiceEnvVar {
	env := make(map[string]string)
	explicitEnvKeys := supervisorServiceExplicitEnvKeys(os.Getenv("GC_SUPERVISOR_ENV"))
	explicitEnvKeySet := make(map[string]bool, len(explicitEnvKeys))
	for _, key := range explicitEnvKeys {
		explicitEnvKeySet[key] = true
	}
	for _, entry := range os.Environ() {
		key, val, ok := strings.Cut(entry, "=")
		if !ok || val == "" || !shouldPersistSupervisorEnv(key) {
			continue
		}
		env[key] = val
	}
	for _, key := range explicitEnvKeys {
		if val := os.Getenv(key); val != "" {
			env[key] = val
		}
	}
	// Merge a persistent machine-local secrets file (${GC_HOME}/secrets.env)
	// as a fallback tier. `gc start` snapshots the calling shell's env into
	// the service file, so a credential that lives only in this file and was
	// never exported into the invoking shell would otherwise be dropped —
	// yielding a blank value and a silent provider auth failure. A non-empty
	// value already in env (from the shell scan or a GC_SUPERVISOR_ENV opt-in)
	// still takes precedence; the file only fills keys those tiers left unset.
	// As elsewhere in this function, an empty value counts as unset. A file
	// entry must clear the same gate the other tiers use — the persist
	// allowlist or an explicit opt-in — so a stray key cannot bloat the
	// service env.
	for key, val := range supervisorSecretsEnvFileEntries() {
		if val == "" {
			continue
		}
		if _, ok := env[key]; ok {
			continue
		}
		if !shouldPersistSupervisorEnv(key) && !explicitEnvKeySet[key] {
			continue
		}
		env[key] = val
	}
	// Fall back to `launchctl getenv` for known-allowlisted keys and
	// for GC_SUPERVISOR_ENV opt-ins. Without this, launchctl-set
	// documented Dolt credential/logging settings are silently dropped:
	// the plist's EnvironmentVariables block scopes the spawned
	// supervisor's env, and `os.Environ()` only sees what's exported in
	// the calling shell.
	launchctlKeys := make([]string, 0, len(supervisorServiceEnvKeys)+len(explicitEnvKeys))
	launchctlSeen := make(map[string]bool, cap(launchctlKeys))
	for key := range supervisorServiceEnvKeys {
		launchctlSeen[key] = true
		launchctlKeys = append(launchctlKeys, key)
	}
	for _, key := range explicitEnvKeys {
		if launchctlSeen[key] {
			continue
		}
		launchctlSeen[key] = true
		launchctlKeys = append(launchctlKeys, key)
	}
	sort.Strings(launchctlKeys)
	for _, key := range launchctlKeys {
		if _, ok := env[key]; ok {
			continue
		}
		if val := supervisorLaunchctlGetenv(key); val != "" {
			env[key] = val
		}
	}

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]supervisorServiceEnvVar, 0, len(keys))
	for _, key := range keys {
		out = append(out, supervisorServiceEnvVar{Name: key, Value: env[key]})
	}
	return out
}

func shouldPersistSupervisorEnv(key string) bool {
	if !supervisorServiceEnvNameRE.MatchString(key) || supervisorServiceFixedEnvKeys[key] {
		return false
	}
	if supervisorServiceEnvKeys[key] {
		return true
	}
	if isProviderCredentialEnv(key) {
		return os.Getenv(supervisorOmitProviderCredsEnv) != "1"
	}
	return false
}

func isProviderCredentialEnv(key string) bool {
	return processenv.IsProviderCredentialEnv(key)
}

// supervisorSecretsEnvFileName is the dotenv-style file under GC_HOME that
// supervisorServiceExtraEnv merges as a persistent, machine-local source of
// provider credentials and other allowlisted service env.
const supervisorSecretsEnvFileName = "secrets.env"

// supervisorSecretsEnvFilePath returns the absolute path to the supervisor
// secrets file (${GC_HOME}/secrets.env).
func supervisorSecretsEnvFilePath() string {
	return filepath.Join(supervisor.DefaultHome(), supervisorSecretsEnvFileName)
}

// supervisorSecretsEnvFileEntries reads ${GC_HOME}/secrets.env and returns its
// parsed key/value pairs. A missing file is the normal case and yields nil. A
// present-but-unreadable or malformed file is logged to stderr and ignored so
// a bad secrets file never blocks supervisor install/start; the caller still
// gates whatever is returned on the persist allowlist or an explicit
// GC_SUPERVISOR_ENV opt-in.
func supervisorSecretsEnvFileEntries() map[string]string {
	path := supervisorSecretsEnvFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "gc: reading supervisor secrets file %q: %v\n", path, err)
		}
		return nil
	}
	entries, err := processenv.ParseEnvFile(string(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "gc: parsing supervisor secrets file %q: %v\n", path, err)
		return nil
	}
	return entries
}

func supervisorServiceExplicitEnvKeys(raw string) []string {
	fields := strings.Fields(strings.NewReplacer(",", " ", ";", " ").Replace(raw))
	out := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		key := strings.TrimSpace(field)
		if key == "" || seen[key] || !supervisorServiceEnvNameRE.MatchString(key) || supervisorServiceFixedEnvKeys[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

const (
	defaultSupervisorLaunchdLabel = "com.gascity.supervisor"
	defaultSupervisorSystemdUnit  = "gascity-supervisor.service"
)

func supervisorServiceSuffix() string {
	if !supervisor.UsesIsolatedGCHomeOverride() {
		return ""
	}
	gcHome := isolatedSupervisorHome()
	base := sanitizeServiceName(filepath.Base(gcHome))
	sum := sha1.Sum([]byte(gcHome))
	hash := hex.EncodeToString(sum[:])[:8]
	if base == "" {
		return "isolated-" + hash
	}
	return base + "-" + hash
}

func supervisorLaunchdLabel() string {
	if suffix := supervisorServiceSuffix(); suffix != "" {
		return defaultSupervisorLaunchdLabel + "." + suffix
	}
	return defaultSupervisorLaunchdLabel
}

func supervisorSystemdServiceName() string {
	if suffix := supervisorServiceSuffix(); suffix != "" {
		return "gascity-supervisor-" + suffix + ".service"
	}
	return defaultSupervisorSystemdUnit
}

const supervisorLaunchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{xmlesc .LaunchdLabel}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{xmlesc .GCPath}}</string>
        <string>supervisor</string>
        <string>run</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>Crashed</key>
        <true/>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>{{xmlesc .LogPath}}</string>
    <key>StandardErrorPath</key>
    <string>{{xmlesc .LogPath}}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>GC_HOME</key>
        <string>{{xmlesc .GCHome}}</string>
        {{if .XDGRuntimeDir}}
        <key>XDG_RUNTIME_DIR</key>
        <string>{{xmlesc .XDGRuntimeDir}}</string>
        {{end}}
        <key>PATH</key>
        <string>{{xmlesc .Path}}</string>
        <key>GC_SUPERVISOR_PRESERVE_SESSIONS_ON_SIGNAL</key>
        <string>1</string>
        {{range .ExtraEnv}}
        <key>{{xmlesc .Name}}</key>
        <string>{{xmlesc .Value}}</string>
        {{end}}
    </dict>
</dict>
</plist>
`

const supervisorSystemdTemplate = `[Unit]
Description=Gas City machine supervisor

[Service]
Type=simple
# Signal only the main supervisor PID on stop. The systemd default
# (control-group) would cascade SIGTERM to tmux servers spawned by
# 'gc supervisor run' that live in this cgroup, killing one-per-bead
# session conversation history. The reconciler re-adopts tmux on start.
KillMode=process
ExecStart={{systemdpath .GCPath}} supervisor run
Restart=always
RestartSec=5s
StandardOutput=append:{{.LogPath}}
StandardError=append:{{.LogPath}}
Environment=GC_HOME="{{.GCHome}}"
{{if .XDGRuntimeDir}}Environment=XDG_RUNTIME_DIR="{{.XDGRuntimeDir}}"
{{end}}Environment=PATH="{{.Path}}"
Environment=GC_SUPERVISOR_PRESERVE_SESSIONS_ON_SIGNAL="1"
{{range .ExtraEnv}}Environment={{systemdenv .Name .Value}}
{{end}}

[Install]
WantedBy=default.target
`

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return r.Replace(s)
}

func systemdEnv(name, value string) string {
	return name + "=" + strconv.Quote(value)
}

// supervisorSystemdQuotePath quotes a path for use in a systemd ExecStart line.
// Paths that contain no spaces, double-quotes, or backslashes are returned as-is;
// all others are wrapped in strconv.Quote to produce Go-style double-quoted strings,
// which systemd unit parsers understand as the quoted-exec-path format.
func supervisorSystemdQuotePath(s string) string {
	if strings.ContainsAny(s, " \"\\") {
		return strconv.Quote(s)
	}
	return s
}

func renderSupervisorTemplate(tmplStr string, data *supervisorServiceData) (string, error) {
	funcMap := template.FuncMap{"xmlesc": xmlEscape, "systemdenv": systemdEnv, "systemdpath": supervisorSystemdQuotePath}
	tmpl, err := template.New("service").Funcs(funcMap).Parse(tmplStr)
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func writeSupervisorServiceFile(path string, content []byte) error {
	if _, err := os.Stat(path); err == nil {
		if err := os.Chmod(path, supervisorServiceFileMode); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(path, content, supervisorServiceFileMode); err != nil {
		return err
	}
	return os.Chmod(path, supervisorServiceFileMode)
}

func supervisorLaunchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", supervisorLaunchdLabel()+".plist")
}

func supervisorLaunchdServiceTarget(label string) string {
	if label == "" {
		label = supervisorLaunchdLabel()
	}
	return "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
}

func loadAndStartSupervisorLaunchd(path, label string) error {
	if err := supervisorLaunchctlRun("load", path); err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}
	target := supervisorLaunchdServiceTarget(label)
	if err := supervisorLaunchctlRun("enable", target); err != nil {
		return fmt.Errorf("enable %s: %w", target, err)
	}
	if err := supervisorLaunchctlRun("kickstart", "-p", target); err != nil {
		return fmt.Errorf("kickstart -p %s: %w", target, err)
	}
	return nil
}

func loadAndStartSupervisorLaunchdForRollback(path, label string, stderr io.Writer) error {
	if err := supervisorLaunchctlRun("load", path); err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}
	target := supervisorLaunchdServiceTarget(label)
	if err := supervisorLaunchctlRun("enable", target); err != nil {
		warnSupervisorLaunchdRollback(stderr, "enable %s: %v", target, err)
	}
	if err := supervisorLaunchctlRun("kickstart", "-p", target); err != nil {
		warnSupervisorLaunchdRollback(stderr, "kickstart -p %s: %v", target, err)
	}
	return nil
}

func warnSupervisorLaunchdRollback(stderr io.Writer, format string, args ...any) {
	if stderr == nil {
		return
	}
	fmt.Fprintf(stderr, "gc supervisor install: warning: restoring launchd service: "+format+"\n", args...) //nolint:errcheck // best-effort stderr
}

func legacySupervisorLaunchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", defaultSupervisorLaunchdLabel+".plist")
}

func supervisorSystemdServicePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "systemd", "user", supervisorSystemdServiceName())
}

func legacySupervisorSystemdServicePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "systemd", "user", defaultSupervisorSystemdUnit)
}

func isolatedSupervisorHome() string {
	return normalizePathForCompare(strings.TrimSpace(os.Getenv("GC_HOME")))
}

func legacySupervisorTargetsCurrentHome(path string) bool {
	if !supervisor.UsesIsolatedGCHomeOverride() {
		return false
	}
	gcHome := isolatedSupervisorHome()
	if gcHome == "" {
		return false
	}
	legacyHome, ok := legacySupervisorHome(path)
	return ok && samePath(legacyHome, gcHome)
}

func legacySupervisorHome(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	switch filepath.Ext(path) {
	case ".plist":
		return launchdSupervisorHome(data)
	case ".service":
		return systemdSupervisorHome(data)
	default:
		return "", false
	}
}

type plistValue struct {
	text string
	dict map[string]plistValue
}

func launchdSupervisorHome(data []byte) (string, bool) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return "", false
		}
		if err != nil {
			return "", false
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "dict" {
			continue
		}
		root, err := parsePlistDict(dec)
		if err != nil {
			return "", false
		}
		env, ok := root["EnvironmentVariables"]
		if !ok || env.dict == nil {
			return "", false
		}
		gcHome, ok := env.dict["GC_HOME"]
		if !ok || gcHome.text == "" {
			return "", false
		}
		return filepath.Clean(gcHome.text), true
	}
}

func parsePlistDict(dec *xml.Decoder) (map[string]plistValue, error) {
	dict := make(map[string]plistValue)
	currentKey := ""
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			switch tok.Name.Local {
			case "key":
				var key string
				if err := dec.DecodeElement(&key, &tok); err != nil {
					return nil, err
				}
				currentKey = key
			case "string":
				var value string
				if err := dec.DecodeElement(&value, &tok); err != nil {
					return nil, err
				}
				if currentKey != "" {
					dict[currentKey] = plistValue{text: value}
					currentKey = ""
				}
			case "dict":
				nested, err := parsePlistDict(dec)
				if err != nil {
					return nil, err
				}
				if currentKey != "" {
					dict[currentKey] = plistValue{dict: nested}
					currentKey = ""
				}
			default:
				if err := skipXMLElement(dec); err != nil {
					return nil, err
				}
				if currentKey != "" {
					dict[currentKey] = plistValue{}
					currentKey = ""
				}
			}
		case xml.EndElement:
			if tok.Name.Local == "dict" {
				return dict, nil
			}
		}
	}
}

func skipXMLElement(dec *xml.Decoder) error {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return nil
}

func systemdSupervisorHome(data []byte) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "Environment=GC_HOME=") {
			continue
		}
		value := strings.TrimPrefix(line, "Environment=GC_HOME=")
		if unquoted, err := strconv.Unquote(value); err == nil {
			return filepath.Clean(unquoted), true
		}
		return filepath.Clean(value), true
	}
	return "", false
}

func unloadLegacySupervisorLaunchd(remove bool) error {
	path := legacySupervisorLaunchdPlistPath()
	if samePath(path, supervisorLaunchdPlistPath()) || !legacySupervisorTargetsCurrentHome(path) {
		return nil
	}
	_ = supervisorLaunchctlRun("unload", path)
	if remove {
		_ = supervisorLaunchctlRun("disable", supervisorLaunchdServiceTarget(defaultSupervisorLaunchdLabel))
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing legacy plist %s: %w", path, err)
		}
	}
	return nil
}

func unloadLegacySupervisorSystemd(remove bool) error {
	path := legacySupervisorSystemdServicePath()
	if samePath(path, supervisorSystemdServicePath()) || !legacySupervisorTargetsCurrentHome(path) {
		return nil
	}
	_ = supervisorSystemctlRun("--user", "stop", defaultSupervisorSystemdUnit)
	if remove {
		_ = supervisorSystemctlRun("--user", "disable", defaultSupervisorSystemdUnit)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing legacy unit %s: %w", path, err)
		}
	}
	return nil
}

func rollbackNewSupervisorLaunchdInstall(path string, restoreLegacy bool, stderr io.Writer) error {
	var errs []error
	_ = supervisorLaunchctlRun("unload", path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("removing failed plist %s during rollback: %w", path, err))
	}
	if restoreLegacy {
		if err := loadAndStartSupervisorLaunchdForRollback(legacySupervisorLaunchdPlistPath(), defaultSupervisorLaunchdLabel, stderr); err != nil {
			errs = append(errs, fmt.Errorf("restoring legacy plist %s: %w", legacySupervisorLaunchdPlistPath(), err))
		}
	}
	return errors.Join(errs...)
}

func restorePreviousSupervisorLaunchdInstall(path string, previousContent []byte, stderr io.Writer) error {
	var errs []error
	_ = supervisorLaunchctlRun("unload", path)
	if err := writeSupervisorServiceFile(path, previousContent); err != nil {
		errs = append(errs, fmt.Errorf("restoring previous plist %s: %w", path, err))
	} else if err := loadAndStartSupervisorLaunchdForRollback(path, supervisorLaunchdLabel(), stderr); err != nil {
		errs = append(errs, fmt.Errorf("reloading previous plist %s: %w", path, err))
	}
	return errors.Join(errs...)
}

func rollbackNewSupervisorSystemdInstall(path, service string, restoreLegacy bool) error {
	var errs []error
	_ = supervisorSystemctlRun("--user", "stop", service)
	_ = supervisorSystemctlRun("--user", "disable", service)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("removing failed unit %s during rollback: %w", path, err))
	}
	if err := supervisorSystemctlRun("--user", "daemon-reload"); err != nil {
		errs = append(errs, fmt.Errorf("systemctl --user daemon-reload during rollback: %w", err))
	}
	if restoreLegacy {
		if err := supervisorSystemctlRun("--user", "start", defaultSupervisorSystemdUnit); err != nil {
			errs = append(errs, fmt.Errorf("restoring legacy unit %s: %w", defaultSupervisorSystemdUnit, err))
		}
	}
	return errors.Join(errs...)
}

func restorePreviousSupervisorSystemdInstall(path, service string, previousContent []byte, restart bool) error {
	var errs []error
	if restart {
		_ = supervisorSystemctlRun("--user", "stop", service)
	}
	if err := writeSupervisorServiceFile(path, previousContent); err != nil {
		errs = append(errs, fmt.Errorf("restoring previous unit %s: %w", path, err))
		return errors.Join(errs...)
	}
	if err := supervisorSystemctlRun("--user", "daemon-reload"); err != nil {
		errs = append(errs, fmt.Errorf("systemctl --user daemon-reload during rollback: %w", err))
	}
	if restart {
		if err := supervisorSystemctlRun("--user", "enable", service); err != nil {
			errs = append(errs, fmt.Errorf("restoring previous unit enable %s: %w", service, err))
		}
		if err := supervisorSystemctlRun("--user", "start", service); err != nil {
			errs = append(errs, fmt.Errorf("restoring previous unit start %s: %w", service, err))
		}
	}
	return errors.Join(errs...)
}

func warnSupervisorSystemdWarmRefreshPreservedUnit(stderr io.Writer, service string) {
	fmt.Fprintf(stderr, "gc supervisor install: leaving refreshed systemd unit %s in place after warm-refresh failure; not restoring the previous unit because it may lack KillMode=process. Resolve the error, then run 'systemctl --user start %s' or rerun 'gc supervisor install'.\n", service, service) //nolint:errcheck // best-effort stderr
}

func installSupervisorLaunchd(data *supervisorServiceData, stdout, stderr io.Writer) int {
	content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, data)
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: rendering plist: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	path := supervisorLaunchdPlistPath()
	legacyPresent := legacySupervisorTargetsCurrentHome(legacySupervisorLaunchdPlistPath())
	existing, err := os.ReadFile(path)
	hadCurrent := err == nil
	contentUnchanged := hadCurrent && string(existing) == content
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor install: reading existing plist: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if hadCurrent && !supervisorInstallForce {
		if existingBinary := supervisorLaunchdPlistGCPath(string(existing)); existingBinary != "" && !supervisorSameBinary(existingBinary, data.GCPath) {
			fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
				"gc supervisor install: existing plist %q references binary %q but the current gc binary resolves to %q; "+
					"refusing to overwrite a plist installed from a different binary. "+
					"Install gc to a stable location first (e.g. 'make install'), then rerun 'gc supervisor install'. "+
					"To override, pass --force.\n",
				path, existingBinary, data.GCPath)
			return 1
		}
	}
	if contentUnchanged && supervisorAliveHook() != 0 {
		fmt.Fprintf(stdout, "Installed launchd service: %s\n", path) //nolint:errcheck // best-effort stdout
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := ensureSupervisorServiceLogDir(data.LogPath); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := writeSupervisorServiceFile(path, []byte(content)); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: writing plist: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := unloadLegacySupervisorLaunchd(false); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	_ = supervisorLaunchctlRun("unload", path)
	if err := loadAndStartSupervisorLaunchd(path, data.LaunchdLabel); err != nil {
		var rollbackErr error
		if hadCurrent {
			rollbackErr = restorePreviousSupervisorLaunchdInstall(path, existing, stderr)
		} else {
			rollbackErr = rollbackNewSupervisorLaunchdInstall(path, legacyPresent, stderr)
		}
		if rollbackErr != nil {
			fmt.Fprintf(stderr, "gc supervisor install: rollback after launchctl failure: %v\n", rollbackErr) //nolint:errcheck // best-effort stderr
		}
		fmt.Fprintf(stderr, "gc supervisor install: launchctl %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := unloadLegacySupervisorLaunchd(true); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: warning: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	fmt.Fprintf(stdout, "Installed launchd service: %s\n", path) //nolint:errcheck // best-effort stdout
	return 0
}

func uninstallSupervisorLaunchd(_ *supervisorServiceData, stdout, stderr io.Writer) int {
	path := supervisorLaunchdPlistPath()
	active := supervisorLaunchdActive(supervisorLaunchdLabel())
	if sockPath, _ := runningSupervisorSocket(); sockPath != "" {
		// Socket-protocol stop, never the delegated redirect: uninstall is
		// cleaning up gc's OWN service and must not stop an operator's
		// delegated unit (or require systemctl on darwin) as a side effect.
		if code := stopSupervisorViaSocket(stdout, stderr, true, 30*time.Second); code != 0 {
			return code
		}
	} else if active {
		fmt.Fprintf(stderr, "gc supervisor uninstall: launchd service %s is active but the control socket is unavailable; run 'gc supervisor start' to re-adopt sessions, then retry uninstall\n", supervisorLaunchdLabel()) //nolint:errcheck // best-effort stderr
		return 1
	}
	_ = supervisorLaunchctlRun("unload", path)
	_ = supervisorLaunchctlRun("disable", supervisorLaunchdServiceTarget(supervisorLaunchdLabel()))
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor uninstall: removing plist: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := unloadLegacySupervisorLaunchd(true); err != nil {
		fmt.Fprintf(stderr, "gc supervisor uninstall: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Uninstalled launchd service: %s\n", path) //nolint:errcheck // best-effort stdout
	return 0
}

func waitSupervisorSystemdInactive(service string, timeout time.Duration) bool {
	if !supervisorSystemctlActive(service) {
		return true
	}
	if timeout <= 0 {
		return false
	}
	poll := supervisorSystemdWarmRefreshPollInterval
	if poll <= 0 {
		poll = time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(poll)
		if !supervisorSystemctlActive(service) {
			return true
		}
	}
	return !supervisorSystemctlActive(service)
}

func runningSupervisorPreserveSignalReady() (int, bool, error) {
	_, pid := runningSupervisorSocket()
	if pid <= 0 {
		return 0, false, errors.New("active supervisor control socket is unavailable")
	}
	env, err := supervisorProcReadFile(filepath.Join(supervisorProcRoot, strconv.Itoa(pid), "environ"))
	if err != nil {
		return pid, false, fmt.Errorf("reading active supervisor pid %d environment: %w", pid, err)
	}
	return pid, supervisorProcessEnvMap(env)[supervisorPreserveSessionsOnSignalEnv] == "1", nil
}

func stopSupervisorSystemdForWarmRefresh(service string) ([]string, error) {
	termArgs := []string{"--user", "kill", "--kill-who=main", "--signal=SIGTERM", service}
	if err := supervisorSystemctlRun(termArgs...); err != nil {
		return termArgs, err
	}
	if waitSupervisorSystemdInactive(service, supervisorSystemdWarmRefreshStopTimeout) {
		return termArgs, nil
	}
	killArgs := []string{"--user", "kill", "--kill-who=main", "--signal=SIGKILL", service}
	if err := supervisorSystemctlRun(killArgs...); err != nil {
		return killArgs, err
	}
	return killArgs, nil
}

func installSupervisorSystemd(data *supervisorServiceData, stdout, stderr io.Writer) int {
	// Check the binary guard before probing systemd so a refused install
	// emits no systemctl calls.
	path := supervisorSystemdServicePath()
	existing, err := os.ReadFile(path)
	hadCurrent := err == nil
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor install: reading existing unit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if hadCurrent && !supervisorInstallForce {
		if existingBinary := supervisorSystemdExecStartBinary(string(existing)); existingBinary != "" && !supervisorSameBinary(existingBinary, data.GCPath) {
			fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
				"gc supervisor install: existing unit %q references binary %q but the current gc binary resolves to %q; "+
					"refusing to overwrite a unit installed from a different binary. "+
					"Install gc to a stable location first (e.g. 'make install'), then rerun 'gc supervisor install'. "+
					"To override, pass --force.\n",
				path, existingBinary, data.GCPath)
			return 1
		}
	}

	// Bail out before we touch the unit file when there is no per-user
	// systemd manager to load it. Otherwise daemon-reload + enable both
	// fail and the rollback path tries daemon-reload again, producing
	// 2-3 cascading "systemctl --user" errors that obscure the real
	// problem. Callers (notably ensureSupervisorRunning) already fall
	// back to a detached supervisor when install returns non-zero, so a
	// single clean error is the right shape here.
	if !supervisorSystemctlUserAvailable() {
		fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
			"gc supervisor install: per-user systemd instance is not available "+
				"(systemctl --user could not reach the user manager). "+
				"Either enable lingering for this account ('sudo loginctl enable-linger %s'), "+
				"log in via a PAM session that starts user-systemd, or run the supervisor "+
				"detached (e.g. 'gc supervisor start' without service install).\n",
			currentUsernameForSystemdHint())
		return 1
	}

	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: rendering unit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	service := supervisorSystemdServiceName()
	legacyPresent := legacySupervisorTargetsCurrentHome(legacySupervisorSystemdServicePath())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := ensureSupervisorServiceLogDir(data.LogPath); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	contentChanged := string(existing) != content
	active := supervisorSystemctlActive(service)
	if contentChanged && active {
		pid, ready, err := supervisorRunningPreserveSignalReady()
		if err != nil {
			fmt.Fprintf(stderr, "gc supervisor install: cannot verify active supervisor preserve-mode readiness: %v. Refusing systemd warm refresh because signaling an older supervisor can stop managed sessions. Stop or drain agents intentionally with 'gc supervisor stop --wait', then rerun 'gc supervisor install'.\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if !ready {
			fmt.Fprintf(stderr, "gc supervisor install: active supervisor pid %d does not have %s=1. Refusing systemd warm refresh because this first post-upgrade install would stop managed sessions. Stop or drain agents intentionally with 'gc supervisor stop --wait', then rerun 'gc supervisor install'.\n", pid, supervisorPreserveSessionsOnSignalEnv) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if err := writeSupervisorServiceFile(path, []byte(content)); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: writing unit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", service},
	} {
		if err := supervisorSystemctlRun(args...); err != nil {
			var rollbackErr error
			if hadCurrent {
				rollbackErr = restorePreviousSupervisorSystemdInstall(path, service, existing, false)
			} else {
				rollbackErr = rollbackNewSupervisorSystemdInstall(path, service, false)
			}
			if rollbackErr != nil {
				fmt.Fprintf(stderr, "gc supervisor install: rollback after systemctl %s failure: %v\n", strings.Join(args, " "), rollbackErr) //nolint:errcheck // best-effort stderr
			}
			fmt.Fprintf(stderr, "gc supervisor install: systemctl %s: %v\n", strings.Join(args, " "), err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if err := unloadLegacySupervisorSystemd(false); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if contentChanged && active {
		stopArgs, err := stopSupervisorSystemdForWarmRefresh(service)
		if err != nil {
			var rollbackErr error
			if hadCurrent {
				rollbackErr = restorePreviousSupervisorSystemdInstall(path, service, existing, true)
			} else {
				rollbackErr = rollbackNewSupervisorSystemdInstall(path, service, legacyPresent)
			}
			if rollbackErr != nil {
				fmt.Fprintf(stderr, "gc supervisor install: rollback after systemctl %s failure: %v\n", strings.Join(stopArgs, " "), rollbackErr) //nolint:errcheck // best-effort stderr
			}
			fmt.Fprintf(stderr, "gc supervisor install: systemctl %s: %v\n", strings.Join(stopArgs, " "), err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := cleanupSupervisorWorkspaceServicesForWarmRefresh(data.GCHome); err != nil {
			warnSupervisorSystemdWarmRefreshPreservedUnit(stderr, service)
			fmt.Fprintf(stderr, "gc supervisor install: workspace-service cleanup after systemctl %s: %v\n", strings.Join(stopArgs, " "), err) //nolint:errcheck // best-effort stderr
			return 1
		}
		_ = supervisorSystemctlRun("--user", "reset-failed", service)
		startArgs := []string{"--user", "start", service}
		if err := supervisorSystemctlRun(startArgs...); err != nil {
			warnSupervisorSystemdWarmRefreshPreservedUnit(stderr, service)
			fmt.Fprintf(stderr, "gc supervisor install: systemctl %s: %v\n", strings.Join(startArgs, " "), err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else if !active {
		args := []string{"--user", "start", service}
		if err := supervisorSystemctlRun(args...); err != nil {
			var rollbackErr error
			if hadCurrent {
				rollbackErr = restorePreviousSupervisorSystemdInstall(path, service, existing, true)
			} else {
				rollbackErr = rollbackNewSupervisorSystemdInstall(path, service, legacyPresent)
			}
			if rollbackErr != nil {
				fmt.Fprintf(stderr, "gc supervisor install: rollback after systemctl %s failure: %v\n", strings.Join(args, " "), rollbackErr) //nolint:errcheck // best-effort stderr
			}
			fmt.Fprintf(stderr, "gc supervisor install: systemctl %s: %v\n", strings.Join(args, " "), err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if err := unloadLegacySupervisorSystemd(true); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: warning: %v\n", err) //nolint:errcheck // best-effort stderr
	} else {
		_ = supervisorSystemctlRun("--user", "daemon-reload")
	}

	ensureSupervisorLinger(stdout, stderr)

	fmt.Fprintf(stdout, "Installed systemd service: %s\n", path) //nolint:errcheck // best-effort stdout
	return 0
}

// ensureSupervisorLinger enables systemd user lingering for the current
// account so the installed --user supervisor unit (WantedBy=default.target)
// survives logout and starts at boot without an interactive login. Without
// linger, systemd-logind tears down the user manager on the last logout and
// stops the supervisor, freezing all claimed work until the next login
// (gascity#3683). Linger is an enhancement layered on an already-successful
// install: when it cannot be enabled (e.g. restrictive polkit, unresolved
// user) this loudly warns with the sudo remediation rather than failing the
// install.
func ensureSupervisorLinger(stdout, stderr io.Writer) {
	u, err := currentUserForSystemdHint()
	if err != nil || strings.TrimSpace(u.Username) == "" {
		fmt.Fprintf(stderr, "gc supervisor install: warning: could not resolve the current user to enable systemd lingering; the supervisor will stop on logout and freeze claimed work until next login. Enable it manually: 'sudo loginctl enable-linger <your-user>'.\n") //nolint:errcheck // best-effort stderr
		return
	}
	user := u.Username
	if supervisorLingerEnabled(user) {
		return
	}
	if err := supervisorLoginctlRun("enable-linger", user); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: warning: could not enable systemd lingering for %s (%v); the supervisor will stop on logout and freeze claimed work until next login. Enable it manually: 'sudo loginctl enable-linger %s'.\n", user, err, user) //nolint:errcheck // best-effort stderr
		return
	}
	fmt.Fprintf(stdout, "Enabled systemd lingering for %s so the supervisor survives logout.\n", user) //nolint:errcheck // best-effort stdout
}

// currentUsernameForSystemdHint returns the current username for use in the
// "loginctl enable-linger <user>" hint, falling back to "<your-user>" if
// the lookup fails so the message stays actionable. The osuser.Current
// lookup is reached via a package var so tests can exercise both
// branches.
func currentUsernameForSystemdHint() string {
	if u, err := currentUserForSystemdHint(); err == nil && strings.TrimSpace(u.Username) != "" {
		return u.Username
	}
	return "<your-user>"
}

// currentUserForSystemdHint is overridable in tests.
var currentUserForSystemdHint = osuser.Current

func uninstallSupervisorSystemd(_ *supervisorServiceData, stdout, stderr io.Writer) int {
	path := supervisorSystemdServicePath()
	service := supervisorSystemdServiceName()
	active := supervisorSystemctlActive(service)
	if active {
		if sockPath, _ := runningSupervisorSocket(); sockPath == "" {
			fmt.Fprintf(stderr, "gc supervisor uninstall: systemd service %s is active but the control socket is unavailable; run 'gc supervisor start' to re-adopt sessions, then retry uninstall\n", service) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Socket-protocol stop, never the delegated redirect: uninstall is
		// cleaning up gc's OWN unit and must not stop an operator's
		// delegated unit as a side effect.
		if code := stopSupervisorViaSocket(stdout, stderr, true, 30*time.Second); code != 0 {
			return code
		}
	}
	_ = supervisorSystemctlRun("--user", "stop", service)
	_ = supervisorSystemctlRun("--user", "disable", service)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor uninstall: removing unit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := unloadLegacySupervisorSystemd(true); err != nil {
		fmt.Fprintf(stderr, "gc supervisor uninstall: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	_ = supervisorSystemctlRun("--user", "daemon-reload")
	fmt.Fprintf(stdout, "Uninstalled systemd service: %s\n", path) //nolint:errcheck // best-effort stdout
	return 0
}
