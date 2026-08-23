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
// confusing zero-valued block. The same idiom applies one level down at
// RowsMeasured: LiveRows alone cannot distinguish a genuinely empty
// store from a row count that failed or timed out, so a caller that
// fabricates LiveRows=0 on measurement failure makes an unmeasured
// store indistinguishable from a healthy one. RowsMeasured is that
// distinction; when false, RatioMB and Warning are never computed and
// LiveRows carries no meaning.
type Health struct {
	Path         string
	SizeBytes    int64
	LiveRows     int
	RowsMeasured bool
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
//
// rowsMeasured tells Compute whether retainedRows is a real count or a
// caller's placeholder for "the count did not complete" (nil store,
// scan error, timeout). Callers MUST NOT pass rowsMeasured=true with a
// fabricated retainedRows value — doing so is exactly the defect this
// parameter exists to prevent: a failed measurement rendering
// byte-identically to a healthy, genuinely-empty store.
func Compute(cityPath string, sizeBytes int64, retainedRows int, rowsMeasured bool, lastGCAt time.Time, lastGCStatus string) Health {
	h := Health{
		Path:         StorePath(cityPath),
		SizeBytes:    sizeBytes,
		LiveRows:     retainedRows,
		RowsMeasured: rowsMeasured,
		ThresholdMB:  DefaultThresholdMB,
		LastGCAt:     lastGCAt,
		LastGCStatus: lastGCStatus,
	}
	if rowsMeasured && retainedRows > 0 {
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

// lastMaintenanceScanWindowBytes bounds the TailProvider fast path in
// LastMaintenance: a rare/optional event type (store maintenance may never
// have run) forces a full-file backward walk otherwise, at the same cost as
// an unfiltered forward scan (#4418). "Not found within the window" is a
// valid, already-representable result — Health.LastGCAt is omitempty and
// the status line simply omits "Last GC:" — so this trades a theoretical
// miss on a maintenance event older than the window for bounded latency on
// every call. 8 MiB comfortably covers many thousands of events at the
// measured ~800 bytes/event average, while capping the worst case (a log
// that has never emitted a matching event) to a small fraction of a full
// scan.
//
// The window is not the only way this can miss, and not the dominant one.
// FileRecorder.ListTail reads the ACTIVE events.jsonl only; it never opens
// the sibling .gz archives that the old List path walked via ReadFiltered.
// With rotation on by default at 256 MiB (defaultRotationMaxSize in
// internal/events/recorder.go), a maintenance event that has aged into an
// archive is invisible to this caller no matter how large the window is.
// That is accepted, not an oversight — see LastMaintenance.
const lastMaintenanceScanWindowBytes = 8 * 1024 * 1024

// LastMaintenance returns the timestamp and status ("success" or
// "failed") of the most-recent store-maintenance event in provider.
// Zero time and empty status when no events, provider is nil, or the
// provider returns an error.
//
// When provider implements [events.TailProvider], this uses the bounded
// backward-tail scan instead of the unbounded forward List — see
// lastMaintenanceScanWindowBytes. Providers without a tail fast path (e.g.
// an exec-script provider) fall back to the original unbounded List call.
//
// This caller takes a short ListTail result at face value. It deliberately
// does NOT take the under-fill fall-through that fetchEventPageAscending
// (internal/api/huma_handlers_events.go) uses, where a tail result shorter
// than the requested limit cannot distinguish "log exhausted" from "active
// file exhausted, older matches in the archives" and so falls back to the
// archive-aware full scan. Falling back here would restore exactly the full
// scan this path exists to remove — and it would fire on the common case
// (a city that has never run store maintenance), which is the worst case.
// The cost is that a maintenance timestamp older than the window, or aged
// into a rotated .gz archive, is reported as absent. Every consumer of the
// resulting LastGCAt renders it conditionally for display and none gates a
// decision on it, so absent is a safe answer. Operators who need a durable
// answer have `gc maintenance status`.
func LastMaintenance(ep events.Provider) (time.Time, string) {
	if ep == nil {
		return time.Time{}, ""
	}
	tp, hasTail := ep.(events.TailProvider)
	var (
		latestTs     time.Time
		latestStatus string
	)
	for _, spec := range []struct {
		typ    string
		status string
	}{
		{events.StoreMaintenanceDone, "success"},
		{events.StoreMaintenanceFailed, "failed"},
	} {
		var (
			evts []events.Event
			err  error
		)
		if hasTail {
			evts, err = tp.ListTail(events.Filter{Type: spec.typ, MaxScanBytes: lastMaintenanceScanWindowBytes}, 1)
		} else {
			evts, err = ep.List(events.Filter{Type: spec.typ})
		}
		if err != nil {
			continue
		}
		for _, e := range evts {
			if e.Ts.After(latestTs) {
				latestTs = e.Ts
				latestStatus = spec.status
			}
		}
	}
	return latestTs, latestStatus
}

const bytesPerMB = 1_000_000
