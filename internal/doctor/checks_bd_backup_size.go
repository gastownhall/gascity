package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// bd auto-backup growth thresholds. bd's PersistentPostRun-driven
// auto-backup writes to a single hardcoded "backup_export" Dolt remote
// at <root>/.beads/backup/ on every bd invocation (15-minute throttle).
// There is no retention or rotation logic upstream
// (gastownhall/beads#2993), so the directory grows unbounded.
//
// Real-world cascade reference: qlandia gc-p831i, 2026-05-20/21 — the
// directory reached 34 GB and filled the disk, which broke dolt writes
// and amplified into a multi-hour outage. Warn well below that.
const (
	bdBackupWarnBytes  = int64(5) * 1024 * 1024 * 1024  // 5 GB
	bdBackupErrorBytes = int64(15) * 1024 * 1024 * 1024 // 15 GB
)

// BdBackupSizeCheck warns when bd's auto-backup directory has grown
// large enough to risk a disk-full cascade.
//
// Upstream context: gastownhall/beads#2993 (snapshot-collection
// redesign), #4070 (non-atomic sync), #3522/#3501/#3878 (auto-backup
// race/config bugs). Until the upstream fix lands, operators rely on
// this canary to catch the growth early.
//
// The check is intentionally narrow: it scans <city>/.beads/backup/,
// the canonical city-level auto-backup destination. Rig-scoped bd
// installations have their own .beads/backup/ subdirectories; extending
// the scan to those is straightforward when an operator needs it.
type BdBackupSizeCheck struct {
	cityPath   string
	measureDir func(string) (int64, bool, error)
}

// NewBdBackupSizeCheck creates a size check against
// <cityPath>/.beads/backup/.
func NewBdBackupSizeCheck(cityPath string) *BdBackupSizeCheck {
	return &BdBackupSizeCheck{cityPath: cityPath, measureDir: duDirBytes}
}

// Name returns the check identifier.
func (c *BdBackupSizeCheck) Name() string { return "bd-backup-size" }

// Run measures the bd auto-backup directory and compares against
// warning/error thresholds.
func (c *BdBackupSizeCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	backupDir := filepath.Join(c.cityPath, ".beads", "backup")
	if info, err := os.Stat(backupDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			r.Status = StatusOK
			r.Message = "no bd backup directory present"
			return r
		}
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("stat bd backup dir: %v", err)
		return r
	} else if !info.IsDir() {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("%s is not a directory", backupDir)
		return r
	}

	measure := c.measureDir
	if measure == nil {
		measure = duDirBytes
	}
	bytes, _, err := measure(backupDir)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("measure bd backup dir: %v", err)
		return r
	}

	size := formatGB(bytes)
	switch {
	case bytes >= bdBackupErrorBytes:
		r.Status = StatusError
		r.Message = fmt.Sprintf("bd auto-backup directory is %s — excessive; cleanup recommended", size)
		r.FixHint = bdBackupFixHint()
	case bytes >= bdBackupWarnBytes:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("bd auto-backup directory is %s — approaching threshold", size)
		r.FixHint = bdBackupFixHint()
	default:
		r.Status = StatusOK
		r.Message = fmt.Sprintf("bd auto-backup directory is %s", size)
	}
	return r
}

// CanFix returns false. Backup cleanup has nontrivial atomicity
// concerns (gastownhall/beads#4070) and the right cleanup is operator
// policy: rotate-and-recreate vs prune-stale-by-manifest vs disable
// auto-backup. We surface the size; the operator picks the recipe.
func (c *BdBackupSizeCheck) CanFix() bool { return false }

// Fix is a no-op. See CanFix.
func (c *BdBackupSizeCheck) Fix(_ *CheckContext) error { return nil }

func bdBackupFixHint() string {
	return "see docs/troubleshooting/bd-backup-cleanup.md"
}
