package storebinding

// PlanRequiresRestart reports whether moving from one frozen plan to another
// requires a restart. Live storage handles are immutable, so this is the only
// answer a reload may produce: it calls no provider, mutates neither plan, and
// never swaps a handle. It layers on config.StorageReloadRequiresRestart by
// additionally detecting pinned Work identity drift.
//
// A missing plan on either side reports restart required: an absent frozen plan
// is never evidence that the current one still holds.
func PlanRequiresRestart(current, next *StoragePlan) bool {
	if current == nil || next == nil {
		return true
	}
	return !current.sameFrozenFacts(next)
}

// sameFrozenFacts compares only frozen desired-state facts: assignments,
// provider IDs, secret-free config digests, pinned Work identities, planned
// programs, and deferred steps. Resolved provider facades are deliberately not
// compared; they own nothing and two runs construct distinct values from the
// same configuration.
func (p *StoragePlan) sameFrozenFacts(other *StoragePlan) bool {
	if p.configDigest != other.configDigest || p.nudgeAuthority != other.nudgeAuthority {
		return false
	}
	if len(p.assignments) != len(other.assignments) {
		return false
	}
	for class, binding := range p.assignments {
		if other.assignments[class] != binding {
			return false
		}
	}
	if !p.workPlan.sameFrozenFacts(other.workPlan) {
		return false
	}
	if len(p.bindings) != len(other.bindings) {
		return false
	}
	for index, binding := range p.bindings {
		if !binding.sameFrozenFacts(other.bindings[index]) {
			return false
		}
	}
	if len(p.deferred) != len(other.deferred) {
		return false
	}
	for index, deferred := range p.deferred {
		if deferred != other.deferred[index] {
			return false
		}
	}
	return p.samePrograms(other)
}

func (p *StoragePlan) samePrograms(other *StoragePlan) bool {
	if len(p.openProgram) != len(other.openProgram) || len(p.closeProgram) != len(other.closeProgram) || len(p.inspections) != len(other.inspections) {
		return false
	}
	for index, step := range p.openProgram {
		peer := other.openProgram[index]
		if step.Binding != peer.Binding || step.Reserved != peer.Reserved || step.Rank != peer.Rank ||
			step.PinnedWork != peer.PinnedWork || !step.AssignedClasses.Equal(peer.AssignedClasses) {
			return false
		}
	}
	for index, step := range p.closeProgram {
		peer := other.closeProgram[index]
		if step.Binding != peer.Binding || step.Reserved != peer.Reserved || step.Rank != peer.Rank {
			return false
		}
	}
	for index, step := range p.inspections {
		peer := other.inspections[index]
		if step.Binding != peer.Binding || step.Rank != peer.Rank || step.Spec != peer.Spec || !step.AssignedClasses.Equal(peer.AssignedClasses) {
			return false
		}
	}
	return true
}

func (b PlannedBinding) sameFrozenFacts(other PlannedBinding) bool {
	if b.Name != other.Name || b.ProviderID != other.ProviderID || b.Spec != other.Spec ||
		b.ConfigDigest != other.ConfigDigest || b.OpenRank != other.OpenRank || !b.AssignedClasses.Equal(other.AssignedClasses) {
		return false
	}
	if len(b.Requirements) != len(other.Requirements) {
		return false
	}
	for index, requirement := range b.Requirements {
		if requirement != other.Requirements[index] {
			return false
		}
	}
	return true
}
