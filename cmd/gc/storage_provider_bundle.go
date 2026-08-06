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
// One factory is compiled in: the canonical Beads-over-SQLite binding, one
// ledger projected into all six class front doors. Registering it is what
// takes the provider lifecycle out of the dark. Before it, the whole
// Inspect / AcquireFence / InspectFenced / Open protocol was exercised only by
// tests and fakes, so the first real implementation to walk it would have been
// an out-of-tree one — discovering every contract mismatch where nobody else
// could see it and where fixing it is expensive.
func compiledStorageProviderFactories() []storebinding.ProviderFactory {
	return []storebinding.ProviderFactory{
		sqlite.BeadsProviderFactory{},
	}
}
