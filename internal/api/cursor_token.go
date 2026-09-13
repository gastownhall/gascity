package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Keyset cursor tokens: versioned, opaque pagination cursors that encode the
// sort-key boundary of the last row served instead of an integer offset.
// Offsets skip or duplicate rows whenever a concurrent write shifts the
// result set — a guarantee on a live work ledger — while a keyset boundary
// stays stable: the next page is "rows strictly after this boundary in the
// collection's total order" regardless of inserts above it.
//
// Wire format: "v1:" + base64url(JSON keysetCursor). The prefix versions the
// token so a future format change (or yesterday's bare-offset cursors) is a
// typed 400 invalid-cursor, never a silent misread.

const cursorVersionPrefix = "v1:"

// Cursor kinds. Each paginated collection accepts exactly one kind; a valid
// token of the wrong kind is rejected by the handler as an invalid cursor.
const (
	// cursorKindCreatedID marks a (created_at, id) boundary — the total order
	// of bead-backed collections (#3208).
	cursorKindCreatedID = "cb"
	// cursorKindSeq marks an event-log sequence boundary.
	cursorKindSeq = "sq"
)

// keysetCursor is the decoded form of a v1 pagination token.
type keysetCursor struct {
	Kind      string    `json:"k"`
	CreatedAt time.Time `json:"ca,omitzero"`
	ID        string    `json:"id,omitempty"`
	Seq       uint64    `json:"s,omitempty"`
}

var errInvalidCursor = errors.New("invalid pagination cursor")

// encodeKeysetCursor serializes a boundary as an opaque v1 token.
func encodeKeysetCursor(c keysetCursor) string {
	data, err := json.Marshal(c)
	if err != nil {
		// keysetCursor contains only marshalable fields; unreachable.
		return ""
	}
	return cursorVersionPrefix + base64.RawURLEncoding.EncodeToString(data)
}

// decodeKeysetCursor parses a v1 token, rejecting anything else — including
// the legacy base64-offset cursors — with an error the handler maps to a 400
// problem+json invalid-cursor response.
func decodeKeysetCursor(token string) (keysetCursor, error) {
	var zero keysetCursor
	rest, ok := strings.CutPrefix(token, cursorVersionPrefix)
	if !ok {
		return zero, errInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return zero, errInvalidCursor
	}
	var c keysetCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return zero, errInvalidCursor
	}
	switch c.Kind {
	case cursorKindCreatedID:
		// A zero CreatedAt is a legal boundary: degraded rows (NULL or
		// unparseable created_at from a drifted store) carry zero timestamps,
		// sort to the created-DESC tail, and the server mints boundaries from
		// them — the decoder must accept every token the server mints or a
		// walk wedges in a 400 loop at the tail. Only the ID is required.
		if c.ID == "" {
			return zero, errInvalidCursor
		}
	case cursorKindSeq:
		// Seq 0 is a legal boundary (before the first event).
	default:
		return zero, errInvalidCursor
	}
	return c, nil
}
