package eventfact

import (
	"errors"
	"fmt"
)

// Sentinel errors, matched with errors.Is.
var (
	// ErrInvalid reports a fact or checkpoint refused before encoding: nothing
	// is written.
	ErrInvalid = errors.New("eventfact: invalid")
	// ErrCorrupt reports a stored row that does not decode as a valid event
	// fact or checkpoint.
	ErrCorrupt = errors.New("eventfact: corrupt row")
	// ErrUnsupportedSchema reports a row written under a schema revision this
	// build does not read: a newer build's row, never skipped silently.
	ErrUnsupportedSchema = errors.New("eventfact: unsupported schema revision")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func corruptf(id, format string, args ...any) error {
	return fmt.Errorf("%w %s: %s", ErrCorrupt, id, fmt.Sprintf(format, args...))
}
