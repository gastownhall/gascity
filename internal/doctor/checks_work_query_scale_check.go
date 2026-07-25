package doctor

import (
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
//
// Real detection lands in the GREEN step (ga-d35ki5); this RED-step stub
// always reports StatusOK.
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

// Run is a RED-step stub; real detection lands in the GREEN step (ga-d35ki5).
func (c *ScaleCheckWorkQueryCorrespondenceCheck) Run(_ *CheckContext) *CheckResult {
	return &CheckResult{Name: c.Name(), Status: StatusOK}
}

// CanFix returns false — the correct scale_check/work_query value is
// context-dependent and cannot be derived mechanically.
func (c *ScaleCheckWorkQueryCorrespondenceCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *ScaleCheckWorkQueryCorrespondenceCheck) Fix(_ *CheckContext) error { return nil }
