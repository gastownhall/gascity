package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// gcOwnsProxiedScope reports whether scopeRoot is a proxied scope gc owns, and
// may therefore pin out of bd's user-level shared-server mode.
//
// Proxied alone is not enough. scopeUsesProxiedDoltMode already excludes an
// externally bound store, but R1 also classifies a proxied bd workspace gc
// merely found (an operator's, a clone's) as provider-owned, and that
// workspace's config.yaml is not gc's to edit. gc's ownership evidence is any
// of: the provider ownership journal (`gc init` / `gc rig add` on the proxied
// default), a committed ownership handoff, or gc's own canonical endpoint
// marker (a legacy managed scope, including one `gc beads city migrate
// proxied` moved onto the proxy).
func gcOwnsProxiedScope(cityPath, scopeRoot string) (bool, error) {
	if !scopeUsesProxiedDoltMode(cityPath, scopeRoot) {
		return false, nil
	}
	if _, journaled, err := providerScopeOwnership(cityPath, scopeRoot); err != nil {
		return false, err
	} else if journaled {
		return true, nil
	}
	if transferred, err := committedBeadsHandoffOwnsScope(scopeRoot); err != nil {
		return false, err
	} else if transferred {
		return true, nil
	}
	return scopeConfigMarksGCManagedEndpoint(scopeRoot), nil
}

// scopeConfigMarksGCManagedEndpoint reports whether scopeRoot's config.yaml
// carries gc's own canonical endpoint marker: the city's managed server, or a
// rig inheriting it. An unreadable config proves nothing about ownership, so it
// reads as unmarked and the file is left alone.
func scopeConfigMarksGCManagedEndpoint(scopeRoot string) bool {
	state, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "config.yaml"))
	if err != nil || !ok {
		return false
	}
	switch state.EndpointOrigin {
	case contract.EndpointOriginManagedCity, contract.EndpointOriginInheritedCity:
		return true
	default:
		return false
	}
}

// gcOwnsScope reports whether scopeRoot is a scope gc owns, and may therefore
// pin out of bd's user-level shared-server mode. A proxied scope is owned on the
// evidence gcOwnsProxiedScope weighs. Any other scope is gc's managed
// server-mode store when its config.yaml carries gc's endpoint marker — it has
// no provider journal or handoff to consult — and only while the store is gc's
// own Dolt: a doltlite scope, or one bound to an external backend, is not.
func gcOwnsScope(cityPath, scopeRoot string) (bool, error) {
	if scopeUsesProxiedDoltMode(cityPath, scopeRoot) {
		return gcOwnsProxiedScope(cityPath, scopeRoot)
	}
	owned := scopeConfigMarksGCManagedEndpoint(scopeRoot) &&
		!scopeBackendIsDoltlite(cityPath, scopeRoot) &&
		!scopeStoreIsExternallyBoundBestEffort(cityPath, scopeRoot)
	return owned, nil
}

// ensureGCOwnedProxiedScopeSharedServerOff pins dolt.shared-server: false into
// the config.yaml of a scope gc owns (gcOwnsScope): a proxied scope or a managed
// server-mode one. It runs after every bd init of such a scope (bd init
// rewrites config.yaml, so a pin written before init does not survive) and on
// every `gc start`, which repairs scopes initialized by a build that did not
// write it.
//
// The env opt-out (applyProxiedSharedServerOptOut) covers every bd process gc
// spawns; this pin covers the ones it does not — an agent running `bd` in its
// shell resolves the scope's config.yaml, which outranks the user-level files.
//
// A scope already pinned ON was bound to the shared server by an earlier bd
// init that ran under the user-level mode. Pinning it off returns it to its own
// Dolt root; the rows it wrote meanwhile are in ~/.beads/shared-server, so say
// so rather than letting the store look silently emptier.
func ensureGCOwnedProxiedScopeSharedServerOff(cityPath, scopeRoot string) error {
	owned, err := gcOwnsScope(cityPath, scopeRoot)
	if err != nil || !owned {
		return err
	}
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if _, err := os.Stat(beadsDir); err != nil {
		if os.IsNotExist(err) {
			// bd has not materialized the scope; there is no config to pin
			// yet. The next init or start that creates it writes the pin.
			return nil
		}
		return fmt.Errorf("inspecting %s: %w", beadsDir, err)
	}
	path := filepath.Join(beadsDir, "config.yaml")
	changed, previous, err := contract.EnsureSharedServerDisabled(fsys.OSFS{}, path)
	if err != nil {
		return fmt.Errorf("pinning %s off in %s: %w", contract.SharedServerConfigKey, path, err)
	}
	if changed {
		forgetProxiedScopeRuntimeEnv(cityPath)
	}
	if previous == contract.SharedServerPinnedOn {
		log.Printf("gc: %s had %s: true (bound to bd's host-wide shared server, ~/.beads/shared-server); gc-owned scopes keep their own Dolt root, so it is now pinned false. Beads written while it was bound live in the shared server's Dolt root, not in the scope's own",
			path, contract.SharedServerConfigKey)
	}
	return nil
}

// gcOwnedProxiedScopeRoots lists the city and rig scopes gcOwnsProxiedScope
// accepts, for the doctor check that reports their shared-server pin, plus the
// scopes whose ownership could not be decided (reported, not dropped).
func gcOwnedProxiedScopeRoots(cityPath string, cfg *config.City) ([]string, []error) {
	candidates := []string{cityPath}
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			if rig.Path != "" {
				candidates = append(candidates, rig.Path)
			}
		}
	}
	var roots []string
	var errs []error
	for _, root := range candidates {
		owned, err := gcOwnsProxiedScope(cityPath, root)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", root, err))
		case owned:
			roots = append(roots, root)
		}
	}
	return roots, errs
}

// applyGCOwnedScopeSharedServerOptOut adds the shared-server opt-out to a bd
// env projected for scopeRoot when, and only when, gc owns that scope
// (gcOwnsScope). A scope gc merely found keeps the operator's own resolution:
// pinning gc's bd off while an agent's bd (which reads the scope's unpinned
// config.yaml) follows the user-level mode would split one store across two
// Dolt roots.
func applyGCOwnedScopeSharedServerOptOut(env map[string]string, cityPath, scopeRoot string) error {
	owned, err := gcOwnsScope(cityPath, scopeRoot)
	if err != nil {
		return fmt.Errorf("classifying %s for the bd shared-server opt-out: %w", scopeRoot, err)
	}
	if owned {
		applyProxiedSharedServerOptOut(env)
	}
	return nil
}

// applySessionSharedServerOptOut carries the shared-server opt-out into an
// agent session's environment when the session's scope (the rig, or the city
// when scopeRoot is empty) is a scope gc owns. The scope's config.yaml
// pin already covers a `bd` the agent runs in its shell, except against an
// inherited BEADS_DOLT_SHARED_SERVER=1, which bd reads before any config file.
// Scopes gc does not own keep whatever the operator's environment says.
func applySessionSharedServerOptOut(env map[string]string, cityPath, scopeRoot string) {
	if scopeRoot == "" {
		scopeRoot = cityPath
	}
	if err := applyGCOwnedScopeSharedServerOptOut(env, cityPath, scopeRoot); err != nil {
		// The scope's config.yaml pin still applies; only the env belt is lost.
		log.Printf("gc: session env: %v", err)
	}
}
