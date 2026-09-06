//go:build !unix

package sqlite

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
)

// ReadLegacySessionsSnapshot is unavailable where the required O_NOFOLLOW
// descriptor pinning primitive is unavailable; callers fail closed.
func ReadLegacySessionsSnapshot(cityPath string) ([]beads.Bead, error) {
	if cityPath == "" {
		return nil, nil
	}
	path := filepath.Join(cityPath, ".gc", "store", "sessions.db")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("inspecting legacy sessions source: %w", err)
	}
	return nil, fmt.Errorf("legacy sessions snapshot requires unix descriptor pinning")
}
