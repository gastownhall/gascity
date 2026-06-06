package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
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
// The check scans the city and all managed rig scope roots, because each bd
// workspace root can maintain its own .beads/backup/ auto-backup directory.
type BdBackupSizeCheck struct {
	cityPath   string
	measureDir func(string) (int64, bool, error)
	scopeRoots []string
}

// NewBdBackupSizeCheck creates a size check against managed
// <scope>/.beads/backup/ directories.
func NewBdBackupSizeCheck(cityPath string) *BdBackupSizeCheck {
	return &BdBackupSizeCheck{cityPath: cityPath, measureDir: duDirBytes}
}

// NewBdBackupSizeCheckForConfig creates a size check using preloaded city
// config to avoid reparsing city.toml during doctor registration.
func NewBdBackupSizeCheckForConfig(cityPath string, cfg *config.City, cfgErr error) *BdBackupSizeCheck {
	return &BdBackupSizeCheck{
		cityPath:   cityPath,
		measureDir: duDirBytes,
		scopeRoots: managedDoltScopeRootsForConfig(cityPath, cfg, cfgErr),
	}
}

// Name returns the check identifier.
func (c *BdBackupSizeCheck) Name() string { return "bd-backup-size" }

// Run measures the bd auto-backup directories and compares them against
// warning/error thresholds.
func (c *BdBackupSizeCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	measure := c.measureDir
	if measure == nil {
		measure = duDirBytes
	}

	var (
		worstTarget bdBackupScanTarget
		worstBytes  int64
		totalBytes  int64
		existsCount int
	)
	targets := c.backupScanTargets()
	for _, target := range targets {
		if info, err := os.Stat(target.BackupDir); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			r.Status = StatusWarning
			r.Message = fmt.Sprintf("stat bd backup dir for %s: %v", target.Label, err)
			return r
		} else if !info.IsDir() {
			r.Status = StatusWarning
			r.Message = fmt.Sprintf("%s for %s is not a directory", target.BackupDir, target.Label)
			return r
		}

		bytes, exists, err := measure(target.BackupDir)
		if err != nil {
			r.Status = StatusWarning
			r.Message = fmt.Sprintf("measure bd backup dir for %s: %v", target.Label, err)
			return r
		}
		if !exists {
			continue
		}
		existsCount++
		totalBytes += bytes
		if bytes > worstBytes {
			worstBytes = bytes
			worstTarget = target
		}
	}

	if existsCount == 0 {
		scopeNoun := "managed scopes"
		if len(targets) == 1 {
			scopeNoun = "managed scope"
		}
		r.Status = StatusOK
		r.Message = fmt.Sprintf("no bd backup directory present across %d %s", len(targets), scopeNoun)
		return r
	}

	size := formatGB(worstBytes)
	scopeNote := ""
	if existsCount > 1 {
		scopeNote = fmt.Sprintf(" (largest of %d backup directories)", existsCount)
	}
	switch {
	case worstBytes >= bdBackupErrorBytes:
		r.Status = StatusError
		r.Message = fmt.Sprintf("bd auto-backup directory for %s is %s%s — excessive; cleanup recommended", worstTarget.Label, size, scopeNote)
		r.FixHint = bdBackupFixHint()
	case totalBytes >= bdBackupErrorBytes:
		r.Status = StatusError
		r.Message = fmt.Sprintf("aggregate bd auto-backup footprint is %s across %d backup directories — excessive; cleanup recommended", formatGB(totalBytes), existsCount)
		r.FixHint = bdBackupFixHint()
	case worstBytes >= bdBackupWarnBytes:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("bd auto-backup directory for %s is %s%s — approaching threshold", worstTarget.Label, size, scopeNote)
		r.FixHint = bdBackupFixHint()
	case totalBytes >= bdBackupWarnBytes:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("aggregate bd auto-backup footprint is %s across %d backup directories — approaching threshold", formatGB(totalBytes), existsCount)
		r.FixHint = bdBackupFixHint()
	default:
		r.Status = StatusOK
		if existsCount > 1 {
			r.Message = fmt.Sprintf("aggregate bd auto-backup footprint is %s across %d backup directories (largest %s: %s)", formatGB(totalBytes), existsCount, worstTarget.Label, size)
		} else {
			r.Message = fmt.Sprintf("bd auto-backup directory for %s is %s", worstTarget.Label, size)
		}
	}
	return r
}

type bdBackupScanTarget struct {
	Label     string
	BackupDir string
}

func (c *BdBackupSizeCheck) backupScanTargets() []bdBackupScanTarget {
	scopeRoots := c.scopeRoots
	if len(scopeRoots) == 0 {
		scopeRoots = managedDoltScopeRoots(c.cityPath)
	}
	if len(scopeRoots) == 0 {
		scopeRoots = []string{c.cityPath}
	}

	seen := make(map[string]struct{}, len(scopeRoots))
	targets := make([]bdBackupScanTarget, 0, len(scopeRoots))
	for _, scopeRoot := range scopeRoots {
		scopeRoot = strings.TrimSpace(scopeRoot)
		if scopeRoot == "" {
			continue
		}
		scopeRoot = filepath.Clean(scopeRoot)
		if _, ok := seen[scopeRoot]; ok {
			continue
		}
		seen[scopeRoot] = struct{}{}
		targets = append(targets, bdBackupScanTarget{
			Label:     bdBackupScopeLabel(c.cityPath, scopeRoot),
			BackupDir: filepath.Join(scopeRoot, ".beads", "backup"),
		})
	}
	if len(targets) == 0 {
		cityPath := filepath.Clean(c.cityPath)
		targets = append(targets, bdBackupScanTarget{
			Label:     bdBackupScopeLabel(c.cityPath, cityPath),
			BackupDir: filepath.Join(cityPath, ".beads", "backup"),
		})
	}
	return targets
}

func bdBackupScopeLabel(cityPath, scopeRoot string) string {
	cityPath = filepath.Clean(cityPath)
	scopeRoot = filepath.Clean(scopeRoot)
	if scopeRoot == cityPath {
		return "city"
	}
	if rel, err := filepath.Rel(cityPath, scopeRoot); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "scope " + rel
	}
	return "scope " + scopeRoot
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
