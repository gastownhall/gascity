// The optional pinned-create capability seam.
//
// A pinned create persists a bead under exactly the caller-chosen ID, or
// refuses without mutating anything. It is the collision guard for writers
// that derive an ID deterministically from the slot they intend to fill —
// two writers racing for the same slot derive the same ID, and the store
// admits only the first, so the loser learns its slot was taken instead of
// silently storing a second row for it.
//
// Resolution mirrors MetadataCASWriterFor: it follows wrapper-declared
// resolution targets (ConditionalWritesResolveTargeter), because
// interface-embedding wrappers do not promote optional capabilities, and it
// does NOT guess at unwrapping — a wrapper participates only by declaring
// its target or implementing the capability itself.

package beads

import (
	"errors"
	"fmt"
	"strings"
)

// ErrPinnedCreateUnsupported reports a store that cannot pin IDs on this
// path, either at all (the capability is absent) or at this instance's
// admission (for example a bd release without insert-only pinned creates).
// It is returned before any entry mutation.
var ErrPinnedCreateUnsupported = errors.New("pinned create unsupported")

// PinnedIDCreator is the create capability for callers that must choose the
// bead's ID themselves: CreatePinned persists b with exactly b.ID when the
// ID is free, and refuses — without modifying the existing occupant — when
// it is taken. The ID must be nonempty.
type PinnedIDCreator interface {
	// CreatePinned persists b with exactly b.ID. A duplicate ID is an
	// error; the existing occupant is neither overwritten nor reminted.
	CreatePinned(b Bead) (Bead, error)
}

// PinnedIDCreatorHandleProvider exposes a pinned-create handle for wrappers
// whose capability depends on wrapped runtime state. It mirrors
// MetadataCASWriterHandleProvider: a wrapper can delegate the capability
// without claiming the interface globally.
type PinnedIDCreatorHandleProvider interface {
	PinnedIDCreatorHandle() (PinnedIDCreator, bool)
}

// PinnedIDCreatorFor returns the pinned-create capability for store when one
// is available. A store that cannot pin IDs reports false, and callers
// decide their own policy: refuse, or fall back to an ordinary create whose
// ID the store may mint.
func PinnedIDCreatorFor(store Store) (PinnedIDCreator, bool) {
	if store == nil {
		return nil, false
	}
	store = followConditionalWritesResolveTarget(store)
	if creator, ok := store.(PinnedIDCreator); ok {
		return creator, true
	}
	if provider, ok := store.(PinnedIDCreatorHandleProvider); ok {
		return provider.PinnedIDCreatorHandle()
	}
	return nil, false
}

// validatePinnedID checks the caller-chosen ID up front so a refusal
// happens before any entry mutation.
func validatePinnedID(b Bead) (string, error) {
	explicit := strings.TrimSpace(b.ID)
	if explicit == "" {
		return "", fmt.Errorf("creating pinned bead: empty id")
	}
	return explicit, nil
}
