package doctor

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var compactStateMarkerDirs = []string{
	"compact-quarantine",
	"compact-pending-gc",
	"compact-pending-push",
	"compact-pending-push-backup",
}

var compactStateMarkerTempName = regexp.MustCompile(`^.+\.(?:tmp|probe)\.[[:alnum:]]{6}$`)

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
	remote     string
}

func (c *DoltCompactStateCheck) scanMarkers() ([]compactStateMarker, []string) {
	packStateDir := doctorDoltPackStateDir(c.cityPath)
	var markers []compactStateMarker
	var readWarnings []string
	for _, markerType := range compactStateMarkerDirs {
		dir := filepath.Join(packStateDir, markerType)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			readWarnings = append(readWarnings, fmt.Sprintf("unreadable marker directory %s: %v", dir, err))
			continue
		}
		for _, e := range entries {
			if e.Type()&fs.ModeType != 0 || strings.HasPrefix(e.Name(), ".") || compactStateMarkerTempName.MatchString(e.Name()) {
				continue
			}
			markerPath := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(markerPath) //nolint:gosec
			if err != nil {
				readWarnings = append(readWarnings, fmt.Sprintf("unreadable marker %s: %v", markerPath, err))
				continue
			}
			m := compactStateMarker{
				markerType: markerType,
				db:         e.Name(),
				path:       markerPath,
			}
			for _, line := range strings.Split(string(data), "\n") {
				if v, ok := strings.CutPrefix(line, "db="); ok && v != "" {
					m.db = v
				} else if v, ok := strings.CutPrefix(line, "reason="); ok {
					m.reason = v
				} else if v, ok := strings.CutPrefix(line, "created_at="); ok {
					m.createdAt = v
				} else if v, ok := strings.CutPrefix(line, "remote="); ok {
					m.remote = v
				}
			}
			markers = append(markers, m)
		}
	}
	return markers, readWarnings
}

// Run scans compact lifecycle markers.
func (c *DoltCompactStateCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip {
		r.Status = StatusOK
		r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		return r
	}

	markers, readWarnings := c.scanMarkers()

	if len(markers) == 0 && len(readWarnings) == 0 {
		r.Status = StatusOK
		r.Message = "no stale compact markers"
		return r
	}

	details := make([]string, 0, len(markers)+len(readWarnings))
	for _, m := range markers {
		details = append(details, fmt.Sprintf("marker: %s db=%s path=%s reason=%s created_at=%s remote=%s",
			m.markerType, m.db, m.path, m.reason, m.createdAt, m.remote))
	}
	details = append(details, readWarnings...)
	r.Details = details

	markerLabels := make([]string, len(markers))
	hintLines := make([]string, len(markers), len(markers)+len(readWarnings))
	for i, m := range markers {
		markerLabels[i] = fmt.Sprintf("%s for %s", m.markerType, m.db)
		hintLines[i] = compactMarkerFixHint(m)
	}
	hintLines = append(hintLines, readWarnings...)

	var msgParts []string
	if len(markerLabels) > 0 {
		msgParts = append(msgParts, fmt.Sprintf("compact lifecycle markers: %s", strings.Join(markerLabels, ", ")))
	}
	if len(readWarnings) > 0 {
		msgParts = append(msgParts, fmt.Sprintf("%d marker path(s) unreadable", len(readWarnings)))
	}

	r.Status = StatusWarning
	r.Message = strings.Join(msgParts, "; ")
	r.FixHint = strings.Join(hintLines, "\n")
	return r
}

func compactMarkerFixHint(m compactStateMarker) string {
	switch m.markerType {
	case "compact-quarantine":
		return fmt.Sprintf("inspect %s; clear the marker only after verifying its recorded evidence", m.path)
	case "compact-pending-gc":
		if m.remote != "" {
			return fmt.Sprintf("remote-backed GC incomplete for %s via %s; inspect %s, then reconcile manually or retry with GC_DOLT_COMPACT_ALLOW_FEDERATED=1 only during an announced compaction window", m.db, m.remote, m.path)
		}
		return fmt.Sprintf("GC incomplete for %s; inspect %s, then run: gc dolt compact --only-db %s", m.db, m.path, m.db)
	case "compact-pending-push":
		if m.remote != "" {
			return fmt.Sprintf("remote push pending for %s via %s; inspect %s, then reconcile manually or retry with GC_DOLT_COMPACT_ALLOW_FEDERATED=1 only during an announced compaction window", m.db, m.remote, m.path)
		}
		return fmt.Sprintf("push pending for %s; inspect %s, then run: gc dolt compact --only-db %s", m.db, m.path, m.db)
	case "compact-pending-push-backup":
		return fmt.Sprintf("backup push pending for %s; inspect %s, reconcile the backup remote manually, then remove the marker after verification", m.db, m.path)
	default:
		return fmt.Sprintf("inspect %s", m.path)
	}
}

// CanFix returns false — compact state requires manual intervention.
func (c *DoltCompactStateCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *DoltCompactStateCheck) Fix(_ *CheckContext) error { return nil }
