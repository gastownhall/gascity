package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// sessionCoreConfigForHash builds the canonical config used for session
// config-drift core hashes. Live drift detection, asleep named-session drift
// detection, drift keys, and soft reload acceptance must use this helper so
// template_overrides participate in the same fingerprint everywhere. Start
// paths may keep their pre-start assembly inline when they need setup-specific
// diagnostics before storing first-start metadata.
func sessionCoreConfigForHash(tp TemplateParams, session beads.Bead) runtime.Config {
	agentCfg := templateParamsToConfig(tp)
	applyTemplateOverridesToConfig(&agentCfg, session, tp)
	return agentCfg
}
