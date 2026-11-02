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
	if strings.TrimSpace(m.cityPath) == "" || !m.beadUsesACP(b) {
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

// beadUsesACP reports whether a session runs on the ACP transport. Beads the
// controller creates carry no transport metadata, so it infers the transport
// the way the rest of the Manager does (transportForBead: stored metadata,
// MCP metadata, the runtime's route), then from the configured template or
// provider.
func (m *Manager) beadUsesACP(b beads.Bead) bool {
	transport, _ := m.transportForBead(b, sessionName(b.ID, b))
	if transport == "" {
		transport = m.resolveConfiguredTransport(b.Metadata["template"], b.Metadata["provider"])
	}
	return transport == "acp"
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
