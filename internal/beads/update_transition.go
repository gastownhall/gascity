package beads

import "errors"

// UpdateTransition describes one backing-owned full Update mutation. Before
// and After are authoritative snapshots observed while the backing store's
// lifecycle serialization remains held. TransitionedToClosed is true only
// when this mutation changed a non-closed row to closed. A backend may return
// an authoritative transition together with a non-nil error when the update
// durably committed but an ancillary hook, transport, or acknowledgement
// failed; callers must inspect the transition before treating every error as
// an uncommitted write.
type UpdateTransition struct {
	Before               Bead
	After                Bead
	TransitionedToClosed bool
}

// AuthoritativeAfter reports whether the transition contains an authoritative
// durable post-attempt snapshot for id. It remains meaningful when the
// operation also returned an error after the update committed.
func (t UpdateTransition) AuthoritativeAfter(id string) bool {
	return id != "" && t.After.ID == id
}

// UpdateTransitioner applies every field in UpdateOpts while owning the
// backing store's lifecycle serialization and returns authoritative snapshots
// from that same serialized operation.
type UpdateTransitioner interface {
	UpdateWithTransition(id string, opts UpdateOpts) (UpdateTransition, error)
}

// UpdateTransitionerHandleProvider exposes an update-transition handle through
// a wrapper whose embedded Store interface hides optional capabilities.
type UpdateTransitionerHandleProvider interface {
	UpdateTransitionerHandle() (UpdateTransitioner, bool)
}

// ErrUpdateTransitionUnsupported reports that a store cannot atomically
// classify a full Update mutation. Implementations return it before applying
// any mutation, allowing decorators to use their conservative legacy path.
var ErrUpdateTransitionUnsupported = errors.New("atomic update transition is unsupported")

// UpdateTransitionerFor resolves the atomic full-Update capability through
// known wrapper layers.
func UpdateTransitionerFor(store Store) (UpdateTransitioner, bool) {
	if store == nil {
		return nil, false
	}
	if transitioner, ok := store.(UpdateTransitioner); ok {
		return transitioner, true
	}
	if provider, ok := store.(UpdateTransitionerHandleProvider); ok {
		return provider.UpdateTransitionerHandle()
	}
	return nil, false
}
