package main

import (
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// sessionCoreConfigForHashInfo builds the canonical config used for session
// config-drift core hashes from a typed session.Info. Live drift detection, asleep
// named-session drift detection, drift keys, and soft reload acceptance must use this
// helper so template_overrides participate in the same fingerprint everywhere. Start
// paths may keep their pre-start assembly inline when they need setup-specific
// diagnostics before storing first-start metadata.
func sessionCoreConfigForHashInfo(tp TemplateParams, info sessionpkg.Info) runtime.Config {
	agentCfg := templateParamsToConfig(tp)
	applyTemplateOverridesToConfigInfo(&agentCfg, info, tp)
	return agentCfg
}
