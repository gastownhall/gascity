package main

import (
	"fmt"
	"os"
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

// v2EffectsReal reports whether the planner submits what admission lets
// through. Until C9 it is false: the planner runs trace-only.
const v2EffectsReal = false

// v2ControllersInBuild reports whether v2 would act. While the planner is
// trace-only it would start, restart and scale nothing, so selecting it is
// refused. The skeleton's constants (reconcile_runtime.go) are dead; C3
// deletes them.
const v2ControllersInBuild = v2EffectsReal

// v2SkeletonEnv is the developer-only override that admits v2 before its
// controllers are in the build, for smoke runs of the skeleton on a throwaway
// city. Only the exact value "1" counts. Deleted in P4.1, when v2 becomes
// admissible on its own (OQ-1).
const v2SkeletonEnv = "GC_RECONCILER_V2_SKELETON"

// reconcilerModeLookupEnv is the environment the composition edges (gc start,
// the supervisor, doctor) latch with. Tests replace it instead of setting the
// process environment; a test that does MUST NOT call t.Parallel().
var reconcilerModeLookupEnv = os.LookupEnv

// latchReconcilerMode resolves the boot mode. Unknown values and an
// inadmissible v2 are errors: the city does not start. v2 is admissible when
// this build carries its controllers, or when lookupEnv reports the developer
// override (a nil lookupEnv has none), and the composed config enables no
// feature v2 defers (v2LatchRefusals). An alias latches legacy silently; its
// load warning already tells the operator.
func latchReconcilerMode(cfg *config.City, lookupEnv func(string) (string, bool)) (reconcilerMode, error) {
	raw := cfg.Daemon.SessionReconciler
	mode, _, ok := cfg.Daemon.SessionReconcilerMode()
	switch {
	case !ok:
		return reconcilerLegacy, fmt.Errorf(`[daemon] session_reconciler = %q is not a known value; remove the key to run the legacy reconciler`, raw)
	case mode == config.SessionReconcilerV2 && !v2ControllersInBuild && !v2SkeletonOverride(lookupEnv):
		return reconcilerLegacy, fmt.Errorf(`[daemon] session_reconciler = %q is not available in this build: the v2 session reconciler has no session controllers yet; remove the key to run the legacy reconciler`, raw)
	case mode != config.SessionReconcilerV2:
		return reconcilerLegacy, nil
	}
	if refusals := v2LatchRefusals(cfg); len(refusals) > 0 {
		parts := make([]string, len(refusals))
		for i, r := range refusals {
			parts[i] = r.String()
		}
		return reconcilerLegacy, fmt.Errorf(`[daemon] session_reconciler = %q is refused: %s; remove those settings or run legacy`, raw, strings.Join(parts, "; "))
	}
	return reconcilerV2, nil
}

func v2SkeletonOverride(lookupEnv func(string) (string, bool)) bool {
	if lookupEnv == nil {
		return false
	}
	v, ok := lookupEnv(v2SkeletonEnv)
	return ok && v == "1"
}

// reconcilerModeDrift reports, once per transition, that the session_reconciler
// on disk no longer matches the mode this controller latched at boot. It never
// re-latches: the running mode is fixed until the controller restarts.
type reconcilerModeDrift struct {
	running   reconcilerMode
	lookupEnv func(string) (string, bool) // what the controller latched with; nil has no override
	reported  string                      // the on-disk value last reported; "" while in sync
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
	if _, err := latchReconcilerMode(cfg, d.lookupEnv); err != nil {
		warning += "; the next controller start will refuse it: " + err.Error()
	}
	return warning
}

// sessionReconcilerDoctorCheck reports the [daemon] session_reconciler choice.
type sessionReconcilerDoctorCheck struct {
	cfg       *config.City
	lookupEnv func(string) (string, bool)
}

func newSessionReconcilerDoctorCheck(cfg *config.City, lookupEnv func(string) (string, bool)) *sessionReconcilerDoctorCheck {
	return &sessionReconcilerDoctorCheck{cfg: cfg, lookupEnv: lookupEnv}
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
// itself, the developer override included, so doctor and controller start never
// disagree. Under every mode it also lists, as Details, the v2 refusals the
// config would hit: a dry run of the switch that never changes the status.
func (c *sessionReconcilerDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	raw := c.cfg.Daemon.SessionReconciler
	_, alias, _ := c.cfg.Daemon.SessionReconcilerMode()
	mode, err := latchReconcilerMode(c.cfg, c.lookupEnv)
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
	for _, refusal := range v2LatchRefusals(c.cfg) {
		r.Details = append(r.Details, "v2 would refuse: "+refusal.String())
	}
	return r
}
