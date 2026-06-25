package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// defaultBackupFreshnessMaxAge is how stale a rig's last bd backup sync may be
// before BdBackupFreshnessCheck warns. bd's auto-backup interval is minutes, so
// a day-old (or older) last sync means the backup pipeline is disabled, broken,
// or the rig is unattended — the silent gap that turns a recoverable store loss
// into a near-permanent one when the only surviving backup is weeks stale.
const defaultBackupFreshnessMaxAge = 24 * time.Hour

// BdBackupFreshnessCheck warns when a rig that HAS a local bd backup
// (.beads/backup/backup_state.json) has not synced within maxAge. It is the
// freshness complement to the existing backup checks: DoltBackupCheck verifies
// a backup is registered, BdBackupSizeCheck guards the backup footprint, and
// BdBackupStateCheck flags quarantines and stale registrations — none notice
// that a configured backup has simply stopped running. Reading only the
// on-disk backup_state.json keeps the check DB-free.
//
// A backup that exists but stopped syncing is invisible to every other signal:
// the registration still looks healthy and the artifact dir is still present,
// so the rig appears protected while its recovery point silently ages out.
type BdBackupFreshnessCheck struct {
	cityPath   string
	scopeRoots []string
	maxAge     time.Duration
	now        func() time.Time
}

// NewBdBackupFreshnessCheckForConfig creates a freshness check across the city
// and all managed rig scope roots, using preloaded city config to avoid
// reparsing city.toml during doctor registration.
func NewBdBackupFreshnessCheckForConfig(cityPath string, cfg *config.City, cfgErr error) *BdBackupFreshnessCheck {
	return &BdBackupFreshnessCheck{
		cityPath:   cityPath,
		scopeRoots: managedDoltScopeRootsForConfig(cityPath, cfg, cfgErr),
		maxAge:     defaultBackupFreshnessMaxAge,
		now:        time.Now,
	}
}

// NewBdBackupFreshnessCheckForScopeRoots creates a freshness check over an
// explicit scope-root list with an injectable max age and clock. Used by tests.
func NewBdBackupFreshnessCheckForScopeRoots(cityPath string, scopeRoots []string, maxAge time.Duration, now func() time.Time) *BdBackupFreshnessCheck {
	if maxAge <= 0 {
		maxAge = defaultBackupFreshnessMaxAge
	}
	if now == nil {
		now = time.Now
	}
	return &BdBackupFreshnessCheck{cityPath: cityPath, scopeRoots: scopeRoots, maxAge: maxAge, now: now}
}

// Name returns the check identifier.
func (c *BdBackupFreshnessCheck) Name() string { return "bd-backup-freshness" }

// WarmupEligible returns false: backup freshness is a steady-state hygiene
// signal, not a fail-fast gate that should block `gc start`.
func (c *BdBackupFreshnessCheck) WarmupEligible() bool { return false }

// CanFix returns false: re-enabling or repairing a backup pipeline is operator
// policy, not a mechanical fix.
func (c *BdBackupFreshnessCheck) CanFix() bool { return false }

// Fix is a no-op; the check is report-only.
func (c *BdBackupFreshnessCheck) Fix(_ *CheckContext) error { return nil }

// Run reads each scope's .beads/backup/backup_state.json and warns on any whose
// last sync is older than maxAge (or whose timestamp is missing or
// unparseable). Scopes with no backup_state.json are skipped — "no backup at
// all" is reported by DoltBackupCheck / BdBackupSizeCheck, not here.
func (c *BdBackupFreshnessCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	now := c.now()

	var findings []string
	for _, target := range c.freshnessScanTargets() {
		if finding, ok := scanBackupFreshness(target.Label, target.BeadsDir, now, c.maxAge); ok {
			findings = append(findings, finding)
		}
	}

	if len(findings) == 0 {
		r.Status = StatusOK
		r.Message = "all configured bd backups synced within " + c.maxAge.String()
		return r
	}
	sort.Strings(findings)
	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	r.Message = strings.Join(findings, "; ")
	r.FixHint = "re-enable or repair the bd backup pipeline for the listed scopes " +
		"(bd backup sync; verify backup.enabled and BD_BACKUP_ENABLED), then confirm " +
		"bd backup status shows a recent sync"
	return r
}

type bdBackupFreshnessTarget struct {
	Label    string
	BeadsDir string
}

func (c *BdBackupFreshnessCheck) freshnessScanTargets() []bdBackupFreshnessTarget {
	scopeRoots := c.scopeRoots
	if len(scopeRoots) == 0 {
		scopeRoots = managedDoltScopeRoots(c.cityPath)
	}
	if len(scopeRoots) == 0 {
		scopeRoots = []string{c.cityPath}
	}

	seen := make(map[string]struct{}, len(scopeRoots))
	targets := make([]bdBackupFreshnessTarget, 0, len(scopeRoots))
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
		targets = append(targets, bdBackupFreshnessTarget{
			Label:    bdBackupScopeLabel(c.cityPath, scopeRoot),
			BeadsDir: filepath.Join(scopeRoot, ".beads"),
		})
	}
	return targets
}

// scanBackupFreshness reads <beadsDir>/backup/backup_state.json and returns a
// finding when the last sync is older than maxAge or the timestamp cannot be
// read. A missing backup_state.json returns ("", false) — not this check's job.
func scanBackupFreshness(label, beadsDir string, now time.Time, maxAge time.Duration) (string, bool) {
	path := filepath.Join(beadsDir, "backup", "backup_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false
		}
		return fmt.Sprintf("%s: read backup_state.json: %v", label, err), true
	}
	var state struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Sprintf("%s: backup_state.json is unparseable: %v", label, err), true
	}
	ts := strings.TrimSpace(state.Timestamp)
	if ts == "" {
		return fmt.Sprintf("%s: backup_state.json has no timestamp", label), true
	}
	synced, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return fmt.Sprintf("%s: backup_state.json timestamp %q is unparseable: %v", label, ts, err), true
	}
	if age := now.Sub(synced); age > maxAge {
		return fmt.Sprintf("%s: last bd backup sync was %s ago (> %s) — backup pipeline may be disabled or broken",
			label, age.Round(time.Minute), maxAge), true
	}
	return "", false
}
