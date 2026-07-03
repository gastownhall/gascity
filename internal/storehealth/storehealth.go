// Package storehealth computes the Dolt bead store health summary used
// by gc status and the /v0/status API. The summary is: store path on
// disk, raw size in bytes, the live row count of the city store, a
// derived MB-per-row ratio, and a warning flag when the ratio exceeds
// the configured threshold.
//
// Design: bead ga-d5y design D9.
package storehealth

import (
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// DefaultThresholdMB is the MB-per-row threshold above which the
// size-to-row ratio is flagged. 1 MB per row matches the bad case
// observed in production (.beads/dolt at ~11 GB with ~64 rows).
const DefaultThresholdMB = 1.0

// Health summarizes disk health of the Dolt bead store. A pointer
// *Health is included in status payloads so "no data" (e.g. supervisor
// not running) is representable as nil rather than a confusing
// zero-valued block.
type Health struct {
	Path        string
	SizeBytes   int64
	LiveRows    int
	RatioMB     float64
	Warning     bool
	ThresholdMB float64
}

// StorePath returns the canonical on-disk location of the Dolt store
// for a city rooted at cityPath.
func StorePath(cityPath string) string {
	metaPath := filepath.Join(cityPath, ".beads", "metadata.json")
	if state, ok, err := contract.LoadMetadataState(fsys.OSFS{}, metaPath); err == nil && ok {
		if strings.EqualFold(strings.TrimSpace(state.Backend), "doltlite") {
			return filepath.Join(cityPath, ".beads", "doltlite")
		}
	}
	return filepath.Join(cityPath, ".beads", "dolt")
}

// Compute builds a Health from measured inputs. Pure function — all
// I/O is performed by the caller via WalkSize.
func Compute(cityPath string, sizeBytes int64, liveRows int) Health {
	h := Health{
		Path:        StorePath(cityPath),
		SizeBytes:   sizeBytes,
		LiveRows:    liveRows,
		ThresholdMB: DefaultThresholdMB,
	}
	if liveRows > 0 {
		h.RatioMB = float64(sizeBytes) / (bytesPerMB * float64(liveRows))
	}
	h.Warning = sizeBytes > int64(DefaultThresholdMB*bytesPerMB)*int64(liveRows)
	return h
}

// WalkSize returns the total size in bytes of path's contents,
// recursing into subdirectories. Missing paths and read errors are
// treated as zero bytes — a fresh city has no Dolt directory yet, and
// partial read failures should not mask the rest of the status output.
func WalkSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

const bytesPerMB = 1_000_000
