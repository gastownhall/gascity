package proctable

import (
	"errors"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ErrIncarnationBoundUnsupported reports that this platform has no process
// inventory to bound to a session incarnation, so an empty scan result is the
// absence of evidence rather than evidence of absence. ScanBySessionIDSince
// returns it for a non-zero incarnationStartedAt on such platforms; a caller
// must treat the scan as incomplete, never as a certified absence.
var ErrIncarnationBoundUnsupported = errors.New("proctable: incarnation start-time bound unsupported on this platform")

// ScanAll returns all live agent root processes with a non-empty
// GC_SESSION_ID.
func ScanAll() ([]runtime.LiveRuntime, error) {
	return ScanBySessionID("")
}
