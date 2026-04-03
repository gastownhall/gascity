package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
)

// applyFeatureFlags propagates daemon-level feature flags to the formula and
// molecule packages. Must be called after config.LoadWithIncludes and before
// any formula compilation or molecule instantiation.
func applyFeatureFlags(cfg *config.City) {
	gw := cfg.Daemon.FormulaV2
	formula.FormulaV2Enabled = gw
	molecule.GraphApplyEnabled = gw
}
