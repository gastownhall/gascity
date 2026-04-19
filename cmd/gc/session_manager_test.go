package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func newSessionManagerWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City) *session.Manager {
	if cfg == nil {
		return session.NewManagerWithCityPath(store, sp, cityPath)
	}
	rigContext := currentRigContext(cfg)
	return session.NewManagerWithTransportResolverAndCityPath(store, sp, cityPath, func(template string) string {
		agentCfg, ok := resolveAgentIdentity(cfg, template, rigContext)
		if !ok {
			return ""
		}
		return agentCfg.Session
	})
}
