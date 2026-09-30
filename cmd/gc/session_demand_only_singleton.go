package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// demandOnlySingletonSessionInfo reports whether info is pool capacity of a
// demand-only singleton template (see session.IsDemandOnlySingletonTemplate).
// buildDesiredState keeps such a session only while the pool has work for it,
// so neither pin_awake nor an explicit wake request can start it or keep it
// running (#6858). Named and manual sessions of the same template start on
// their own terms and are excluded.
func demandOnlySingletonSessionInfo(info session.Info, agent *config.Agent, cfg *config.City) bool {
	if agent == nil || !session.IsDemandOnlySingletonTemplate(cfg, agent) {
		return false
	}
	if isNamedSessionInfo(info) || isManualSessionInfoForAgent(info, agent) {
		return false
	}
	return isEphemeralSessionInfoForAgent(info, agent)
}

// sessionRunningInfo reports whether the session's lifecycle state says its
// runtime is up.
func sessionRunningInfo(info session.Info) bool {
	return sessionMetadataStateInfo(info) == "active"
}
