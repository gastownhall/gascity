package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// resolvedLaunchModel is the provider-facing model id a launch pins for the
// session, for the launch_model session-metadata stamp (worker.operation
// events and usage facts then share one model vocabulary).
//
// The "model" OptionsSchema entry's selected VALUE is, in precedence order,
// the launch override (template_overrides / work-bead opt_model), the
// provider's EffectiveDefaults, then the option's own Default. That value is a
// choice KEY (the builtin claude provider's "sonnet" carries flag_args
// "--model claude-sonnet-5"), so it is rendered through OptionFlagArgs and the flag's
// value is what is recorded — the id the provider's usage facts carry. An
// open option renders the value through FlagTemplate, so the value itself
// comes back. It returns "" when the provider has no model option or the
// selected value cannot be honored (an unhonored pin is not a model the
// session runs with).
func resolvedLaunchModel(rp *config.ResolvedProvider, overrides map[string]string) string {
	if rp == nil {
		return ""
	}
	var opt *config.ProviderOption
	for i := range rp.OptionsSchema {
		if rp.OptionsSchema[i].Key == "model" {
			opt = &rp.OptionsSchema[i]
			break
		}
	}
	if opt == nil {
		return ""
	}
	value, ok := overrides["model"]
	if !ok || strings.TrimSpace(value) == "" {
		value, ok = rp.EffectiveDefaults["model"]
		if !ok || strings.TrimSpace(value) == "" {
			value = opt.Default
		}
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	args, ok := config.OptionFlagArgs(*opt, value)
	if !ok {
		return ""
	}
	return modelFlagValue(args, value)
}

// modelFlagValue extracts the model id from a rendered model flag sequence:
// the token after "--model" or "-m", else the value of a "--model=<id>"
// token, else the last token, else fallback (an empty sequence — the schema's
// explicit "leave unset" choice — pins nothing, so fallback is "" there).
func modelFlagValue(args []string, fallback string) string {
	for i, tok := range args {
		switch {
		case (tok == "--model" || tok == "-m") && i+1 < len(args):
			return strings.TrimSpace(args[i+1])
		case strings.HasPrefix(tok, "--model="):
			return strings.TrimSpace(strings.TrimPrefix(tok, "--model="))
		}
	}
	if len(args) > 0 {
		return strings.TrimSpace(args[len(args)-1])
	}
	return strings.TrimSpace(fallback)
}
