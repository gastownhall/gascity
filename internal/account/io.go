// Package account — io.go provides atomic JSON persistence for the account
// registry file (accounts.json).
package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Load reads the account registry from the JSON file at path.
// If the file does not exist, Load returns an empty Registry and nil error.
// If the file exists but cannot be parsed, Load returns an error that
// includes the file path for diagnostics.
func Load(path string) (Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Registry{}, nil
		}
		return Registry{}, fmt.Errorf("reading account registry %q: %w", path, err)
	}

	if len(data) == 0 {
		return Registry{}, nil
	}

	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		return Registry{}, fmt.Errorf("parsing account registry %q: %w", path, err)
	}
	return reg, nil
}

// Save writes the account registry to the JSON file at path using an
// atomic temp-file + rename pattern. Parent directories are created if
// they do not exist.
func Save(path string, reg Registry) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory for account registry %q: %w", path, err)
	}

	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling account registry: %w", err)
	}

	// Write to a temp file in the same directory, then rename for atomicity.
	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file for account registry %q: %w", path, err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()        //nolint:errcheck // best-effort cleanup
		os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("writing temp file for account registry %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("closing temp file for account registry %q: %w", path, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("renaming temp file for account registry %q: %w", path, err)
	}
	return nil
}
