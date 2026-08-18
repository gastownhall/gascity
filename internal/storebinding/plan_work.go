package storebinding

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	// ErrInvalidWorkPin reports a pinned Work scope that cannot describe one
	// addressable bootstrap workspace.
	ErrInvalidWorkPin = errors.New("invalid pinned work scope")
	// ErrWorkPinDrift reports recorded pins that disagree with freshly observed
	// workspace metadata. Recorded pins always win, so the disagreement blocks
	// instead of refreshing the pin.
	ErrWorkPinDrift = errors.New("pinned work identity drifted from observed metadata")
	// ErrWorkTopologyDrift reports an opened Work topology whose facts differ
	// from the pinned plan.
	ErrWorkTopologyDrift = errors.New("opened work topology differs from pinned plan")
)

// WorkScopePin is one caller-supplied pinned Work scope: either a recorded
// bootstrap pin from the durable manifest or, on true genesis, the
// mutation-free startup enumeration handed in as data. The planner never reads
// live bd workspace metadata to build one.
type WorkScopePin struct {
	Scope       WorkScope
	Prefix      string
	Suspended   bool
	OpenerID    string
	ComponentID string
	PhysicalID  string
}

// WorkPinInputs carries the complete pinned bootstrap Work topology. Rigs are
// in config order and every configured rig with a workspace belongs here,
// suspended ones included. Observed is optional freshly observed metadata used
// only for drift detection: it never overrides a pin.
type WorkPinInputs struct {
	Recorded      bool
	ConfigContext ConfigRefDigest
	HQ            WorkScopePin
	Rigs          []WorkScopePin
	Observed      []WorkScopePin
}

// PinnedWorkScope is one validated scope of the frozen Work topology plan.
type PinnedWorkScope struct {
	Scope       WorkScope
	Prefix      string
	Suspended   bool
	OpenerID    string
	ComponentID string
	PhysicalID  string
	ConfigRank  int
}

func (p PinnedWorkScope) identity() pinnedPhysicalIdentity {
	return pinnedPhysicalIdentity{opener: p.OpenerID, component: p.ComponentID, physical: p.PhysicalID}
}

type pinnedPhysicalIdentity struct{ opener, component, physical string }

// PinnedPhysicalGroup is every semantic scope sharing one pinned physical
// workspace. Unified and shared workspaces group once while keeping all member
// scopes.
type PinnedPhysicalGroup struct {
	OpenerID    string
	ComponentID string
	PhysicalID  string
	Scopes      []WorkScope
}

// Clone returns a detached physical group.
func (g PinnedPhysicalGroup) Clone() PinnedPhysicalGroup {
	out := g
	out.Scopes = append([]WorkScope(nil), g.Scopes...)
	return out
}

// WorkTopologyPlan is the pinned identity-only plan for the reserved work
// binding. It carries facts, not handles: plan resolution opens nothing, so the real
// WorkTopology is composed by the provider at open time and fact-checked
// against this plan by the StoreSet builder.
type WorkTopologyPlan struct {
	present       bool
	recorded      bool
	configContext ConfigRefDigest
	hq            PinnedWorkScope
	rigs          []PinnedWorkScope
	physical      []PinnedPhysicalGroup
}

func newWorkTopologyPlan(in WorkPinInputs) (WorkTopologyPlan, error) {
	if err := validateCanonicalSHA256Digest("work config context", string(in.ConfigContext)); err != nil {
		return WorkTopologyPlan{}, fmt.Errorf("%w: %w", ErrInvalidWorkPin, err)
	}
	if !in.HQ.Scope.IsHQ() {
		return WorkTopologyPlan{}, fmt.Errorf("%w: exactly one HQ scope is required", ErrInvalidWorkPin)
	}
	plan := WorkTopologyPlan{present: true, recorded: in.Recorded, configContext: in.ConfigContext}
	pinned := make([]PinnedWorkScope, 0, len(in.Rigs)+1)
	pinned = append(pinned, pinScope(in.HQ, 0))
	for index, rig := range in.Rigs {
		if rig.Scope.IsHQ() {
			return WorkTopologyPlan{}, fmt.Errorf("%w: duplicate HQ scope", ErrInvalidWorkPin)
		}
		if name, ok := rig.Scope.Rig(); !ok || name == "" {
			return WorkTopologyPlan{}, fmt.Errorf("%w: rig scope has no name", ErrInvalidWorkPin)
		}
		pinned = append(pinned, pinScope(rig, index+1))
	}
	if err := validatePinnedScopes(pinned); err != nil {
		return WorkTopologyPlan{}, err
	}
	plan.hq = pinned[0]
	plan.rigs = append([]PinnedWorkScope(nil), pinned[1:]...)
	plan.physical = groupPinnedPhysical(pinned)
	if err := plan.checkObserved(in.Observed); err != nil {
		return WorkTopologyPlan{}, err
	}
	return plan, nil
}

func pinScope(pin WorkScopePin, rank int) PinnedWorkScope {
	return PinnedWorkScope{
		Scope:       pin.Scope,
		Prefix:      pin.Prefix,
		Suspended:   pin.Suspended,
		OpenerID:    pin.OpenerID,
		ComponentID: pin.ComponentID,
		PhysicalID:  pin.PhysicalID,
		ConfigRank:  rank,
	}
}

// validatePinnedScopes rejects every aliasing shape the shipped WorkTopology
// would only discover at runtime: duplicate scopes, unusable identities, and
// prefixes that cannot select one unique scope for an ID.
func validatePinnedScopes(pinned []PinnedWorkScope) error {
	scopes := make(map[WorkScope]struct{}, len(pinned))
	for _, scope := range pinned {
		if _, duplicate := scopes[scope.Scope]; duplicate {
			return fmt.Errorf("%w: duplicate scope %s", ErrInvalidWorkPin, scope.Scope)
		}
		scopes[scope.Scope] = struct{}{}
		if !validWorkPrefix(scope.Prefix) {
			return fmt.Errorf("%w: scope %s has an invalid prefix", ErrInvalidWorkPin, scope.Scope)
		}
		if err := validateIdentifier("work opener ID", scope.OpenerID); err != nil {
			return fmt.Errorf("%w: scope %s: %w", ErrInvalidWorkPin, scope.Scope, err)
		}
		if err := validateIdentifier("work component ID", scope.ComponentID); err != nil {
			return fmt.Errorf("%w: scope %s: %w", ErrInvalidWorkPin, scope.Scope, err)
		}
		if strings.TrimSpace(scope.PhysicalID) == "" {
			return fmt.Errorf("%w: scope %s has no physical identity", ErrInvalidWorkPin, scope.Scope)
		}
		if err := validateSecretFree("work physical identity", scope.PhysicalID); err != nil {
			return err
		}
	}
	for outer := range pinned {
		for inner := range pinned {
			if outer == inner {
				continue
			}
			if strings.HasPrefix(pinned[outer].Prefix+"-", pinned[inner].Prefix+"-") {
				return fmt.Errorf("%w: scopes %s and %s cannot be selected by prefix", ErrInvalidWorkPin, pinned[inner].Scope, pinned[outer].Scope)
			}
		}
	}
	return nil
}

// groupPinnedPhysical mirrors groupPhysical: HQ first, then rigs in config
// order, grouped by the full (opener, component, physical) identity triple.
func groupPinnedPhysical(pinned []PinnedWorkScope) []PinnedPhysicalGroup {
	index := make(map[pinnedPhysicalIdentity]int, len(pinned))
	groups := make([]PinnedPhysicalGroup, 0, len(pinned))
	for _, scope := range pinned {
		key := scope.identity()
		if position, exists := index[key]; exists {
			groups[position].Scopes = append(groups[position].Scopes, scope.Scope)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, PinnedPhysicalGroup{
			OpenerID:    scope.OpenerID,
			ComponentID: scope.ComponentID,
			PhysicalID:  scope.PhysicalID,
			Scopes:      []WorkScope{scope.Scope},
		})
	}
	return groups
}

func (p WorkTopologyPlan) checkObserved(observed []WorkScopePin) error {
	if len(observed) == 0 {
		return nil
	}
	pinned := p.allInConfigOrder()
	if len(observed) != len(pinned) {
		return fmt.Errorf("%w: observed %d scopes for %d pinned scopes", ErrWorkPinDrift, len(observed), len(pinned))
	}
	byScope := make(map[WorkScope]PinnedWorkScope, len(pinned))
	for _, scope := range pinned {
		byScope[scope.Scope] = scope
	}
	for _, seen := range observed {
		scope, found := byScope[seen.Scope]
		if !found {
			return fmt.Errorf("%w: observed unpinned scope %s", ErrWorkPinDrift, seen.Scope)
		}
		if scope.Prefix != seen.Prefix || scope.Suspended != seen.Suspended ||
			scope.OpenerID != seen.OpenerID || scope.ComponentID != seen.ComponentID || scope.PhysicalID != seen.PhysicalID {
			return fmt.Errorf("%w: scope %s", ErrWorkPinDrift, seen.Scope)
		}
	}
	return nil
}

// Present reports whether a pinned Work topology was planned.
func (p WorkTopologyPlan) Present() bool { return p.present }

// Recorded reports whether the pins came from a durable record rather than a
// genesis enumeration.
func (p WorkTopologyPlan) Recorded() bool { return p.recorded }

// ConfigContext returns the secret-free config digest pinned with the topology.
func (p WorkTopologyPlan) ConfigContext() ConfigRefDigest { return p.configContext }

// HQ returns the pinned HQ scope.
func (p WorkTopologyPlan) HQ() PinnedWorkScope { return p.hq }

// RigsInConfigOrder returns the pinned rig scopes in config order, suspended
// rigs included. Consumers keep their own inclusion rules.
func (p WorkTopologyPlan) RigsInConfigOrder() []PinnedWorkScope {
	return append([]PinnedWorkScope(nil), p.rigs...)
}

// RigsInLexicalOrder returns the pinned rig scopes in lexical rig-name order.
func (p WorkTopologyPlan) RigsInLexicalOrder() []PinnedWorkScope {
	out := append([]PinnedWorkScope(nil), p.rigs...)
	sort.SliceStable(out, func(i, j int) bool {
		left, _ := out[i].Scope.Rig()
		right, _ := out[j].Scope.Rig()
		return left < right
	})
	return out
}

// All returns HQ followed by every rig in lexical order, matching the
// deterministic WorkTopology.All order the builder fact-checks against.
func (p WorkTopologyPlan) All() []PinnedWorkScope {
	return append([]PinnedWorkScope{p.hq}, p.RigsInLexicalOrder()...)
}

func (p WorkTopologyPlan) allInConfigOrder() []PinnedWorkScope {
	return append([]PinnedWorkScope{p.hq}, p.rigs...)
}

// Physical returns one pinned group per physical workspace, each retaining
// every member scope.
func (p WorkTopologyPlan) Physical() []PinnedPhysicalGroup {
	out := make([]PinnedPhysicalGroup, len(p.physical))
	for index, group := range p.physical {
		out[index] = group.Clone()
	}
	return out
}

// ForScope returns one pinned scope with the shipped typed not-found error.
func (p WorkTopologyPlan) ForScope(scope WorkScope) (PinnedWorkScope, error) {
	for _, pinned := range p.allInConfigOrder() {
		if pinned.Scope == scope {
			return pinned, nil
		}
	}
	return PinnedWorkScope{}, &WorkScopeNotFoundError{Scope: scope}
}

// PrefixScopeForID applies the pinned exact-prefix half of by-ID selection.
// It reports false when no pinned prefix matches, because the residence probe
// that follows needs live stores the plan deliberately does not hold.
func (p WorkTopologyPlan) PrefixScopeForID(id string) (WorkScope, bool, error) {
	var matched []PinnedWorkScope
	for _, pinned := range p.All() {
		if strings.HasPrefix(id, pinned.Prefix+"-") {
			matched = append(matched, pinned)
		}
	}
	switch len(matched) {
	case 0:
		return WorkScope{}, false, nil
	case 1:
		return matched[0].Scope, true, nil
	default:
		scopes := make([]WorkScope, len(matched))
		for index, pinned := range matched {
			scopes[index] = pinned.Scope
		}
		return WorkScope{}, false, &DuplicateWorkResidenceError{ID: id, Candidates: scopes}
	}
}

// Members projects the pinned scopes into the grouped Work migration member
// shape intent derivation records at INTENT_FSYNCED.
func (p WorkTopologyPlan) Members() []WorkWorkspaceMember {
	pinned := p.allInConfigOrder()
	members := make([]WorkWorkspaceMember, 0, len(pinned))
	for _, scope := range pinned {
		members = append(members, WorkWorkspaceMember{
			Scope:            scope.Scope,
			Prefix:           scope.Prefix,
			ConfigContext:    p.configContext,
			Suspended:        scope.Suspended,
			ConfigOrder:      scope.ConfigRank,
			Provider:         ProviderID(scope.OpenerID),
			Component:        ComponentID(scope.ComponentID),
			PhysicalIdentity: PhysicalIdentity(scope.PhysicalID),
		})
	}
	return members
}

// Participants groups the pinned members into one migration participant per
// physical workspace, so one physical workspace migrates and closes once while
// every semantic scope survives as a member.
func (p WorkTopologyPlan) Participants() ([]WorkWorkspaceParticipant, error) {
	if !p.present {
		return nil, nil
	}
	return GroupWorkParticipants(p.Members())
}

// checkTopology compares an opened Work topology against the pins. Semantic
// facts (scope set, prefixes, suspension, config order) are always compared;
// the pinned identity triples and physical grouping are compared only for the
// reserved binding, because a moved Work class is served by another provider's
// identities while keeping every semantic fact.
func (p WorkTopologyPlan) checkTopology(topology WorkTopology, identity bool) error {
	if !p.present {
		return fmt.Errorf("%w: no pinned topology", ErrWorkTopologyDrift)
	}
	pinned := p.allInConfigOrder()
	opened := append([]Workspace{}, topology.All()...)
	if len(opened) != len(pinned) {
		return fmt.Errorf("%w: opened %d scopes for %d pinned scopes", ErrWorkTopologyDrift, len(opened), len(pinned))
	}
	for _, scope := range pinned {
		workspace, err := topology.ForScope(scope.Scope)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrWorkTopologyDrift, err)
		}
		if workspace.Prefix != scope.Prefix || workspace.Suspended != scope.Suspended {
			return fmt.Errorf("%w: scope %s facts differ", ErrWorkTopologyDrift, scope.Scope)
		}
		if identity && (workspace.OpenerID != scope.OpenerID || workspace.ComponentID != scope.ComponentID || workspace.PhysicalID != scope.PhysicalID) {
			return fmt.Errorf("%w: scope %s identity differs", ErrWorkTopologyDrift, scope.Scope)
		}
	}
	openedRigs := topology.RigsInConfigOrder(true)
	if len(openedRigs) != len(p.rigs) {
		return fmt.Errorf("%w: opened %d rigs for %d pinned rigs", ErrWorkTopologyDrift, len(openedRigs), len(p.rigs))
	}
	for index, rig := range p.rigs {
		if openedRigs[index].Scope != rig.Scope {
			return fmt.Errorf("%w: rig config order differs at position %d", ErrWorkTopologyDrift, index)
		}
	}
	if !identity {
		return nil
	}
	return p.checkPhysicalGrouping(topology)
}

func (p WorkTopologyPlan) checkPhysicalGrouping(topology WorkTopology) error {
	openedGroups := topology.PhysicalWorkspaces()
	if len(openedGroups) != len(p.physical) {
		return fmt.Errorf("%w: opened %d physical workspaces for %d pinned", ErrWorkTopologyDrift, len(openedGroups), len(p.physical))
	}
	for index, group := range p.physical {
		opened := openedGroups[index]
		if opened.Workspace.OpenerID != group.OpenerID || opened.Workspace.ComponentID != group.ComponentID || opened.Workspace.PhysicalID != group.PhysicalID {
			return fmt.Errorf("%w: physical workspace %d identity differs", ErrWorkTopologyDrift, index)
		}
		if len(opened.Scopes) != len(group.Scopes) {
			return fmt.Errorf("%w: physical workspace %d membership differs", ErrWorkTopologyDrift, index)
		}
		for member, scope := range group.Scopes {
			if opened.Scopes[member] != scope {
				return fmt.Errorf("%w: physical workspace %d membership differs", ErrWorkTopologyDrift, index)
			}
		}
	}
	return nil
}

func (p WorkTopologyPlan) sameFrozenFacts(other WorkTopologyPlan) bool {
	if p.present != other.present || p.recorded != other.recorded || p.configContext != other.configContext {
		return false
	}
	left, right := p.allInConfigOrder(), other.allInConfigOrder()
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
