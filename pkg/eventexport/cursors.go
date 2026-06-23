package eventexport

import (
	"encoding/json"
	"os"
)

// LoadCursors reads persisted per-city resume cursors. A missing or unreadable
// file yields an empty map (a fresh exporter floors each city at its head).
func LoadCursors(path string) map[string]uint64 {
	out := map[string]uint64{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out) //nolint:errcheck // a corrupt cursor file resets to empty
	return out
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
