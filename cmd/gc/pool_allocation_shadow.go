package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/config"
)

type poolAllocationShadowAction string

const (
	poolAllocationShadowLegacy   poolAllocationShadowAction = "legacy"
	poolAllocationShadowStartOne poolAllocationShadowAction = "start_one"
)

type poolAllocationShadowReason string

const (
	poolAllocationShadowEligible              poolAllocationShadowReason = "eligible_default_pool"
	poolAllocationShadowEligibleAgentCap      poolAllocationShadowReason = "eligible_agent_cap"
	poolAllocationShadowColdFromZero          poolAllocationShadowReason = "cold_from_zero"
	poolAllocationShadowInvalidConfig         poolAllocationShadowReason = "invalid_config"
	poolAllocationShadowSuspended             poolAllocationShadowReason = "suspended"
	poolAllocationShadowDisabled              poolAllocationShadowReason = "disabled"
	poolAllocationShadowCustomScaleCheck      poolAllocationShadowReason = "custom_scale_check"
	poolAllocationShadowDependencies          poolAllocationShadowReason = "dependencies"
	poolAllocationShadowNamedSession          poolAllocationShadowReason = "named_session"
	poolAllocationShadowMinFloor              poolAllocationShadowReason = "min_floor"
	poolAllocationShadowWorkspaceCap          poolAllocationShadowReason = "workspace_cap"
	poolAllocationShadowRigCap                poolAllocationShadowReason = "rig_cap"
	poolAllocationShadowNamepool              poolAllocationShadowReason = "namepool"
	poolAllocationShadowAgentCap              poolAllocationShadowReason = "agent_cap"
	poolAllocationShadowSourceStore           poolAllocationShadowReason = "source_store"
	poolAllocationShadowDemandUnsupported     poolAllocationShadowReason = "demand_unsupported"
	poolAllocationShadowMembershipUncertified poolAllocationShadowReason = "membership_uncertified"
	poolAllocationShadowInvalidMembership     poolAllocationShadowReason = "invalid_membership"
	poolAllocationShadowNonemptyPool          poolAllocationShadowReason = "nonempty_pool"
	poolAllocationShadowOccupiedGrowth        poolAllocationShadowReason = "occupied_growth"
)

// poolAllocationShadowPolicy answers ONE question — is this agent's pool the
// keyed lane's to allocate — in exactly two fields, and the split between them
// is the uniform predicate contract every pool-family site obeys
// (ga-f7v2ft.116 Q1, folded at WD.10a / council F13):
//
//	clause 1, ELIGIBILITY, is supported() and nothing else. A caller that
//	instead compares `reason` to a specific eligible value encodes its own
//	slice's scope into the shared vocabulary: the excluded arm becomes silently
//	unsatisfiable at that site while every sibling still accepts it, and a
//	future eligibility reason reaches only the sites spelled by supported().
//	Both indicted sites were exactly that, inverted relative to each other —
//	one demanded Eligible and took unlimited pools only, the other demanded
//	EligibleAgentCap and took bounded pools only.
//
//	clause 2, CAPACITY, is a test on maxActiveSessions, spelled as such. This
//	is the ONLY honest reading of a cap, because reason and maxActiveSessions
//	are not independent: the constructor sets maxActiveSessions exactly once,
//	on the EligibleAgentCap branch, and poolAllocationShadowHasCap admits only
//	non-negative caps. So under supported(), reason == EligibleAgentCap if and
//	only if maxActiveSessions >= 0, and reason == Eligible if and only if
//	maxActiveSessions == -1. Any `reason == EligibleAgentCap && max <op> N`
//	conjunction is therefore a capacity clause wearing a reason's clothes —
//	behavior-identical to the capacity half alone, and readable only by someone
//	who has re-derived this paragraph. TestPoolAllocationShadowPolicyCapacity
//	IsTheOnlyCapSpelling holds the biconditional so the shorter spelling stays
//	true.
//
// Identity-model exclusions (the canonical singleton, max == 1, whose rows ride
// the configured-named and configured-dependency families) are capacity-shaped
// and belong in clause 2 under their own name — never smuggled into clause 1.
type poolAllocationShadowPolicy struct {
	reason              poolAllocationShadowReason
	contributionPresent bool
	maxActiveSessions   int
}

// supported is clause 1 of the uniform predicate contract for sites that TAKE a
// seat: the single eligibility spelling for every pool-family allocation site.
// See the type doc.
func (p poolAllocationShadowPolicy) supported() bool {
	return p.reason == poolAllocationShadowEligible || p.reason == poolAllocationShadowEligibleAgentCap
}

// releaseEligible is clause 1 for sites that GIVE a seat BACK. A release — a
// drain acknowledgement finalizing into stop — never consumes capacity, so
// every capacity-shaped reason is eligible here: a min floor, a workspace or
// rig cap, a namepool, and an agent cap all bound how many seats may be TAKEN,
// and refusing a release under one leaks the very seat it exists to bound. Only
// the identity-model exclusions can disqualify a release, because only they say
// the keyed lane does not own this agent's pool at all — and that is exactly
// what contributionPresent already records, which is why this reads the field
// rather than re-spelling the partition as a reason list. Gating a release on
// supported() is the allocation question asked at a release site: it makes any
// city with a workspace cap refuse every agent drain acknowledgement forever,
// stranding the row in draining and the pool slot with it (ga-f7v2ft, mc-by7s).
func (p poolAllocationShadowPolicy) releaseEligible() bool {
	return p.contributionPresent
}

func newPoolAllocationShadowPolicy(
	cfg *config.City,
	agent *config.Agent,
	namedTemplates map[string]struct{},
) poolAllocationShadowPolicy {
	policy := poolAllocationShadowPolicy{
		reason:              poolAllocationShadowEligible,
		contributionPresent: true,
		maxActiveSessions:   -1,
	}
	if cfg == nil || agent == nil {
		policy.reason = poolAllocationShadowInvalidConfig
		policy.contributionPresent = false
		return policy
	}
	if agent.Suspended {
		policy.reason = poolAllocationShadowSuspended
		policy.contributionPresent = false
		return policy
	}
	if !agent.SupportsGenericEphemeralSessions() {
		policy.reason = poolAllocationShadowDisabled
		policy.contributionPresent = false
		return policy
	}
	if strings.TrimSpace(agent.ScaleCheck) != "" {
		policy.reason = poolAllocationShadowCustomScaleCheck
		policy.contributionPresent = false
		return policy
	}
	if len(agent.DependsOn) > 0 {
		policy.reason = poolAllocationShadowDependencies
		policy.contributionPresent = false
		return policy
	}
	if _, exists := namedTemplates[agent.QualifiedName()]; exists {
		policy.reason = poolAllocationShadowNamedSession
		policy.contributionPresent = false
		return policy
	}
	if agent.EffectiveMinActiveSessions() > 0 {
		policy.reason = poolAllocationShadowMinFloor
		return policy
	}
	if poolAllocationShadowHasCap(cfg.Workspace.MaxActiveSessions) {
		policy.reason = poolAllocationShadowWorkspaceCap
		return policy
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == agent.Dir && poolAllocationShadowHasCap(cfg.Rigs[i].MaxActiveSessions) {
			policy.reason = poolAllocationShadowRigCap
			return policy
		}
	}
	if strings.TrimSpace(agent.Namepool) != "" || len(agent.NamepoolNames) > 0 {
		policy.reason = poolAllocationShadowNamepool
		return policy
	}
	if maximum := agent.EffectiveMaxActiveSessions(); poolAllocationShadowHasCap(maximum) {
		policy.reason = poolAllocationShadowEligibleAgentCap
		policy.maxActiveSessions = *maximum
		return policy
	}
	return policy
}

func (p poolAllocationShadowPolicy) forSourceStore(
	cfg *config.City,
	agent *config.Agent,
	cityPath string,
	storeRef string,
) poolAllocationShadowPolicy {
	if !p.supported() {
		return p
	}
	if strings.TrimSpace(storeRef) == "" || !agentutil.AgentReachesWorkflowStore(storeRef, agent, cityPath, cfg) {
		p.reason = poolAllocationShadowSourceStore
	}
	return p
}

func poolAllocationShadowHasCap(limit *int) bool {
	return limit != nil && *limit >= 0
}

type poolAllocationShadowDecision struct {
	workID             string
	poolTarget         string
	action             poolAllocationShadowAction
	reason             poolAllocationShadowReason
	startCount         int
	poolSlot           int
	membershipRevision uint64
}

func decideRoutedWorkPoolAllocationShadow(
	contribution readyRoutedWorkDemandContribution,
	membership poolMembershipObservation,
) poolAllocationShadowDecision {
	decision := poolAllocationShadowDecision{
		workID:             contribution.WorkID,
		poolTarget:         contribution.PoolTarget,
		action:             poolAllocationShadowLegacy,
		reason:             contribution.AllocationPolicy.reason,
		membershipRevision: membership.revision,
	}
	if !contribution.ContributionPresent {
		if decision.reason == poolAllocationShadowEligible || decision.reason == "" {
			decision.reason = poolAllocationShadowDemandUnsupported
		}
		return decision
	}
	if !contribution.AllocationPolicy.supported() {
		if decision.reason == "" {
			decision.reason = poolAllocationShadowInvalidConfig
		}
		return decision
	}
	if !membership.certified {
		decision.reason = poolAllocationShadowMembershipUncertified
		return decision
	}
	if membership.members < 0 || membership.occupied < 0 || membership.occupied > membership.members {
		decision.reason = poolAllocationShadowInvalidMembership
		return decision
	}
	if contribution.AllocationPolicy.maxActiveSessions > 0 &&
		membership.occupied >= contribution.AllocationPolicy.maxActiveSessions {
		decision.reason = poolAllocationShadowAgentCap
		return decision
	}
	if membership.members != 0 {
		if membership.members == membership.occupied && membership.occupied > 0 {
			if membership.nextFreeSlot <= 0 {
				decision.reason = poolAllocationShadowInvalidMembership
				return decision
			}
			decision.action = poolAllocationShadowStartOne
			decision.reason = poolAllocationShadowOccupiedGrowth
			decision.startCount = 1
			decision.poolSlot = membership.nextFreeSlot
			return decision
		}
		decision.reason = poolAllocationShadowNonemptyPool
		return decision
	}
	decision.action = poolAllocationShadowStartOne
	decision.reason = poolAllocationShadowColdFromZero
	decision.startCount = 1
	decision.poolSlot = 1
	return decision
}
