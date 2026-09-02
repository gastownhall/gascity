package doctor

import "fmt"

// SupervisorUnitOwnership is the caller-gathered relationship between a live
// supervisor PID and gc's own systemd user unit (if any). Callers (cmd/gc)
// compute this once per doctor run — mirroring how supervisorRunning is
// gathered before NewSupervisorHTTPCheck — so the check itself stays pure
// data-in, data-out and internal/doctor never needs to shell out to
// systemctl or import cmd/gc.
type SupervisorUnitOwnership struct {
	// Status is one of "owned", "outside_unit", "no_unit".
	Status string
	// Unit is the systemd user unit name (e.g. "gc-supervisor.service").
	// Empty when Status is "no_unit".
	Unit string
	// UnitActive reports systemd's is-active state for Unit.
	UnitActive bool
	// UnitPID is the MainPID systemd tracks for Unit (0 if unknown).
	UnitPID int
}

// SupervisorUnitOwnershipCheck verifies that a live supervisor process is
// owned by gc's own systemd user unit whenever that unit is installed. A
// supervisor forked directly (e.g. `gc supervisor start` from an agent's
// tmux pane, or any path that bypasses systemd) can end up running outside
// the unit's Restart=always policy — silently dropping the
// preserve-sessions-on-signal contract the unit provides (ga-9pjtoy).
type SupervisorUnitOwnershipCheck struct {
	supervisorRunning bool
	supervisorPID     int
	ownership         SupervisorUnitOwnership
}

// NewSupervisorUnitOwnershipCheck returns a check configured to compare the
// live supervisor PID against gc's own systemd user unit. All three
// arguments should come from the same liveness probe and unit-ownership
// lookup already performed once per doctor run in cmd/gc.
func NewSupervisorUnitOwnershipCheck(supervisorRunning bool, supervisorPID int, ownership SupervisorUnitOwnership) *SupervisorUnitOwnershipCheck {
	return &SupervisorUnitOwnershipCheck{
		supervisorRunning: supervisorRunning,
		supervisorPID:     supervisorPID,
		ownership:         ownership,
	}
}

// Name returns the check identifier.
func (c *SupervisorUnitOwnershipCheck) Name() string { return "supervisor-unit-ownership" }

// CanFix reports that this check does not support automatic remediation:
// the remedy (systemctl --user reset-failed, or gc supervisor install) is
// an operator decision about which lifecycle should own the process.
func (c *SupervisorUnitOwnershipCheck) CanFix() bool { return false }

// Fix is a no-op; CanFix returns false.
func (c *SupervisorUnitOwnershipCheck) Fix(_ *CheckContext) error { return nil }

// Run compares the live supervisor PID against gc's own systemd user unit.
func (c *SupervisorUnitOwnershipCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if !c.supervisorRunning {
		r.Status = StatusOK
		r.Message = "supervisor is not running"
		return r
	}
	switch c.ownership.Status {
	case "no_unit":
		r.Status = StatusOK
		r.Message = "no systemd unit installed for the supervisor"
	case "owned":
		r.Status = StatusOK
		r.Message = fmt.Sprintf("supervisor (PID %d) is owned by systemd unit %s", c.supervisorPID, c.ownership.Unit)
	default:
		r.Status = StatusError
		r.Message = fmt.Sprintf(
			"supervisor is running (PID %d) outside its systemd unit %s (unit PID %d, unit active=%v) — systemd's Restart=always is not protecting it",
			c.supervisorPID, c.ownership.Unit, c.ownership.UnitPID, c.ownership.UnitActive,
		)
		r.FixHint = fmt.Sprintf(
			"run 'systemctl --user reset-failed %s && systemctl --user start %s' to let the unit reclaim the process, or 'gc supervisor install' to reinstall it",
			c.ownership.Unit, c.ownership.Unit,
		)
	}
	return r
}
