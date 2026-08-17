package beads

import "encoding/json"

// DecodeBeadEventPayload extracts a Bead from a bead.* event payload. It is the
// single shared decoder for the bead-event wire shape; the API layer, the
// caching store, and the run-view projection all route through it so their
// tolerance can never drift apart on a future cleanup.
//
// Canonical shape = the RAW bead snapshot (json.Marshal(b)) — that is exactly
// what CachingStore.notifyChange emits and what every .gc/events.jsonl row
// holds. The wrapped {"bead":<snapshot>} shape (the registered BeadEventPayload
// contract) is accepted as a tolerant fallback so a producer that ever emits it
// does not silently starve consumers. A payload that decodes to a bead with an
// empty id, an undecodable payload, or an empty payload is a decode miss:
// (Bead{}, false).
//
// Bead's own json tags round-trip the raw wire faithfully (issue_type→Type,
// parent→ParentID, ref/from/needs/metadata/dependencies, and StringMap coerces
// the non-string metadata values the external bd CLI emits). The one field a
// plain unmarshal misses is the exec-style "type" alias for issue_type, so the
// decoder applies that compat fallback after the raw unmarshal.
func DecodeBeadEventPayload(payload json.RawMessage) (Bead, bool) {
	if len(payload) == 0 {
		return Bead{}, false
	}
	if b, ok := decodeRawBead(payload); ok {
		return b, true
	}
	// Tolerant fallback: the wrapped {"bead":<snapshot>} shape.
	var env struct {
		Bead json.RawMessage `json:"bead"`
	}
	if err := json.Unmarshal(payload, &env); err == nil && len(env.Bead) > 0 {
		if b, ok := decodeRawBead(env.Bead); ok {
			return b, true
		}
	}
	return Bead{}, false
}

// decodeRawBead unmarshals a raw bead snapshot and applies the exec-style "type"
// alias for issue_type. Returns false on decode error or empty id.
func decodeRawBead(data json.RawMessage) (Bead, bool) {
	var b Bead
	if err := json.Unmarshal(data, &b); err != nil || b.ID == "" {
		return Bead{}, false
	}
	if b.Type == "" {
		var compat struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &compat); err == nil && compat.Type != "" {
			b.Type = compat.Type
		}
	}
	return b, true
}
