package storebinding

import (
	"io"

	"github.com/gastownhall/gascity/internal/beads"
)

// EngineOpener is the serving hook a binding-backed bead engine exposes.
//
// A provider whose binding IS a bead engine implements it and hands back the
// canonical beads.Store for the classes that binding serves. That store is
// what the class front doors upstream route reads and writes at, so the seam
// is the whole reason an out-of-tree provider can serve a relocated class
// without a single edit here: a downstream fork registers its factory in its
// own tree and implements this one method.
//
// It is deliberately narrow and deliberately optional.
//
// Narrow, because the alternative was wider in the wrong direction. The typed
// front-door lifecycle (Inspect / Open / OpenedBinding) exposes class adapters
// rather than a store, and the StoreSet it composes is mintable only through
// the publication path. Forcing a plan's binding through that path to reach a
// store would open the publication authority to every boot, which is a much
// larger promise than "this binding is a bead engine, here it is". Opening the
// database by hand in the composition root instead would be smaller — and would
// leave an out-of-tree provider with nowhere to plug in at all.
//
// Optional, because not every provider is a bead engine. A planned binding
// whose provider does not implement this is a refusal that names the provider,
// never a silent fall-through to the work store: a routed class that quietly
// resolves somewhere else is the exact failure this seam exists to prevent.
//
// The returned io.Closer owns the engine's durable resources. The caller closes
// it once, when the process that resolved the plan is done with the binding;
// storage handles are immutable for the life of a process, so nothing reopens
// or swaps one mid-flight.
type EngineOpener interface {
	// OpenEngine opens the binding's bead engine for the classes it serves.
	//
	// spec is the same specification the provider was constructed from. It is
	// passed rather than assumed so an implementation can refuse a caller that
	// resolved a different binding than the one it holds, and so a stateless
	// factory can implement the seam directly.
	OpenEngine(spec BindingSpec, classes ClassSet) (beads.Store, io.Closer, error)
}

// BindingLocator reports WHERE a provider actually serves a binding from.
//
// It exists because a city records that location durably — the served-binding
// note is the one thing that keeps a later boot from silently re-pointing a
// city's infrastructure classes at a binding that holds none of its state —
// and the recorded value has to be the place the provider really opens, not
// the configuration string it was derived from. Two bindings can carry the
// same opaque reference and resolve to different directories in different
// cities; a note holding the reference cannot tell them apart.
//
// The location is secret-free and absolute, and answering is READ-ONLY: it
// must create and modify nothing, because the note is written on paths that
// have deliberately built nothing yet. It may read the filesystem, and both
// compiled providers do — resolving a location means following the symlinks in
// it, or two spellings of one path record as two different bindings. What a
// locator must never do is bring the location into existence.
//
// Optional, like the opener beside it. A provider that offers no locator has
// its configured reference recorded instead, which is what a city did before
// this seam existed.
type BindingLocator interface {
	// BindingLocation returns the absolute location this provider serves the
	// given specification from. spec is passed rather than assumed so a
	// stateless factory can implement the seam directly.
	BindingLocation(spec BindingSpec) (string, error)
}

// BindingLocatorFor returns the location hook for a planned binding's
// provider, and whether that provider offers one.
func BindingLocatorFor(binding PlannedBinding) (BindingLocator, bool) {
	if isNilInterface(binding.Provider) {
		return nil, false
	}
	locator, ok := binding.Provider.(BindingLocator)
	return locator, ok
}

// EngineOpenerFor returns the engine-opening hook for a planned binding's
// provider, and whether that provider offers one.
//
// It exists so every caller asks the question the same way. The type assertion
// is on the resolved provider facade the plan already carries, so no consumer
// re-resolves a binding name or re-enters the registry to find out whether a
// binding can serve.
func EngineOpenerFor(binding PlannedBinding) (EngineOpener, bool) {
	if isNilInterface(binding.Provider) {
		return nil, false
	}
	opener, ok := binding.Provider.(EngineOpener)
	return opener, ok
}
