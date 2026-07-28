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

// StatusUnknown marks a LastMaintenance result that could not be
// determined from the scanned window: a matching event may exist earlier
// in the active log (past the window floor) or in a rotated .gz archive
// that the windowed tail scan never reads. It is distinct from "" (no
// event exists anywhere — only ever returned via the unbounded List path,
// for providers that do not implement [events.TailProvider]) and from
// "success"/"failed" (a matching event was actually found). Callers must
// not render StatusUnknown as "never ran" — see #4418/#4427 review: a
// bounded probe that reports a confident zero is worse than one that
// admits it does not know.
const StatusUnknown = "unknown"

// minScanWindow floors [ScanWindow]'s result at the size #4427 originally
// shipped, so a very short maintenance interval never shrinks the window
// below a size that is already cheap to scan.
const minScanWindow = 8 * 1024 * 1024

// busyCityDailyRotationBytes matches events.defaultRotationMaxSize
// (internal/events/recorder.go): the events log rotates at 256 MiB, sized
// so a busy city rotates roughly once per day. ListTail only ever reads
// the active (post-rotation) file, so it can never usefully see more than
// one rotation's worth of bytes — a window larger than this buys nothing.
const busyCityDailyRotationBytes = 256 * 1024 * 1024

// ScanWindow returns the LastMaintenance windowed-scan bound for a
// maintenance loop with the given cadence: enough of the active log's
// tail to cover interval at busyCityDailyRotationBytes/day of throughput,
// floored at minScanWindow and capped at busyCityDailyRotationBytes since
// ListTail cannot see past the active file regardless of window size.
// interval <= 0 falls back to the 168h (weekly) maintenance default.
func ScanWindow(interval time.Duration) int64 {
	if interval <= 0 {
		interval = 168 * time.Hour
	}
	days := interval.Hours() / 24
	window := int64(days * float64(busyCityDailyRotationBytes))
	if window < minScanWindow {
		window = minScanWindow
	}
	if window > busyCityDailyRotationBytes {
		window = busyCityDailyRotationBytes
	}
	return window
}

// LastMaintenance returns the timestamp and status ("success", "failed",
// or [StatusUnknown]) of the most-recent store-maintenance event in
// provider. Zero time and empty status when provider is nil.
//
// When ep implements [events.TailProvider], LastMaintenance scans only the
// trailing window bytes of the log (see [ScanWindow]) — never an unbounded
// scan. A windowed miss reports StatusUnknown rather than "never ran",
// because a miss cannot distinguish "no matching event" from "the event is
// past the window floor or in a rotated .gz archive the tail scan does not
// read." Only providers without ListTail (the exec provider and
// transientCityEventProvider, neither production caller implements
// TailProvider) run the unbounded List and can therefore return a
// definitive "never ran" ("").
func LastMaintenance(ep events.Provider, window int64) (time.Time, string) {
	if ep == nil {
		return time.Time{}, ""
	}
	tp, hasTail := ep.(events.TailProvider)
	var (
		latestTs     time.Time
		latestStatus string
		sawUnknown   bool
	)
	for _, spec := range []struct {
		typ    string
		status string
	}{
		{events.StoreMaintenanceDone, "success"},
		{events.StoreMaintenanceFailed, "failed"},
	} {
		var evts []events.Event
		var err error
		if hasTail {
			evts, err = tp.ListTail(events.Filter{Type: spec.typ, WindowBytes: window}, 1)
			if err != nil {
				continue
			}
			if len(evts) == 0 {
				sawUnknown = true
				continue
			}
		} else {
			evts, err = ep.List(events.Filter{Type: spec.typ})
			if err != nil {
				continue
			}
		}
		for _, e := range evts {
			if e.Ts.After(latestTs) {
				latestTs = e.Ts
				latestStatus = spec.status
			}
		}
	}
	if !latestTs.IsZero() {
		return latestTs, latestStatus
	}
	if sawUnknown {
		return time.Time{}, StatusUnknown
	}
	return time.Time{}, ""
}

const bytesPerMB = 1_000_000
