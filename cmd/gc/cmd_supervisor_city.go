package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/supervisor"
	"golang.org/x/term"
)

var (
	// supervisorCityReadyTimeout bounds how long `gc start` and
	// `gc register` wait for the supervisor to report a city as Running.
	// Sized for cities with up to ~40 sessions at the default per-tick
	// wake budget; cities with more sessions bump it via
	// [daemon].start_ready_timeout. Tests override this variable directly.
	supervisorCityReadyTimeout = config.DefaultStartReadyTimeout
	// supervisorCityStopTimeoutFloor preserves the historical default
	// stop/unregister wait floor independently of start-ready sizing.
	supervisorCityStopTimeoutFloor = 180 * time.Second
	supervisorCityPollInterval     = 100 * time.Millisecond
)

// registerCityWithSupervisorTestHook lets tests intercept registration after
// the registry entry is written but before any real supervisor lifecycle runs.
// It is nil in production.
var (
	registerCityWithSupervisorTestHook func(cityPath, commandName string, stdout, stderr io.Writer) (bool, int)
	supervisorCityErrorHook            = supervisorCityError
	reloadSupervisorNoWaitHook         = reloadSupervisorNoWait
)

// assumeYesForSupervisorCycle is set by the --yes flag on commands that
// may trigger a cross-city supervisor reconcile (currently `gc init` and
// `gc register`). When true, confirmCrossCitySupervisorImpact still prints
// the warning (audit trail) but skips the interactive prompt.
var assumeYesForSupervisorCycle bool

// confirmCrossCitySupervisorImpactStdin is the input source for the
// interactive confirmation prompt. Defaults to os.Stdin; tests override
// to inject canned responses.
var confirmCrossCitySupervisorImpactStdin io.Reader = os.Stdin

// confirmCrossCitySupervisorImpactStdinIsTerminal reports whether stdin
// is an interactive terminal (tty). When false (CI, scripts, pipes,
// `< /dev/null`), the guard cannot meaningfully prompt — it falls
// through to a silent proceed after printing the warning, matching
// standard Unix-tool convention for interactive prompts in
// non-interactive contexts. Tests override this hook.
//
// Uses golang.org/x/term.IsTerminal rather than the file-mode-based
// `isTerminalFunc` helper because the latter returns true for /dev/null
// (a character device but not a tty), which gave a false-positive in
// CI acceptance tests where child processes inherited a /dev/null
// stdin from `exec.Command` (see PR #2638 first CI run).
var confirmCrossCitySupervisorImpactStdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

type supervisorRegistry interface {
	List() ([]supervisor.CityEntry, error)
	Register(cityPath, effectiveName string) error
	Unregister(cityPath string) error
}

var newSupervisorRegistry = func() supervisorRegistry {
	return supervisor.NewRegistry(supervisor.RegistryPath())
}

func supervisorCityStartTimeout(cityPath string) time.Duration {
	timeout := supervisorCityReadyTimeout
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return timeout
	}
	// daemon.start_ready_timeout is the canonical operator knob for the
	// start/register ready budget. Only honor an explicit value so tests
	// can shrink the timeout via the package variable without the daemon
	// default silently dominating.
	if cfg.Daemon.StartReadyTimeout != "" {
		timeout = cfg.Daemon.StartReadyTimeoutDuration()
	}
	// session.startup_timeout escape hatch: a single session that takes
	// longer than the ready budget extends the wait so the supervisor
	// has time to surface init failures from that slow session.
	if startup := cfg.Session.StartupTimeoutDuration(); startup > timeout {
		timeout = startup
	}
	return timeout
}

func supervisorCityStopTimeout(cityPath string) time.Duration {
	timeout := supervisorCityStopTimeoutFloor
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return timeout
	}
	if shutdown := cfg.Daemon.ShutdownTimeoutDuration() + 5*time.Second; shutdown > timeout {
		timeout = shutdown
	}
	return timeout
}

func effectiveCityName(cityPath string) (string, error) {
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		return "", fmt.Errorf("materializing builtin packs: %w", err)
	}
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if err != nil {
		return "", err
	}
	return config.EffectiveCityName(cfg, filepath.Base(filepath.Clean(cityPath))), nil
}

func registeredCityName(cityPath, nameOverride string) (string, error) {
	if alias := strings.TrimSpace(nameOverride); alias != "" {
		return alias, nil
	}
	if entry, registered, err := registeredCityEntry(cityPath); err != nil {
		return "", err
	} else if registered {
		return entry.EffectiveName(), nil
	}
	return effectiveCityName(cityPath)
}

func normalizeRegisteredCityPath(cityPath string) (string, error) {
	abs, err := filepath.Abs(cityPath)
	if err != nil {
		return "", err
	}
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		abs = resolved
	}
	return normalizePathForCompare(abs), nil
}

func registeredCityEntry(cityPath string) (supervisor.CityEntry, bool, error) {
	normalized, err := normalizeRegisteredCityPath(cityPath)
	if err != nil {
		return supervisor.CityEntry{}, false, err
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		return supervisor.CityEntry{}, false, err
	}
	for _, entry := range entries {
		if samePath(entry.Path, normalized) {
			return entry, true, nil
		}
	}
	return supervisor.CityEntry{}, false, nil
}

func cityUsesManagedReconciler(cityPath string) bool {
	if controllerAlive(cityPath) != 0 {
		return true
	}
	_, registered, err := registeredCityEntry(cityPath)
	if err != nil || !registered {
		return false
	}
	return supervisorAlive() != 0
}

// justRestartedSupervisorPID records the PID of a supervisor we just
// auto-restarted in this invocation. Set by runStartDriftCheck after a
// successful restart so that ensureNoStandaloneController can recognize
// the new supervisor on the controller socket and not misclassify it as
// a competing standalone during the brief window before the registry
// reflects it managing the city. Zero when no restart has happened in
// this process.
var justRestartedSupervisorPID int

func ensureNoStandaloneController(cityPath string) (int, error) {
	if pid := controllerAlive(cityPath); pid != 0 {
		// If we just auto-restarted the supervisor in this invocation,
		// the new supervisor process is briefly visible on the controller
		// socket before the registry catches up. Treat that as our own
		// supervisor, not a competing standalone controller. Match by
		// PID is deterministic — no polling or sleeping required.
		if justRestartedSupervisorPID != 0 && pid == justRestartedSupervisorPID {
			return 0, nil
		}
		return pid, errControllerAlreadyRunning
	}
	gcDir := filepath.Join(cityPath, ".gc")
	if fi, err := os.Stat(gcDir); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	} else if !fi.IsDir() {
		return 0, nil
	}
	lock, err := acquireControllerLock(cityPath)
	if err == nil {
		lock.Close() //nolint:errcheck // best-effort probe cleanup
		return 0, nil
	}
	if errors.Is(err, errControllerAlreadyRunning) {
		return 0, err
	}
	return 0, err
}

// otherRegisteredCities returns the cities currently in the supervisor
// registry whose normalized path is not the given target. Used to assess
// blast radius before operations that may cycle the shared supervisor.
// Returns nil + the registry error on failure so callers can choose to
// fail-open (proceed without warning) on a registry read failure rather
// than block valid operations.
func otherRegisteredCities(targetCityPath string) ([]supervisor.CityEntry, error) {
	reg := newSupervisorRegistry()
	entries, err := reg.List()
	if err != nil {
		return nil, err
	}
	target := normalizePathForCompare(targetCityPath)
	var others []supervisor.CityEntry
	for _, e := range entries {
		if normalizePathForCompare(e.Path) != target {
			others = append(others, e)
		}
	}
	return others, nil
}

// confirmCrossCitySupervisorImpact warns the operator when registering or
// reconciling cityPath is about to interact with a supervisor that is
// currently managing other registered cities. The reconcile path normally
// uses a graceful socket reload (same supervisor PID), but escalates to a
// non-graceful kill-and-respawn when the supervisor is absent, drifted
// from the local binary, or in a zombie state after a recent
// `gc supervisor stop`. The new supervisor inherits all previously-
// registered cities, cycling their in-flight work without prior notice.
//
// This guard makes the blast radius visible: it enumerates other registered
// cities and asks the operator to confirm before any registry mutation or
// reload command is issued. Single-city and supervisor-absent cases skip
// the prompt (nothing at risk). The --yes flag bypasses the prompt but
// still prints the warning for the audit trail. When stdin is not a
// terminal (CI, scripts, pipes, `< /dev/null`), the guard cannot
// meaningfully prompt — it prints the warning and proceeds silently,
// matching standard Unix-tool convention for interactive prompts in
// non-interactive contexts.
//
// promptOnImpact selects the interactive policy. The registry-mutating
// intent commands (gc init, gc register) pass true: they gate on an
// interactive [y/N] confirmation. Operational entry points (gc start)
// pass false: they print the same warning for the audit trail but proceed
// without blocking, so a multi-city operator's routine start isn't held at
// an unbypassable prompt.
//
// Returns true to proceed, false to abort.
//
// The registry is checked BEFORE supervisorAliveHook so that single-city
// callers (the common case, including most tests with isolated GC_HOME)
// don't burn a probe call to the alive hook. This keeps the guard's
// observable side effects scoped to the actual multi-city case it
// protects against.
//
// Registry read errors are surfaced via stderr but the guard fails open
// (proceeds without blocking) — blocking on a registry read error would
// turn an unrelated I/O fault into an unrelated registration failure,
// which is a worse failure mode than the unguarded reconcile.
func confirmCrossCitySupervisorImpact(cityPath string, promptOnImpact bool, stderr io.Writer) bool {
	others, err := otherRegisteredCities(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "Warning: unable to read city registry while checking cross-city supervisor impact (%v); proceeding without cross-city check.\n", err) //nolint:errcheck // best-effort stderr
		return true
	}
	if len(others) == 0 {
		return true
	}
	if supervisorAliveHook() == 0 {
		return true
	}
	noun := "city"
	if len(others) != 1 {
		noun = "cities"
	}
	fmt.Fprintf(stderr, "Warning: this operation reconciles the supervisor managing %d other registered %s:\n", len(others), noun) //nolint:errcheck // best-effort stderr
	for _, e := range others {
		fmt.Fprintf(stderr, "  - %s (%s)\n", e.EffectiveName(), e.Path) //nolint:errcheck // best-effort stderr
	}
	fmt.Fprintln(stderr, "Reload normally uses a graceful socket reload (same supervisor PID), but escalates to a non-graceful kill-and-respawn") //nolint:errcheck // best-effort stderr
	fmt.Fprintln(stderr, "if the supervisor is absent, drifted, or in a zombie state — which cycles those cities' in-flight work.")               //nolint:errcheck // best-effort stderr
	if assumeYesForSupervisorCycle {
		fmt.Fprintln(stderr, "Continuing (--yes).") //nolint:errcheck // best-effort stderr
		return true
	}
	if !promptOnImpact {
		fmt.Fprintln(stderr, "Proceeding (this command does not gate on cross-city impact; the warning above is recorded for the audit trail).") //nolint:errcheck // best-effort stderr
		return true
	}
	if !confirmCrossCitySupervisorImpactStdinIsTerminal() {
		fmt.Fprintln(stderr, "Continuing (stdin is not a terminal; pass --yes to silence this notice in scripted contexts).") //nolint:errcheck // best-effort stderr
		return true
	}
	fmt.Fprint(stderr, "Continue? [y/N]: ") //nolint:errcheck // best-effort stderr
	br := bufio.NewReader(confirmCrossCitySupervisorImpactStdin)
	line := readLine(br)
	if strings.EqualFold(line, "y") || strings.EqualFold(line, "yes") {
		return true
	}
	fmt.Fprintln(stderr, "Aborted.") //nolint:errcheck // best-effort stderr
	return false
}

func registerCityWithSupervisor(cityPath string, stdout, stderr io.Writer, commandName string, showProgress bool) int {
	return registerCityWithSupervisorNamed(cityPath, "", stdout, stderr, commandName, showProgress)
}

func supervisorAlreadyManagesCity(cityPath string) bool {
	running, _, known := supervisorCityRunningHook(cityPath)
	return known && running
}

func registerCityWithSupervisorNamed(cityPath, nameOverride string, stdout, stderr io.Writer, commandName string, showProgress bool) int {
	cityPath = normalizePathForCompare(cityPath)
	// Only the registry-mutating intent commands gate interactively on
	// cross-city impact. Operational entry points (gc start) and any future
	// caller warn-and-proceed: the guard still prints the audit warning but
	// never blocks, so a multi-city operator's routine start isn't held at an
	// unbypassable prompt. See PR #2638 review.
	promptOnImpact := commandName == "gc init" || commandName == "gc register"
	if !confirmCrossCitySupervisorImpact(cityPath, promptOnImpact, stderr) {
		fmt.Fprintf(stderr, "%s: aborted by user (pass --yes to bypass the cross-city supervisor cycle check)\n", commandName) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !supervisorAlreadyManagesCity(cityPath) {
		if pid, err := ensureNoStandaloneController(cityPath); err != nil {
			if errors.Is(err, errControllerAlreadyRunning) {
				writeStandaloneControllerConflict(stderr, commandName, cityPath, pid)
			} else {
				fmt.Fprintf(stderr, "%s: probing standalone controller: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
			}
			return 1
		}
	}
	if err := ensureLegacyNamedPacksCached(cityPath); err != nil {
		fmt.Fprintf(stderr, "%s: fetching packs: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		fmt.Fprintf(stderr, "%s: materializing builtin packs: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	name, err := registeredCityName(cityPath, nameOverride)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Test hook: intercept before writing to the real registry so tests
	// don't pollute the production cities.toml.
	if registerCityWithSupervisorTestHook != nil {
		if handled, code := registerCityWithSupervisorTestHook(cityPath, commandName, stdout, stderr); handled {
			return code
		}
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, name); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	entry, _, err := registeredCityEntry(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Registered city '%s' (%s)\n", entry.EffectiveName(), entry.Path) //nolint:errcheck // best-effort stdout

	if ensureSupervisorRunningHook(stdout, stderr) != 0 {
		keepRegisteredCity(entry, stderr, commandName, "supervisor did not start")
		return 1
	}
	if reloadSupervisorHook(io.Discard, io.Discard) != 0 {
		// The supervisor may be a zombie from a recent "gc supervisor stop" —
		// alive enough to accept connections but unable to process reload
		// because its main loop has exited. Poll for it to finish dying,
		// start a fresh supervisor, and retry.
		deadline := time.Now().Add(10 * time.Second)
		for supervisorAliveHook() != 0 && time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
		}
		if ensureSupervisorRunningHook(stdout, stderr) != 0 {
			keepRegisteredCity(entry, stderr, commandName, "supervisor did not start after retry")
			return 1
		}
		if reloadSupervisorHook(stdout, stderr) != 0 {
			keepRegisteredCity(entry, stderr, commandName, "reconcile failed")
			return 1
		}
	}
	if supervisorAliveHook() != 0 {
		if showProgress {
			logInitProgress(stdout, 8, "Waiting for supervisor to start city")
		} else if stdout != nil {
			fmt.Fprintln(stdout, "Waiting for supervisor to start city...") //nolint:errcheck // best-effort stdout
		}
		if err := waitForSupervisorCityHook(cityPath, true, supervisorCityStartTimeout(cityPath), stdout); err != nil {
			if retried, retriedErr := retrySupervisorCityStartAfterControllerLock(cityPath, stdout, stderr, err); retried {
				if retriedErr == nil {
					return 0
				}
				err = retriedErr
			}
			keepRegisteredCity(entry, stderr, commandName, err.Error())
			fmt.Fprintf(stderr, "%s: check 'gc supervisor logs' for details\n", commandName) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	return 0
}

// registerCityForAPI is the registry-write portion of async
// POST /v0/city. It records the city in the supervisor registry but
// intentionally does NOT wait for readiness. Callers are responsible
// for emitting any lifecycle events they need before waking the
// reconciler, so event ordering stays deterministic.
//
// It also intentionally omits confirmCrossCitySupervisorImpact. That guard
// is an interactive operator affordance: it warns on stderr and (for
// gc init / gc register) blocks on a [y/N] prompt. The async API path has
// neither a tty to prompt nor a per-request stderr to warn on — its audit
// trail is the city lifecycle event stream (CityCreated, etc.) recorded by
// the caller, not the guard's stderr notice. Cross-city impact for API
// registrations is therefore surfaced through those events rather than the
// interactive guard. See PR #2638 review.
func registerCityForAPI(cityPath, nameOverride string) error {
	cityPath = normalizePathForCompare(cityPath)
	name, err := registeredCityName(cityPath, nameOverride)
	if err != nil {
		return err
	}
	reg := newSupervisorRegistry()
	if err := reg.Register(cityPath, name); err != nil {
		return err
	}
	return nil
}

// reloadSupervisorNoWait sends a "reload" command to the supervisor
// socket without waiting for the reply. Used by registerCityForAPI
// so the async POST /v0/city handler doesn't block on the
// reconciler tick.
func reloadSupervisorNoWait() error {
	sockPath, _ := runningSupervisorSocket()
	if sockPath == "" {
		return errors.New("supervisor is not running; start it with 'gc supervisor start'")
	}
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connecting to supervisor reload socket: %w", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort
	if err := conn.SetWriteDeadline(time.Now().Add(1 * time.Second)); err != nil {
		return fmt.Errorf("setting supervisor reload deadline: %w", err)
	}
	if _, err := conn.Write([]byte("reload\n")); err != nil {
		return fmt.Errorf("writing supervisor reload command: %w", err)
	}
	return nil
}

func retrySupervisorCityStartAfterControllerLock(cityPath string, stdout, stderr io.Writer, startErr error) (bool, error) {
	if startErr == nil || !strings.Contains(startErr.Error(), "city failed to start: controller lock: controller already running") {
		return false, startErr
	}
	if err := waitForSupervisorControllerStopHook(cityPath, supervisorCityStopTimeout(cityPath)); err != nil {
		return true, errors.Join(startErr, fmt.Errorf("previous controller did not finish stopping: %w", err))
	}
	if err := bumpSupervisorCityConfigModTime(cityPath); err != nil {
		return true, errors.Join(startErr, fmt.Errorf("retry trigger failed: %w", err))
	}
	if reloadSupervisorHook(stdout, stderr) != 0 {
		return true, fmt.Errorf("%w; reconcile retry failed", startErr)
	}
	if err := waitForSupervisorCityHook(cityPath, true, supervisorCityStartTimeout(cityPath), stdout); err != nil {
		return true, err
	}
	return true, nil
}

func bumpSupervisorCityConfigModTime(cityPath string) error {
	tomlPath := filepath.Join(cityPath, "city.toml")
	info, err := os.Stat(tomlPath)
	if err != nil {
		return err
	}
	next := time.Now()
	if !next.After(info.ModTime()) {
		next = info.ModTime().Add(time.Second)
	}
	return os.Chtimes(tomlPath, next, next)
}

func writeStandaloneControllerConflict(stderr io.Writer, commandName, cityPath string, pid int) {
	pidSuffix := ""
	authority := "standalone controller"
	if pid != 0 {
		pidSuffix = fmt.Sprintf(" (PID %d)", pid)
		authority = fmt.Sprintf("standalone controller PID %d", pid)
	}
	nextCommand := "gc stop " + shellQuotePath(cityPath) + " && " + supervisorRetryCommand(commandName, cityPath)
	_, _ = fmt.Fprintf(stderr,
		"%s: standalone controller already running for %s%s; supervisor cannot manage this city until it stops\n",
		commandName, shellQuotePath(cityPath), pidSuffix)
	fmt.Fprintf(stderr, "%s: Authority: %s\n", commandName, authority) //nolint:errcheck // best-effort stderr
	fmt.Fprintf(stderr, "%s: Next: %s\n", commandName, nextCommand)    //nolint:errcheck // best-effort stderr
}

func supervisorRetryCommand(commandName, cityPath string) string {
	quotedPath := shellQuotePath(cityPath)
	switch strings.TrimSpace(commandName) {
	case "gc register":
		return "gc register " + quotedPath
	default:
		return "gc start " + quotedPath
	}
}

func keepRegisteredCity(entry supervisor.CityEntry, stderr io.Writer, commandName, reason string) {
	fmt.Fprintf(stderr, "%s: %s; keeping registration for '%s' so the supervisor can retry automatically\n", //nolint:errcheck // best-effort stderr
		commandName, reason, entry.EffectiveName())
}

func waitForSupervisorCity(cityPath string, wantRunning bool, timeout time.Duration, stdout io.Writer) error {
	deadline := time.Now().Add(timeout)
	var lastStatus string
	for {
		running, status, known := supervisorCityRunningHook(cityPath)
		switch {
		case known && running == wantRunning:
			return nil
		case known && !wantRunning:
			return fmt.Errorf("city is still running under supervisor")
		case known && wantRunning && status == "init_failed":
			// If the supervisor reports an init failure, surface the
			// error immediately instead of polling until timeout.
			if errMsg := supervisorCityErrorHook(cityPath); errMsg != "" {
				return fmt.Errorf("city failed to start: %s", errMsg)
			}
			return fmt.Errorf("city failed to start under supervisor")
		case !known && !wantRunning:
			return nil
		case !known && supervisorAliveHook() == 0:
			return fmt.Errorf("supervisor stopped before city became ready")
		}
		if stdout != nil && status != "" && status != lastStatus {
			fmt.Fprintf(stdout, "  %s\n", statusDisplayText(status)) //nolint:errcheck // best-effort stdout
			lastStatus = status
		}
		if time.Now().After(deadline) {
			if wantRunning {
				return fmt.Errorf("city did not become ready under supervisor within %s (increase [daemon].start_ready_timeout or [session].startup_timeout for cities with many or slow-starting sessions)", timeout)
			}
			return fmt.Errorf("city did not stop under supervisor")
		}
		time.Sleep(supervisorCityPollInterval)
	}
}

// supervisorCityError fetches the error message for a city from the supervisor API.
func supervisorCityError(cityPath string) string {
	baseURL, err := supervisorAPIBaseURL()
	if err != nil {
		return ""
	}
	client := api.NewClient(baseURL)
	cities, err := client.ListCities()
	if err != nil {
		return ""
	}
	normalized, err := normalizeRegisteredCityPath(cityPath)
	if err != nil {
		return ""
	}
	for _, city := range cities {
		path, pathErr := normalizeRegisteredCityPath(city.Path)
		if pathErr == nil && path == normalized {
			return city.Error
		}
	}
	return ""
}

// statusDisplayText maps an init status string to a human-readable display line.
func statusDisplayText(status string) string {
	switch status {
	case "loading_config":
		return "Loading configuration..."
	case "starting_bead_store":
		return "Starting bead store..."
	case "resolving_formulas":
		return "Resolving formulas..."
	case "adopting_sessions":
		return "Adopting sessions..."
	case "starting_agents":
		return "Starting agents..."
	default:
		return status + "..."
	}
}

type supervisorUnregisterOptions struct {
	Force bool
}

func unregisterCityFromSupervisor(cityPath string, stdout, stderr io.Writer) (bool, int) {
	return unregisterCityFromSupervisorWithOptions(cityPath, stdout, stderr, "gc unregister", supervisorUnregisterOptions{})
}

func unregisterCityFromSupervisorWithForce(cityPath string, stdout, stderr io.Writer, commandName string, force bool) (bool, int) {
	return unregisterCityFromSupervisorWithOptions(cityPath, stdout, stderr, commandName, supervisorUnregisterOptions{
		Force: force,
	})
}

func unregisterCityFromSupervisorWithOptions(cityPath string, stdout, stderr io.Writer, commandName string, opts supervisorUnregisterOptions) (bool, int) {
	cityPath = normalizePathForCompare(cityPath)
	entry, registered, err := registeredCityEntry(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return false, 1
	}
	if !registered {
		return false, 0
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if opts.Force && supervisorAliveHook() != 0 {
		tryStopControllerWithForce(cityPath, io.Discard, true)
	}
	if err := reg.Unregister(cityPath); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return true, 1
	}

	fmt.Fprintf(stdout, "Unregistered city '%s' (%s)\n", entry.EffectiveName(), entry.Path) //nolint:errcheck // best-effort stdout

	// If the city directory is gone, there's nothing to wait on or restore.
	// Skip the supervisor-side probes that would otherwise spew
	// "probing standalone controller" + "restore failed" on a missing path
	// (the unregister itself already succeeded; the supervisor's next
	// reconcile will drop the dead city).
	if _, statErr := os.Stat(cityPath); errors.Is(statErr, os.ErrNotExist) {
		if supervisorAliveHook() != 0 && reloadSupervisorHook(stdout, stderr) != 0 {
			return true, 1
		}
		return true, 0
	}

	if supervisorAliveHook() != 0 {
		if reloadSupervisorHook(stdout, stderr) != 0 {
			if reErr := reg.Register(entry.Path, entry.EffectiveName()); reErr != nil {
				fmt.Fprintf(stderr, "%s: reconcile failed and restore failed for '%s': %v\n", commandName, entry.EffectiveName(), reErr) //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "%s: reconcile failed; restored registration for '%s'\n", commandName, entry.EffectiveName()) //nolint:errcheck
			}
			return true, 1
		}
		if err := waitForSupervisorCityHook(cityPath, false, supervisorCityStopTimeout(cityPath), nil); err != nil {
			if reErr := reg.Register(entry.Path, entry.EffectiveName()); reErr != nil {
				fmt.Fprintf(stderr, "%s: %v; restore failed for '%s': %v\n", commandName, err, entry.EffectiveName(), reErr) //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "%s: %v; restored registration for '%s'\n", commandName, err, entry.EffectiveName()) //nolint:errcheck
			}
			return true, 1
		}
		if err := waitForSupervisorControllerStopHook(cityPath, supervisorCityStopTimeout(cityPath)); err != nil {
			if reErr := reg.Register(entry.Path, entry.EffectiveName()); reErr != nil {
				fmt.Fprintf(stderr, "%s: %v; restore failed for '%s': %v\n", commandName, err, entry.EffectiveName(), reErr) //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "%s: %v; restored registration for '%s'\n", commandName, err, entry.EffectiveName()) //nolint:errcheck
			}
			return true, 1
		}
	}
	return true, 0
}

var waitForSupervisorControllerStopHook = waitForStandaloneControllerStop

var waitForSupervisorCityHook = waitForSupervisorCity

func supervisorAPIBaseURL() (string, error) {
	cfg, err := supervisor.LoadConfig(supervisor.ConfigPath())
	if err != nil {
		return "", err
	}
	bind := cfg.Supervisor.BindOrDefault()
	switch bind {
	case "0.0.0.0":
		bind = "127.0.0.1"
	case "::", "[::]":
		bind = "::1"
	}
	return fmt.Sprintf("http://%s", net.JoinHostPort(bind, strconv.Itoa(cfg.Supervisor.PortOrDefault()))), nil
}

var supervisorCityRunningHook = supervisorCityRunning

func supervisorCityAPIClient(cityPath string) *api.Client {
	entry, registered, err := registeredCityEntry(cityPath)
	if err != nil || !registered || supervisorAliveHook() == 0 {
		return nil
	}
	if running, _, known := supervisorCityRunningHook(cityPath); !known || !running {
		return nil
	}
	baseURL, err := supervisorAPIBaseURL()
	if err != nil {
		return nil
	}
	return api.NewCityScopedClient(baseURL, entry.EffectiveName())
}

func supervisorCityRunning(cityPath string) (running bool, status string, known bool) {
	if supervisorAliveHook() == 0 {
		return false, "", false
	}
	baseURL, err := supervisorAPIBaseURL()
	if err != nil {
		return false, "", false
	}
	client := api.NewClient(baseURL)
	cities, err := client.ListCities()
	if err != nil {
		return false, "", false
	}
	normalized, err := normalizeRegisteredCityPath(cityPath)
	if err != nil {
		return false, "", false
	}
	for _, city := range cities {
		path, pathErr := normalizeRegisteredCityPath(city.Path)
		if pathErr == nil && path == normalized {
			return city.Running, city.Status, true
		}
	}
	return false, "", false
}
