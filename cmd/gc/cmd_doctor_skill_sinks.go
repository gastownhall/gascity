package main

import (
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/session"
)

// doctorSkillStaticSinks resolves the config-derived skill sink
// directories the dangling-sink doctor check scans: every agent's
// scope-root × provider vendor sink, mirroring the stage-1
// materializer's targeting (skill_supervisor.go) so the check sees
// exactly the directories the materializer writes.
func doctorSkillStaticSinks(cityPath string, cfg *config.City) []string {
	var sinks []string
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		provider := effectiveAgentProviderFamily(agent, cfg.Workspace.Provider, cfg.Providers)
		vendor, ok := materialize.VendorSink(provider)
		if !ok {
			continue
		}
		scopeRoot := resolveAgentScopeRoot(agent, cityPath, cfg.Rigs)
		if !filepath.IsAbs(scopeRoot) {
			scopeRoot = filepath.Join(cityPath, scopeRoot)
		}
		sinks = append(sinks, filepath.Join(scopeRoot, vendor))
	}
	return sinks
}

// doctorLiveSessionSinks returns a lazy enumerator for the dangling-sink
// doctor check: each live (non-closed) session's WorkDir × vendor sink.
// Stage-2 sessions materialize into their per-session worktree, not the
// scope root, so scope-root-only scanning misses exactly the crew sinks
// hq-38je found broken. Laziness keeps the session store out of doctor
// check construction; a store failure yields no live sinks rather than
// failing the whole check (the static sinks still scan).
func doctorLiveSessionSinks(cityPath string, cfg *config.City) func() []string {
	return func() []string {
		store, err := openSessionProviderStore(cityPath)
		if err != nil {
			return nil
		}
		infos, err := session.NewStore(beads.SessionStore{Store: cliSessionStore(store, cfg, cityPath)}).ListLabeledSessionInfosUnfiltered()
		if err != nil {
			return nil
		}
		var sinks []string
		for _, info := range infos {
			if info.Closed || info.WorkDir == "" {
				continue
			}
			vendor, ok := materialize.VendorSink(info.Provider)
			if !ok {
				continue
			}
			sinks = append(sinks, filepath.Join(info.WorkDir, vendor))
		}
		return sinks
	}
}
