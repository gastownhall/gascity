package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// driftFlags captures the operator-visible inputs that influence drift
// handling. DryRun and NoAutoRestart are the `--dry-run` and
// `--no-auto-restart` CLI flags; KillSwitchActive reflects the
// `[daemon].auto_restart_on_drift = false` config kill switch.
type driftFlags struct {
	DryRun           bool
	NoAutoRestart    bool
	KillSwitchActive bool
}

// driftCheckResult describes the action `gc start` should take for a
// given (drift state, flags) combination. Exactly one of the four
// disposition fields is set.
type driftCheckResult struct {
	// ProceedNormally means no drift; no action required.
	ProceedNormally bool

	// Restart means drift was detected and the operator authorized an
	// auto-restart. Caller must invoke restartSupervisor + PollReady.
	Restart bool

	// Error means drift was detected and the operator opted out of
	// auto-restart (or the kill switch was active). Caller must print
	// a remediation message and exit non-zero.
	Error bool

	// DryRun means drift was detected but the operator passed
	// `--dry-run`. Caller prints "(would auto-restart; --dry-run)" and
	// exits 0 — the supervisor stays put.
	DryRun bool

	// BinaryDrift is true when the supervisor's reported buildID
	// differs from the local gc binary's. Surfaced separately so
	// callers can tailor the drift report.
	BinaryDrift bool

	// PackDrift lists the directories whose newest mtime is later
	// than the supervisor's recorded ParsedAt. Empty when no pack
	// roots drifted.
	PackDrift []string
}

// decideDriftAction is the pure decision function for `gc start`'s
// drift handling. It is called after a drift check has produced
// (binaryDrift, packDrift); given those plus the operator's flags, it
// returns the single action the caller should take.
//
// The function is intentionally pure: no I/O, no clocks, no globals.
// All twelve flag×outcome combinations from the designer brief
// (§ "Flag-combination matrix") are pinned by table tests, so
// behavioral changes here will surface as test diffs.
func decideDriftAction(localBuildID string, sv SupervisorStatus, packDrift []string, flags driftFlags) driftCheckResult {
	binaryDrift := DetectBinaryDrift(localBuildID, sv)
	hasDrift := binaryDrift || len(packDrift) > 0
	if !hasDrift {
		return driftCheckResult{ProceedNormally: true}
	}
	res := driftCheckResult{
		BinaryDrift: binaryDrift,
		PackDrift:   packDrift,
	}
	switch {
	case flags.DryRun:
		// Dry-run wins over every other flag. The whole point is to
		// observe state without acting.
		res.DryRun = true
	case flags.NoAutoRestart || flags.KillSwitchActive:
		res.Error = true
	default:
		res.Restart = true
	}
	return res
}

// supervisorIdentity is the data printSupervisorIdentity needs to
// render the always-print first line of `gc start` output.
type supervisorIdentity struct {
	PID     int
	ExePath string
	BuildID string
	Started time.Time
}

// printSupervisorIdentity writes the operator-facing first line of
// `gc start` output (FR-5 from the architect's brief). The format is
// pinned by tests:
//
//	Supervisor: pid=… exe=… buildID=… started=…
//
// `started=` is humanized (`2m ago`, `1h ago`, `just now`) per the
// designer brief's a11y guidance. An empty buildID surfaces as
// `buildID=(unknown)` so the operator sees why drift detection might
// be disabled.
func printSupervisorIdentity(w io.Writer, id supervisorIdentity, now time.Time) {
	buildToken := id.BuildID
	if buildToken == "" {
		buildToken = "(unknown)"
	}
	fmt.Fprintf(w, "Supervisor: pid=%d exe=%s buildID=%s started=%s\n", //nolint:errcheck // best-effort stderr
		id.PID, id.ExePath, buildToken, humanizeAge(now.Sub(id.Started)))
}

// humanizeAge formats a positive duration into the operator-friendly
// strings the designer brief requires. Negative durations (clocks
// jumped backward, or Started==zero) collapse to "just now".
func humanizeAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < 30*time.Second:
		return "just now"
	case d < time.Hour:
		minutes := int(d / time.Minute)
		if minutes < 1 {
			minutes = 1
		}
		return fmt.Sprintf("%dm ago", minutes)
	case d < 24*time.Hour:
		hours := int(d / time.Hour)
		return fmt.Sprintf("%dh ago", hours)
	default:
		days := int(d / (24 * time.Hour))
		return fmt.Sprintf("%dd ago", days)
	}
}

// driftReport collects what printDriftReport renders.
type driftReport struct {
	BinaryDrift  bool
	LocalBuildID string
	SupervisorID string
	PackDrifted  []string
}

// printDriftReport writes the `Drift detected:` block. Wording is
// pinned by tests because both runbooks and log scrapers grep against
// these prefixes.
func printDriftReport(w io.Writer, r driftReport) {
	fmt.Fprintln(w, "Drift detected:") //nolint:errcheck // best-effort stderr
	if r.BinaryDrift {
		local := r.LocalBuildID
		if local == "" {
			local = "(unknown)"
		}
		sup := r.SupervisorID
		if sup == "" {
			sup = "(unknown)"
		}
		fmt.Fprintf(w, "  binary: local=%s supervisor=%s\n", local, sup) //nolint:errcheck // best-effort stderr
	}
	for _, dir := range r.PackDrifted {
		fmt.Fprintf(w, "  pack: %s\n", dir) //nolint:errcheck // best-effort stderr
	}
}

// driftReadyTimeout caps how long PollReady waits after a restart for
// the new supervisor to come up. Five seconds matches NFR-2 in the
// architect's brief. It also caps pollDelegatedRestartVerified's wait for
// a delegated restart to land its replacement.
var driftReadyTimeout = 5 * time.Second

// driftVerifyProbeTimeout bounds each supervisor Status probe inside
// pollDelegatedRestartVerified's post-restart verification poll.
var driftVerifyProbeTimeout = 2 * time.Second

// restartHelpersHook lets tests substitute a fake set of helpers for
// the production kill+spawn (or systemctl) side effects.
var restartHelpersHook = defaultRestartHelpers

// readSupervisorExePathHook lets tests avoid platform-specific /proc lookups.
var readSupervisorExePathHook = readSupervisorExePath

// driftRestartLoopMax / driftRestartLoopWindow define the loop-guard
// budget: at most 3 supervisor auto-restarts may occur within any
// 60-second window. Persistence is via driftRestartHistoryPath so the
// budget survives across `gc start` invocations — an in-memory guard
// would reset each cycle and never refuse a runaway loop.
const (
	driftRestartLoopMax    = 3
	driftRestartLoopWindow = 60 * time.Second
)

// runStartDriftCheck performs supervisor binary-drift detection and
// optionally auto-restarts the supervisor. It is called from
// `gc start` after the city path has been resolved but before the
// supervisor handshake — at that point we know:
//
//   - whether a supervisor is already running (supervisorAlive)
//   - the kill-switch state (city.toml [daemon].auto_restart_on_drift)
//   - the operator's flags (--no-auto-restart, --dry-run)
//
// Returns (exitCode, continue) where continue=false means the caller
// should `return exitCode` immediately. continue=true means no terminal
// drift action happened and the caller should proceed with normal start.
func runStartDriftCheck(cityPath string, stdout, stderr io.Writer) (int, bool) {
	pid := supervisorAliveHook()
	if pid == 0 {
		// No supervisor running. There's nothing to be stale relative
		// to; the registration step will spawn a fresh one.
		return 0, true
	}

	exePath, exeErr := readSupervisorExePathHook(pid)
	baseURL, urlErr := supervisorAPIBaseURLHook()
	if urlErr != nil {
		// Without a base URL we can't query /health. Don't block
		// startup — just continue silently. (The operator's `gc start`
		// today doesn't do drift detection, so we prefer fail-open.)
		return 0, true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := newHTTPSupervisorClient(baseURL)
	status, statusErr := client.Status(ctx)
	if statusErr != nil {
		// Supervisor is alive (PID exists) but /health didn't respond.
		// Don't block startup; the registration step has its own retry.
		return 0, true
	}

	now := time.Now()
	identity := supervisorIdentity{
		PID:     pid,
		ExePath: exePath,
		BuildID: status.BuildID,
		Started: now.Add(-time.Duration(status.UptimeSec) * time.Second),
	}
	if exeErr != nil {
		identity.ExePath = "(unreadable)"
	}
	printSupervisorIdentity(stdout, identity, now)

	flags := driftFlags{
		DryRun:           dryRunMode,
		NoAutoRestart:    noAutoRestartMode,
		KillSwitchActive: !readDaemonAutoRestart(cityPath),
	}
	res := decideDriftAction(commit, status, nil, flags)

	switch {
	case res.ProceedNormally:
		return 0, true
	case res.DryRun:
		printDriftReport(stdout, driftReport{
			BinaryDrift:  res.BinaryDrift,
			LocalBuildID: commit,
			SupervisorID: status.BuildID,
			PackDrifted:  res.PackDrift,
		})
		fmt.Fprintln(stdout, "(would auto-restart; --dry-run)") //nolint:errcheck // best-effort stdout
		return 0, false
	case res.Error:
		printDriftReport(stderr, driftReport{
			BinaryDrift:  res.BinaryDrift,
			LocalBuildID: commit,
			SupervisorID: status.BuildID,
			PackDrifted:  res.PackDrift,
		})
		if flags.KillSwitchActive {
			fmt.Fprintf(stderr, "error: supervisor binary drift; auto-restart disabled by [daemon].auto_restart_on_drift in city.toml. Restart manually with '%s'.\n", supervisorRestartGuidance()) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintf(stderr, "error: supervisor binary drift; rerun 'gc start' (or '%s') to apply changes.\n", supervisorRestartGuidance()) //nolint:errcheck // best-effort stderr
		}
		return 1, false
	case res.Restart:
		printDriftReport(stdout, driftReport{
			BinaryDrift:  res.BinaryDrift,
			LocalBuildID: commit,
			SupervisorID: status.BuildID,
			PackDrifted:  res.PackDrift,
		})
		delegation, delegated, derr := supervisorSystemdDelegation()
		if derr != nil {
			fmt.Fprintf(stderr, "error: cannot auto-restart supervisor: %v\n", derr) //nolint:errcheck // best-effort stderr
			return 1, false
		}
		serviceName := supervisorSystemdServiceName()
		systemdManaged := !delegated && supervisorSystemctlActive(serviceName)
		launchdLabel := supervisorLaunchdLabel()
		launchdManaged := !delegated && supervisorRuntimeGOOS == "darwin" && supervisorLaunchdActive(launchdLabel)
		if exeErr != nil && !delegated && !systemdManaged && !launchdManaged {
			// We can't safely auto-restart a supervisor whose
			// /proc/<pid>/exe we can't read — the kernel readlink is
			// the only reliable way to learn which binary to spawn,
			// and falling back to os.Executable() would launch the
			// caller's gc, not necessarily the supervisor's. The
			// usual cause is a uid mismatch between the operator
			// running gc start and the supervisor itself. Surface a
			// descriptive error rather than the silent
			// `(unreadable)` fallback so the operator can fix the
			// uid or opt out via --no-auto-restart.
			printUnreadableSupervisorRestartError(stderr, pid, exeErr)
			return 1, false
		}
		if !recordDriftRestartAttempt(driftRestartHistoryPath(), driftRestartLoopMax, driftRestartLoopWindow, now) {
			fmt.Fprintln(stderr, "error: supervisor restart loop detected (3 restarts in 60s); refusing further restarts. Investigate the stale state with 'gc trace' and consider 'gc stop --force'.") //nolint:errcheck // best-effort stderr
			return 1, false
		}
		spec := restartSpec{
			SystemdManaged: systemdManaged,
			LaunchdManaged: launchdManaged,
			PID:            pid,
			ExePath:        exePath,
			Argv:           []string{"supervisor", "run"},
			ServiceName:    serviceName,
			LaunchdLabel:   launchdLabel,
		}
		mode := "direct"
		switch {
		case delegated:
			mode = "systemd-delegated"
		case systemdManaged:
			mode = "systemd-managed"
		case launchdManaged:
			mode = "launchd-managed"
		}
		fmt.Fprintf(stdout, "Restarting supervisor (%s)...", mode) //nolint:errcheck // best-effort stdout
		t0 := time.Now()
		var restartErr error
		if delegated {
			// try-restart: restart only if the unit is running. The drift
			// path only fires when a supervisor is alive, and a stopped
			// delegated unit must stay stopped — its operator owns starts.
			restartErr = runDelegatedSystemctlTimeout(delegation, "try-restart", delegatedSystemctlJobTimeout)
		} else {
			restartErr = restartSupervisor(spec, restartHelpersHook())
		}
		// A bounded delegated try-restart timeout is not terminal: the
		// restart job can still complete inside systemd after the CLI stops
		// waiting, so fall through to PollReady and the post-restart drift
		// verification below, which confirm whether the supervisor was
		// actually replaced. Ordinary restart failures stay terminal.
		if restartErr != nil && !isDelegatedSystemctlTimeout(restartErr) {
			fmt.Fprintln(stdout)                                                      //nolint:errcheck // best-effort stdout
			fmt.Fprintf(stderr, "error: supervisor restart failed: %v\n", restartErr) //nolint:errcheck // best-effort stderr
			return 1, false
		}
		if delegated {
			// A delegated try-restart can fall through a bounded CLI timeout
			// (or restart the unit asynchronously) with systemd's restart job
			// still in flight, so the OLD supervisor may keep answering
			// /health for a moment. A single early probe — or PollReady, which
			// answers from whatever serves /health — would then misreport "was
			// not replaced" before the late replacement lands. Poll for genuine
			// replacement *and* drift-clearance evidence until it is observed
			// or driftReadyTimeout expires, then surface the last obstacle.
			if msg := pollDelegatedRestartVerified(baseURL, pid, status.BuildID, commit, delegation, driftReadyTimeout); msg != "" {
				fmt.Fprintln(stdout)             //nolint:errcheck // best-effort stdout
				fmt.Fprintf(stderr, "%s\n", msg) //nolint:errcheck // best-effort stderr
				return 1, false
			}
		} else {
			// Wait for the new supervisor to come up.
			if err := PollReady(newHTTPSupervisorClient(baseURL), driftReadyTimeout); err != nil {
				fmt.Fprintln(stdout)                                                                                                                                                  //nolint:errcheck // best-effort stdout
				fmt.Fprintf(stderr, "error: supervisor restart timed out after %s; check '%s' for details. Last known pid=%d.\n", driftReadyTimeout, supervisorStatusGuidance(), pid) //nolint:errcheck // best-effort stderr
				return 1, false
			}
		}
		fmt.Fprintf(stdout, " ready (%s).\n", humanizeReadyDuration(time.Since(t0))) //nolint:errcheck // best-effort stdout

		// Re-print the Supervisor: line so the operator's last memory
		// is of the post-restart state, not the drift report.
		newPID := supervisorAliveHook()
		if newPID != 0 {
			// Record the just-restarted PID so the downstream
			// registerCityWithSupervisorNamed → ensureNoStandaloneController
			// check doesn't misclassify our own new supervisor as a
			// competing standalone during the brief window before the
			// registry catches up.
			justRestartedSupervisorPID = newPID
			newExe, _ := readSupervisorExePathHook(newPID)
			ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel2()
			newStatus, statusErr2 := newHTTPSupervisorClient(baseURL).Status(ctx2)
			if statusErr2 == nil {
				printSupervisorIdentity(stdout, supervisorIdentity{
					PID:     newPID,
					ExePath: newExe,
					BuildID: newStatus.BuildID,
					Started: time.Now().Add(-time.Duration(newStatus.UptimeSec) * time.Second),
				}, time.Now())
			}
		}
		// Successful auto-restart is non-terminal: the caller continues
		// into normal supervisor registration / start so the requested
		// city actually comes up. Returning false here would leave the
		// restarted supervisor running but the city un-registered.
		return 0, true
	}
	// Unreachable; decideDriftAction always sets exactly one disposition.
	return 0, true
}

// pollDelegatedRestartVerified polls the supervisor API after a delegated
// `systemctl try-restart` until it observes that the supervisor was both
// replaced (a new PID, or a new served build identity) and is no longer
// drifted (the served build matches localBuildID), or readyTimeout
// expires. It returns "" on success; otherwise it returns the
// operator-facing diagnostic for the last obstacle observed.
//
// Polling — rather than a single probe — is load-bearing for the
// bounded-timeout fall-through: a `systemctl try-restart` that exceeds
// delegatedSystemctlJobTimeout leaves systemd's restart job running while
// the OLD supervisor still answers /health, so verifying once would
// misreport "was not replaced" before the late replacement lands. The
// failure arms it can report — an unverifiable probe, a supervisor that was
// never replaced, and a replacement whose ExecStart still serves the
// drifted build — mirror the single-probe checks they replace.
func pollDelegatedRestartVerified(baseURL string, oldPID int, oldBuildID, localBuildID string, d systemdDelegation, readyTimeout time.Duration) string {
	client := newHTTPSupervisorClient(baseURL)
	deadline := time.Now().Add(readyTimeout)
	var lastMsg string
	for {
		vctx, vcancel := context.WithTimeout(context.Background(), driftVerifyProbeTimeout)
		verifyStatus, verifyErr := client.Status(vctx)
		vcancel()
		switch {
		case verifyErr != nil:
			lastMsg = fmt.Sprintf("error: cannot verify supervisor after '%s': %v; check '%s'", d.commandHint("try-restart"), verifyErr, supervisorStatusGuidance())
		default:
			verifyPID := supervisorAliveHook()
			switch {
			case verifyPID == oldPID && verifyStatus.BuildID == oldBuildID:
				lastMsg = fmt.Sprintf("error: supervisor was not replaced by '%s': PID %d still serving build %s; it is not managed by delegated unit %s — stop it with 'gc supervisor stop' (%s unset), or fix the delegation env", d.commandHint("try-restart"), verifyPID, verifyStatus.BuildID, d.Unit, supervisorSystemdUnitEnv)
			case DetectBinaryDrift(localBuildID, verifyStatus):
				lastMsg = fmt.Sprintf("error: supervisor restarted by '%s' but still serves drifted build %s (local build %s); unit %s's ExecStart does not launch the updated gc binary — point the unit at the new binary, or fix the delegation env", d.commandHint("try-restart"), verifyStatus.BuildID, localBuildID, d.Unit)
			default:
				// Replaced and drift cleared.
				return ""
			}
		}
		if !time.Now().Before(deadline) {
			return lastMsg
		}
		time.Sleep(supervisorReadyPollInterval)
	}
}

func printUnreadableSupervisorRestartError(stderr io.Writer, pid int, exeErr error) {
	if supervisorRuntimeGOOS == "darwin" {
		fmt.Fprintf(stderr, "error: cannot auto-restart supervisor: macOS cannot resolve the executable path for a direct supervisor (%v). If the supervisor is launchd-managed, rerun 'gc supervisor install' and then 'gc start'; otherwise stop the supervisor manually and rerun 'gc start', or pass --no-auto-restart to skip the restart and surface the drift as an error.\n", exeErr) //nolint:errcheck // best-effort stderr
		return
	}
	fmt.Fprintf(stderr, "error: cannot auto-restart supervisor: /proc/%d/exe is owned by a different user (permission denied: %v). Either rerun gc start as the supervisor's uid, or pass --no-auto-restart to skip the restart and surface the drift as an error.\n", pid, exeErr) //nolint:errcheck // best-effort stderr
}

// readSupervisorExePath returns the resolved path of the supervisor's
// executable via /proc/<pid>/exe. The kernel readlink resolves
// symlinks for us — no extra realpath layer needed.
//
// When the binary on disk has been replaced (the typical drift case:
// `go install` writes a new file at the same path, unlinking the
// original inode the supervisor still has open), the kernel decorates
// the link target with a literal " (deleted)" suffix. We strip that
// suffix because the on-disk path is what the auto-restart needs to
// spawn — the new bytes already live at the un-suffixed path.
func readSupervisorExePath(pid int) (string, error) {
	target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(target, " (deleted)"), nil
}

// readDaemonAutoRestart loads city.toml's [daemon].auto_restart_on_drift
// setting. Returns true (the safe default) when the config can't be
// loaded — startup-blocking config errors are a separate concern and
// shouldn't piggy-back on the drift path.
func readDaemonAutoRestart(cityPath string) bool {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := config.Load(fsys.OSFS{}, tomlPath)
	if err != nil {
		return true
	}
	return cfg.Daemon.AutoRestartOnDriftEnabled()
}

// defaultRestartHelpers wires the production restartHelpers using the
// existing mockable supervisorSystemctlRun hook for systemd, and
// syscall.Kill / a backgrounded exec.Cmd for direct launches.
func defaultRestartHelpers() restartHelpers {
	return restartHelpers{
		Systemctl: supervisorSystemctlRun,
		Launchctl: supervisorLaunchctlRun,
		Kill: func(pid int) error {
			return syscall.Kill(pid, syscall.SIGTERM)
		},
		WaitExit: func(pid int) error {
			return waitForPIDExit(pid, driftKillTimeout, driftKillEscalateTimeout)
		},
		Spawn: spawnDetachedSupervisor,
	}
}

// driftKillTimeout caps how long the direct-restart path waits for the
// SIGTERM'd supervisor to actually exit before escalating to SIGKILL.
// Five seconds matches the supervisor's own graceful-shutdown budget.
//
// driftKillEscalateTimeout caps the post-SIGKILL wait. The kernel
// reaps almost immediately; one second is a generous upper bound.
var (
	driftKillTimeout         = 5 * time.Second
	driftKillEscalateTimeout = 1 * time.Second
)

// waitForPIDExit blocks until the process at pid is gone, escalating
// to SIGKILL if SIGTERM did not take effect within timeout. Returns
// nil once the kernel reports ESRCH on a signal-zero probe.
//
// PID-recycling races are not addressed here — the window between
// SIGTERM and SIGKILL is short enough (seconds) that a recycled PID
// reaching the same value is vanishingly unlikely under normal load.
func waitForPIDExit(pid int, timeout, escalate time.Duration) error {
	if pidGone(pid) {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if pidGone(pid) {
			return nil
		}
	}
	// Escalate to SIGKILL. We deliberately ignore the SIGKILL error —
	// the only failure mode here is that the process already exited
	// between our last probe and the signal call, which is not a real
	// error from the caller's perspective.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	deadline = time.Now().Add(escalate)
	for time.Now().Before(deadline) {
		if pidGone(pid) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("pid %d still alive after SIGKILL", pid)
}

// pidGone reports whether the given pid no longer represents a live
// process — either the entry has been reaped (ESRCH on signal-zero)
// or it has exited and is awaiting wait() from its parent (zombie).
// Both cases mean the process can no longer hold ports or files, so
// the supervisor restart can safely proceed.
//
// We probe via signal-zero first because it covers both "PID never
// existed" and "PID was reaped" without an extra /proc syscall. The
// /proc/<pid>/status fallback handles the zombie case that signal
// zero reports as alive.
func pidGone(pid int) bool {
	if err := syscall.Kill(pid, syscall.Signal(0)); err == syscall.ESRCH {
		return true
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		// If /proc/<pid>/status is missing, the kernel has already
		// torn down the entry — ESRCH-equivalent.
		return os.IsNotExist(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "State:") {
			continue
		}
		// State lines look like "State:\tZ (zombie)" or "State:\tR
		// (running)" — a zombie has already released its ports and
		// FDs even though the parent has not reaped it.
		return strings.Contains(line, "Z")
	}
	return false
}

// humanizeReadyDuration formats a sub-minute duration as `0.7s`-style
// for the `Restarting... ready (X)` line.
func humanizeReadyDuration(d time.Duration) string {
	secs := d.Seconds()
	return fmt.Sprintf("%.1fs", secs)
}

// spawnDetachedSupervisor starts a backgrounded supervisor process,
// inheriting the operator's environment and writing logs to the same
// path doSupervisorStart uses. The child is fully detached so the
// `gc start` invocation can return without orphaning it.
func spawnDetachedSupervisor(exe string, argv ...string) error {
	logPath := supervisorLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("creating log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening log: %w", err)
	}
	defer logFile.Close() //nolint:errcheck // best-effort cleanup
	child := exec.Command(exe, argv...)
	child.SysProcAttr = backgroundSysProcAttr()
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	child.Env = os.Environ()
	return child.Start()
}
