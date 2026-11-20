package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// freshBootstrapInputOrigin is evaluated after authoritative launch selection
// and startup-content assembly. Zero preserves the unknown-input contract.
func freshBootstrapInputOrigin(tp TemplateParams, city *config.City, cfg runtime.Config, info session.Info, firstStart, forceFresh, resumed bool, parentSID string) runtime.BootstrapInputOrigin {
	if info.StartedConfigHash != "" || info.ContinuationResetPending != "" ||
		(info.Generation != "" && info.Generation != "1") ||
		(info.ContinuationEpoch != "" && info.ContinuationEpoch != "1") {
		return ""
	}
	rp := tp.ResolvedProvider
	if rp == nil || tp.IsACP || !firstStart || forceFresh || resumed || parentSID != "" {
		return ""
	}
	if config.FreshBuiltinCommandFamily(city, rp) == "" {
		return ""
	}
	if cfg.PromptSuffix != "" || cfg.Nudge != "" || len(cfg.SessionSetup) != 0 || cfg.SessionSetupScript != "" || len(cfg.SessionLive) != 0 {
		return ""
	}
	return runtime.BootstrapInputFreshBuiltin
}
