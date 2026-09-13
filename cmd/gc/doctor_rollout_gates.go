package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/rollout"
)

// rolloutGateCheck renders one registered rollout gate — its resolved value,
// origin, and any per-gate notices — for `gc doctor`. It is REPORT-ONLY: always
// SeverityAdvisory and never StatusError, so it never gates the exit code. The
// degraded/fail-closed capability verdict depends on the per-store capability
// probe (S3) and is deliberately not computed here; PR-1c is render-only.
//
// The snapshot is resolved fresh from the on-disk config PLUS this doctor
// process's own environment — so it can disagree with a running controller,
// which latched ITS value at ITS boot from ITS environment (a systemd unit's
// env need not match the operator's shell). doctor therefore cannot observe the
// live latch or its pending-restart drift; the controller's own logs carry
// those. The Run scope-qualifier Details line makes this explicit. Full runtime
// reconciliation lands with the S4 status wire.
type rolloutGateCheck struct {
	spec  rollout.Spec
	flags rollout.Flags
}

func (c rolloutGateCheck) Name() string { return "rollout:" + c.spec.Key }

func (c rolloutGateCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	res := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory, Status: doctor.StatusOK}
	res.Message = fmt.Sprintf("%s = %s (origin=%s)", c.spec.Key, c.flags.ValueOf(c.spec.Key), c.flags.OriginOf(c.spec.Key))

	ctxLine := fmt.Sprintf("category=%s owner=%s", c.spec.Category, c.spec.Owner.GitHub)
	if c.spec.Expires != "" {
		ctxLine += " expires=" + c.spec.Expires
	}
	res.Details = append(res.Details, ctxLine)
	res.Details = append(res.Details, "resolved from on-disk config + this process's env; a running controller latched its value at its own boot — see the controller logs for the live value")

	for _, n := range c.flags.Notices() {
		if n.FlagKey == c.spec.Key {
			res.Status = doctor.StatusWarning
			res.Details = append(res.Details, n.Message)
		}
	}
	return res
}

func (c rolloutGateCheck) CanFix() bool                     { return false }
func (c rolloutGateCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c rolloutGateCheck) WarmupEligible() bool             { return false }

// rolloutResolveErrCheck is the single advisory check registered when doctor's
// rollout.Resolve failed (an out-of-enum config value; a nil cfg is excluded by
// the caller's cfg guard, so it never reaches here).
type rolloutResolveErrCheck struct{ err error }

func (c rolloutResolveErrCheck) Name() string { return "rollout:resolve" }

func (c rolloutResolveErrCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	return &doctor.CheckResult{
		Name:     c.Name(),
		Severity: doctor.SeverityAdvisory,
		Status:   doctor.StatusWarning,
		Message:  fmt.Sprintf("rollout gates unresolved: %v", c.err),
	}
}

func (c rolloutResolveErrCheck) CanFix() bool                     { return false }
func (c rolloutResolveErrCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c rolloutResolveErrCheck) WarmupEligible() bool             { return false }

// rolloutGateChecks builds the doctor "Rollout gates" section: one advisory
// check per registered rollout.Specs() gate, or a single resolve-failure check
// when resolveErr is non-nil. Callers register these only when cfg loaded
// (cfgErr == nil && cfg != nil).
func rolloutGateChecks(flags rollout.Flags, resolveErr error) []doctor.Check {
	if resolveErr != nil {
		return []doctor.Check{rolloutResolveErrCheck{err: resolveErr}}
	}
	checks := make([]doctor.Check, 0, len(rollout.Specs()))
	for _, s := range rollout.Specs() {
		checks = append(checks, rolloutGateCheck{spec: s, flags: flags})
	}
	return checks
}
