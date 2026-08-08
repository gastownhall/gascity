package main

// The sole storage-provider composition root.
//
// Storage providers are compiled in, never discovered. This file constructs
// ONE registry from compiledStorageProviderFactories, freezes it, and hands it
// to the caller. There is no mutable global registry and no init-ordering
// dependency: the registry is frozen before config resolution and passed
// explicitly to whatever composes storage.
//
// That shape is what makes the seam extension-safe. A downstream fork adds an
// out-of-tree provider by appending its factory to the list below in its own
// tree — one line, in one file, with no edit anywhere else and no registration
// side effect to reason about. The single construction site is what keeps
// "which providers does this binary have?" answerable by reading one function.
//
// storage_provider_bundle_boundary_test.go enforces every clause above.

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storebinding/beadsworkspace"
	"github.com/gastownhall/gascity/internal/storebinding/sqlite"
)

// newStorageProviderRegistry builds and freezes the storage provider registry
// for this binary. It is the only place a registry is constructed.
func newStorageProviderRegistry() (*storebinding.ProviderRegistry, error) {
	registry := storebinding.NewProviderRegistry()
	for _, factory := range compiledStorageProviderFactories() {
		if err := registry.Register(factory); err != nil {
			return nil, fmt.Errorf("registering compiled storage provider: %w", err)
		}
	}
	if err := registry.Freeze(); err != nil {
		return nil, fmt.Errorf("freezing the storage provider registry: %w", err)
	}
	return registry, nil
}

// compiledStorageProviderFactories returns the built-in storage provider
// factories, in a deterministic order.
//
// Two factories are compiled in, and they differ in what they own rather than
// in what they store. The canonical Beads-over-SQLite binding owns the file it
// serves: it censuses it, fences it, and this build migrates a city onto it,
// so it walks the whole Inspect / AcquireFence / InspectFenced / Open protocol
// against a real database instead of a fake. The beads workspace binding owns
// none of that — the workspace's own configuration names the backend and the
// linked beads library serves it — so it implements the engine-opening seam
// and refuses the arms that would have to claim authority it does not have.
//
// That second shape is the one an out-of-tree provider has. Compiling an
// example of it here is what keeps the seam honest: the refusing arms are
// exercised by this tree's own tests rather than discovered downstream.
func compiledStorageProviderFactories() []storebinding.ProviderFactory {
	return []storebinding.ProviderFactory{
		sqlite.BeadsProviderFactory{},
		beadsworkspace.ProviderFactory{},
	}
}
