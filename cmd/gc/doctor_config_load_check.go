package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/doctor"
)

type configLoadCheck struct {
	err error
}

func newConfigLoadCheck(err error) *configLoadCheck { return &configLoadCheck{err: err} }

func (c *configLoadCheck) Name() string { return "config-load" }

func (c *configLoadCheck) CanFix() bool { return false }

func (c *configLoadCheck) WarmupEligible() bool { return false }

func (c *configLoadCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *configLoadCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	if c.err == nil {
		return okCheck(c.Name(), "full config load succeeded; config-dependent checks are registered")
	}
	err := c.err
	if ctx != nil && ctx.CityPath != "" {
		_, err = loadCityConfig(ctx.CityPath, io.Discard)
	}
	if err == nil {
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
