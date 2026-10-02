package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// reconcilerMode is the session reconciler a controller latched at boot from
// [daemon] session_reconciler. It never changes for the life of the process: a
// reload that names another mode only warns (reconcilerModeDrift).
type reconcilerMode uint8

const (
	reconcilerLegacy reconcilerMode = iota // zero value: every directly-built runtime is legacy
	reconcilerV2
)

// String returns the config spelling of the mode.
func (m reconcilerMode) String() string {
	if m == reconcilerV2 {
		return config.SessionReconcilerV2
	}
	return config.SessionReconcilerLegacy
}

// v2ControllersInBuild reports whether this build carries the v2 allocator
// (P3) and the first session controller group (P4.1). Until both land, v2
// would start, restart and scale nothing, so selecting it is refused.
const v2ControllersInBuild = false

// latchReconcilerMode resolves the boot mode. Unknown values and an
// inadmissible v2 are errors: the city does not start. An alias latches legacy
// silently; its load warning already tells the operator.
func latchReconcilerMode(cfg *config.City) (reconcilerMode, error) {
	raw := cfg.Daemon.SessionReconciler
	mode, _, ok := cfg.Daemon.SessionReconcilerMode()
	switch {
	case !ok:
		return reconcilerLegacy, fmt.Errorf(`[daemon] session_reconciler = %q is not a known value; remove the key to run the legacy reconciler`, raw)
	case mode == config.SessionReconcilerV2 && !v2ControllersInBuild:
		return reconcilerLegacy, fmt.Errorf(`[daemon] session_reconciler = %q is not available in this build: the v2 session reconciler has no session controllers yet; remove the key to run the legacy reconciler`, raw)
	case mode == config.SessionReconcilerV2:
		return reconcilerV2, nil
	}
	return reconcilerLegacy, nil
}

// reconcilerModeDrift reports, once per transition, that the session_reconciler
// on disk no longer matches the mode this controller latched at boot. It never
// re-latches: the running mode is fixed until the controller restarts.
type reconcilerModeDrift struct {
	running  reconcilerMode
	reported string // the on-disk value last reported; "" while in sync
}

// observe returns the pending-restart warning for a reload candidate, or ""
// when the candidate matches the running mode or the same drift was already
// reported. The warning says when the next controller start would refuse the
// candidate, so a reload never presents an inadmissible value as pending.
//
// Under strict mode a standalone controller (gc start --foreground) rejects a
// candidate with an unknown value in reloadConfig, before observe runs: the
// unknown-value load warning is strict-fatal. Only --no-strict and the
// supervisor reach observe with one.
func (d *reconcilerModeDrift) observe(cfg *config.City) string {
	mode, _, ok := cfg.Daemon.SessionReconcilerMode()
	if ok && mode == d.running.String() {
		d.reported = ""
		return ""
	}
	onDisk := mode
	if !ok {
		onDisk = fmt.Sprintf("%q (not a known value)", strings.TrimSpace(cfg.Daemon.SessionReconciler))
	}
	if onDisk == d.reported {
		return ""
	}
	d.reported = onDisk
	warning := fmt.Sprintf("pending restart: session_reconciler on disk is %s; this controller runs %s", onDisk, d.running)
	if _, err := latchReconcilerMode(cfg); err != nil {
		warning += "; the next controller start will refuse it: " + err.Error()
	}
	return warning
}

// sessionReconcilerDoctorCheck reports the [daemon] session_reconciler choice.
type sessionReconcilerDoctorCheck struct {
	cfg *config.City
}

func newSessionReconcilerDoctorCheck(cfg *config.City) *sessionReconcilerDoctorCheck {
	return &sessionReconcilerDoctorCheck{cfg: cfg}
}

// Name implements doctor.Check.
func (*sessionReconcilerDoctorCheck) Name() string { return "daemon-session-reconciler" }

// CanFix implements doctor.Check. Choosing the reconciler is an operator edit.
func (*sessionReconcilerDoctorCheck) CanFix() bool { return false }

// WarmupEligible implements doctor.Check. `gc start` already refuses an
// unknown value or an inadmissible v2 itself.
func (*sessionReconcilerDoctorCheck) WarmupEligible() bool { return false }

// Fix implements doctor.Check.
func (*sessionReconcilerDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run implements doctor.Check: an error for any value the controller latch
// refuses (unknown, or v2 while inadmissible), a warning for an alias or an
// admissible v2, OK for legacy or unset. Admissibility comes from the latch
// itself; the v2 override P2-8 adds must feed the same decision, so doctor and
// controller start never disagree.
func (c *sessionReconcilerDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	raw := c.cfg.Daemon.SessionReconciler
	_, alias, _ := c.cfg.Daemon.SessionReconcilerMode()
	mode, err := latchReconcilerMode(c.cfg)
	switch {
	case err != nil:
		r.Status = doctor.StatusError
		r.Message = err.Error() + "; the controller refuses to start"
		r.FixHint = "remove the key to run the legacy reconciler"
	case alias:
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("[daemon] session_reconciler = %q is a deprecated alias for legacy", raw)
		r.FixHint = "remove the key: legacy is the default, and a gc that predates the key rejects it under strict mode"
	case mode == reconcilerV2:
		r.Status = doctor.StatusWarning
		r.Message = "session reconciler: v2 (skeleton: no session controllers)"
		r.FixHint = "remove the key to run the legacy reconciler"
	default:
		r.Status = doctor.StatusOK
		r.Message = "session reconciler: legacy"
	}
	return r
}
