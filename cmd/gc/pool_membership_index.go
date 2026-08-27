package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type poolMembershipUncertifiedReason string

const (
	poolMembershipUncertifiedNotInitialized  poolMembershipUncertifiedReason = "not_initialized"
	poolMembershipUncertifiedSnapshotGap     poolMembershipUncertifiedReason = "snapshot_gap"
	poolMembershipUncertifiedInvalidSnapshot poolMembershipUncertifiedReason = "invalid_snapshot"
	poolMembershipUncertifiedDeltaRead       poolMembershipUncertifiedReason = "delta_read_failed"
	poolMembershipUncertifiedInvalidDelta    poolMembershipUncertifiedReason = "invalid_delta"
	poolMembershipUncertifiedConfigChanged   poolMembershipUncertifiedReason = "config_changed"
)

type poolMembershipContribution struct {
	sessionID        string
	poolTarget       string
	slot             int
	baseState        sessionpkg.BaseState
	countsAgainstCap bool
}

type poolMembershipState struct {
	bySession map[string]poolMembershipContribution
	memberIDs map[string]map[string]struct{}
	members   map[string]int
	occupied  map[string]int
	slots     map[string]map[int]string
}

type poolMembershipObservation struct {
	members      int
	occupied     int
	nextFreeSlot int
	certified    bool
	revision     uint64
	reason       poolMembershipUncertifiedReason
}

// poolMembershipIndex is the shadow read model for exact ephemeral-pool
// capacity. Rebuilds scan a supplied authoritative snapshot; one session
// mutation performs one map replacement and touches at most its old/new pools.
type poolMembershipIndex struct {
	mu        sync.RWMutex
	state     poolMembershipState
	revision  uint64
	certified bool
	reason    poolMembershipUncertifiedReason
}

func newPoolMembershipIndex() *poolMembershipIndex {
	return &poolMembershipIndex{
		state:  newPoolMembershipState(),
		reason: poolMembershipUncertifiedNotInitialized,
	}
}

func newPoolMembershipState() poolMembershipState {
	return poolMembershipState{
		bySession: make(map[string]poolMembershipContribution),
		memberIDs: make(map[string]map[string]struct{}),
		members:   make(map[string]int),
		occupied:  make(map[string]int),
		slots:     make(map[string]map[int]string),
	}
}

func buildPoolMembershipState(cfg *config.City, infos []sessionpkg.Info) (poolMembershipState, error) {
	if cfg == nil {
		return poolMembershipState{}, fmt.Errorf("building pool membership: config is nil")
	}
	state := newPoolMembershipState()
	seen := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		id := strings.TrimSpace(info.ID)
		if id == "" || id != info.ID {
			return poolMembershipState{}, fmt.Errorf("building pool membership: invalid session ID %q", info.ID)
		}
		if _, duplicate := seen[id]; duplicate {
			return poolMembershipState{}, fmt.Errorf("building pool membership: duplicate session ID %q", id)
		}
		seen[id] = struct{}{}

		contribution, present, err := poolMembershipContributionFromInfo(cfg, info)
		if err != nil {
			return poolMembershipState{}, fmt.Errorf("building pool membership for %q: %w", id, err)
		}
		if present {
			if err := state.add(contribution); err != nil {
				return poolMembershipState{}, fmt.Errorf("adding pool membership for %q: %w", id, err)
			}
		}
	}
	return state, nil
}

func poolMembershipContributionFromInfo(cfg *config.City, info sessionpkg.Info) (poolMembershipContribution, bool, error) {
	id := strings.TrimSpace(info.ID)
	if id == "" || id != info.ID {
		return poolMembershipContribution{}, false, fmt.Errorf("invalid session ID %q", info.ID)
	}
	if info.Closed || !isPoolManagedSessionInfo(info) {
		return poolMembershipContribution{}, false, nil
	}
	if isNamedSessionInfo(info) {
		return poolMembershipContribution{}, false, fmt.Errorf("session is both configured named and pool-managed")
	}
	template := strings.TrimSpace(normalizedSessionTemplateInfo(info, cfg))
	agent := findAgentByTemplate(cfg, template)
	if agent == nil {
		return poolMembershipContribution{}, false, fmt.Errorf("pool template %q is not configured", template)
	}
	if !isEphemeralSessionInfoForAgent(info, agent) {
		return poolMembershipContribution{}, false, nil
	}

	view := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(info))
	if !knownPoolMembershipBaseState(view.BaseState) || view.BaseState == sessionpkg.BaseStateNone {
		return poolMembershipContribution{}, false, fmt.Errorf("unsupported lifecycle state %q", view.BaseState)
	}
	slot := 0
	if !agent.UsesCanonicalSingletonPoolIdentity() {
		slot = existingPoolSlotWithConfigInfo(cfg, agent, info)
		if slot <= 0 {
			return poolMembershipContribution{}, false, fmt.Errorf("expandable pool session has no valid concrete slot")
		}
	}
	return poolMembershipContribution{
		sessionID:        id,
		poolTarget:       agent.QualifiedName(),
		slot:             slot,
		baseState:        view.BaseState,
		countsAgainstCap: view.CountsAgainstCap,
	}, true, nil
}

func knownPoolMembershipBaseState(state sessionpkg.BaseState) bool {
	switch state {
	case sessionpkg.BaseStateNone,
		sessionpkg.BaseStateCreating,
		sessionpkg.BaseStateStartPending,
		sessionpkg.BaseStateActive,
		sessionpkg.BaseStateAsleep,
		sessionpkg.BaseStateSuspended,
		sessionpkg.BaseStateFailedCreate,
		sessionpkg.BaseStateDraining,
		sessionpkg.BaseStateDrained,
		sessionpkg.BaseStateArchived,
		sessionpkg.BaseStateOrphaned,
		sessionpkg.BaseStateClosed,
		sessionpkg.BaseStateClosing,
		sessionpkg.BaseStateQuarantined,
		sessionpkg.BaseStateStopped:
		return true
	default:
		return false
	}
}

func (s *poolMembershipState) add(contribution poolMembershipContribution) error {
	if contribution.slot > 0 {
		slots := s.slots[contribution.poolTarget]
		if slots == nil {
			slots = make(map[int]string)
			s.slots[contribution.poolTarget] = slots
		}
		if existing, duplicate := slots[contribution.slot]; duplicate && existing != contribution.sessionID {
			return fmt.Errorf("duplicate pool slot %d occupied by %q and %q", contribution.slot, existing, contribution.sessionID)
		}
		slots[contribution.slot] = contribution.sessionID
	}
	s.bySession[contribution.sessionID] = contribution
	ids := s.memberIDs[contribution.poolTarget]
	if ids == nil {
		ids = make(map[string]struct{})
		s.memberIDs[contribution.poolTarget] = ids
	}
	ids[contribution.sessionID] = struct{}{}
	s.members[contribution.poolTarget]++
	if contribution.countsAgainstCap {
		s.occupied[contribution.poolTarget]++
	}
	return nil
}

func (s *poolMembershipState) remove(sessionID string) {
	old, ok := s.bySession[sessionID]
	if !ok {
		return
	}
	delete(s.bySession, sessionID)
	if ids := s.memberIDs[old.poolTarget]; ids != nil {
		delete(ids, sessionID)
		if len(ids) == 0 {
			delete(s.memberIDs, old.poolTarget)
		}
	}
	if old.slot > 0 {
		if slots := s.slots[old.poolTarget]; slots != nil && slots[old.slot] == sessionID {
			delete(slots, old.slot)
			if len(slots) == 0 {
				delete(s.slots, old.poolTarget)
			}
		}
	}
	decrementPoolMembershipCount(s.members, old.poolTarget)
	if old.countsAgainstCap {
		decrementPoolMembershipCount(s.occupied, old.poolTarget)
	}
}

func decrementPoolMembershipCount(counts map[string]int, target string) {
	if counts[target] <= 1 {
		delete(counts, target)
		return
	}
	counts[target]--
}

func (i *poolMembershipIndex) rebuildToken() uint64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.revision
}

// publishRebuild installs a complete candidate only if no exact delta arrived
// after its caller began observing the source snapshot.
func (i *poolMembershipIndex) publishRebuild(token uint64, state poolMembershipState) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.revision != token {
		return false
	}
	i.state = state
	i.revision++
	i.certified = true
	i.reason = ""
	return true
}

func (i *poolMembershipIndex) replace(cfg *config.City, info sessionpkg.Info) error {
	contribution, present, err := poolMembershipContributionFromInfo(cfg, info)
	i.mu.Lock()
	defer i.mu.Unlock()
	if err != nil {
		i.revision++
		i.certified = false
		i.reason = poolMembershipUncertifiedInvalidDelta
		return err
	}
	old, oldPresent := i.state.bySession[info.ID]
	if oldPresent == present && (!present || old == contribution) {
		return nil
	}
	i.revision++
	i.state.remove(info.ID)
	if present {
		if err := i.state.add(contribution); err != nil {
			i.certified = false
			i.reason = poolMembershipUncertifiedInvalidDelta
			return err
		}
	}
	return nil
}

func (i *poolMembershipIndex) remove(sessionID string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.revision++
	i.state.remove(sessionID)
}

func (i *poolMembershipIndex) invalidate(reason poolMembershipUncertifiedReason) {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.revision++
	i.certified = false
	i.reason = reason
}

func (i *poolMembershipIndex) observe(poolTarget string) poolMembershipObservation {
	if i == nil {
		return poolMembershipObservation{reason: poolMembershipUncertifiedNotInitialized}
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	poolTarget = strings.TrimSpace(poolTarget)
	return poolMembershipObservation{
		members:      i.state.members[poolTarget],
		occupied:     i.state.occupied[poolTarget],
		nextFreeSlot: lowestFreePositivePoolSlot(i.state.slots[poolTarget]),
		certified:    i.certified,
		revision:     i.revision,
		reason:       i.reason,
	}
}

// observeMemberIDs returns a stable copy of the exact certified member IDs for
// one pool. Callers must load each returned row through the store; this index
// is only the bounded candidate set, never an authority for session contents.
func (i *poolMembershipIndex) observeMemberIDs(poolTarget string) (poolMembershipObservation, []string, bool) {
	if i == nil {
		return poolMembershipObservation{reason: poolMembershipUncertifiedNotInitialized}, nil, false
	}
	poolTarget = strings.TrimSpace(poolTarget)
	i.mu.RLock()
	defer i.mu.RUnlock()
	observation := poolMembershipObservation{
		members:      i.state.members[poolTarget],
		occupied:     i.state.occupied[poolTarget],
		nextFreeSlot: lowestFreePositivePoolSlot(i.state.slots[poolTarget]),
		certified:    i.certified,
		revision:     i.revision,
		reason:       i.reason,
	}
	ids := i.state.memberIDs[poolTarget]
	if !observation.certified || observation.members == 0 || len(ids) != observation.members {
		return observation, nil, false
	}
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Strings(result)
	return observation, result, true
}

// observeOccupiedMember returns the same certified observation plus whether
// the named session is an occupied member. The check and snapshot are taken
// under one read lock so an allocation lease cannot combine fields from
// different revisions.
func (i *poolMembershipIndex) observeOccupiedMember(poolTarget, sessionID string) (poolMembershipObservation, bool) {
	if i == nil {
		return poolMembershipObservation{reason: poolMembershipUncertifiedNotInitialized}, false
	}
	poolTarget = strings.TrimSpace(poolTarget)
	sessionID = strings.TrimSpace(sessionID)
	i.mu.RLock()
	defer i.mu.RUnlock()
	observation := poolMembershipObservation{
		members:      i.state.members[poolTarget],
		occupied:     i.state.occupied[poolTarget],
		nextFreeSlot: lowestFreePositivePoolSlot(i.state.slots[poolTarget]),
		certified:    i.certified,
		revision:     i.revision,
		reason:       i.reason,
	}
	contribution, present := i.state.bySession[sessionID]
	occupied := observation.certified &&
		present && contribution.poolTarget == poolTarget && contribution.countsAgainstCap
	return observation, occupied
}

func lowestFreePositivePoolSlot(used map[int]string) int {
	for slot := 1; ; slot++ {
		if _, occupied := used[slot]; !occupied {
			return slot
		}
	}
}

// refreshPoolMembershipSession performs one authoritative exact-key read after
// the start controller has admitted the same event. It can never delay keyed
// start admission and never performs a provider or store write.
func (cr *CityRuntime) refreshPoolMembershipSession(sessionID string) {
	if cr == nil || cr.poolMembershipShadow == nil || cr.cs == nil {
		return
	}
	snapshot, release, err := cr.cs.acquireSessionStartSnapshot()
	if err != nil {
		cr.poolMembershipShadow.invalidate(poolMembershipUncertifiedDeltaRead)
		return
	}
	defer release()
	info, _, err := getAuthoritativeSessionStartRecord(snapshot.Store, sessionID)
	if errors.Is(err, beads.ErrNotFound) {
		cr.poolMembershipShadow.remove(sessionID)
		return
	}
	if err != nil {
		cr.poolMembershipShadow.invalidate(poolMembershipUncertifiedDeltaRead)
		fmt.Fprintf(shadowWorkerStderr(cr.stderr), "%s: pool membership shadow read for %s: %v\n", cr.logPrefix, sessionID, err) //nolint:errcheck // shadow failure must not affect reconciliation
		return
	}
	if err := cr.poolMembershipShadow.replace(snapshot.Config, info); err != nil {
		fmt.Fprintf(shadowWorkerStderr(cr.stderr), "%s: pool membership shadow delta for %s: %v\n", cr.logPrefix, sessionID, err) //nolint:errcheck // shadow failure must not affect reconciliation
	}
}
