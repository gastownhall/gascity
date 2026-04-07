package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func newSessionManager(store beads.Store, sp runtime.Provider) *session.Manager {
	cityPath, err := resolveCity()
	if err != nil {
		return session.NewManager(store, sp)
	}
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		return session.NewManagerWithCityPath(store, sp, cityPath)
	}
	return newSessionManagerWithConfig(cityPath, store, sp, cfg)
}

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
