package doctor

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// compactStateSizeWarnBytesPerRow is the bytes-per-live-row threshold above
	// which the doctor suggests running compact maintenance on a managed Dolt
	// store. Below this ratio the store is within normal operation; above it the
	// journal and MVCC history have grown large relative to live data.
	compactStateSizeWarnBytesPerRow = int64(1_000_000) // 1 MB/row
)

// compactStateMarkerDirs are the subdirectories the compact run.sh writes
// lifecycle markers into under the pack state directory.
var compactStateMarkerDirs = []string{
	"compact-quarantine",
	"compact-pending-gc",
	"compact-pending-push",
}

// DoltCompactStateCheck inspects compact lifecycle markers and store-size
// heuristics to surface stale quarantine or pending-GC/push state and managed
// stores that are overdue for compact maintenance.
//
// Registered once per city — the managed Dolt server is shared across all rigs.
type DoltCompactStateCheck struct {
	cityPath   string
	skip       bool
	measureDir func(string) (int64, bool, error)
	liveRows   func(string) (int, error)
}

// NewDoltCompactStateCheck creates a DoltCompactStateCheck for the given city.
func NewDoltCompactStateCheck(cityPath string, skip bool) *DoltCompactStateCheck {
	return &DoltCompactStateCheck{
		cityPath:   cityPath,
		skip:       skip,
		measureDir: duDirBytes,
		liveRows:   func(string) (int, error) { return 0, nil },
	}
}

// Name returns the check identifier.
func (c *DoltCompactStateCheck) Name() string { return "dolt-compact-state" }

type compactStateMarker struct {
	markerType string
	db         string
	path       string
	reason     string
	createdAt  string
}

func (c *DoltCompactStateCheck) scanMarkers() ([]compactStateMarker, error) {
	packStateDir := doctorDoltPackStateDir(c.cityPath)
	var markers []compactStateMarker
	for _, markerType := range compactStateMarkerDirs {
		dir := filepath.Join(packStateDir, markerType)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.Type()&fs.ModeType != 0 || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			markerPath := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(markerPath) //nolint:gosec
			if err != nil {
				continue
			}
			m := compactStateMarker{
				markerType: markerType,
				db:         e.Name(),
				path:       markerPath,
			}
			for _, line := range strings.Split(string(data), "\n") {
				if v, ok := strings.CutPrefix(line, "reason="); ok {
					m.reason = v
				} else if v, ok := strings.CutPrefix(line, "created_at="); ok {
					m.createdAt = v
				}
			}
			markers = append(markers, m)
		}
	}
	return markers, nil
}

type compactStoreSizeReport struct {
	db          string
	sizeBytes   int64
	rows        int
	bytesPerRow int64
}

func (c *DoltCompactStateCheck) scanStores() ([]compactStoreSizeReport, error) {
	dataDir := resolveManagedDoltDataDir(c.cityPath)
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dataDir, err)
	}
	var reports []compactStoreSizeReport
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		db := e.Name()
		doltPath := filepath.Join(dataDir, db, ".dolt")
		sizeBytes, ok, err := c.measureDir(doltPath)
		if err != nil || !ok {
			continue
		}
		rows, err := c.liveRows(db)
		if err != nil || rows <= 0 {
			continue
		}
		reports = append(reports, compactStoreSizeReport{
			db:          db,
			sizeBytes:   sizeBytes,
			rows:        rows,
			bytesPerRow: sizeBytes / int64(rows),
		})
	}
	return reports, nil
}

// Run scans compact lifecycle markers and managed store sizes.
func (c *DoltCompactStateCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip {
		r.Status = StatusOK
		r.Message = "skipped (managed dolt check disabled)"
		return r
	}

	markers, err := c.scanMarkers()
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("scan compact markers: %v", err)
		return r
	}

	stores, err := c.scanStores()
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("scan managed dolt stores: %v", err)
		return r
	}

	var overdueStores []compactStoreSizeReport
	for _, s := range stores {
		if s.bytesPerRow >= compactStateSizeWarnBytesPerRow {
			overdueStores = append(overdueStores, s)
		}
	}

	if len(markers) == 0 && len(overdueStores) == 0 {
		r.Status = StatusOK
		r.Message = "no stale compact markers"
		return r
	}

	var details []string
	for _, m := range markers {
		details = append(details, fmt.Sprintf("marker: %s db=%s path=%s reason=%s created_at=%s",
			m.markerType, m.db, m.path, m.reason, m.createdAt))
	}
	for _, s := range overdueStores {
		mbPerRow := float64(s.bytesPerRow) / 1_000_000
		details = append(details, fmt.Sprintf("store: db=%s %.2f MB/row (%d live rows) — maintenance overdue",
			s.db, mbPerRow, s.rows))
	}
	r.Details = details

	var msgParts []string
	var hintLines []string

	if len(markers) > 0 {
		markerLabels := make([]string, len(markers))
		for i, m := range markers {
			markerLabels[i] = fmt.Sprintf("%s for %s", m.markerType, m.db)
		}
		msgParts = append(msgParts, fmt.Sprintf("compact lifecycle markers: %s", strings.Join(markerLabels, ", ")))
		for _, m := range markers {
			hintLines = append(hintLines, compactMarkerFixHint(m))
		}
	}

	if len(overdueStores) > 0 {
		storeLabels := make([]string, len(overdueStores))
		for i, s := range overdueStores {
			mbPerRow := float64(s.bytesPerRow) / 1_000_000
			storeLabels[i] = fmt.Sprintf("%s (%.2f MB/row, %d live rows)", s.db, mbPerRow, s.rows)
		}
		msgParts = append(msgParts, fmt.Sprintf("maintenance overdue: %s", strings.Join(storeLabels, ", ")))
		hintLines = append(hintLines, "run: gc dolt compact")
	}

	r.Status = StatusWarning
	r.Message = strings.Join(msgParts, "; ")
	r.FixHint = strings.Join(hintLines, "\n")
	return r
}

func compactMarkerFixHint(m compactStateMarker) string {
	switch m.markerType {
	case "compact-quarantine":
		return fmt.Sprintf("inspect %s; if safe to clear, run: rm %s", m.path, m.path)
	case "compact-pending-gc":
		return fmt.Sprintf("GC incomplete for %s; run: gc dolt compact --resume or inspect %s", m.db, m.path)
	case "compact-pending-push":
		return fmt.Sprintf("push pending for %s; run: gc dolt push or inspect %s", m.db, m.path)
	default:
		return fmt.Sprintf("inspect %s", m.path)
	}
}

// CanFix returns false — compact state requires manual intervention.
func (c *DoltCompactStateCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *DoltCompactStateCheck) Fix(_ *CheckContext) error { return nil }
