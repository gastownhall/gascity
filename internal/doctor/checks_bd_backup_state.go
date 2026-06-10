package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// BdBackupStateCheck flags stale bd backup state that accumulates silently:
//
//   - corrupt-store quarantine directories (.beads/*.corrupt-*): bd renames a
//     corrupt Dolt store aside on detection and nothing ever reclaims it — a
//     1.8GB quarantine sat unnoticed from 2026-05-27 to 2026-06-10
//     (ga-yfbs28). Quarantines deserve a diagnosis and then deletion, not
//     indefinite disk residency.
//   - a .beads/dolt-backup.json registration whose file:// backup_url points
//     at a path that no longer exists (e.g. a deleted mktemp dir): syncs
//     then fall back to the live .beads/backup directory while the
//     registration still looks healthy.
//
// The check reports only; cleanup is operator policy (archive-then-remove
// for quarantines, re-register or delete for stale registrations).
type BdBackupStateCheck struct {
	cityPath   string
	scopeRoots []string
}

// NewBdBackupStateCheckForConfig creates a backup-state check across the city
// and all managed rig scope roots, using preloaded city config to avoid
// reparsing city.toml during doctor registration.
func NewBdBackupStateCheckForConfig(cityPath string, cfg *config.City, cfgErr error) *BdBackupStateCheck {
	return &BdBackupStateCheck{
		cityPath:   cityPath,
		scopeRoots: managedDoltScopeRootsForConfig(cityPath, cfg, cfgErr),
	}
}

// NewBdBackupStateCheckForScopeRoots creates a backup-state check over an
// explicit scope-root list. Used by tests.
func NewBdBackupStateCheckForScopeRoots(cityPath string, scopeRoots []string) *BdBackupStateCheck {
	return &BdBackupStateCheck{cityPath: cityPath, scopeRoots: scopeRoots}
}

// Name returns the check identifier.
func (c *BdBackupStateCheck) Name() string { return "bd-backup-state" }

// Run scans each scope's .beads directory for corrupt-store quarantines and
// stale backup registrations.
func (c *BdBackupStateCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	var findings []string
	for _, target := range c.stateScanTargets() {
		findings = append(findings, scanBdBackupState(target.Label, target.BeadsDir)...)
	}

	if len(findings) == 0 {
		r.Status = StatusOK
		r.Message = "no corrupt-store quarantines or stale backup registrations"
		return r
	}
	r.Status = StatusWarning
	r.Message = strings.Join(findings, "; ")
	r.FixHint = "archive then remove quarantines (tar -czf <name>.tgz <dir> && rm -rf <dir>); " +
		"delete or re-register stale dolt-backup.json files"
	return r
}

// CanFix returns false: quarantine forensics and backup re-registration are
// operator decisions.
func (c *BdBackupStateCheck) CanFix() bool { return false }

// Fix is a no-op; the check is report-only.
func (c *BdBackupStateCheck) Fix(_ *CheckContext) error { return nil }

type bdBackupStateTarget struct {
	Label    string
	BeadsDir string
}

func (c *BdBackupStateCheck) stateScanTargets() []bdBackupStateTarget {
	scopeRoots := c.scopeRoots
	if len(scopeRoots) == 0 {
		scopeRoots = managedDoltScopeRoots(c.cityPath)
	}
	if len(scopeRoots) == 0 {
		scopeRoots = []string{c.cityPath}
	}

	seen := make(map[string]struct{}, len(scopeRoots))
	targets := make([]bdBackupStateTarget, 0, len(scopeRoots))
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
		targets = append(targets, bdBackupStateTarget{
			Label:    bdBackupScopeLabel(c.cityPath, scopeRoot),
			BeadsDir: filepath.Join(scopeRoot, ".beads"),
		})
	}
	return targets
}

// scanBdBackupState inspects one scope's .beads directory and returns
// human-readable findings. A missing .beads directory is not a finding.
func scanBdBackupState(label, beadsDir string) []string {
	var findings []string

	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return []string{fmt.Sprintf("%s: read %s: %v", label, beadsDir, err)}
	}

	for _, entry := range entries {
		if !entry.IsDir() || !strings.Contains(entry.Name(), ".corrupt-") {
			continue
		}
		path := filepath.Join(beadsDir, entry.Name())
		size, _, err := duDirBytes(path)
		if err != nil {
			findings = append(findings, fmt.Sprintf("%s: corrupt-store quarantine %s (size unknown: %v)", label, entry.Name(), err))
			continue
		}
		findings = append(findings, fmt.Sprintf("%s: corrupt-store quarantine %s (%s)", label, entry.Name(), formatGB(size)))
	}

	if finding, ok := staleBackupRegistration(label, beadsDir); ok {
		findings = append(findings, finding)
	}
	return findings
}

// staleBackupRegistration reports a dolt-backup.json whose file:// backup_url
// points at a path that no longer exists. Non-file URLs (remote backups)
// cannot be liveness-checked from this host and are never flagged.
func staleBackupRegistration(label, beadsDir string) (string, bool) {
	path := filepath.Join(beadsDir, "dolt-backup.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var reg struct {
		BackupURL string `json:"backup_url"`
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		return fmt.Sprintf("%s: dolt-backup.json is unparseable: %v", label, err), true
	}
	u, err := url.Parse(reg.BackupURL)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	backupPath := filepath.FromSlash(u.Path)
	if backupPath == "" {
		return "", false
	}
	if _, err := os.Stat(backupPath); errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("%s: dolt-backup.json registers backup_url %q whose path no longer exists", label, reg.BackupURL), true
	}
	return "", false
}
