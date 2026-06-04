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
