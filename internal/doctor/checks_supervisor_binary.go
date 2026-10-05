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
	// ExeDeleted reports that ExePath no longer exists on disk: the
	// supervisor is still executing a removed or replaced build.
	ExeDeleted bool
	// ExeErr is why ExePath could not be determined, if it could not.
	ExeErr error
	// BuildID and Version are what the supervisor reports on /health.
	// Empty when unavailable (see StatusErr).
	BuildID string
	Version string
	// StatusErr is why /health could not be read, if it could not.
	StatusErr error
	// LocalBuildID and LocalVersion identify the gc binary running doctor.
	LocalBuildID string
	LocalVersion string
	// RestartHint is the operator command that restarts the supervisor from
	// the current gc binary in this deployment mode.
	RestartHint string
}

// unknownBuildID is the build identity gc reports when no VCS stamp exists.
const unknownBuildID = "unknown"

// SupervisorBinaryCheck warns when the running supervisor executes a gc build
// other than the invoking one: either its executable was removed from disk
// after it started (package upgrade, `brew unlink`), or it reports a different
// build identity. Delegating commands such as `gc reload` are otherwise
// handled by that older build without any visible hint, and pack helpers it
// spawns inherit its dead executable path as GC_BIN.
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
			"its executable %s no longer exists on disk, so it is still running a removed or replaced gc build and passes that dead path to pack helpers as GC_BIN",
			info.ExePath))
	}
	if knownBuildID(info.BuildID) && knownBuildID(info.LocalBuildID) && info.BuildID != info.LocalBuildID {
		problems = append(problems, fmt.Sprintf(
			"it serves build %s but this gc is build %s, so commands this gc delegates to it (e.g. 'gc reload') are handled by a different build",
			describeBuild(info.BuildID, info.Version), describeBuild(info.LocalBuildID, info.LocalVersion)))
	}
	if len(problems) > 0 {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("supervisor (PID %d): %s", c.supervisorPID, strings.Join(problems, "; "))
		r.FixHint = "restart the supervisor from the current gc binary: " + info.RestartHint
		return r
	}

	r.Status = StatusOK
	exe := info.ExePath
	if exe == "" {
		exe = "unknown executable"
	}
	build := "unknown build"
	if knownBuildID(info.BuildID) {
		build = "build " + describeBuild(info.BuildID, info.Version)
	}
	r.Message = fmt.Sprintf("supervisor (PID %d) runs %s, %s", c.supervisorPID, exe, build)
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

func knownBuildID(id string) bool {
	return id != "" && id != unknownBuildID
}

func describeBuild(buildID, version string) string {
	if version == "" {
		return buildID
	}
	return fmt.Sprintf("%s (version %s)", buildID, version)
}
