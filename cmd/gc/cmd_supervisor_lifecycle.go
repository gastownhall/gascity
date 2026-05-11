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
	if pgid <= 1 || pgid == supervisorGetpgrp() {
		return fmt.Errorf("refusing to signal unsafe process group %d", pgid)
	}
	if err := supervisorKill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if err := waitForProcessGroupExit(pgid, timeout); err == nil {
		return nil
	}
	if err := supervisorKill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return waitForProcessGroupExit(pgid, timeout)
}

func waitForProcessGroupExit(pgid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !processGroupAlive(pgid) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process group %d did not exit within %s", pgid, timeout)
		}
		time.Sleep(supervisorProcessGroupPollPeriod)
	}
}

func processGroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := supervisorKill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func newSupervisorRunCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the machine-wide supervisor in the foreground",
		Long: `Run the machine-wide supervisor in the foreground.

This is the canonical long-running control loop. It reads ~/.gc/cities.toml
for registered cities, manages them from one process, and hosts the shared
API server.`,
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
			fmt.Fprintf(stdout, "Supervisor started (PID %d)\n", pid) //nolint:errcheck // best-effort stdout
			return 0
		}
		time.Sleep(supervisorReadyPollInterval)
	}

	fmt.Fprintf(stderr, "gc supervisor start: supervisor did not become ready; see %s\n", logPath) //nolint:errcheck // best-effort stderr
	return 1
}

func ensureSupervisorRunning(stdout, stderr io.Writer) int {
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

Shows recent log output from background and service-managed supervisor runs.`,
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

func doSupervisorLogs(numLines int, follow bool, stdout, stderr io.Writer) int {
	logPath := supervisorLogPath()
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor logs: log file not found: %s\n", logPath) //nolint:errcheck // best-effort stderr
		return 1
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
	return &cobra.Command{
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
}

func doSupervisorInstall(stdout, stderr io.Writer) int {
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

var providerCredentialEnvPrefixes = []string{
	"ANTHROPIC_",
	"GEMINI_",
	"GOOGLE_",
	"OPENAI_",
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
	for _, prefix := range providerCredentialEnvPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
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

func renderSupervisorTemplate(tmplStr string, data *supervisorServiceData) (string, error) {
	funcMap := template.FuncMap{"xmlesc": xmlEscape, "systemdenv": systemdEnv, "systemdpath": strconv.Quote}
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
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor install: reading existing plist: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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
		if code := stopSupervisorWithWait(stdout, stderr, true, 30*time.Second); code != 0 {
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

	path := supervisorSystemdServicePath()
	service := supervisorSystemdServiceName()
	legacyPresent := legacySupervisorTargetsCurrentHome(legacySupervisorSystemdServicePath())
	existing, err := os.ReadFile(path)
	hadCurrent := err == nil
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor install: reading existing unit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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

	fmt.Fprintf(stdout, "Installed systemd service: %s\n", path) //nolint:errcheck // best-effort stdout
	return 0
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
		if code := stopSupervisorWithWait(stdout, stderr, true, 30*time.Second); code != 0 {
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
