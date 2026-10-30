package doctor

import (
	"fmt"
	"strings"
)

// SupervisorBinary is the caller-gathered identity of the running supervisor
// relative to the gc binary running doctor. Callers (cmd/gc) gather it once
// per doctor run — from the process table and the supervisor's /health
// endpoint — so the check itself stays pure data-in, data-out.
type SupervisorBinary struct {
	// ExePath is the executable the supervisor process was started from.
	// Empty when it could not be determined (see ExeErr).
	ExePath string
	// ExeDeleted reports that ExePath was removed or replaced on disk after
	// the supervisor started: it is still executing the old build.
	ExeDeleted bool
	// ExeErr is why ExePath could not be determined, if it could not.
	ExeErr error
	// BuildID and Version are what the supervisor reports on /health. Each
	// is empty when unknown (see StatusErr).
	BuildID string
	Version string
	// StatusErr is why /health could not be read, if it could not.
	StatusErr error
	// LocalBuildID and LocalVersion identify the gc binary running doctor.
	// Each is empty when unknown.
	LocalBuildID string
	LocalVersion string
	// BuildDiffers is the caller's verdict that the supervisor serves a
	// different gc build than the invoking gc. It is false when the two
	// identities cannot be compared.
	BuildDiffers bool
	// ServiceBinary is the gc executable the supervisor's managing service
	// launches on a restart, when that is not ExePath; empty otherwise.
	ServiceBinary string
	// FixHint is the caller's remediation for this deployment, shown when
	// the check warns. It may span several lines.
	FixHint string
}

// SupervisorBinaryCheck warns when the running supervisor executes a gc build
// other than the invoking one: either its executable was removed or replaced
// on disk after it started (package upgrade, `brew unlink`), or it reports a
// different build identity. Otherwise delegating commands such as `gc reload`
// are handled by that older build without any visible hint, and the pack
// helpers it spawns get its stale executable path as GC_BIN.
//
// On macOS, which has no /proc, a removed executable is recognized only when
// it is the binary the installed launchd service launches. A supervisor
// started outside launchd from some other since-removed gc is reported OK but
// not fully verified (executable unavailable) unless its build also differs:
// ps(1) alone cannot tell a removed binary from a stale or truncated command
// name.
type SupervisorBinaryCheck struct {
	supervisorRunning bool
	supervisorPID     int
	info              SupervisorBinary
}

// NewSupervisorBinaryCheck returns a check comparing the running supervisor's
// executable and build identity against the invoking gc. supervisorRunning and
// supervisorPID should come from the same liveness probe used by the other
// supervisor checks in the doctor run.
func NewSupervisorBinaryCheck(supervisorRunning bool, supervisorPID int, info SupervisorBinary) *SupervisorBinaryCheck {
	return &SupervisorBinaryCheck{
		supervisorRunning: supervisorRunning,
		supervisorPID:     supervisorPID,
		info:              info,
	}
}

// Name returns the check identifier.
func (c *SupervisorBinaryCheck) Name() string { return "supervisor-binary" }

// CanFix reports that this check does not support automatic remediation:
// restarting the supervisor restarts every managed city, which is an
// operator decision.
func (c *SupervisorBinaryCheck) CanFix() bool { return false }

// Fix is a no-op; CanFix returns false.
func (c *SupervisorBinaryCheck) Fix(_ *CheckContext) error { return nil }

// Run compares the running supervisor's binary against the invoking gc.
func (c *SupervisorBinaryCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if !c.supervisorRunning {
		r.Status = StatusOK
		r.Message = "supervisor is not running"
		return r
	}
	info := c.info
	var problems []string
	if info.ExeDeleted {
		problems = append(problems, fmt.Sprintf(
			"its executable %s was removed or replaced on disk after it started, so it is still running an old gc build and passes that path to pack helpers as GC_BIN",
			info.ExePath))
	}
	if info.BuildDiffers {
		problems = append(problems, fmt.Sprintf(
			"it serves %s%s but this gc is %s, so commands this gc delegates to it (e.g. 'gc reload') are handled by a different build",
			describeBuild(info.BuildID, info.Version), describeSupervisorExecutable(info),
			describeBuild(info.LocalBuildID, info.LocalVersion)))
	}
	if len(problems) > 0 {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("supervisor (PID %d): %s", c.supervisorPID, strings.Join(problems, "; "))
		r.FixHint = info.FixHint
		return r
	}

	r.Status = StatusOK
	exe := info.ExePath
	if exe == "" {
		exe = "unknown executable"
	}
	r.Message = fmt.Sprintf("supervisor (PID %d) runs %s, %s", c.supervisorPID, exe, describeBuild(info.BuildID, info.Version))
	var unverified []string
	if info.ExeErr != nil {
		unverified = append(unverified, fmt.Sprintf("executable unavailable: %v", info.ExeErr))
	}
	if info.StatusErr != nil {
		unverified = append(unverified, fmt.Sprintf("build identity unavailable: %v", info.StatusErr))
	}
	if len(unverified) > 0 {
		r.Message += " (not fully verified — " + strings.Join(unverified, "; ") + ")"
	}
	return r
}

// describeBuild names a gc build by whichever of its build ID and version
// are known.
func describeBuild(buildID, version string) string {
	switch {
	case buildID != "" && version != "":
		return fmt.Sprintf("build %s (version %s)", buildID, version)
	case buildID != "":
		return "build " + buildID
	case version != "":
		return "version " + version
	}
	return "unknown build"
}

// describeSupervisorExecutable names the file the supervisor runs and, when a
// restart would launch a different one, the binary its service launches. It
// is empty when neither is known.
func describeSupervisorExecutable(info SupervisorBinary) string {
	var desc string
	if info.ExePath != "" {
		desc = " from " + info.ExePath
	}
	if info.ServiceBinary != "" {
		desc += " (its service launches " + info.ServiceBinary + ")"
	}
	return desc
}
