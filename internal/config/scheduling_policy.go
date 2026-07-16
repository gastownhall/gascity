package config

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// EffectiveSchedulingPolicy returns the work-admission policy in force for this
// agent or worker pool, mapping unset to beads.DefaultAdmissionPolicy.
//
// This is the single resolver every admission path calls, so one pool cannot end
// up ordering its query one way and its claims another. It reads the already
// composed value: a pack ships the default for a pool it owns, and city patches
// and rig overrides are applied after the pack layer, which is what keeps
// city/operator configuration authoritative for shared pools.
func (a *Agent) EffectiveSchedulingPolicy() beads.AdmissionPolicy {
	return beads.AdmissionPolicy(a.SchedulingPolicy).Resolve()
}

// ValidateWorkspaceSchedulingPolicy rejects an unknown [workspace]
// scheduling_policy at load time. Without it an unrecognized value would resolve
// to priority_fifo semantics and silently reorder every shared cap in the city,
// which is precisely the drift this setting exists to make explicit.
func ValidateWorkspaceSchedulingPolicy(c *City) error {
	if c == nil {
		return nil
	}
	if err := beads.AdmissionPolicy(c.Workspace.SchedulingPolicy).Validate(); err != nil {
		return fmt.Errorf("workspace: scheduling_policy: %w", err)
	}
	return nil
}

// SharedAdmissionPolicy returns the policy that orders requests from different
// pools competing for the same workspace, rig, or agent cap.
//
// Admission to shared capacity cannot read a per-pool setting: two pools in the
// same cap can disagree, and there is no pool-scoped answer to "which of these
// two requests wins". The city's value is the arbiter, which is what keeps
// city/operator configuration authoritative for shared pools.
func (c *City) SharedAdmissionPolicy() beads.AdmissionPolicy {
	if c == nil {
		return beads.DefaultAdmissionPolicy
	}
	return beads.AdmissionPolicy(c.Workspace.SchedulingPolicy).Resolve()
}
