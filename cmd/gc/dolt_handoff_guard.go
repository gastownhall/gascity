package main

import (
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// managedDoltScopeRefusal reports why cityPath cannot own a managed Dolt
// lifecycle, or nil when nothing about its shape rules one out. Starting a
// supervised server is heavyweight (a port, a data dir, a long-lived process),
// so it needs affirmative proof the scope owns its store, not merely the
// absence of a reason to refuse:
//
//   - A literal city.toml at cityPath. findCity's legacy runtime-root branch
//     also claims a source directory that leaked a .gc/, which is not a city.
//   - No .beads/redirect to a different .beads. A git worktree of the city repo
//     carries city.toml and a redirect to the real store; bd follows the
//     redirect and the managed-dolt layout does not, so a server started there
//     serves an empty store nothing reads.
func managedDoltScopeRefusal(cityPath string) error {
	cityPath = normalizePathForCompare(cityPath)
	if !citylayout.HasCityConfig(cityPath) {
		return fmt.Errorf("managed Dolt refused for %s: no %s at this path, so it is not a city that owns a store (give it a %s, or run against the city that does)",
			cityPath, citylayout.CityConfigFile, citylayout.CityConfigFile)
	}
	return managedDoltRedirectRefusal(cityPath)
}

// managedDoltRedirectRefusal reports a .beads/redirect at cityPath that names a
// different .beads, or nil. It is the part of managedDoltScopeRefusal that is
// safe to ask on inspection paths: a redirect is positive evidence the store
// lives elsewhere, where a missing city.toml is only the absence of evidence.
func managedDoltRedirectRefusal(cityPath string) error {
	cityPath = normalizePathForCompare(cityPath)
	beadsDir := filepath.Join(cityPath, ".beads")
	target := beadsRedirectTarget(beadsDir)
	if target != "" && !samePath(target, beadsDir) {
		return fmt.Errorf("managed Dolt refused for %s: its .beads is a redirect to %s, so the store lives there and not here (run against that store's city)",
			cityPath, target)
	}
	return nil
}

// admitManagedDoltStart is the gate for operations that start or recover a
// managed server. On top of admitLegacyManagedDoltLifecycle it requires the
// scope to be able to own a store at all (managedDoltScopeRefusal). Inspection,
// probe, wait and stop stay on the plain admission, so a server that was
// already started in a scope that cannot own it can still be found and stopped.
func admitManagedDoltStart(cityPath string) error {
	if err := admitLegacyManagedDoltLifecycle(cityPath); err != nil {
		return err
	}
	return managedDoltScopeRefusal(cityPath)
}

// admitLegacyManagedDoltLifecycle rejects a legacy GC lifecycle operation
// when a durable provider or handoff record owns the city's Dolt scope.
// Callers that acquire the managed lifecycle lock recheck after acquisition;
// recovery also checks before it can probe while waiting for that lock.
//
// Ownership is the R1 classification, not the journal alone: a workspace
// migrated in place with `bd migrate from-server-to-proxied-server`, or cloned
// from a proxied city, carries bd's binding in committed metadata with no
// journal record at all. Consulting only the journal let `gc dolt-state
// start-managed` raise a GC-managed sql-server over bd's proxy root.
// The three arms answer in order of how specific their evidence is, because the
// refusal has to name the file that actually proves it. A handed-off city has no
// .gc/scope-ownership.json and no proxied binding — its journal is the only
// record — so answering with the generic message sent the operator to two paths
// that do not exist and never mentioned the one that does.
func admitLegacyManagedDoltLifecycle(cityPath string) error {
	cityPath = normalizePathForCompare(cityPath)
	if err := handoffJournalBlocksManagedDoltStart(cityPath); err != nil {
		return err
	}
	providerOwned, err := cityScopeProviderOwned(cityPath)
	if err != nil {
		return fmt.Errorf("classify provider scope ownership: %w", err)
	}
	if !providerOwned {
		return nil
	}
	return fmt.Errorf("provider scope ownership blocks the managed Dolt lifecycle for %s: bd owns this scope's Dolt process (%s)",
		cityPath, providerOwnershipEvidence(cityPath))
}

// providerOwnershipEvidence names the durable record that makes a scope
// provider-owned, for a refusal message. The handoff arm is answered before this
// is reached, so what is left is the ownership journal or bd's own binding.
func providerOwnershipEvidence(cityPath string) string {
	if _, journaled, err := providerScopeOwnership(cityPath, cityPath); err == nil && journaled {
		return "ownership journal " + providerScopeOwnershipPath(cityPath)
	}
	return "beads metadata " + scopeMetadataJSONPath(cityPath)
}

// handoffJournalBlocksManagedDoltStart refuses a managed-city lifecycle
// start while bd owns, or is in the middle of acquiring, that city's direct
// Dolt endpoint. Callers hold the managed lifecycle lock before invoking it.
// A corrupt record fails closed: starting a second server is less recoverable
// than requiring the handoff operator to repair its journal.
func handoffJournalBlocksManagedDoltStart(cityPath string) error {
	cityPath = normalizePathForCompare(cityPath)
	owned, err := committedBeadsHandoffOwnsScope(cityPath)
	if err != nil {
		return err
	}
	if owned {
		return fmt.Errorf("ownership handoff journal at %s blocks managed Dolt start", filepath.Join(cityPath, ".beads", "ownership-handoff.json"))
	}
	return nil
}
