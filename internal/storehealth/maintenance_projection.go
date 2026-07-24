package storehealth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	// runtimeRoot is the city's per-install runtime directory, beside which
	// the event log and this projection sidecar live.
	runtimeRoot = ".gc"

	// maintenanceProjectionBasename is the sidecar the latest-by-status store
	// maintenance projection is persisted to.
	maintenanceProjectionBasename = "store-maintenance-latest.json"
)

// projectionWriteMu serializes the load-modify-store on the projection
// sidecar within a process. Correctness relies on a single-writer-process
// invariant: only the supervisor writes the sidecar — SeedMaintenanceProjection
// on the first /status read and RecordMaintenanceEvent at maintenance-emit
// time, both in the one supervisor process (which holds the city's instance
// lock, so there is never a second supervisor). The transient `gc status`
// CLI reads via the read-only LastMaintenance and never writes. With exactly
// one writer process, a process-local mutex plus an atomic rename fully
// serializes every write; there is no cross-process writer to coordinate,
// which is why a process-local lock is sufficient and why the CLI path must
// stay read-only (a second writing process would defeat this mutex and could
// clobber a fresher emit-time value — the review blocker on the first cut).
var projectionWriteMu sync.Mutex

// MaintenanceProjection is the persisted latest-by-status view of Dolt
// store maintenance. It exists so the /status read answers "when did
// maintenance last succeed / fail" in O(1) instead of scanning the
// (multi-hundred-MB, archived) event history on every call — the scan that
// consumed 56% of supervisor CPU. The sidecar is maintained at
// maintenance-emit time by RecordMaintenanceEvent and seeded once from a
// bounded history scan by SeedMaintenanceProjection when first absent; both
// writers run only in the supervisor process (see projectionWriteMu).
type MaintenanceProjection struct {
	LastDoneAt   time.Time `json:"last_done_at,omitempty"`
	LastFailedAt time.Time `json:"last_failed_at,omitempty"`
}

// MaintenanceProjectionPath returns the projection sidecar path for the
// city rooted at cityPath.
func MaintenanceProjectionPath(cityPath string) string {
	return filepath.Join(cityPath, runtimeRoot, maintenanceProjectionBasename)
}

// Latest returns the timestamp and status label ("success" or "failed")
// of the most recent maintenance event in the projection, or (zero, "")
// when neither has been recorded. A tie resolves to "success", matching
// the prior full-scan behavior, which replaced a done with a failed only
// at a strictly later timestamp.
func (p MaintenanceProjection) Latest() (time.Time, string) {
	if p.LastFailedAt.After(p.LastDoneAt) {
		return p.LastFailedAt, "failed"
	}
	if !p.LastDoneAt.IsZero() {
		return p.LastDoneAt, "success"
	}
	return time.Time{}, ""
}

// LoadMaintenanceProjection reads the projection sidecar for cityPath. ok
// is false with a nil error when the sidecar does not exist yet — the
// first-read / fresh-install signal that triggers the one-time seed in
// SeedMaintenanceProjection. A read or decode error is returned so the
// caller can decide to re-seed rather than trust a partial value.
func LoadMaintenanceProjection(fs fsys.FS, cityPath string) (MaintenanceProjection, bool, error) {
	data, err := fs.ReadFile(MaintenanceProjectionPath(cityPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return MaintenanceProjection{}, false, nil
		}
		return MaintenanceProjection{}, false, err
	}
	var p MaintenanceProjection
	if err := json.Unmarshal(data, &p); err != nil {
		return MaintenanceProjection{}, false, err
	}
	return p, true, nil
}

// writeMaintenanceProjectionLocked persists p atomically (temp write +
// rename). The caller must hold projectionWriteMu. The temp path carries
// the writer's pid so that even if the single-writer-process invariant
// were ever violated, two processes would stage to distinct temp files and
// never interleave bytes into one — the rename remains last-writer-wins,
// but the file is never left half-written.
func writeMaintenanceProjectionLocked(fs fsys.FS, cityPath string, p MaintenanceProjection) error {
	if err := fs.MkdirAll(filepath.Join(cityPath, runtimeRoot), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	final := MaintenanceProjectionPath(cityPath)
	tmp := final + ".tmp." + strconv.Itoa(os.Getpid())
	if err := fs.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := fs.Rename(tmp, final); err != nil {
		_ = fs.Remove(tmp) //nolint:errcheck // best-effort cleanup of the temp file
		return err
	}
	return nil
}

// RecordMaintenanceEvent updates the projection sidecar for a completed
// maintenance run: it raises the last-done or last-failed timestamp
// (whichever status names) to ts, preserving the other field. This is the
// append-time upkeep that keeps the projection O(1)-readable without a
// history scan. status is "success" or "failed"; any other value, an
// empty cityPath, or a zero ts is a no-op. A corrupt existing sidecar is
// overwritten from the fresh event, which is authoritative-latest.
func RecordMaintenanceEvent(fs fsys.FS, cityPath string, ts time.Time, status string) error {
	if cityPath == "" || ts.IsZero() {
		return nil
	}
	if status != "success" && status != "failed" {
		return nil
	}
	projectionWriteMu.Lock()
	defer projectionWriteMu.Unlock()
	p, _, err := LoadMaintenanceProjection(fs, cityPath)
	if err != nil {
		p = MaintenanceProjection{}
	}
	switch status {
	case "success":
		if ts.After(p.LastDoneAt) {
			p.LastDoneAt = ts
		}
	case "failed":
		if ts.After(p.LastFailedAt) {
			p.LastFailedAt = ts
		}
	}
	return writeMaintenanceProjectionLocked(fs, cityPath, p)
}

// scanMaintenanceProjection derives a projection from a full provider
// history scan (both maintenance types, archives included). It is the
// one-time seed path: expensive on a large archived history, run only
// when the sidecar is first absent, never on the steady-state read path.
func scanMaintenanceProjection(ep events.Provider) MaintenanceProjection {
	var p MaintenanceProjection
	if ep == nil {
		return p
	}
	for _, spec := range []struct {
		typ string
		dst *time.Time
	}{
		{events.StoreMaintenanceDone, &p.LastDoneAt},
		{events.StoreMaintenanceFailed, &p.LastFailedAt},
	} {
		evts, err := ep.List(events.Filter{Type: spec.typ})
		if err != nil {
			continue
		}
		for _, e := range evts {
			if e.Ts.After(*spec.dst) {
				*spec.dst = e.Ts
			}
		}
	}
	return p
}

// maxTime returns the later of a and b.
func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
