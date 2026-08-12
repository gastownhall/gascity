package session

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// PollerKeyFromBead returns the concrete nudge-poller ownership key for a
// session bead, preferring the store-assigned session ID before metadata
// fallbacks used by older or partially materialized session beads.
func PollerKeyFromBead(b beads.Bead) string {
	if id := strings.TrimSpace(b.ID); id != "" {
		return id
	}
	for _, value := range []string{
		b.Metadata["alias"],
		b.Metadata["agent_name"],
		b.Metadata["template"],
		b.Metadata["session_name"],
		b.Title,
	} {
		if key := strings.TrimSpace(value); key != "" {
			return key
		}
	}
	return ""
}

// PollerKeyFromInfo is the session.Info twin of PollerKeyFromBead: it returns the
// same nudge-poller ownership key off an already-projected session.Info instead
// of a raw bead, using the identical ID-first preference and metadata fallback
// order (alias → agent_name → template → session_name → title).
// SessionNameMetadata is the RAW session_name mirror (matching
// PollerKeyFromBead's Metadata["session_name"], not the sessionNameFor-filled
// SessionName). For any info == infoFromPersistedBead(b) it equals
// PollerKeyFromBead(b); TestPollerKeyFromInfoMatchesBead pins that.
func PollerKeyFromInfo(info Info) string {
	if id := strings.TrimSpace(info.ID); id != "" {
		return id
	}
	for _, value := range []string{
		info.Alias,
		info.AgentName,
		info.Template,
		info.SessionNameMetadata,
		info.Title,
	} {
		if key := strings.TrimSpace(value); key != "" {
			return key
		}
	}
	return ""
}
