//go:build windows

package proctable

import (
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ScanBySessionIDSince returns the full Windows session-ID scan. It does not
// excuse inspection failures from processes that predate incarnationStartedAt,
// so it is never less complete than ScanBySessionID.
func ScanBySessionIDSince(id string, _ time.Time) ([]runtime.LiveRuntime, error) {
	return ScanBySessionID(id)
}
