package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// stuckCreatingSessionsCheck reports session beads wedged in an in-flight
// create past the point the sweep and `gc session wake` treat as abandoned.
type stuckCreatingSessionsCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newStuckCreatingSessionsCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *stuckCreatingSessionsCheck {
	return &stuckCreatingSessionsCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *stuckCreatingSessionsCheck) Name() string { return "stuck-creating-sessions" }

func (c *stuckCreatingSessionsCheck) CanFix() bool { return false }

func (c *stuckCreatingSessionsCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *stuckCreatingSessionsCheck) WarmupEligible() bool { return false }

func (c *stuckCreatingSessionsCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	return errorCheck(c.Name(), "not implemented", "", nil)
}
