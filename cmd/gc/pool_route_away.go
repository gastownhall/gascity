package main

import (
	"errors"
	"fmt"
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

// routeAwayReleasable reports whether claimed bead wb is held away from the
// pool it is routed to and its route has settled at now: wb is no control
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

// routeAwayJudge is one orphan-release sweep's route-away judgment: the
// owners of the sweep's open sessions, and the clock it judges routes at.
type routeAwayJudge struct {
	cfg           *config.City
	owners        routeAwayOwnerIndex
	storeRefAware bool
	now           time.Time
}

// newRouteAwayJudge builds the judge of one sweep over openSessionInfos.
// claimRefs is the city's assignedWorkClaimRefs, resolved by the caller.
func newRouteAwayJudge(cityPath string, cfg *config.City, claimRefs []string, openSessionInfos []session.Info, storeRefAware bool, now time.Time) *routeAwayJudge {
	return &routeAwayJudge{
		cfg:           cfg,
		owners:        makeRouteAwayOwnerIndex(cityPath, cfg, claimRefs, openSessionInfos, storeRefAware),
		storeRefAware: storeRefAware,
		now:           now,
	}
}

// releasable reports whether claim wb, listed under workStoreRef, is
// routeAwayReleasable, judged on wb's own route: a bead waiting on a person,
// or one whose route names no generic-ephemeral pool, is not. The sweep
// judges its listed row and the release its live row with this one test.
func (j *routeAwayJudge) releasable(wb beads.Bead, workStoreRef string) bool {
	if beadmeta.HasHumanLabel(wb.Labels) {
		return false
	}
	template := routedToOrLegacyWorkflowTarget(wb)
	target := findAgentByTemplate(j.cfg, template)
	if template == "" || !target.SupportsGenericEphemeralSessions() {
		return false
	}
	return routeAwayReleasable(j.owners, wb, target, template, workStoreRef, j.storeRefAware, j.now)
}

// release gives claim c, which the sweep judged releasable on the tick's
// listed row, back to its route's pool, only while its live row still is
// one. A route change after the list is invisible to the orphan release's
// fence: on a store whose lists carry no revision (BdStore) it checks status
// and assignee alone, so a bead routed back to its holder's agent, re-routed
// with a fresh stamp or labeled for a person since would still be released.
// So release reads c live, keeps it unless that row holds c's claim (status
// and assignee) and is releasable judged afresh, and releases that row under
// the route-away policy, whose one write is fenced on the row it judged
// (releaseJudgedRow): on its revision, so that any write landing after the
// read keeps the claim for the next tick, or, on a store that cannot fence a
// revision (a bd without --if-revision), on its status and assignee.
func (j *routeAwayJudge) release(c seatWorkHit, workStoreRef string) ReleaseOutcome {
	live, err := beads.HandlesFor(c.store).Live.Get(c.bead.ID)
	if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return ReleaseOutcome{Failed: []seatWorkHit{c}, Why: map[string]string{c.bead.ID: fmt.Sprintf("re-reading routed-away claim: %v", err)}}
	}
	if err != nil || live.Status != c.bead.Status || strings.TrimSpace(live.Assignee) != strings.TrimSpace(c.bead.Assignee) || !j.releasable(live, workStoreRef) {
		return ReleaseOutcome{Kept: []seatWorkHit{c}, Why: map[string]string{c.bead.ID: keptMoved}}
	}
	return releaseListed([]seatWorkHit{{store: c.store, bead: live}}, releaseRouteAway, releaseSeat{}, j.cfg, ReleaseOpts{})
}
