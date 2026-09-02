package doctor

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type stalledBeadsCheck struct {
	store beads.Store
	now   func() time.Time
}

func newStalledBeadsCheck(store beads.Store, now func() time.Time) Check {
	return &stalledBeadsCheck{store: store, now: now}
}

func (c *stalledBeadsCheck) Name() string { return "stalled-beads" }

func (c *stalledBeadsCheck) Run(*CheckContext) *CheckResult {
	return &CheckResult{Name: c.Name(), Status: StatusOK, Message: "not implemented"}
}

func (c *stalledBeadsCheck) CanFix() bool { return false }

func (c *stalledBeadsCheck) Fix(*CheckContext) error { return nil }

func (c *stalledBeadsCheck) WarmupEligible() bool { return false }
