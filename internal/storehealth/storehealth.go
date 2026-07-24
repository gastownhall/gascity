// Package storehealth computes the Dolt bead store health summary used
// by gc status and the /v0/status API. The summary is: store path on
// disk, raw size in bytes, the retained row count of the city store
// (including open and closed beads), a derived MB-per-row ratio, and a
// warning flag when the ratio exceeds the configured threshold.
//
// Design: ADR 0002 (docs/adr/0002-dolt-store-maintenance-runbook.md)
// and bead ga-d5y design D9.
package storehealth

import (
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

// DefaultThresholdMB is the MB-per-row threshold above which maintenance
// is flagged overdue. 1 MB per row matches the bad case observed in
// production (.beads/dolt at ~11 GB with ~64 rows).
const DefaultThresholdMB = 1.0

// MinWarnSizeBytes is the absolute floor below which the ratio-based
// warning never fires, regardless of row count. A pure MB-per-row ratio
// degenerates at small denominators: a healthy young city with only a
// handful of live rows still carries Dolt's own baseline footprint
// (oldgen archives, system tables) well into the hundreds of MB, which
// would otherwise permanently trip the ratio threshold with nothing for
// maintenance to reclaim -- gc dolt compact's own commit-count gate
// correctly finds nothing to do, but the warning can never clear (#3374).
const MinWarnSizeBytes = 1_000_000_000 // 1 GB

// Health summarizes disk and maintenance health of the Dolt bead store.
// A pointer *Health is included in status payloads so "no data" (e.g.
// supervisor not running) is representable as nil rather than a
// confusing zero-valued block.
type Health struct {
	Path         string
	SizeBytes    int64
	LiveRows     int
	RatioMB      float64
	Warning      bool
	ThresholdMB  float64
	LastGCAt     time.Time
	LastGCStatus string
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
// I/O is performed by the caller via WalkSize and LastMaintenance.
func Compute(cityPath string, sizeBytes int64, retainedRows int, lastGCAt time.Time, lastGCStatus string) Health {
	h := Health{
		Path:         StorePath(cityPath),
		SizeBytes:    sizeBytes,
		LiveRows:     retainedRows,
		ThresholdMB:  DefaultThresholdMB,
		LastGCAt:     lastGCAt,
		LastGCStatus: lastGCStatus,
	}
	if retainedRows > 0 {
		h.RatioMB = float64(sizeBytes) / (bytesPerMB * float64(retainedRows))
		h.Warning = sizeBytes > MinWarnSizeBytes && sizeBytes > int64(DefaultThresholdMB*bytesPerMB)*int64(retainedRows)
	}
	return h
}

// WalkSize returns the total size in bytes of path's contents,
// recursing into subdirectories. Missing paths and read errors are
// treated as zero bytes — a fresh city has no Dolt directory yet, and
// partial read failures during maintenance should not mask the rest
// of the status output.
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

// LastMaintenance returns the timestamp and status ("success" or
// "failed") of the most-recent store-maintenance event for the city at
// cityPath. It is READ-ONLY and safe to call from any process — including
// the transient `gc status` CLI — because it never writes the shared
// projection sidecar. It reads the persisted latest-by-status sidecar in
// O(1); when the sidecar is absent (a fresh install, or a supervisor that
// has not seeded yet) it falls back to a bounded read-only scan of ep's
// history (rotated .gz archives included) and returns that without
// persisting. Persisting the sidecar is the supervisor's job alone — see
// SeedMaintenanceProjection and RecordMaintenanceEvent — so there is
// exactly one writer process and the process-local projectionWriteMu fully
// serializes it. Zero time and empty status when no maintenance has been
// recorded or cityPath is empty.
func LastMaintenance(fs fsys.FS, cityPath string, ep events.Provider) (time.Time, string) {
	if cityPath == "" {
		return time.Time{}, ""
	}
	if p, ok, err := LoadMaintenanceProjection(fs, cityPath); err == nil && ok {
		return p.Latest()
	}
	// Read-only fallback: derive the answer from history but do not write.
	return scanMaintenanceProjection(ep).Latest()
}

// SeedMaintenanceProjection returns the latest maintenance timestamp/status
// exactly like LastMaintenance, but on an absent sidecar it also PERSISTS
// the one-time seed so subsequent reads are O(1). It is the supervisor-only
// persisting path: the supervisor process is the single writer of the
// projection sidecar (this first-read seed plus RecordMaintenanceEvent at
// emit time), which is why projectionWriteMu — a process-local lock — is
// sufficient and the CLI fallback (LastMaintenance) must stay read-only.
//
// The scan runs outside the lock (it can be seconds on a large archive);
// the result is then merged under the lock against any value the emit path
// wrote meanwhile, taking the max per field, so a concurrent same-process
// emit is never lowered. A no-match scan still persists an (empty) sidecar,
// so the full scan runs at most once per city rather than on every
// no-maintenance-events read — the failure mode that made request-time
// scanning (and its archive-blind ListTail workaround) unacceptable.
func SeedMaintenanceProjection(fs fsys.FS, cityPath string, ep events.Provider) (time.Time, string) {
	if cityPath == "" {
		return time.Time{}, ""
	}
	if p, ok, err := LoadMaintenanceProjection(fs, cityPath); err == nil && ok {
		return p.Latest()
	}

	scanned := scanMaintenanceProjection(ep)
	projectionWriteMu.Lock()
	defer projectionWriteMu.Unlock()
	current, _, _ := LoadMaintenanceProjection(fs, cityPath)
	current.LastDoneAt = maxTime(current.LastDoneAt, scanned.LastDoneAt)
	current.LastFailedAt = maxTime(current.LastFailedAt, scanned.LastFailedAt)
	_ = writeMaintenanceProjectionLocked(fs, cityPath, current) //nolint:errcheck // best-effort seed
	return current.Latest()
}

const bytesPerMB = 1_000_000
