package opjournal

import (
	"errors"
	"fmt"
)

// Sentinel errors. Callers match them with errors.Is; the store wraps them with
// the op id and the cause.
var (
	// ErrUnknownKind reports a kind the registry does not define.
	ErrUnknownKind = errors.New("opjournal: unknown op kind")
	// ErrUnsupportedKind reports a stored row whose kind this build does not
	// know. A reader never skips such a row silently.
	ErrUnsupportedKind = errors.New("opjournal: unsupported op kind")
	// ErrUnsupportedSchema reports a stored row written under a schema
	// revision this build does not read.
	ErrUnsupportedSchema = errors.New("opjournal: unsupported op schema revision")
	// ErrUnsupportedValue reports a stored row carrying a vocabulary value
	// (a delivery mode, wake, role, fallback, failure policy, restart mode,
	// guarantee or scope kind) this build does not know: a row written by a
	// newer build, which a reader must not mistake for a damaged one.
	ErrUnsupportedValue = errors.New("opjournal: unsupported op vocabulary value")
	// ErrInvalidRequest reports a request or outcome refused at the front door.
	// Nothing is written.
	ErrInvalidRequest = errors.New("opjournal: invalid request")
	// ErrPayloadTooLarge reports a payload above MaxInlinePayload. Nothing is
	// written.
	ErrPayloadTooLarge = errors.New("opjournal: payload too large")
	// ErrKeyReuse reports a client key reused with a different request digest.
	// Nothing is written.
	ErrKeyReuse = errors.New("opjournal: client key reused with a different request")
	// ErrCorruptOp reports a stored row that does not decode as a valid op or
	// fact.
	ErrCorruptOp = errors.New("opjournal: corrupt op row")
	// ErrIllegalTransition reports a status change outside the op state
	// machine.
	ErrIllegalTransition = errors.New("opjournal: illegal status transition")
	// ErrStatusMismatch reports that the op is not in the status the caller
	// expected.
	ErrStatusMismatch = errors.New("opjournal: op status mismatch")
	// ErrContention reports that a revision-fenced write lost every retry.
	ErrContention = errors.New("opjournal: revision contention")
	// ErrAttemptMoved reports an adoption that lost to another adopter: the
	// op's attempt is no longer the one the adopter read.
	ErrAttemptMoved = errors.New("opjournal: op attempt moved")
	// ErrAlreadyTerminal reports a second, different outcome for a closed op.
	ErrAlreadyTerminal = errors.New("opjournal: op already closed with another outcome")
	// ErrBeadTypeUnregistered reports a store that refuses the "op" bead type.
	// The remedy is `gc doctor --fix` or a city restart, which registers it.
	ErrBeadTypeUnregistered = errors.New("opjournal: bead type \"op\" is not registered in this store (run `gc doctor --fix` or restart the city)")
	// ErrPinnedIDNotHonored reports a store that minted its own id instead of
	// the pinned op id.
	ErrPinnedIDNotHonored = errors.New("opjournal: store did not honor the pinned op id")
	// ErrNoNamespace reports a store whose mint prefix could not be resolved.
	ErrNoNamespace = errors.New("opjournal: store has no id prefix")
)

// KeyReuseError reports that a client key already names an op with another
// request digest. Existing is the op already stored under the id.
type KeyReuseError struct {
	Existing Op
}

// Error describes the reused key.
func (e *KeyReuseError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("op %s: client key %q reused with a different request (stored digest %s)",
		e.Existing.ID, e.Existing.ClientKey, e.Existing.PayloadDigest)
}

// Unwrap returns ErrKeyReuse.
func (e *KeyReuseError) Unwrap() error { return ErrKeyReuse }

// StatusError reports an op whose status is not the one the caller expected.
type StatusError struct {
	ID       string
	Expected Status
	Current  Status
}

// Error describes the mismatch.
func (e *StatusError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("op %s: status is %s, expected %s", e.ID, e.Current, e.Expected)
}

// Unwrap returns ErrStatusMismatch.
func (e *StatusError) Unwrap() error { return ErrStatusMismatch }

// invalidf returns an ErrInvalidRequest with context.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, args...))
}

// corruptf returns an ErrCorruptOp with context.
func corruptf(id, format string, args ...any) error {
	return fmt.Errorf("%w %s: %s", ErrCorruptOp, id, fmt.Sprintf(format, args...))
}
