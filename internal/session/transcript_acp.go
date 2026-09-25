package session

import (
	"os"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
)

// acpCaptureTranscriptPath returns the JSON-RPC capture transcript gc wrote
// for an ACP session's current continuation epoch, or "" when the session is
// not ACP, the manager has no city, or the capture does not exist yet.
//
// The capture is the transcript of every ACP session whatever agent it runs,
// so it outranks workdir-based discovery, which could hand back another
// provider's transcript from the same directory.
func (m *Manager) acpCaptureTranscriptPath(b beads.Bead) string {
	if transportFromMetadata(b) != "acp" || strings.TrimSpace(m.cityPath) == "" {
		return ""
	}
	path, err := citylayout.ACPTranscriptPath(m.cityPath, b.ID, transcriptContinuationEpoch(b.Metadata["continuation_epoch"]))
	if err != nil {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}

// transcriptContinuationEpoch renders the epoch a runtime start publishes as
// GC_CONTINUATION_EPOCH for the stored metadata value (see RuntimeEnv and
// commitPendingContinuationReset): a missing or invalid value means the
// default epoch.
func transcriptContinuationEpoch(value string) string {
	epoch, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || epoch <= 0 {
		epoch = DefaultContinuationEpoch
	}
	return strconv.Itoa(epoch)
}
