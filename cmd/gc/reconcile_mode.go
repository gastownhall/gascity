package main

import (
	"fmt"
	"os"
	"strings"
	"time"

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

// v2EffectsReal reports that the planner submits what admission lets
// through (C9, the cutover): v2 is admissible from config alone, and its
// passes act. The v2purity analyzer reads it as its cutover gate.
const v2EffectsReal = true

// reconcilerModeLookupEnv is the environment the composition edges (gc start,
// the supervisor, doctor, the drain-ack CLI) latch with: the latch reads
// GC_SESSION from it for its runtime refusals (v2SessionRuntimeName), and an
// unset key's resolution reads GC_BEADS_CONDITIONAL_WRITES
// (v2DefaultConditionalWritesFallback). Tests replace it instead of setting
// the process environment; a test that does MUST NOT call t.Parallel().
var reconcilerModeLookupEnv = os.LookupEnv

// reconcilerModeReason is why the latch resolved its mode.
type reconcilerModeReason uint8

const (
	reconcilerExplicit       reconcilerModeReason = iota // [daemon] session_reconciler names the mode (or an alias)
	reconcilerDefaultV2                                  // unset, and the city can run v2
	reconcilerFallbackLegacy                             // unset, and v2DefaultFallbacks names why it cannot
)

// reconcilerResolution is the latch's answer: the mode, why, and, for a
// fallback, every reason v2 is not the default.
type reconcilerResolution struct {
	Mode      reconcilerMode
	Reason    reconcilerModeReason
	Fallbacks []string
}

// String renders the resolution as the boot log and doctor report it.
func (r reconcilerResolution) String() string {
	switch r.Reason {
	case reconcilerDefaultV2:
		return "v2 (default: session_reconciler is unset)"
	case reconcilerFallbackLegacy:
		return "legacy (default fallback: session_reconciler is unset and v2 is not available: " + strings.Join(r.Fallbacks, "; ") + ")"
	}
	return r.Mode.String() + " (set by [daemon] session_reconciler)"
}

// reconcilerFallbackHint is the fix doctor and the boot log give for a
// fallback.
const reconcilerFallbackHint = `to run v2, remove or change the settings named; or set session_reconciler = "legacy" to keep legacy and silence this warning`

// latchReconcilerMode resolves the boot mode (resolveReconcilerMode); a
// refused mode is legacy with the error.
func latchReconcilerMode(cfg *config.City, lookupEnv func(string) (string, bool)) (reconcilerMode, error) {
	r, err := resolveReconcilerMode(cfg, lookupEnv)
	if err != nil {
		return reconcilerLegacy, err
	}
	return r.Mode, nil
}

// resolveReconcilerMode resolves the boot mode from config alone. Unknown
// values and an inadmissible explicit v2 are errors: the city does not start.
// An explicit v2 is admissible when the composed config enables no feature v2
// defers (v2LatchRefusals). An unset key is v2 when v2DefaultFallbacks names
// nothing, else legacy with those reasons (mc-dpo2b). An alias latches legacy
// silently; its load warning already tells the operator. On an error the
// resolution still carries the mode the key names (v2 for a refused v2), for
// doctor.
func resolveReconcilerMode(cfg *config.City, lookupEnv func(string) (string, bool)) (reconcilerResolution, error) {
	raw := cfg.Daemon.SessionReconciler
	mode, _, ok := cfg.Daemon.SessionReconcilerMode()
	switch {
	case !ok:
		return reconcilerResolution{}, fmt.Errorf(`[daemon] session_reconciler = %q is not a known value; set session_reconciler = "legacy" or "v2"`, raw)
	case mode == "":
		if fallbacks := v2DefaultFallbacks(cfg, lookupEnv); len(fallbacks) > 0 {
			return reconcilerResolution{Mode: reconcilerLegacy, Reason: reconcilerFallbackLegacy, Fallbacks: fallbacks}, nil
		}
		return reconcilerResolution{Mode: reconcilerV2, Reason: reconcilerDefaultV2}, nil
	case mode != config.SessionReconcilerV2:
		return reconcilerResolution{}, nil
	}
	r := reconcilerResolution{Mode: reconcilerV2}
	if refusals := v2LatchRefusals(cfg, lookupEnv); len(refusals) > 0 {
		parts := make([]string, len(refusals))
		for i, refusal := range refusals {
			parts[i] = refusal.String()
		}
		return r, fmt.Errorf(`[daemon] session_reconciler = %q is refused: %s; remove those settings or set session_reconciler = "legacy"`, raw, strings.Join(parts, "; "))
	}
	return r, nil
}

// reconcilerBootWarnings is what a controller logs once at boot about its
// resolution: the fallback and its reasons, and under v2 each setting v2
// ignores (v2IgnoredSettings).
func reconcilerBootWarnings(cfg *config.City, r reconcilerResolution) []string {
	var out []string
	if r.Reason == reconcilerFallbackLegacy {
		out = append(out, "session reconciler: "+r.String()+"; "+reconcilerFallbackHint)
	}
	if r.Mode == reconcilerV2 {
		out = append(out, v2IgnoredSettings(cfg)...)
	}
	return out
}

// doctorReconcilerMode is the session reconciler a doctor check reads: the
// latch's resolution under lookupEnv (reconcilerModeLookupEnv when nil), with
// a refused explicit v2 read as the v2 it names, and the line naming the mode
// and why, for the check's details.
func doctorReconcilerMode(cfg *config.City, lookupEnv func(string) (string, bool)) (reconcilerMode, string) {
	if lookupEnv == nil {
		lookupEnv = reconcilerModeLookupEnv
	}
	res, err := resolveReconcilerMode(cfg, lookupEnv)
	switch {
	case err == nil:
		return res.Mode, "session reconciler: " + res.String()
	case res.Mode == reconcilerV2:
		return res.Mode, "session reconciler: v2 (set by [daemon] session_reconciler), which the controller refuses to start: " + err.Error()
	}
	return res.Mode, "session reconciler: not resolved: " + err.Error()
}

// reconcilerModeDrift reports, once per transition, that the session_reconciler
// on disk no longer resolves to the mode this controller latched at boot: a
// changed key, or an unset key whose config now resolves the other way (a
// default-v2 city that gains a true gap, or a fallback city that loses its
// last reason). It never re-latches: the running mode is fixed until the
// controller restarts.
type reconcilerModeDrift struct {
	running   reconcilerMode
	lookupEnv func(string) (string, bool) // what the controller latched with; nil has no override
	reported  string                      // the on-disk value last reported; "" while in sync
}

// reconcilerDriftCLINote ends every pending-restart warning: the drain
// commands ask the running controller its mode (cityRunsV2), so they keep to
// the running mode too.
const reconcilerDriftCLINote = "until it restarts, gc runtime drain, drain-ack and undrain (gc hook's drain-ack among them) keep to the mode it runs"

// observe returns the pending-restart warning for a reload candidate, or ""
// when the candidate resolves to the running mode or the same drift was
// already reported. The warning says when the next controller start would
// refuse the candidate, so a reload never presents an inadmissible value as
// pending, and for an unset key it names what the key resolves to and why.
//
// Under strict mode a standalone controller (gc start --foreground) rejects a
// candidate with an unknown value in reloadConfig, before observe runs: the
// unknown-value load warning is strict-fatal. Only --no-strict and the
// supervisor reach observe with one.
func (d *reconcilerModeDrift) observe(cfg *config.City) string {
	r, err := resolveReconcilerMode(cfg, d.lookupEnv)
	if err == nil && r.Mode == d.running {
		d.reported = ""
		return ""
	}
	mode, _, ok := cfg.Daemon.SessionReconcilerMode()
	var onDisk string
	switch {
	case !ok:
		onDisk = fmt.Sprintf("%q (not a known value)", strings.TrimSpace(cfg.Daemon.SessionReconciler))
	case mode == "":
		onDisk = "unset, which resolves to " + r.String()
	default:
		onDisk = mode
	}
	if onDisk == d.reported {
		return ""
	}
	d.reported = onDisk
	warning := fmt.Sprintf("pending restart: session_reconciler on disk is %s; this controller runs %s", onDisk, d.running)
	if err != nil {
		warning += "; the next controller start will refuse it: " + err.Error()
	}
	return warning + "; " + reconcilerDriftCLINote
}

// sessionReconcilerDoctorCheck reports the [daemon] session_reconciler choice.
// queryPass asks a v2 controller for its last pass (queryV2PassStatus);
// capabilities, when the store checks run, reads C0.7's capabilities of the
// sessions and graph class stores.
type sessionReconcilerDoctorCheck struct {
	cfg          *config.City
	lookupEnv    func(string) (string, bool)
	queryPass    func(cityPath string) (v2PassStatus, error)
	capabilities func() ([]v2StoreCapability, error)
}

func newSessionReconcilerDoctorCheck(cfg *config.City, lookupEnv func(string) (string, bool)) *sessionReconcilerDoctorCheck {
	return &sessionReconcilerDoctorCheck{cfg: cfg, lookupEnv: lookupEnv, queryPass: queryV2PassStatus}
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
// refuses (unknown, or an explicit v2 while inadmissible); a warning for an
// alias, for an unset key that falls back to legacy (naming every reason),
// and for a v2 city that sets a key v2 ignores (v2IgnoredSettings); OK
// otherwise. The message names the resolved mode and why: set explicitly, the
// v2 default, or the legacy fallback. Resolution comes from the latch itself,
// so doctor and controller start never disagree. Under every mode it also
// lists, as Details, the v2 refusals the config would hit, a dry run of an
// explicit v2; under v2, the last pass's age when the controller answers; and
// each class store's C0.7 capabilities. C0.7 makes a latched v2 whose boot it
// would refuse an error.
func (c *sessionReconcilerDoctorCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	raw := c.cfg.Daemon.SessionReconciler
	_, alias, _ := c.cfg.Daemon.SessionReconcilerMode()
	res, err := resolveReconcilerMode(c.cfg, c.lookupEnv)
	mode := res.Mode
	switch {
	case err != nil:
		mode = reconcilerLegacy
		r.Status = doctor.StatusError
		r.Message = err.Error() + "; the controller refuses to start"
		r.FixHint = `set session_reconciler = "legacy" to run the legacy reconciler`
	case alias:
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("[daemon] session_reconciler = %q is a deprecated alias for legacy; session reconciler: %s", raw, res)
		r.FixHint = `set session_reconciler = "legacy"`
	case res.Reason == reconcilerFallbackLegacy:
		r.Status = doctor.StatusWarning
		r.Message = "session reconciler: " + res.String()
		r.FixHint = reconcilerFallbackHint
	default:
		r.Status = doctor.StatusOK
		r.Message = "session reconciler: " + res.String()
	}
	if ignored := v2IgnoredSettings(c.cfg); mode == reconcilerV2 && len(ignored) > 0 {
		r.Status = doctor.StatusWarning
		r.Message += "; " + strings.Join(ignored, "; ")
		r.FixHint = "remove the settings v2 ignores"
	}
	for _, refusal := range v2LatchRefusals(c.cfg, c.lookupEnv) {
		r.Details = append(r.Details, "v2 would refuse: "+refusal.String())
	}
	if mode == reconcilerV2 && ctx != nil && ctx.CityPath != "" && c.queryPass != nil {
		if st, qerr := c.queryPass(ctx.CityPath); qerr == nil && st.Passes > 0 {
			r.Details = append(r.Details, fmt.Sprintf("v2 last pass record: %s ago (%d passes)", time.Duration(st.LastPassAgeMS)*time.Millisecond, st.Passes))
		}
	}
	c.reportCapabilities(r, mode)
	return r
}

// reportCapabilities adds each class store's C0.7 capabilities to r, and
// makes r an error when mode is v2 and C0.7 would refuse its boot.
func (c *sessionReconcilerDoctorCheck) reportCapabilities(r *doctor.CheckResult, mode reconcilerMode) {
	if c.capabilities == nil {
		return
	}
	caps, err := c.capabilities()
	if err != nil {
		r.Details = append(r.Details, "C0.7: not checked: "+err.Error())
		return
	}
	for _, got := range caps {
		r.Details = append(r.Details, "C0.7 "+got.String())
	}
	if refusal := v2CapabilityRefusal(caps); refusal != nil && mode == reconcilerV2 {
		r.Status, r.Message = doctor.StatusError, refusal.Error()
		r.FixHint = `see the message for the fix, or set session_reconciler = "legacy"`
	}
}
