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
		"bd backup status shows a recent sync for the store named in the finding — " +
		"a 'dolt backup' finding clears via the Dolt Backup: Last sync field, not " +
		"the legacy Backup: block, which stays frozen after migration"
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

// scanBackupFreshness reports whether a scope's ACTIVE backup pipeline has
// stopped syncing.
//
// A scope has two possible pipelines and they record their progress in
// different files, and the two are ORTHOGONAL: registering a Dolt backup
// destination (.beads/dolt-backup.json) does not disable the legacy
// embedded-store pipeline, and `bd backup sync` writes only
// .beads/dolt-backup-state.json (updateDoltBackupState) — never
// .beads/backup/backup_state.json.
//
// So the two files are advanced by two different writers. On a scope where the
// legacy auto-backup is disabled, its writer never runs, and
// backup_state.json holds whatever it last recorded — while every BACKUP
// action the operator can take drives the OTHER pipeline's state file.
//
// The state this check did not model is "had a legacy bd backup, THEN gained a
// Dolt destination". It handles never-had-one (skip) and had-one-that-stopped
// (warn), but a scope whose legacy file is stale *because the live pipeline
// moved elsewhere* is reported as a broken backup pipeline while its actual
// backup is current. The FixHint compounds it by prescribing `bd backup sync`,
// which drives the Dolt pipeline and so cannot refresh the field being read.
//
// Note the warning condition itself implies the legacy pipeline is not
// running: were it still writing, backup_state.json would be fresh and this
// check would not fire at all.
//
// Note the check is not unclearable in the absolute — removing the legacy
// .beads/backup directory makes scanBackupFreshness skip the scope entirely.
// But no BACKUP action clears it: syncing the pipeline that is actually
// protecting the scope never moves the field this check reads.
//
// Reading the Dolt registration here is consistent with the rest of the
// package rather than novel: checks_bd_backup_state.go already treats
// .beads/dolt-backup.json as first-class when detecting stale registrations.
// This check alone ignored it.
//
// There is also a correctness stake beyond noise: the stale legacy state
// advertises a Dolt commit written before the destination was registered, so an
// incident responder restoring from that pointer recovers a pre-migration
// snapshot while believing the scope is current.
//
// So: prefer the Dolt backup state whenever a Dolt destination is registered,
// and fall back to the legacy file only for scopes that never migrated. Each
// finding names the store it describes, so the reader is never left guessing
// which of the two a message is about. A scope with neither file returns
// ("", false) — "no backup at all" is DoltBackupCheck's job, not this one's.
func scanBackupFreshness(label, beadsDir string, now time.Time, maxAge time.Duration) (string, bool) {
	if _, err := os.Stat(filepath.Join(beadsDir, "dolt-backup.json")); err == nil {
		return scanDoltBackupFreshness(label, beadsDir, now, maxAge)
	}
	return scanLegacyBackupFreshness(label, beadsDir, now, maxAge)
}

// scanDoltBackupFreshness reads <beadsDir>/dolt-backup-state.json, the file a
// successful Dolt backup sync stamps. A registered destination with no state
// file at all is a real finding — it means the backup has never once completed.
func scanDoltBackupFreshness(label, beadsDir string, now time.Time, maxAge time.Duration) (string, bool) {
	const store = "dolt backup"
	path := filepath.Join(beadsDir, "dolt-backup-state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Sprintf("%s: %s is registered (dolt-backup.json) but has never synced "+
				"— no dolt-backup-state.json", label, store), true
		}
		return fmt.Sprintf("%s: read dolt-backup-state.json: %v", label, err), true
	}
	var state struct {
		LastSync string `json:"last_sync"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Sprintf("%s: dolt-backup-state.json is unparseable: %v", label, err), true
	}
	return freshnessFinding(label, store, "dolt-backup-state.json", "last_sync", state.LastSync, now, maxAge)
}

// scanLegacyBackupFreshness reads <beadsDir>/backup/backup_state.json for scopes
// that have not migrated to a Dolt backup destination.
func scanLegacyBackupFreshness(label, beadsDir string, now time.Time, maxAge time.Duration) (string, bool) {
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
	return freshnessFinding(label, "embedded-store backup", "backup_state.json", "timestamp", state.Timestamp, now, maxAge)
}

// freshnessFinding turns one pipeline's recorded sync timestamp into a finding,
// naming both the store and the field it came from so the message is traceable
// back to the file the check actually read.
func freshnessFinding(label, store, file, field, raw string, now time.Time, maxAge time.Duration) (string, bool) {
	ts := strings.TrimSpace(raw)
	if ts == "" {
		return fmt.Sprintf("%s: %s: %s has no %s", label, store, file, field), true
	}
	synced, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return fmt.Sprintf("%s: %s: %s %s %q is unparseable: %v", label, store, file, field, ts, err), true
	}
	if age := now.Sub(synced); age > maxAge {
		return fmt.Sprintf("%s: %s: last sync was %s ago (> %s) — backup pipeline may be disabled or broken",
			label, store, age.Round(time.Minute), maxAge), true
	}
	return "", false
}
