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
	"time"
)

// allowSupervisorMismatch is the --allow-supervisor-mismatch flag shared by
// gc init and gc start. It lets the operator register or start a city with a
// supervisor that runs a different gc installation than the invoking binary.
var allowSupervisorMismatch bool

const allowSupervisorMismatchFlag = "allow-supervisor-mismatch"

// supervisorBinaryProbeTimeout bounds the supervisor /health round-trip the
// mismatch check makes. The check is advisory, so a slow or unreachable
// supervisor must never stall the calling command for long.
var supervisorBinaryProbeTimeout = 2 * time.Second

// supervisorHealthStatusHook fetches the supervisor's /health identity.
// Overridable for tests.
var supervisorHealthStatusHook = func(ctx context.Context, baseURL string) (SupervisorStatus, error) {
	return newHTTPSupervisorClient(baseURL).Status(ctx)
}

// localGCExecutableHook returns the path of the invoking gc binary.
// Overridable for tests.
var localGCExecutableHook = os.Executable

// supervisorServiceBinaryHook reports the gc binary the platform service
// manager launches for the supervisor (launchd ProgramArguments on macOS,
// systemd ExecStart on Linux) together with a human-readable description of
// that service. It returns ("", "") when no active gc-owned service manages
// the supervisor. Overridable for tests.
var supervisorServiceBinaryHook = supervisorServiceBinary

// readProcessExePathViaPSHook resolves a process's executable path with
// ps(1). It is the macOS fallback for /proc/<pid>/exe, which exists only on
// Linux; without it a supervisor forked outside launchd (gc supervisor start)
// would have no known executable path. Overridable for tests.
var readProcessExePathViaPSHook = readProcessExePathViaPS

// readProcessCommViaPSHook returns a process's raw ps(1) comm value, before
// verifiedPSExecutablePath validates it. Overridable for tests.
var readProcessCommViaPSHook = readProcessCommViaPS

func readProcessExePathViaPS(pid int) (string, error) {
	comm, err := readProcessCommViaPSHook(pid)
	if err != nil {
		return "", err
	}
	return verifiedPSExecutablePath(pid, comm)
}

func readProcessCommViaPS(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", fmt.Errorf("ps -p %d: %w", pid, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// verifiedPSExecutablePath accepts a ps(1) comm value only when it names an
// existing regular file. On macOS comm is derived from argv[0] and may be
// relative, truncated, or stale; such a value would misidentify the
// supervisor's binary, so it is rejected and the caller falls back to the
// service definition or to "unknown" (which never refuses).
func verifiedPSExecutablePath(pid int, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("ps -p %d reported non-absolute command %q", pid, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("ps -p %d reported %q: %w", pid, path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("ps -p %d reported %q, which is not a regular file", pid, path)
	}
	return path, nil
}

// gcBinaryIdentity describes one gc binary: where it lives and what it
// reports as its version and build.
type gcBinaryIdentity struct {
	ExePath string
	Version string
	BuildID string
}

// supervisorBinaryMismatch describes a running supervisor whose gc binary
// differs from the invoking gc.
type supervisorBinaryMismatch struct {
	PID        int
	Supervisor gcBinaryIdentity
	Local      gcBinaryIdentity
	// Service names the platform service that manages the supervisor
	// (for example `launchd service "com.gascity.supervisor"`), or "" when
	// the supervisor is not managed by an active gc-owned service.
	Service string
	// RelaunchPath is the gc executable a supervisor restart would launch:
	// the service definition's binary when a service manages the
	// supervisor, otherwise the running process's executable.
	RelaunchPath string
	// RelaunchMissing is true when RelaunchPath is not the invoking gc and
	// no longer exists (an upgrade removed it). No restart can launch it
	// and nothing remains to keep using, but `gc supervisor install`
	// replaces a service file that names it without --force, so stopping
	// the supervisor and running gc start from this gc resolves the
	// mismatch.
	RelaunchMissing bool
	// DifferentInstall is true when restarting the supervisor would launch
	// a different gc executable file than the invoking gc, so no restart
	// can bring it onto this binary; only repointing the service (or using
	// the other binary) resolves it.
	DifferentInstall bool
	// BuildDiffers is true when both sides report a build identity and the
	// identities differ. gc start's binary-drift handling already reacts to
	// this case for a supervisor that runs the same executable path.
	BuildDiffers bool
}

// detectSupervisorBinaryMismatch compares the running supervisor's binary
// with the invoking gc. It reports ok=false when no supervisor is running,
// the supervisor cannot be queried, or the two binaries are known to match.
// Version and build identity come from the supervisor's /health endpoint;
// the executable path comes from the process (Linux /proc) or, when that is
// unreadable (macOS, foreign uid), from the managing service definition.
func detectSupervisorBinaryMismatch() (supervisorBinaryMismatch, bool) {
	pid := supervisorAliveHook()
	if pid == 0 {
		return supervisorBinaryMismatch{}, false
	}
	status, err := readSupervisorHealthStatus()
	if err != nil {
		return supervisorBinaryMismatch{}, false
	}
	m := newSupervisorBinaryMismatch(pid, gcBinaryIdentity{
		ExePath: runningSupervisorExePath(pid),
		Version: status.Version,
		BuildID: status.BuildID,
	})
	return m, m.classify()
}

// newSupervisorBinaryMismatch builds the mismatch model for the supervisor
// running as pid, whose identity is sv (sv.ExePath is "" when the process
// executable is unknown). It adds the managing service, the path a restart
// would relaunch and whether that binary is gone, and the invoking gc's
// identity and executable. classify fills the derived identity fields.
func newSupervisorBinaryMismatch(pid int, sv gcBinaryIdentity) supervisorBinaryMismatch {
	m := supervisorBinaryMismatch{PID: pid, Supervisor: sv, Local: localGCBinaryIdentity()}
	serviceBinary, service := supervisorServiceBinaryHook()
	m.Service = service
	if m.Supervisor.ExePath == "" {
		m.Supervisor.ExePath = serviceBinary
	}
	m.RelaunchPath = serviceBinary
	if m.RelaunchPath == "" {
		m.RelaunchPath = m.Supervisor.ExePath
	}
	if exe, err := localGCExecutableHook(); err == nil {
		m.Local.ExePath = resolveExecutablePath(exe)
	}
	m.RelaunchMissing = supervisorInstallBinaryGuard(m.RelaunchPath, m.Local.ExePath) == supervisorInstallBinaryMissing
	return m
}

// readSupervisorHealthStatus reads the supervisor's /health identity within
// supervisorBinaryProbeTimeout.
func readSupervisorHealthStatus() (SupervisorStatus, error) {
	baseURL, err := supervisorAPIBaseURLHook()
	if err != nil {
		return SupervisorStatus{}, fmt.Errorf("resolving supervisor API URL: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), supervisorBinaryProbeTimeout)
	defer cancel()
	return supervisorHealthStatusHook(ctx, baseURL)
}

// runningSupervisorExePath returns the running supervisor process's
// executable path, or "" when it cannot be determined.
func runningSupervisorExePath(pid int) string {
	if exe, err := readSupervisorExePathHook(pid); err == nil && exe != "" {
		return exe
	}
	if supervisorRuntimeGOOS == "darwin" {
		if exe, err := readProcessExePathViaPSHook(pid); err == nil {
			return exe
		}
	}
	return ""
}

// classify fills the derived fields and reports whether the two binaries
// should be treated as mismatched.
//
// A differing version or build is always a mismatch. Two different
// executable files are a mismatch unless their version or build is known to
// match (identical builds installed at two paths behave identically).
func (m *supervisorBinaryMismatch) classify() bool {
	versionKnown := knownGCVersion(m.Supervisor.Version) && knownGCVersion(m.Local.Version)
	versionDiffers := versionKnown && normalizeGCVersion(m.Supervisor.Version) != normalizeGCVersion(m.Local.Version)
	buildKnown := knownGCBuildID(m.Supervisor.BuildID) && knownGCBuildID(m.Local.BuildID)
	m.BuildDiffers = buildKnown && !sameGCBuildID(m.Supervisor.BuildID, m.Local.BuildID)
	m.DifferentInstall = m.RelaunchPath != "" && m.Local.ExePath != "" &&
		!supervisorSameBinary(m.RelaunchPath, m.Local.ExePath)
	if versionDiffers || m.BuildDiffers {
		return true
	}
	if !m.DifferentInstall {
		return false
	}
	identityMatches := (versionKnown && !versionDiffers) || (buildKnown && !m.BuildDiffers)
	return !identityMatches
}

// supervisorServiceBinary is the production supervisorServiceBinaryHook.
func supervisorServiceBinary() (string, string) {
	if _, delegated, err := supervisorSystemdDelegation(); err != nil || delegated {
		// A delegated unit is operator-owned; gc does not know its ExecStart.
		return "", ""
	}
	switch supervisorRuntimeGOOS {
	case "darwin":
		label := supervisorLaunchdLabel()
		if !supervisorLaunchdActive(label) {
			return "", ""
		}
		plist, err := os.ReadFile(supervisorLaunchdPlistPath())
		if err != nil {
			return "", fmt.Sprintf("launchd service %q", label)
		}
		return supervisorLaunchdPlistGCPath(string(plist)), fmt.Sprintf("launchd service %q", label)
	case "linux":
		service := supervisorSystemdServiceName()
		if !supervisorSystemctlActive(service) {
			return "", ""
		}
		unit, err := os.ReadFile(supervisorSystemdServicePath())
		if err != nil {
			return "", fmt.Sprintf("systemd user unit %q", service)
		}
		return supervisorSystemdExecStartBinary(string(unit)), fmt.Sprintf("systemd user unit %q", service)
	}
	return "", ""
}

func resolveExecutablePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func knownGCVersion(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && v != "dev"
}

func normalizeGCVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

func knownGCBuildID(id string) bool {
	id = strings.TrimSpace(id)
	return id != "" && id != "unknown"
}

// sameGCBuildID compares build identities, accepting an abbreviated commit
// hash on either side (release builds inject a short hash, toolchain stamps
// carry the full one).
func sameGCBuildID(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == b {
		return true
	}
	if strings.HasSuffix(a, dirtySuffix) != strings.HasSuffix(b, dirtySuffix) {
		return false
	}
	a = strings.TrimSuffix(a, dirtySuffix)
	b = strings.TrimSuffix(b, dirtySuffix)
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= minAbbrevCommitLen && strings.HasPrefix(b, a)
}

func describeGCBinary(id gcBinaryIdentity) string {
	v := id.Version
	if !knownGCVersion(v) {
		v = "(unknown version)"
	}
	desc := "gc " + v
	if knownGCBuildID(id.BuildID) {
		desc += " (build " + id.BuildID + ")"
	}
	if id.ExePath != "" {
		desc += " at " + id.ExePath
	} else {
		desc += " at (unknown path)"
	}
	return desc
}

// printSupervisorBinaryMismatch writes the mismatch description and the fix.
// commandName prefixes every line ("gc init", "gc start", "gc status").
func printSupervisorBinaryMismatch(w io.Writer, commandName string, m supervisorBinaryMismatch) {
	supervisorLine := describeGCBinary(m.Supervisor) + fmt.Sprintf(", pid %d", m.PID)
	if m.Service != "" {
		supervisorLine += ", " + m.Service
	}
	fmt.Fprintf(w, "%s: warning: the running gc supervisor does not match this gc binary\n", commandName)                         //nolint:errcheck // best-effort stderr
	fmt.Fprintf(w, "  supervisor: %s\n", supervisorLine)                                                                          //nolint:errcheck // best-effort stderr
	fmt.Fprintf(w, "  this gc:    %s\n", describeGCBinary(m.Local))                                                               //nolint:errcheck // best-effort stderr
	fmt.Fprintln(w, "  Cities are run by the supervisor's gc, not by the gc you invoke, so commands can fail in confusing ways.") //nolint:errcheck // best-effort stderr
	for _, line := range supervisorBinaryMismatchFix(m) {
		fmt.Fprintf(w, "  %s\n", line) //nolint:errcheck // best-effort stderr
	}
}

// supervisorBinaryMismatchFix returns the remediation lines for m.
func supervisorBinaryMismatchFix(m supervisorBinaryMismatch) []string {
	self := "gc"
	if m.Local.ExePath != "" {
		self = shellQuotePath(m.Local.ExePath)
	}
	if d, delegated, err := supervisorSystemdDelegation(); err == nil && delegated {
		return []string{
			fmt.Sprintf("Fix: point systemd unit %s at this gc binary, then run '%s'.", d.Unit, d.commandHint("restart")),
		}
	}
	if m.RelaunchMissing {
		// A plain stop can return before the supervisor exits, and gc start
		// refuses while it still runs.
		return []string{
			fmt.Sprintf("Fix: the gc binary a supervisor restart would launch no longer exists, so restart the supervisor from this gc: '%s supervisor stop --wait', then '%s start'.", self, self),
		}
	}
	if !m.DifferentInstall {
		// A plain stop can return before the supervisor exits, and gc start
		// treats a still-running supervisor as started. When the builds
		// match, no drift check restarts it either.
		return []string{
			fmt.Sprintf("Fix: restart the supervisor so it runs this binary: '%s supervisor stop --wait', then '%s start'.", self, self),
		}
	}
	lines := []string{"Fix, either:"}
	if m.Service != "" {
		lines = append(lines, fmt.Sprintf("  - run cities with this gc: '%s supervisor install --force' (repoints the %s at this binary and restarts it; it serves every registered city), then 'gc start'", self, m.Service))
	} else {
		lines = append(lines, fmt.Sprintf("  - run cities with this gc: stop the other supervisor ('%s supervisor stop'), then rerun with this gc", self))
	}
	if m.RelaunchPath != "" {
		lines = append(lines, fmt.Sprintf("  - or keep the running supervisor and use its gc: %s", shellQuotePath(m.RelaunchPath)))
	}
	return lines
}

// checkSupervisorBinaryBeforeRegister runs the mismatch check for commands
// that are about to hand a city to the running supervisor (gc init, gc start).
//
// A supervisor running a different gc installation blocks the command unless
// --allow-supervisor-mismatch is set: neither command can fix that case on
// its own (restarting the supervisor through its service relaunches the other
// binary), and proceeding registers the city with the wrong gc, which fails
// later and far from the cause. Any other mismatch — the same executable at a
// different version, i.e. an in-place upgrade — only warns, because
// restarting the supervisor resolves it and gc start already does that for
// build drift.
//
// It returns proceed=false when the caller must stop, and reports whether
// a different-install mismatch was accepted via the flag, so gc start can
// skip a binary-drift auto-restart that could only relaunch the other binary.
func checkSupervisorBinaryBeforeRegister(commandName string, stderr io.Writer, warnOnBuildDrift bool) (proceed, acceptedDifferentInstall bool) {
	m, mismatched := detectSupervisorBinaryMismatch()
	if !mismatched {
		return true, false
	}
	if !m.DifferentInstall {
		if !warnOnBuildDrift && detectGCBinaryDrift(m.Local, SupervisorStatus{BuildID: m.Supervisor.BuildID, Version: m.Supervisor.Version}) {
			// gc start's binary-drift handling reports and resolves this.
			return true, false
		}
		printSupervisorBinaryMismatch(stderr, commandName, m)
		return true, false
	}
	printSupervisorBinaryMismatch(stderr, commandName, m)
	if allowSupervisorMismatch {
		fmt.Fprintf(stderr, "%s: continuing with the mismatched supervisor (--%s)\n", commandName, allowSupervisorMismatchFlag) //nolint:errcheck // best-effort stderr
		return true, true
	}
	fmt.Fprintf(stderr, "%s: refusing to hand the city to a supervisor running a different gc installation; fix it as above, or pass --%s to proceed anyway\n", commandName, allowSupervisorMismatchFlag) //nolint:errcheck // best-effort stderr
	return false, false
}

// warnSupervisorBinaryMismatch prints the mismatch warning, if any. Used by
// read-only commands such as gc status that must never change their exit
// code because of it.
func warnSupervisorBinaryMismatch(commandName string, stderr io.Writer) {
	if m, mismatched := detectSupervisorBinaryMismatch(); mismatched {
		printSupervisorBinaryMismatch(stderr, commandName, m)
	}
}
