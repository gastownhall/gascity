package main

// The canonical singleton seat's orphan close (#9245). A max_active_sessions
// = 1 pool with no namepool keeps one seat under the template's own qualified
// name, and that seat claims work under it (hookClaimAssigneeIdentity). Work
// claimed under that alias belongs to the configured identity, not to the
// row that happened to carry it, as a named session's work does (ga-n2d). An
// orphaned, not-running seat blocked only by such claims closes, and the
// claims keep the alias for the next seat the pool starts under it.

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// canonicalSingletonSeatIdentity reports the configured identity info's
// seat holds as a canonical singleton pool seat: a pool-managed row, neither
// named nor manual, whose agent uses the canonical singleton identity, whose
// stable alias is that agent's qualified name, and which no operator hold
// keeps. Any other row reports false.
func canonicalSingletonSeatIdentity(info sessionpkg.Info, cfg *config.City) (string, bool) {
	if cfg == nil || !isPoolManagedSessionInfo(info) || isNamedSessionInfo(info) || isManualSessionInfo(info) {
		return "", false
	}
	agent := findAgentByTemplate(cfg, normalizedSessionTemplateInfo(info, cfg))
	if !agent.UsesCanonicalSingletonPoolIdentity() {
		return "", false
	}
	identity := agent.QualifiedName()
	if alias := strings.TrimSpace(stableAssignmentAliasForConfigInfo(info, cfg)); identity == "" || alias != identity {
		return "", false
	}
	if sessionpkg.DispositionOfInfo(info).KeepsSlot() {
		return "", false
	}
	return identity, true
}
