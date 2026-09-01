package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// startupHealthEpisodesCheck surfaces active startup-health episodes (a
// session whose provider start keeps failing) so an operator sees a session
// stuck at or past its wake-failure threshold without needing to know the
// startup_health_* metadata keys or query the session-class store by hand.
type startupHealthEpisodesCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newStartupHealthEpisodesCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *startupHealthEpisodesCheck {
	return &startupHealthEpisodesCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *startupHealthEpisodesCheck) Name() string { return "startup-health-episodes" }

// CanFix returns false: an operator (or a dedicated recovery order), not this
// check, decides whether to wake, reset, or leave a quarantined session alone.
func (c *startupHealthEpisodesCheck) CanFix() bool { return false }

// Fix is a no-op. Detection only.
func (c *startupHealthEpisodesCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run is a RED-phase placeholder (ga-o04bfr.1.4): it always reports OK,
// deliberately failing every new test in doctor_startup_health_test.go via a
// plain assertion mismatch rather than a panic. cmd/gc is one large shared
// package, so a panic-stub Run reachable from a Doctor-registered check risks
// aborting the whole package's test binary mid-run (an unrecovered test panic
// takes the process down) if it were ever invoked outside this file's own
// tests; a wrong-but-safe return value keeps RED contained to assertion
// failures here. Replaced with the real classification logic at GREEN.
func (c *startupHealthEpisodesCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	return okCheck(c.Name(), "not implemented yet")
}
