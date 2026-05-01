package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// architect's brief.
var driftReadyTimeout = 5 * time.Second

// driftRestartLoopGuard rate-limits supervisor auto-restarts so a
// misbehaving start path can't thrash the system. Three restarts in a
// 60-second window is the architect's threshold.
var driftRestartLoopGuard = newRestartLoopGuard(3, 60*time.Second)

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
// should `return exitCode` immediately. continue=true means drift
// handling completed successfully (no drift, restart succeeded, or
// dry-run reported the result) and the caller should proceed.
func runStartDriftCheck(cityPath string, stdout, stderr io.Writer) (int, bool) {
	pid := supervisorAliveHook()
	if pid == 0 {
		// No supervisor running. There's nothing to be stale relative
		// to; the registration step will spawn a fresh one.
		return 0, true
	}

	exePath, exeErr := readSupervisorExePath(pid)
	baseURL, urlErr := supervisorAPIBaseURL()
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
			fmt.Fprintln(stderr, "error: supervisor binary/pack drift; auto-restart disabled by [daemon].auto_restart_on_drift in city.toml. Restart manually with 'systemctl --user restart gascity-supervisor'.") //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintln(stderr, "error: supervisor binary/pack drift; rerun 'gc start' (or 'systemctl --user restart gascity-supervisor') to apply changes.") //nolint:errcheck // best-effort stderr
		}
		return 1, false
	case res.Restart:
		printDriftReport(stdout, driftReport{
			BinaryDrift:  res.BinaryDrift,
			LocalBuildID: commit,
			SupervisorID: status.BuildID,
			PackDrifted:  res.PackDrift,
		})
		if !driftRestartLoopGuard.allowAt(now) {
			fmt.Fprintln(stderr, "error: supervisor restart loop detected (3 restarts in 60s); refusing further restarts. Investigate the stale state with 'gc trace' and consider 'gc stop --force'.") //nolint:errcheck // best-effort stderr
			return 1, false
		}
		serviceName := supervisorSystemdServiceName()
		systemdManaged := supervisorSystemctlActive(serviceName)
		spec := restartSpec{
			SystemdManaged: systemdManaged,
			PID:            pid,
			ExePath:        exePath,
			Argv:           []string{"supervisor", "run"},
			ServiceName:    serviceName,
		}
		mode := "direct"
		if systemdManaged {
			mode = "systemd-managed"
		}
		fmt.Fprintf(stdout, "Restarting supervisor (%s)...", mode) //nolint:errcheck // best-effort stdout
		t0 := time.Now()
		if err := restartSupervisor(spec, defaultRestartHelpers()); err != nil {
			fmt.Fprintln(stdout)                                               //nolint:errcheck // best-effort stdout
			fmt.Fprintf(stderr, "error: supervisor restart failed: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1, false
		}
		// Wait for the new supervisor to come up.
		if err := PollReady(newHTTPSupervisorClient(baseURL), driftReadyTimeout); err != nil {
			fmt.Fprintln(stdout)                                                                                                                                                              //nolint:errcheck // best-effort stdout
			fmt.Fprintf(stderr, "error: supervisor restart timed out after %s; check 'systemctl --user status gascity-supervisor' for details. Last known pid=%d.\n", driftReadyTimeout, pid) //nolint:errcheck // best-effort stderr
			return 1, false
		}
		fmt.Fprintf(stdout, " ready (%s).\n", humanizeReadyDuration(time.Since(t0))) //nolint:errcheck // best-effort stdout

		// Re-print the Supervisor: line so the operator's last memory
		// is of the post-restart state, not the drift report.
		newPID := supervisorAliveHook()
		if newPID != 0 {
			newExe, _ := readSupervisorExePath(newPID)
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
		return 0, true
	}
	// Unreachable; decideDriftAction always sets exactly one disposition.
	return 0, true
}

// readSupervisorExePath returns the resolved path of the supervisor's
// executable via /proc/<pid>/exe (Linux) or os.Executable() as a
// best-effort fallback. The kernel readlink resolves symlinks for us
// — no extra realpath layer needed.
func readSupervisorExePath(pid int) (string, error) {
	target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err == nil {
		return target, nil
	}
	return "", err
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
		Kill: func(pid int) error {
			return syscall.Kill(pid, syscall.SIGTERM)
		},
		Spawn: spawnDetachedSupervisor,
	}
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
