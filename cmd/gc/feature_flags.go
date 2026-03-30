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
	gw := cfg.Daemon.GraphWorkflows
	formula.GraphWorkflowsEnabled = gw
	molecule.GraphApplyEnabled = gw
}
