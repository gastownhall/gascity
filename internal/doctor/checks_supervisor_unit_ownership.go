package doctor

import "fmt"

// SupervisorUnitOwnership is the caller-gathered relationship between a live
// supervisor PID and the systemd unit that owns it (if any). Callers (cmd/gc)
// compute this once per doctor run — mirroring how supervisorRunning is
// gathered before NewSupervisorHTTPCheck — so the check itself stays pure
// data-in, data-out and internal/doctor never needs to shell out to
// systemctl or import cmd/gc.
//
// The unit may be a per-user unit (what gc installs) or a system unit (what a
// host manager may install instead); Scope records which, because the remedy
// differs and the wrong remedy is actively harmful — see
// supervisorUnitOwnershipFixHint (city-8xj7).
type SupervisorUnitOwnership struct {
	// Status is one of "owned", "outside_unit", "no_unit".
	Status string
	// Unit is the systemd unit name (e.g. "gc-supervisor.service").
	// Empty when Status is "no_unit".
	Unit string
	// Scope is "user" or "system": which systemd scope Unit lives in. Empty
	// when Status is "no_unit", and treated as "user" when unset, matching
	// the era when only the user scope was consulted.
	Scope string
	// UnitActive reports systemd's is-active state for Unit.
	UnitActive bool
	// UnitPID is the MainPID systemd tracks for Unit (0 if unknown).
	UnitPID int
	// ProbeErr is why the live PID's process ancestry could not be read, if
	// it could not. An "outside_unit" Status is then unverified.
	ProbeErr error
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
		r.Message = fmt.Sprintf("supervisor (PID %d) is owned by systemd unit %s (%s scope)", c.supervisorPID, c.ownership.Unit, scopeLabel(c.ownership.Scope))
	default:
		if c.ownership.ProbeErr != nil {
			r.Status = StatusWarning
			r.Message = fmt.Sprintf(
				"could not verify that systemd unit %s (unit PID %d) owns the supervisor (PID %d): %v",
				c.ownership.Unit, c.ownership.UnitPID, c.supervisorPID, c.ownership.ProbeErr,
			)
			return r
		}
		r.Status = StatusError
		r.Message = fmt.Sprintf(
			"supervisor is running (PID %d) outside its systemd %s unit %s (unit PID %d, unit active=%v) — systemd's Restart=always is not protecting it",
			c.supervisorPID, scopeLabel(c.ownership.Scope), c.ownership.Unit, c.ownership.UnitPID, c.ownership.UnitActive,
		)
		r.FixHint = supervisorUnitOwnershipFixHint(c.ownership)
	}
	return r
}

// scopeLabel names a systemd scope for operator-facing text. An empty Scope
// predates scope reporting, so it reads as the user scope, which was the only
// one consulted before this field existed.
func scopeLabel(scope string) string {
	if scope == "" {
		return "user"
	}
	return scope
}

// supervisorUnitOwnershipFixHint names a remedy that is correct for the scope
// the unit was found in.
//
// This distinction is load-bearing, not cosmetic. The user-scope remedy is
// `systemctl --user start`. On a host where a SYSTEM unit owns the supervisor,
// running that both fails to reclaim the process and — when a user unit file is
// also installed — starts a second supervisor, which is the two-unit crash loop
// city-5tas and city-irg6 exist to prevent (city-8xj7). So the system scope
// gets the system-scope remedy and an explicit warning not to reach for --user.
func supervisorUnitOwnershipFixHint(ownership SupervisorUnitOwnership) string {
	if ownership.Scope == "system" {
		return fmt.Sprintf(
			"this unit is in the system scope: run 'sudo systemctl restart %s' to let it reclaim the process. Do NOT run 'systemctl --user start' — with a user unit also installed that starts a second supervisor (the city-5tas/city-irg6 crash loop)",
			ownership.Unit,
		)
	}
	return fmt.Sprintf(
		"run 'systemctl --user reset-failed %s && systemctl --user start %s' to let the unit reclaim the process, or 'gc supervisor install' to reinstall it",
		ownership.Unit, ownership.Unit,
	)
}
