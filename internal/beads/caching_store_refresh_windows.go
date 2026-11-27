//go:build windows

package beads

import (
	"errors"
	"fmt"
	"strings"
)

// refreshIDsFencedRetries bounds how often RefreshIDs re-reads a row whose
// refresh a newer write, event or scan fenced.
const refreshIDsFencedRetries = 3

// RefreshIDs refreshes the named rows from the backing store through
// RefreshRow, which installs each into the cache and announces what changed.
// A row the backing no longer has is omitted. A row whose refresh stays fenced
// by newer state returns the cached row, which that newer state wrote; a cache
// that is not serving reads the backing directly.
func (c *CachingStore) RefreshIDs(ids []string) ([]Bead, error) {
	if c == nil || c.backing == nil {
		return nil, fmt.Errorf("refreshing cache rows: backing store unavailable")
	}
	ids = compactRefreshIDs(ids)
	rows := make([]Bead, 0, len(ids))
	for _, id := range ids {
		row, found, err := c.refreshID(id)
		if err != nil {
			return nil, err
		}
		if found {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (c *CachingStore) refreshID(id string) (Bead, bool, error) {
	for attempt := 1; ; attempt++ {
		row, err := c.RefreshRow(id)
		switch {
		case err == nil:
			return row, true, nil
		case errors.Is(err, ErrNotFound):
			return Bead{}, false, nil
		case errors.Is(err, ErrCacheUnavailable):
			row, err := c.backing.Get(id)
			if errors.Is(err, ErrNotFound) {
				return Bead{}, false, nil
			}
			return row, err == nil, err
		case errors.Is(err, ErrRowRefreshFenced) && attempt >= refreshIDsFencedRetries:
			row, err := c.Get(id)
			if errors.Is(err, ErrNotFound) {
				return Bead{}, false, nil
			}
			return row, err == nil, err
		case errors.Is(err, ErrRowRefreshFenced):
			continue
		default:
			return Bead{}, false, err
		}
	}
}

func compactRefreshIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}
