package eventexport

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// LoadCursors reads persisted per-city resume cursors. A missing file is the
// expected first-run case and yields an empty map with no error (a fresh
// exporter then floors each city at its head). An existing-but-unreadable or
// corrupt file is reported as an error instead of being silently reset: doing
// so would floor every tracked city at its current head and skip every event
// accumulated since the last durable cursor, so the caller must decide (it
// fails closed) rather than lose exports.
func LoadCursors(path string) (map[string]uint64, error) {
	out := map[string]uint64{}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil // first run: no durable cursor yet
		}
		return nil, fmt.Errorf("eventexport: read cursor file %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("eventexport: parse cursor file %s: %w", path, err)
	}
	if out == nil {
		// A cursor file containing JSON null unmarshals to a nil map without an
		// error. Treat it as corrupt and fail closed: a nil map reaching
		// MuxSource reads as no durable cursors and floors every tracked city at
		// head, skipping events accumulated since the last durable save. A
		// legitimate empty object ("{}") stays a non-nil empty map and is kept.
		return nil, fmt.Errorf("eventexport: cursor file %s contains null instead of a cursor object", path)
	}
	return out, nil
}

// SaveCursors atomically persists per-city resume cursors.
func SaveCursors(path string, cursors map[string]uint64) error {
	b, err := json.Marshal(cursors)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
