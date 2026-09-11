package main

import (
	"fmt"
	"path/filepath"
)

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
func admitLegacyManagedDoltLifecycle(cityPath string) error {
	cityPath = normalizePathForCompare(cityPath)
	providerOwned, err := cityScopeProviderOwned(cityPath)
	if err != nil {
		return fmt.Errorf("classify provider scope ownership: %w", err)
	}
	if providerOwned {
		return fmt.Errorf("provider scope ownership blocks the managed Dolt lifecycle for %s: bd owns this scope's Dolt process (ownership journal %s, beads metadata %s)",
			cityPath, providerScopeOwnershipPath(cityPath), scopeMetadataJSONPath(cityPath))
	}
	return handoffJournalBlocksManagedDoltStart(cityPath)
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
