package contract

import (
	"errors"
	"fmt"
	"sync"

	beadsbackend "github.com/steveyegge/beads/backend"
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
// set with no other edit. A backend gc does not implement but the linked beads
// library serves is not added here: the assembly registers it with the library,
// and RecognizeBackend admits it from there (IsLibraryExtensionBackend).

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
// the whole projected namespace for it and never learns the name. The same
// holds for a backend the linked library registered as an extension (see
// IsLibraryExtensionBackend): it is recognized, never listed.
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
// backend — returns nil, as does a backend the linked beads library registered
// as an extension (IsLibraryExtensionBackend). Anything else returns an
// *UnknownBackendError naming the backend and enumerating what gc registers.
func RecognizeBackend(backend string) error {
	registry, err := compiledBackendRegistry()
	if err != nil {
		return err
	}
	err = registry.Lookup(BackendName(backend))
	if errors.Is(err, ErrUnknownBackend) && beadsbackend.Registered(backend) {
		return nil
	}
	return err
}

// IsLibraryExtensionBackend reports whether backend names a storage backend the
// linked beads library has registered as an extension and gc does not itself
// implement.
//
// The library's registry is consulted directly rather than mirrored into gc's,
// so the two cannot disagree: whatever a build's distribution wiring registers
// with the library (process-start wiring that precedes every metadata read) is
// exactly what gc recognizes here, and a build that wires nothing — OSS — sees
// no extension names at all. gc treats such a workspace as opaque: the library
// opens it, bd reads its own metadata and credentials, and gc projects no
// backend environment and manages no runtime for it. A name in the compiled
// bundle is never an extension, even if a library also registers it.
func IsLibraryExtensionBackend(backend string) bool {
	if backend == "" {
		return false
	}
	registry, err := compiledBackendRegistry()
	if err != nil {
		// Not an answer this predicate can give; RecognizeBackend surfaces it
		// on the metadata read every caller of this predicate also makes.
		return false
	}
	if registry.Lookup(BackendName(backend)) == nil {
		return false
	}
	return beadsbackend.Registered(backend)
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
