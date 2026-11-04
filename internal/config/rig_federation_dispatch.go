package config

import "fmt"

// GuardedRigDispatcherWarnings returns one warning per rig with federation =
// "rig" that has no control dispatcher of its own. A graph workflow in a
// rig's store needs the rig's own dispatcher for its control steps whether
// or not the rig is guarded; on a guarded rig the missing dispatcher also
// cannot be stood in for by one outside the rig, since a route outside the
// namespace is refused, so every graph launch into the rig fails.
func GuardedRigDispatcherWarnings(cfg *City) []string {
	if cfg == nil {
		return nil
	}
	var warnings []string
	for i := range cfg.Rigs {
		r := &cfg.Rigs[i]
		if !r.FederationGuarded() || r.Name == "" {
			continue
		}
		if _, ok := ControlDispatcherForScope(cfg, r.Name); ok {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"rig %q has no control dispatcher of its own, so graph workflows into its store cannot run their control steps; with %s = %q a dispatcher outside its namespace %q cannot stand in (the route is refused); give the rig its own %s",
			r.Name, rigFederationKey, RigFederationRig, r.IdentityNamespace(), ControlDispatcherAgentName))
	}
	return warnings
}
