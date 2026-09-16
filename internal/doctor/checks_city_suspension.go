package doctor

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// CitySuspensionCheck surfaces the city's *effective* suspension state —
// the same city/agent-blocking condition `gc status --json` reports via
// suspensionstate.EffectiveCitySuspended — as a doctor diagnostic.
//
// Without this check, an operator can run `gc doctor`, see every
// controller/database check pass, and never learn that the reason the
// city is doing no work is that it is suspended: doctor's other checks
// verify infrastructure health, not whether agent work is intentionally
// paused. Status already reports suspension; doctor had no equivalent.
//
// The check is deliberately informational: suspension is a normal,
// intentional operator state, not a defect, so it is reported as
// StatusWarning with SeverityAdvisory (never blocking) and it never
// offers to auto-resume — CanFix is always false. It distinguishes an
// explicit runtime override (`gc suspend`) from the configured
// suspended_on_start default in city.toml so the message points the
// operator at the right lever, and it reports an unreadable suspension
// state as unknown rather than silently treating it as "not suspended".
type CitySuspensionCheck struct {
	cfg *config.City
}

// NewCitySuspensionCheck creates a check that reports the city's
// effective suspension state, resolved with the workspace's configured
// suspended_on_start default (mirrors NewLegacySuspendedFieldCheck's
// closure-over-cfg shape).
func NewCitySuspensionCheck(cfg *config.City) *CitySuspensionCheck {
	return &CitySuspensionCheck{cfg: cfg}
}

// Name returns the check identifier.
func (c *CitySuspensionCheck) Name() string { return "city-suspension" }

// Run loads the runtime suspension state and reports the city's
// effective suspension using the same resolution the runtime and
// `gc status` use (suspensionstate.EffectiveCitySuspended merged with
// cfg.Workspace.EffectiveSuspendedOnStart). A read error (a corrupt or
// unreadable suspension-state.json — a missing file is not an error,
// see suspensionstate.Load) is reported as unknown, never inferred to
// mean the city is running.
func (c *CitySuspensionCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no city config loaded"
		return r
	}
	if ctx == nil || ctx.CityPath == "" {
		r.Status = StatusOK
		r.Message = "no city path available"
		return r
	}

	st, err := suspensionstate.Load(fsys.OSFS{}, ctx.CityPath)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("city suspension state is unknown: failed to read suspension-state.json: %v", err)
		return r
	}

	_, hasOverride := suspensionstate.ExplicitCity(st)
	effective := suspensionstate.EffectiveCitySuspended(st, c.cfg.Workspace.EffectiveSuspendedOnStart())

	if !effective {
		r.Status = StatusOK
		r.Message = "city is not suspended; agent work proceeds normally"
		return r
	}

	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	if hasOverride {
		r.Message = "city is suspended via an explicit runtime override (gc suspend); agent work is intentionally paused even though infrastructure is healthy. Run `gc resume` to resume it, or `gc status` to inspect."
	} else {
		r.Message = "city is suspended via the configured suspended_on_start default in city.toml; agent work is intentionally paused even though infrastructure is healthy. Run `gc resume` to resume for now, edit suspended_on_start to change the default, or `gc status` to inspect."
	}
	return r
}

// CanFix returns false: whether to resume a suspended city is an
// operator decision, not a mechanical fix. The issue this check
// addresses is explicit that suspension must never be auto-resumed.
func (c *CitySuspensionCheck) CanFix() bool { return false }

// Fix is a no-op; the check is report-only.
func (c *CitySuspensionCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns true: like LegacySuspendedFieldCheck, this is
// most useful right when the user is about to act on a stale-config
// view — telling an operator at `gc start` time that the city is
// suspended (and so will do no agent work regardless of how healthy
// everything else looks) is exactly the top-level condition warm-up is
// for.
func (c *CitySuspensionCheck) WarmupEligible() bool { return true }
