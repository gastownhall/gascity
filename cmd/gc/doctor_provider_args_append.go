package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// providerArgsAppendDoctorCheck warns when a resolved agent launch drops a
// provider args_append entry: every entry the provider contributes must
// reach the agent's final Args.
type providerArgsAppendDoctorCheck struct {
	cityPath string
}

func newProviderArgsAppendDoctorCheck(cityPath string) *providerArgsAppendDoctorCheck {
	return &providerArgsAppendDoctorCheck{cityPath: cityPath}
}

func (c *providerArgsAppendDoctorCheck) Name() string { return "provider-args-append" }

func (c *providerArgsAppendDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	cfg, err := loadCityConfigAllowMissingProviderReferences(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusOK
		r.Message = "provider args_append check skipped until expanded config loads"
		return r
	}
	var details []string
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		// A start_command is the complete command line; nothing is appended
		// to it, so there is no args_append to lose.
		if agent.StartCommand != "" || cfg.Workspace.StartCommand != "" {
			continue
		}
		// An agent whose provider does not resolve is reported by the
		// provider checks; this check has no args to compare for it.
		resolved, err := config.ResolveProvider(agent, &cfg.Workspace, cfg.Providers, presentOnPath)
		if err != nil {
			continue
		}
		if missing := missingArgsAppend(resolved); len(missing) > 0 {
			details = append(details, fmt.Sprintf("agent %q (provider %q) is missing args_append entries %q from its resolved args",
				agent.QualifiedName(), resolved.Name, missing))
		}
	}
	if len(details) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "provider args_append reaches every agent's resolved args"
		return r
	}
	r.Status = doctor.StatusWarning
	r.Message = fmt.Sprintf("%d agent(s) lose provider args_append entries", len(details))
	r.Details = details
	r.FixHint = "the agent's args replace the provider's args; add the missing entries to the agent's args, or drop them from the provider's args_append"
	return r
}

func (c *providerArgsAppendDoctorCheck) CanFix() bool { return false }

func (c *providerArgsAppendDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *providerArgsAppendDoctorCheck) WarmupEligible() bool { return false }

// presentOnPath is a config.LookPathFunc that accepts every binary: the
// check compares resolved args and must not depend on what this host has
// installed.
func presentOnPath(name string) (string, error) { return name, nil }

// missingArgsAppend returns the entries of rp.ArgsAppend that rp.Args does not
// carry, counting repeats: an entry that appears twice in ArgsAppend must
// appear twice in Args.
func missingArgsAppend(rp *config.ResolvedProvider) []string {
	have := make(map[string]int, len(rp.Args))
	for _, a := range rp.Args {
		have[a]++
	}
	var missing []string
	for _, a := range rp.ArgsAppend {
		if have[a] > 0 {
			have[a]--
			continue
		}
		missing = append(missing, a)
	}
	return missing
}
