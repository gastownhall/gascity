package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/doctor"
)

// configLoadCheck reports the deep config load that gates most of gc doctor.
//
// A block of config-dependent checks in cmd_doctor.go (beads-store,
// v2-routed-to-namespace and assignee-resolves among them) is registered only
// when this load succeeds. The exact membership moves as checks are added, so
// do not read the list here as current; what does not move is the mechanism.
// When the load fails those checks are not skipped with a message, they are
// never registered, so they do not appear in the output at all and the summary
// line counts only what did run.
//
// Observed once, on the ds-research city 2026-09-15: `gc doctor` printed
// "26 passed, 2 warnings, 1 failed" with fourteen checks silently absent,
// while the separate city-config check reported the file as loaded.
//
// A health report that omits its own omissions is the failure it exists to
// catch, so the load result is now a check of its own.
type configLoadCheck struct {
	err error
}

func newConfigLoadCheck(err error) *configLoadCheck { return &configLoadCheck{err: err} }

func (c *configLoadCheck) Name() string { return "config-load" }

func (c *configLoadCheck) CanFix() bool { return false }

func (c *configLoadCheck) WarmupEligible() bool { return false }

func (c *configLoadCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *configLoadCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	// registrationErr is what gated whether config-dependent checks got
	// registered for THIS run — that decision was already made before Run
	// ever executes, and nothing here can undo it.
	registrationErr := c.err
	// Re-load live rather than trust registrationErr alone: an earlier check
	// in the same run (e.g. v2-formulas-dir) may have fixed the config on
	// disk since then, and this check must reflect that, the same way
	// expanded-config-load does.
	err := registrationErr
	if ctx != nil && ctx.CityPath != "" {
		_, err = loadCityConfig(ctx.CityPath, io.Discard)
	}
	if err == nil && registrationErr == nil {
		return okCheck(c.Name(), "full config load succeeded; config-dependent checks are registered")
	}
	if err == nil {
		// The live reload just succeeded, but registration ran earlier
		// against a load that had not yet been repaired, so the checks this
		// one exists to vouch for were never added to this run's report.
		// Reporting OK here would be the exact false-clean this check was
		// added to prevent, just one step removed.
		return warnCheck(c.Name(),
			"config load now succeeds, but config-dependent checks were NOT registered for this run because the load failed earlier at registration time; this report is incomplete",
			"rerun gc doctor once more so the checks register against the repaired config",
			nil)
	}
	return &doctor.CheckResult{
		Name:    c.Name(),
		Status:  doctor.StatusError,
		Message: fmt.Sprintf("full config load failed, so config-dependent checks did not run: %v", err),
		FixHint: "fix the config load (start with packv2-import-state and city-config), then rerun gc doctor; until then this report is incomplete",
	}
}
