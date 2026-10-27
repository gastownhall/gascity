package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// unreachableRoutedWorkCheck reports routed beads whose type the controller's
// demand probe excludes. It does not inspect any store yet: Run reports OK.
type unreachableRoutedWorkCheck struct{}

func newUnreachableRoutedWorkCheck(_ *config.City, _ string, _ func(path string) (beads.Store, error)) *unreachableRoutedWorkCheck {
	return &unreachableRoutedWorkCheck{}
}

func (c *unreachableRoutedWorkCheck) Name() string { return "unreachable-routed-work" }

func (c *unreachableRoutedWorkCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	return &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK}
}

func (c *unreachableRoutedWorkCheck) CanFix() bool                     { return false }
func (c *unreachableRoutedWorkCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c *unreachableRoutedWorkCheck) WarmupEligible() bool             { return false }
