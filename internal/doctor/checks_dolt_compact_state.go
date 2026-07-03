package doctor

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// compactStateMarkerDirs are the subdirectories the compact run.sh writes
// lifecycle markers into under the pack state directory.
var compactStateMarkerDirs = []string{
	"compact-quarantine",
	"compact-pending-gc",
	"compact-pending-push",
}

// DoltCompactStateCheck inspects compact lifecycle markers to surface stale
// quarantine or pending-GC/push state on managed Dolt stores.
//
// Registered once per city — the managed Dolt server is shared across all rigs.
type DoltCompactStateCheck struct {
	cityPath string
	skip     bool
}

// NewDoltCompactStateCheck creates a DoltCompactStateCheck for the given city.
func NewDoltCompactStateCheck(cityPath string, skip bool) *DoltCompactStateCheck {
	return &DoltCompactStateCheck{
		cityPath: cityPath,
		skip:     skip,
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

// Run scans compact lifecycle markers.
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

	if len(markers) == 0 {
		r.Status = StatusOK
		r.Message = "no stale compact markers"
		return r
	}

	details := make([]string, 0, len(markers))
	for _, m := range markers {
		details = append(details, fmt.Sprintf("marker: %s db=%s path=%s reason=%s created_at=%s",
			m.markerType, m.db, m.path, m.reason, m.createdAt))
	}
	r.Details = details

	markerLabels := make([]string, len(markers))
	hintLines := make([]string, len(markers))
	for i, m := range markers {
		markerLabels[i] = fmt.Sprintf("%s for %s", m.markerType, m.db)
		hintLines[i] = compactMarkerFixHint(m)
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("compact lifecycle markers: %s", strings.Join(markerLabels, ", "))
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
