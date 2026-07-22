package beads

import "errors"

// CloseAllTransitionResult preserves Store.CloseAll's exact reported count
// while carrying the authoritative per-ID snapshots observed by the backing
// operation. Transitions may contain entries for already-closed rows
// (Transitioned=false) and for rows whose metadata committed before an
// ancillary or partial error. An absent entry means the backing did not return
// an authoritative post-attempt snapshot for that ID.
type CloseAllTransitionResult struct {
	Count       int
	Transitions map[string]CloseTransition
}

// TransitionFor returns a complete authoritative before/after result for id.
// Invalid or incomplete backend results fail closed so decorators cannot infer
// lifecycle ownership from one snapshot or from their own cached pre-state.
func (r CloseAllTransitionResult) TransitionFor(id string) (CloseTransition, bool) {
	transition, ok := r.Transitions[id]
	if !ok || id == "" || transition.Before.ID != id || transition.After.ID != id {
		return CloseTransition{}, false
	}
	return transition, true
}

// CloseAllTransitioner performs Store.CloseAll while the backing store owns
// lifecycle serialization and returns authoritative snapshots for every ID it
// can classify. The result remains meaningful alongside a non-nil error when
// a prefix or ancillary part of the operation committed durably.
type CloseAllTransitioner interface {
	CloseAllWithTransitions(ids []string, metadata map[string]string) (CloseAllTransitionResult, error)
}

// CloseAllTransitionerHandleProvider exposes the batch-transition capability
// through a wrapper whose embedded Store interface hides optional methods.
type CloseAllTransitionerHandleProvider interface {
	CloseAllTransitionerHandle() (CloseAllTransitioner, bool)
}

// ErrCloseAllTransitionUnsupported reports that a store cannot classify a
// batch close without applying it. Implementations return it before mutation,
// allowing decorators to use conservative refresh-only legacy behavior.
var ErrCloseAllTransitionUnsupported = errors.New("atomic close-all transition is unsupported")

// CloseAllTransitionerFor resolves backing-owned batch classification through
// known wrapper layers.
func CloseAllTransitionerFor(store Store) (CloseAllTransitioner, bool) {
	if store == nil {
		return nil, false
	}
	if transitioner, ok := store.(CloseAllTransitioner); ok {
		return transitioner, true
	}
	if provider, ok := store.(CloseAllTransitionerHandleProvider); ok {
		return provider.CloseAllTransitionerHandle()
	}
	return nil, false
}
