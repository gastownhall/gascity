package main

// The canonical singleton seat's orphan close (#9245). A max_active_sessions
// = 1 pool with no namepool keeps one seat under the template's own qualified
// name, and that seat claims work under it (hookClaimAssigneeIdentity). Work
// claimed under that alias belongs to the configured identity, not to the
// row that happened to carry it, so it never keeps an orphaned, not-running
// seat open: the seat closes once the row owns no work a lane serves under
// its own identifiers (rowOwnScope). Both reconcilers judge the row's work,
// and whether all of it is stranded, over that scope alone. A claim of the
// row's own that no lane serves stays on the closed row, stamped, as any
// orphan's does (O5, revised).
//
// The close gives every alias claim a way back to the pool. The orphan
// release returns the routed ones before the close, as it returns any
// orphan's (ga-jrnou). The cascade then releases the rest with the seat's
// route, as every close releases a routeless claim (laneCarried), and route
// recovery turns that route into pool demand once the claim is ready. A claim
// the cascade leaves on the alias is one routed to the identity that the
// orphan release kept (its detached runtime may still run it). The pool's
// demand for the identity reaches such a claim: the wake-known-identity tier,
// or the dead-assignee sweep, which probes it again. A routeless claim kept
// on the alias would be reachable by neither and would strand silently.

import (
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
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

// rowOwnScope is info's ReleaseScope without identity: the identifiers the
// row's own work is held under, which gate its canonical singleton orphan
// close. An empty identity leaves the scope whole.
func rowOwnScope(info sessionpkg.Info, cfg *config.City, identity string) ReleaseScope {
	return releaseScope(info, cfg).except(assigneePreserveSet([]string{identity}))
}

// rowOwnsNoServedWorkBesidesIdentity reports that info's seat holds no work a
// lane serves under its own identifiers (rowOwnScope): no open or in_progress
// work, on the tick's read and again on a live re-read, as a work-gated close
// reads it; or only claims no lane serves (onlyStrandedWorkIn), which the
// close keeps on the closed row, stamps and alerts on, as any orphan's (O5,
// revised). A failed read counts as work.
func rowOwnsNoServedWorkBesidesIdentity(sw *SeatWork, info sessionpkg.Info, cfg *config.City, identity string) bool {
	scope := rowOwnScope(info, cfg, identity)
	if len(scope.scopeIDs()) == 0 {
		return false
	}
	if held, err := seatHasWorkForCloseGate(sw, scope); err == nil && !held {
		if late, err := lateSeatWork(sw.Legs(), scope, seatWorkStatuses, notOwnDrainStep); err == nil && len(late) == 0 {
			return true
		}
	}
	return onlyStrandedWorkIn(sw, scope, info)
}

// canonicalIdentityKeeps reports that a canonical singleton seat's orphan
// close leaves b with identity (ReleaseOpts.Identity): b is held under
// identity and routes to it, so the pool's demand for identity reaches it.
// A workflow root routes by its run target only until it is expanded, as the
// release reads it (releaseRoute). No other claim stays: a routeless one
// would have no way back once the seat is gone.
func canonicalIdentityKeeps(cfg *config.City, identity string, b beads.Bead) bool {
	identity = strings.TrimSpace(identity)
	if identity == "" || strings.TrimSpace(b.Assignee) != identity {
		return false
	}
	route := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
	if route == "" && workflowRunTargetFallbackEligible(b) {
		route = strings.TrimSpace(b.Metadata[beadmeta.RunTargetMetadataKey])
	}
	return route != "" && agentTemplateIdentitiesEquivalent(cfg, agentutil.NormalizePoolRouteTarget(cfg, route), identity)
}
