package config

import "fmt"

// GuardedRigDispatcherWarnings returns one warning per rig with federation =
// "rig" that has no control dispatcher of its own. A graph workflow in a
// guarded rig's store routes its control steps to the rig's dispatcher;
// without one they would go to a dispatcher outside the rig's namespace,
// which the rig refuses, so every graph launch into the rig fails.
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
			"rig %q sets %s = %q but has no control dispatcher of its own: graph workflows into its store would route control steps outside its namespace %q and are refused; give the rig its own %s",
			r.Name, rigFederationKey, RigFederationRig, r.IdentityNamespace(), ControlDispatcherAgentName))
	}
	return warnings
}
