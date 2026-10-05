package main

import (
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// beadsScopeSuspension answers whether a beads scope is quiescent because it
// belongs to a suspended rig or to a suspended city. Suspension is the
// operator asking for a scope to be left cold: gc does not touch a suspended
// scope's bd-owned store, because any bd read restarts its proxy and Dolt.
type beadsScopeSuspension struct {
	city bool
	rigs map[string]bool
}

// suspendedBeadsScopes loads the runtime suspension state and answers for
// cfg's city and rigs. A nil cfg suspends nothing.
func suspendedBeadsScopes(cityPath string, cfg *config.City) beadsScopeSuspension {
	if cfg == nil {
		return beadsScopeSuspension{}
	}
	return suspendedBeadsScopesWithState(cityPath, cfg, loadSuspensionStateBestEffort(cityPath))
}

// suspendedBeadsScopesWithState is suspendedBeadsScopes for a state the caller
// already loaded.
func suspendedBeadsScopesWithState(cityPath string, cfg *config.City, st suspensionstate.State) beadsScopeSuspension {
	if cfg == nil {
		return beadsScopeSuspension{}
	}
	s := beadsScopeSuspension{city: effectiveCitySuspended(cfg, st), rigs: map[string]bool{}}
	names := buildEffectiveSuspendedRigNames(cfg, st)
	for _, r := range cfg.Rigs {
		if !names[r.Name] || r.Path == "" {
			continue
		}
		path := r.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(cityPath, path)
		}
		s.rigs[normalizePathForCompare(path)] = true
	}
	return s
}

// Suspended reports whether scopeRoot is the city or a rig of a suspended
// city, or a suspended rig.
func (s beadsScopeSuspension) Suspended(scopeRoot string) bool {
	if s.city {
		return true
	}
	return s.rigs[normalizePathForCompare(scopeRoot)]
}

// City reports whether the whole city is suspended.
func (s beadsScopeSuspension) City() bool { return s.city }

// beadsScopeIdleRetired reports whether scopeRoot is a bd-owned proxied scope
// whose proxy is not running because it retired on its idle timeout. It reads
// the proxy record, the process table and the sidecar; nothing is dialed. A
// variable so tests can model a retired scope without a real one.
var beadsScopeIdleRetired = func(scopeRoot string) bool {
	return doctor.ProxiedStoreNotRunning(scopeRoot) && doctor.ProxiedStoreRetiresWhenIdle(scopeRoot)
}

// providerOwnedScopeReady reports whether scopeRoot is a provider-owned scope
// whose store is already initialized.
func providerOwnedScopeReady(cityPath, scopeRoot string) bool {
	entry, owned, err := providerOwnedScopeState(cityPath, scopeRoot)
	return err == nil && owned && entry.State == providerScopeReady
}
