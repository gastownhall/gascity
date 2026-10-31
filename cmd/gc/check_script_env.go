package main

import (
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/config"
)

// checkScriptEnvFromConfig returns the city's [check_script_env]
// table with every value expanded against the controller's environment, ready
// to hand to convergence.ConditionEnv, dispatch.ProcessOptions and
// convergence.Handler. Expansion happens here, at the controller edge, rather
// than inside the check environment: there HOME is the city directory, so a
// value such as "$HOME/Library/Python/3.14" would re-derive the very path the
// setting exists to override. The controller-only keys read as empty, as they
// do for session env (expandEnvMap).
//
// A nil config or an empty table returns nil, which leaves the check
// environment exactly as it is without the setting.
func checkScriptEnvFromConfig(cfg *config.City) map[string]string {
	if cfg == nil || len(cfg.CheckScriptEnv) == 0 {
		return nil
	}
	return expandEnvMap(cfg.CheckScriptEnv)
}

// convergeTestCheckScriptEnv resolves the check-script env for the
// `gc converge test-gate` and `test-trigger` dry runs from the city the
// command runs in, so a dry run sees the same environment the controller gives
// the real condition and cannot drift from it. A city whose config does not
// load is an error, as it is for the controller: testing silently without the
// table would report a result the real gate can never reproduce.
func convergeTestCheckScriptEnv(cityPath string) (map[string]string, error) {
	cfg, err := loadCityConfigAdvisory(cityPath)
	if err != nil {
		return nil, fmt.Errorf("loading city config for [check_script_env]: %w", err)
	}
	return checkScriptEnvFromConfig(cfg), nil
}

// checkScriptEnvKeys lists the configured variable names in sorted order, for
// diagnostics that must not print the values (a value may have expanded from a
// controller secret).
func checkScriptEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
