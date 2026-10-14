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

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// defaultBackupFreshnessMaxAge is how stale a rig's last bd backup sync may be
// before BdBackupFreshnessCheck warns. bd's auto-backup interval is minutes, so
// a day-old (or older) last sync means the backup pipeline is disabled, broken,
// or the rig is unattended — the silent gap that turns a recoverable store loss
// into a near-permanent one when the only surviving backup is weeks stale.
const defaultBackupFreshnessMaxAge = 24 * time.Hour

// BdBackupFreshnessCheck warns when a rig that HAS a bd backup has not synced
// within maxAge. A scope with a registered Dolt destination is judged on
// .beads/dolt-backup-state.json and any other on the legacy
// .beads/backup/backup_state.json, whose finding a fresh sync in the city's
// managed destination withdraws (see scanBackupFreshness). It is the
// freshness complement to the existing backup checks: DoltBackupCheck verifies
// a backup is registered, BdBackupSizeCheck guards the backup footprint, and
// BdBackupStateCheck flags quarantines and stale registrations — none notice
// that a configured backup has simply stopped running. Reading on-disk state
// rather than querying the store keeps the check DB-free.
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

// Run reads each scope's active backup state (see scanBackupFreshness) and
// warns on any whose last sync is older than maxAge (or whose timestamp is
// missing or unparseable), unless a sync of the scope completed within maxAge
// in the city's managed Dolt destination withdraws a finding on its legacy
// state file (see managedDoltBackupIsFresh). Scopes with no backup state are
// skipped — "no backup at all" is reported by DoltBackupCheck /
// BdBackupSizeCheck, not here.
func (c *BdBackupFreshnessCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	now := c.now()

	var findings []string
	for _, target := range c.freshnessScanTargets() {
		if finding, ok := scanBackupFreshness(c.cityPath, target, now, c.maxAge); ok {
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
	Label     string
	ScopeRoot string
	BeadsDir  string
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
			Label:     bdBackupScopeLabel(c.cityPath, scopeRoot),
			ScopeRoot: scopeRoot,
			BeadsDir:  filepath.Join(scopeRoot, ".beads"),
		})
	}
	return targets
}

// BulkDeleteSafe reports whether it is safe to perform a bulk bead deletion
// given the current backup freshness across all managed scopes. It returns
// safe=false and a human-readable reason as soon as one managed scope's ACTIVE
// backup pipeline is not demonstrably current.
//
// Which pipeline is "active" per scope, and therefore which state file decides
// freshness, is scanBackupFreshness's judgement — this gate deliberately does
// not re-derive it, so the gate and BdBackupFreshnessCheck can never disagree
// about whether a scope is protected. Concretely that means a scope with a
// registered Dolt destination is judged on its Dolt sync state (including the
// registered-but-never-synced case, which is unsafe), a scope that never
// migrated is judged on the legacy embedded-store state, and only a fresh sync
// of that scope in the city's managed destination waives a legacy verdict.
//
// The gate is fail-closed on doubt: an unreadable, unparseable, or
// timestamp-less state file blocks the deletion rather than being ignored,
// because it leaves the recovery point unknown. There are two deliberate
// exceptions. A scope with NO backup state at all is treated as safe — "no
// backup configured" is DoltBackupCheck's concern, and failing closed there
// would block bulk deletion on every unbacked city. And doubt about a scope's
// legacy state file does not block when managedDoltBackupIsFresh proves a sync
// of the scope within maxAge: that sync is a known recovery point, whatever the
// legacy file holds.
//
// maxAge is used as given and is not clamped, so a non-positive value reads
// every scope as stale and blocks every deletion.
func BulkDeleteSafe(cityPath string, cfg *config.City, maxAge time.Duration, now time.Time) (bool, string) {
	check := NewBdBackupFreshnessCheckForConfig(cityPath, cfg, nil)
	if cfg == nil {
		// No config in hand: discover scopes from disk, the same fallback the
		// check uses when city.toml fails to load. Silently narrowing to the
		// city root here would leave every rig unscanned and fail this gate
		// OPEN — the one direction a delete gate must never fail.
		check = NewBdBackupFreshnessCheckForScopeRoots(cityPath, managedDoltScopeRoots(cityPath), maxAge, nil)
	}
	for _, target := range check.freshnessScanTargets() {
		if finding, ok := scanBackupFreshness(cityPath, target, now, maxAge); ok {
			return false, finding
		}
	}
	return true, ""
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
// One more writer backs a scope up without touching either file until it has
// registered the scope: mol-dog-backup syncs the city's managed destination,
// <city>/.dolt-backup/<db>. It registers each scope it reaches (bd backup init,
// then bd backup sync), after which the registration above decides. A scope it
// has not registered is still in the frozen-legacy shape — during the window
// before its first run after an upgrade, under a dolt pack that predates the
// registration step, or when bd failed to save the registration. Its database
// is being synced while its legacy file stays frozen, so it reads as the same
// unclearable warning and, because BulkDeleteSafe shares this function, blocks
// order-tracking retention on a city whose backup is current.
//
// The managed destination may therefore withdraw a legacy finding, and do
// nothing else. It never raises a finding of its own: when it is stale, absent,
// unreadable, or not this scope's to judge, the legacy verdict stands exactly as
// it would without it. Its own staleness is for the dolt pack's health probe and
// mol-dog-doctor to report against the backup order's interval; here it answers
// only whether the scope holds a recovery point within maxAge, the horizon this
// check and BulkDeleteSafe apply to every store.
//
// So: prefer the Dolt backup state whenever a Dolt destination is registered,
// and fall back to the legacy file only for scopes that never migrated, unless a
// fresh managed sync shows that file is merely frozen. Each finding names the
// store it describes, so the reader is never left guessing which of the two a
// message is about. A scope with neither file returns ("", false) — "no backup
// at all" is DoltBackupCheck's job, not this one's.
func scanBackupFreshness(cityPath string, target bdBackupFreshnessTarget, now time.Time, maxAge time.Duration) (string, bool) {
	if _, err := os.Stat(filepath.Join(target.BeadsDir, "dolt-backup.json")); err == nil {
		return scanDoltBackupFreshness(target.Label, target.BeadsDir, now, maxAge)
	}
	finding, isFinding := scanLegacyBackupFreshness(target.Label, target.BeadsDir, now, maxAge)
	if isFinding && managedDoltBackupIsFresh(cityPath, target, now, maxAge) {
		return "", false
	}
	return finding, isFinding
}

// managedDoltBackupIsFresh reports whether the city's managed Dolt backup
// destination holds a sync of the scope's database that completed within
// maxAge. Every condition must be proven; any doubt answers false, which leaves
// the caller's legacy verdict in force.
//
// The database is the one the scope's metadata.json names, and it must pass
// isManagedDoltUserDatabase: an unnamed database is not guessed at, since a
// wrong guess would read another scope's backup, and a name outside that class
// could join a path that escapes .dolt-backup.
//
// Completion is dated by <db>/manifest alone, as the dolt pack's health probe
// dates it. `dolt backup sync` writes chunk files first and adopts them by
// rewriting the manifest last, so a chunk newer than the manifest belongs to a
// sync that never finished, and chunks with no manifest are no backup at all.
// DoltBackupCheck reads the same directory only for presence, which says
// nothing about when the last sync completed.
//
// The directory is evidence only where DoltBackupCheck would read it: not for a
// scope bd owns, and not for one whose endpoint resolves as external, since
// both keep their backups outside gc's managed destination. A resolution error
// is doubt and answers false. The common one is a stopped city: resolving a
// scope on gc's managed server needs that server's live runtime state, and
// without it the resolver returns contract.ErrManagedRuntimeUnavailable, so the
// legacy finding stands until the city is back up. DoltBackupCheck reads the
// directory on the same error, but only for presence; withdrawing a finding
// needs the scope proven to back up there.
func managedDoltBackupIsFresh(cityPath string, target bdBackupFreshnessTarget, now time.Time, maxAge time.Duration) bool {
	if strings.TrimSpace(cityPath) == "" {
		return false
	}
	dbName := scopeDoltDatabaseName(target.BeadsDir)
	if !isManagedDoltUserDatabase(dbName) {
		return false
	}
	info, err := os.Stat(filepath.Join(cityPath, ".dolt-backup", dbName, "manifest"))
	if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) > maxAge {
		return false
	}
	if scopeIsProviderOwned(cityPath, target.ScopeRoot) {
		return false
	}
	resolved, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, target.ScopeRoot)
	return err == nil && !resolved.External
}

// scopeDoltDatabaseName returns the Dolt database a scope's metadata.json names,
// or "" when the file is absent, unparseable, or carries no name.
func scopeDoltDatabaseName(beadsDir string) string {
	data, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return ""
	}
	return strings.TrimSpace(meta.DoltDatabase)
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
