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
