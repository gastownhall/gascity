package storehealth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	LastDoneAt   time.Time `json:"last_done_at"`
	LastFailedAt time.Time `json:"last_failed_at"`
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
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return MaintenanceProjection{}, false, err
	}
	if raw == nil {
		return MaintenanceProjection{}, false, errors.New("maintenance projection is null")
	}
	for _, field := range []string{"last_done_at", "last_failed_at"} {
		value, ok := raw[field]
		if !ok {
			return MaintenanceProjection{}, false, fmt.Errorf("maintenance projection field %s is missing", field)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return MaintenanceProjection{}, false, fmt.Errorf("maintenance projection field %s is null", field)
		}
	}
	for field := range raw {
		if field != "last_done_at" && field != "last_failed_at" {
			return MaintenanceProjection{}, false, fmt.Errorf("unknown maintenance projection field %s", field)
		}
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

// RecordMaintenanceEvent records a completed maintenance event and updates the
// projection with a crash-safe ordering: remove the old projection, invoke
// record, then atomically write the new projection. Holding projectionWriteMu
// across all three steps prevents a concurrent seed from observing the
// deliberate gap. If removal fails, record is not called, so an event can
// never become newer than a still-valid stale projection. If recording or the
// final write is interrupted, the absent projection makes the next supervisor
// status read reconstruct once from retained history.
func RecordMaintenanceEvent(fs fsys.FS, cityPath string, ts time.Time, status string, record func()) error {
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
		// A corrupt or unreadable projection is not authoritative. The current
		// event is still safe to publish once the stale file is invalidated.
		p = MaintenanceProjection{}
	}
	path := MaintenanceProjectionPath(cityPath)
	if err := fs.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("invalidate maintenance projection: %w", err)
	}

	if record != nil {
		record()
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
	if err := writeMaintenanceProjectionLocked(fs, cityPath, p); err != nil {
		return fmt.Errorf("write maintenance projection: %w", err)
	}
	return nil
}

// MaintenanceEventProvider is the read-only event surface required to derive
// the maintenance projection. Full event providers and path-backed CLI readers
// both satisfy it without giving transient status commands a write handle.
type MaintenanceEventProvider interface {
	List(events.Filter) ([]events.Event, error)
}

// scanMaintenanceProjection derives a projection from a full provider
// history scan (both maintenance types, archives included). A provider error
// aborts the scan so callers never persist an incomplete projection as truth.
func scanMaintenanceProjection(ep MaintenanceEventProvider) (MaintenanceProjection, error) {
	var p MaintenanceProjection
	if ep == nil {
		return p, errors.New("events provider unavailable")
	}
	for _, spec := range []struct {
		typ string
		dst *time.Time
	}{
		{events.StoreMaintenanceDone, &p.LastDoneAt},
		{events.StoreMaintenanceFailed, &p.LastFailedAt},
	} {
		filter := events.Filter{Type: spec.typ}
		var (
			evts []events.Event
			err  error
		)
		if inFlight, ok := ep.(events.InFlightProvider); ok {
			evts, err = inFlight.ListInFlight(filter)
		} else {
			evts, err = ep.List(filter)
		}
		if err != nil {
			return MaintenanceProjection{}, fmt.Errorf("list %s events: %w", spec.typ, err)
		}
		for _, e := range evts {
			if e.Ts.After(*spec.dst) {
				*spec.dst = e.Ts
			}
		}
	}
	return p, nil
}

// maxTime returns the later of a and b.
func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
