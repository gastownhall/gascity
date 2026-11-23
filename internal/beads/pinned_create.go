package beads

import (
	"errors"
	"fmt"
	"strings"
)

// ErrPinnedCreateUnsupported is returned when a caller needs an atomic
// pinned-id create from a store that cannot provide one: a store without the
// PinnedIDCreator capability, or a backend whose create would overwrite an
// existing row instead of refusing it (bd before the conditional-write
// release).
var ErrPinnedCreateUnsupported = errors.New("pinned-id create unsupported")

// PinnedIDCreator is the optional capability to create a bead at an id the
// caller chose, atomically refusing an id that already names a row.
//
// The contract, verified by beadstest.RunPinnedIDDuplicateConformance:
//   - the id is required: an empty or whitespace-padded id is refused before
//     the store is touched, because a pinned create that falls back to minting
//     gives the caller a row it cannot find again by the id it chose;
//   - on success the returned bead carries exactly that id;
//   - when the id already names a row (open or closed) the create fails and
//     the existing row is left exactly as it was, so the caller can read the
//     id back to learn who holds it;
//   - a namespace-fenced store refuses an id outside its namespaces as its
//     ordinary Create does.
//
// Ordinary Create keeps its own semantics; some stores mint over a supplied
// id, and old bd upserts it. Callers that need the refusal resolve this
// capability with PinnedIDCreatorFor (or beads.As) and refuse to proceed when
// it is absent rather than fall back to Create.
type PinnedIDCreator interface {
	CreatePinned(b Bead) (Bead, error)
}

// PinnedIDCreatorHandleProvider exposes a pinned-create handle for stores
// whose capability depends on wrapped runtime state, mirroring
// MetadataCASWriterHandleProvider.
type PinnedIDCreatorHandleProvider interface {
	PinnedIDCreatorHandle() (PinnedIDCreator, bool)
}

// PinnedIDCreatorFor returns the pinned-id create capability for store when
// one is available. Like MetadataCASWriterFor it follows wrapper-declared
// resolution targets (ConditionalWritesResolveTargeter) and never guesses at
// unwrapping.
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

// checkPinnedCreateID refuses an id a pinned create cannot honor verbatim.
func checkPinnedCreateID(op, id string) error {
	if id == "" {
		return fmt.Errorf("%s: a pinned create requires an id", op)
	}
	if strings.TrimSpace(id) != id {
		return fmt.Errorf("%s: pinned id %q carries surrounding whitespace", op, id)
	}
	return nil
}

// checkPinnedCreateResult refuses a create that did not keep the pinned id.
func checkPinnedCreateResult(op, want string, created Bead) (Bead, error) {
	if created.ID != want {
		return Bead{}, fmt.Errorf("%s: store returned id %q for pinned id %q", op, created.ID, want)
	}
	return created, nil
}
