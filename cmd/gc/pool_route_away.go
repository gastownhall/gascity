package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// routeAwaySettleWindow is how long a handoff away from a live holder settles
// before the orphan-release sweep may release it.
const routeAwaySettleWindow = 90 * time.Second

// routeAwayOwner is one open session that answers to an assignee identity:
// the store ref it reaches work under (makeOpenSessionStoreRefIndex's refs,
// including its wildcards) and its resolved agent, nil when unresolved.
type routeAwayOwner struct {
	storeRef string
	agent    *config.Agent
}

// routeAwayOwnerIndex maps an assignee identity to every open session that
// answers to it. It is built from the same helpers as
// makeOpenSessionStoreRefIndex, so a holder the sweep's ownership guard sees
// is a holder this index sees.
type routeAwayOwnerIndex map[string][]routeAwayOwner

// makeRouteAwayOwnerIndex indexes the open sessions of one sweep by assignee
// identity. claimRefs is the city's assignedWorkClaimRefs, resolved by the
// caller. In legacy (!storeRefAware) mode every owner is recorded under the
// empty ref, and reach is by identity alone.
func makeRouteAwayOwnerIndex(cityPath string, cfg *config.City, claimRefs []string, openSessionInfos []session.Info, storeRefAware bool) routeAwayOwnerIndex {
	index := make(routeAwayOwnerIndex, len(openSessionInfos))
	for _, info := range openSessionInfos {
		if info.Closed {
			continue
		}
		agent := sessionAgentConfigInfo(cfg, info)
		refs := []string{""}
		if storeRefAware {
			refs = openSessionReachableStoreRefInfo(cityPath, cfg, claimRefs, info)
		}
		for _, id := range sessionBeadAssigneeIdentitiesForConfigInfo(info, cfg) {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			for _, ref := range refs {
				index[id] = append(index[id], routeAwayOwner{storeRef: ref, agent: agent})
			}
		}
	}
	return index
}

// ownersRouteAway reports whether at least one open session answering to
// assignee reaches workStoreRef, and every such session resolves to an agent
// other than target. An unresolved owner, or one serving target itself (see
// routeAwayOwnerServesTarget), keeps the claim; with no reaching owner the
// dead-assignee gates decide instead.
func (ix routeAwayOwnerIndex) ownersRouteAway(target *config.Agent, assignee, workStoreRef string, storeRefAware bool) bool {
	reached := false
	for _, owner := range ix[assignee] {
		if storeRefAware && !openSessionRefReaches(owner.storeRef, workStoreRef) {
			continue
		}
		if owner.agent == nil || routeAwayOwnerServesTarget(owner.agent, target) {
			return false
		}
		reached = true
	}
	return reached
}

// routeAwayOwnerServesTarget reports whether an owner of agent owner already
// serves route target: owner is target, or owner is a cross-store-eligible
// (city-scoped) agent of target's name, which federates over target's
// rig-routed work (vp-kvp).
func routeAwayOwnerServesTarget(owner, target *config.Agent) bool {
	if owner.QualifiedName() == target.QualifiedName() {
		return true
	}
	return agentIsCrossStoreEligible(owner) && owner.Name == target.Name
}

// openSessionRefReaches is openSessionOwnsWork's per-ref test: a wildcard ref
// reaches every work store ref, any other ref only its own.
func openSessionRefReaches(ownerRef, workStoreRef string) bool {
	return ownerRef == unresolvedOpenSessionStoreRef || ownerRef == crossStoreOpenSessionStoreRef || ownerRef == workStoreRef
}

// routeAwaySettled reports whether a handoff stamped routedAt (gc.routed_at)
// has settled at now: false only while the stamp parses and lies in
// (now-routeAwaySettleWindow, now+routeAwaySettleWindow]. A missing or
// malformed stamp, or one more than a window ahead of now (a skewed or forged
// clock), is settled, so no stamp can keep a routed-away claim from its target.
func routeAwaySettled(routedAt string, now time.Time) bool {
	at, ok := beadmeta.ParseRoutedAt(routedAt)
	if !ok {
		return true
	}
	return !at.After(now.Add(-routeAwaySettleWindow)) || at.After(now.Add(routeAwaySettleWindow))
}

// routeAwayReleasable reports whether claimed bead wb was handed off away
// from its live holder and the handoff has settled at now: wb is no control
// bead (the dispatcher, not a pool, owns those), it is routed to the
// generic-ephemeral pool target (template, resolved by the caller), its
// assignee is a session identity rather than the bare target template, every
// open session answering to that identity in wb's store resolves to an agent
// other than target, and its gc.routed_at stamp is settled. Such a claim is
// released to the target pool even though its holder lives: otherwise the
// holder keeps it forever and the target never sees demand.
func routeAwayReleasable(owners routeAwayOwnerIndex, wb beads.Bead, target *config.Agent, template, workStoreRef string, storeRefAware bool, now time.Time) bool {
	assignee := strings.TrimSpace(wb.Assignee)
	if target == nil || assignee == "" || beadmeta.IsControlKind(wb.Metadata[beadmeta.KindMetadataKey]) {
		return false
	}
	if assignee == strings.TrimSpace(template) || assignee == target.QualifiedName() {
		return false
	}
	if !owners.ownersRouteAway(target, assignee, workStoreRef, storeRefAware) {
		return false
	}
	return routeAwaySettled(wb.Metadata[beadmeta.RoutedAtMetadataKey], now)
}
