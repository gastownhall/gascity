package doctor

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// ScaleCheckWorkQueryCorrespondenceCheck warns when a configured agent
// overrides exactly one of WorkQuery / ScaleCheck and leaves the other at
// its shared default. The two fields are independent override slots that
// both default to the same predicate (bdReadyPoolDemandShell); overriding
// only one lets the reconciler's spawn decision (ScaleCheck) and the
// worker's claim decision (WorkQuery) silently diverge. See
// engdocs/architecture/dispatch.md "scale_check ↔ work_query
// correspondence" (Invariant 11).
type ScaleCheckWorkQueryCorrespondenceCheck struct {
	cfg *config.City
}

// NewScaleCheckWorkQueryCorrespondenceCheck creates the check.
func NewScaleCheckWorkQueryCorrespondenceCheck(cfg *config.City) *ScaleCheckWorkQueryCorrespondenceCheck {
	return &ScaleCheckWorkQueryCorrespondenceCheck{cfg: cfg}
}

// Name returns the check identifier.
func (c *ScaleCheckWorkQueryCorrespondenceCheck) Name() string {
	return "scale-check-work-query-correspondence"
}

// Run reports, for each configured agent, whether exactly one of
// WorkQuery / ScaleCheck is overridden while the other is left at its
// shared default.
func (c *ScaleCheckWorkQueryCorrespondenceCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no config; nothing to check"
		return r
	}

	var details []string
	for _, a := range c.cfg.Agents {
		hasWorkQuery := strings.TrimSpace(a.WorkQuery) != ""
		hasScaleCheck := strings.TrimSpace(a.ScaleCheck) != ""
		if hasWorkQuery == hasScaleCheck {
			continue
		}
		missing, set := "scale_check", "work_query"
		if hasScaleCheck {
			missing, set = "work_query", "scale_check"
		}
		details = append(details, fmt.Sprintf(
			"agent %q overrides %s but not %s: the reconciler's spawn decision (scale_check) and the worker's claim decision (work_query) can silently diverge — see engdocs/architecture/dispatch.md \"scale_check ↔ work_query correspondence\" (Invariant 11)",
			a.QualifiedName(), set, missing,
		))
	}

	if len(details) == 0 {
		r.Status = StatusOK
		r.Message = "scale_check and work_query overrides are consistent"
		return r
	}
	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	r.Message = fmt.Sprintf("%d agent(s) override only one of work_query/scale_check", len(details))
	r.Details = details
	return r
}

// CanFix returns false — the correct scale_check/work_query value is
// context-dependent and cannot be derived mechanically.
func (c *ScaleCheckWorkQueryCorrespondenceCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *ScaleCheckWorkQueryCorrespondenceCheck) Fix(_ *CheckContext) error { return nil }
