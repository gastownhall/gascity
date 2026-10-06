package contract

import (
	"fmt"
	"sync"
)

// The sole backend-name composition root.
//
// Backend names are compiled in, never discovered. This file names the set once,
// builds ONE registry from it, freezes it, and every backend refusal in gc reads
// its enumeration from that registry — so the refusal an operator sees is a
// statement about the binary in front of them rather than a list some other
// build's author typed into a format string.
//
// There is no init() and no blank-import registration: which backends a build
// knows is a property of the assembly being built, not of the import graph, and
// a registration side effect fired by an import has no ordering relation to
// config load or boot. Construction is deferred to first use, so importing this
// package still does nothing.
//
// An assembly that serves a backend this list omits adds it here — one line, in
// one file — and every refusal message, in all four paths, enumerates the new
// set with no other edit.

// compiledBackendNames returns the backend names this build recognizes, in the
// order refusals enumerate them.
//
// UnsetBackend leads because it is not a choice: metadata written before the
// key existed, and a scope that inherits its city's backend, both name nothing,
// and that has always been legal at the parse layer. It is registered rather
// than special-cased so "does this build recognize this backend?" has exactly
// one answer, in one place.
//
// The list is short on purpose. A backend belongs here only when gc itself
// implements it — reads its metadata shape, projects its environment, and
// manages its runtime. A workspace served by the linked beads library through
// an opaque storage binding needs none of that and is not listed: gc withholds
// the whole projected namespace for it and never learns the name.
func compiledBackendNames() []BackendName {
	return []BackendName{
		UnsetBackend,
		"dolt",
		"doltlite",
	}
}

// compiledBackendRegistry builds and freezes this build's backend-name registry
// exactly once. It is the only place a registry is constructed outside tests.
var compiledBackendRegistry = sync.OnceValues(newCompiledBackendRegistry)

func newCompiledBackendRegistry() (*BackendRegistry, error) {
	registry := NewBackendRegistry()
	for _, name := range compiledBackendNames() {
		if err := registry.Register(name); err != nil {
			return nil, fmt.Errorf("registering compiled beads backend: %w", err)
		}
	}
	if err := registry.Freeze(); err != nil {
		return nil, fmt.Errorf("freezing the beads backend registry: %w", err)
	}
	return registry, nil
}

// RecognizeBackend reports whether this build registers the given backend name.
// A registered name — including the empty name, which is metadata that names no
// backend — returns nil. Anything else returns an *UnknownBackendError naming
// the backend and enumerating what is registered.
func RecognizeBackend(backend string) error {
	registry, err := compiledBackendRegistry()
	if err != nil {
		return err
	}
	return registry.Lookup(BackendName(backend))
}

// RegisteredBackends returns the operator-selectable backend names this build
// registers, in registration order. Callers that compose their own refusal
// wording use it so their enumeration cannot drift from the loader's.
func RegisteredBackends() ([]string, error) {
	registry, err := compiledBackendRegistry()
	if err != nil {
		return nil, err
	}
	return registry.Names(), nil
}
