package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// demandOnlySingletonSessionInfo reports whether info is pool capacity of a
// demand-only singleton template; see session.IsDemandOnlySingletonSession,
// which the API's wake refusal shares.
func demandOnlySingletonSessionInfo(info session.Info, agent *config.Agent, cfg *config.City) bool {
	return session.IsDemandOnlySingletonSession(cfg, agent, info)
}
