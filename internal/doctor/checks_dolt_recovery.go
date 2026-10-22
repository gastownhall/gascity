package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// DoltUnboundDatabasesCheck warns when the managed dolt data dir holds a
// committed database that no scope is bound to and no usable local backup
// covers. The data dir is shared by every database in the city, and since
// v1.5.0 the backup and JSONL-export orders maintain only bound scopes, so
// an unbound database has no restore source at all: it is exactly the
// database a recovery flow that rebuilds from one scope's export would
// silently drop (#1979). The check is read-only; what to do about an
// unbound database (bind it, back it up, or drop it) is operator judgment.
type DoltUnboundDatabasesCheck struct {
	cityPath        string
	skip            bool
	applicableKnown bool
	applicable      bool
	scopeRoots      []string
}

// NewDoltUnboundDatabasesCheck creates an unbound-database recovery check.
func NewDoltUnboundDatabasesCheck(cityPath string, skip bool) *DoltUnboundDatabasesCheck {
	return &DoltUnboundDatabasesCheck{cityPath: cityPath, skip: skip}
}

// NewDoltUnboundDatabasesCheckForConfig creates an unbound-database check
// using preloaded city config.
func NewDoltUnboundDatabasesCheckForConfig(cityPath string, skip bool, cfg *config.City, cfgErr error) *DoltUnboundDatabasesCheck {
	return &DoltUnboundDatabasesCheck{
		cityPath:        cityPath,
		skip:            skip,
		applicableKnown: true,
		applicable:      ManagedLocalDoltChecksApplicableForConfig(cityPath, cfg, cfgErr),
		scopeRoots:      managedDoltScopeRootsForConfig(cityPath, cfg, cfgErr),
	}
}

func (c *DoltUnboundDatabasesCheck) managedApplicable() bool {
	if c.applicableKnown {
		return c.applicable
	}
	return managedLocalDoltChecksApplicable(c.cityPath)
}

// Name returns the check identifier.
func (c *DoltUnboundDatabasesCheck) Name() string { return "dolt-unbound-databases" }

// Run inspects the workspace's managed local Dolt databases and warns for
// each on-disk orphan that has a committed store, matches no bound scope,
// and has no usable local backup.
func (c *DoltUnboundDatabasesCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip || !c.managedApplicable() {
		r.Status = StatusOK
		r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		return r
	}

	targets, unresolved := managedLocalDoltScanTargets(c.cityPath)
	if c.applicableKnown {
		targets, unresolved = managedLocalDoltScanTargetsForScopeRoots(c.cityPath, c.scopeRoots)
	}
	if unresolved {
		// An unresolved binding might be the missing bound scope for any
		// orphan on disk; an incomplete inventory must never be reported
		// as a flock of false orphans.
		r.Status = StatusOK
		r.Message = "skipped (dolt target unresolved)"
		return r
	}

	bound := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if !target.Orphan {
			bound[normalizeDoltDatabaseName(target.Database)] = struct{}{}
		}
	}
	var unbound []string
	for _, target := range targets {
		if !target.Orphan {
			continue
		}
		db := strings.TrimSpace(target.Database)
		// isManagedDoltUserDatabase rejects system databases (__gc_probe)
		// and dotted names, which are moved-aside stores, not databases.
		if !isManagedDoltUserDatabase(db) {
			continue
		}
		if _, ok := bound[normalizeDoltDatabaseName(db)]; ok {
			continue
		}
		manifest := filepath.Join(target.ScanRoot, "noms", "manifest")
		if _, err := os.Stat(manifest); err != nil {
			if !os.IsNotExist(err) {
				r.Details = append(r.Details, fmt.Sprintf("stat %s: %v", manifest, err))
			}
			// No committed store: a phantom the phantom-db order already
			// knows how to reap.
			continue
		}
		if usableLocalBackupName(filepath.Dir(target.ScanRoot), db) != "" {
			continue
		}
		unbound = append(unbound, db)
	}

	if len(unbound) == 0 {
		r.Status = StatusOK
		r.Message = "every database in the data dir is bound or has a usable local backup"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("unbound dolt databases with no usable local backup: %s", strings.Join(unbound, ", "))
	r.FixHint = "bind the database to a rig, or register and sync a local backup via gc dolt sql (USE <db>; CALL DOLT_BACKUP('add','<db>-backup','file://…'); CALL DOLT_BACKUP('sync','<db>-backup')) — the backup order never syncs unbound databases — or drop the store; see examples/bd/dolt/docs/journal-corruption-recovery.md"
	return r
}

// CanFix returns false. Whether an unbound database is live data to bind, a
// store to back up, or a corpse to drop is operator judgment.
func (c *DoltUnboundDatabasesCheck) CanFix() bool { return false }

// Fix is a no-op. See CanFix.
func (c *DoltUnboundDatabasesCheck) Fix(_ *CheckContext) error { return nil }

// normalizeDoltDatabaseName applies dolt's '-'→'_' SQL-name normalization,
// so a scope bound as <db> matches an on-disk directory spelled with dashes.
func normalizeDoltDatabaseName(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), "-", "_")
}

// usableLocalBackupName returns the name of the backup remote registered
// for db that can actually be restored from locally: bd's "default"
// destination (what mol-dog-backup registers and keeps current) whose URL
// names db, or the legacy <db>-backup name; either must be a file:// URL to
// a non-empty directory. It mirrors select_recovery_backup in
// examples/bd/assets/scripts/gc-beads-bd.sh. A missing or unparsable
// repo_state.json reads as no usable backup — the safe direction for a
// check whose warning is "this database cannot be restored".
func usableLocalBackupName(dbDir, db string) string {
	data, err := os.ReadFile(filepath.Join(dbDir, ".dolt", "repo_state.json"))
	if err != nil {
		return ""
	}
	var state struct {
		Backups map[string]json.RawMessage `json:"backups"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return ""
	}
	for _, name := range []string{"default", db + "-backup"} {
		url := repoStateBackupURL(state.Backups, name)
		if url == "" {
			continue
		}
		// bd registers one destination per database as "default", so the
		// URL is the only place that backup's database identity appears.
		if name == "default" && !backupURLNamesDatabase(url, db) {
			continue
		}
		if hasContent, err := dirHasEntries(strings.TrimPrefix(url, "file://")); err != nil || !hasContent {
			continue
		}
		return name
	}
	return ""
}

// repoStateBackupURL reads one backup remote's URL from a repo_state.json
// backups object, handling both the object form ({"name": {"url": "…"}})
// and the legacy plain-string form.
func repoStateBackupURL(backups map[string]json.RawMessage, name string) string {
	raw, ok := backups[name]
	if !ok {
		return ""
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && strings.TrimSpace(obj.URL) != "" {
		return strings.TrimSpace(obj.URL)
	}
	var legacy string
	if err := json.Unmarshal(raw, &legacy); err == nil {
		return strings.TrimSpace(legacy)
	}
	return ""
}

// backupURLNamesDatabase reports whether a backup URL's last path element
// names db after dolt's '-'→'_' SQL-name normalization.
func backupURLNamesDatabase(url, db string) bool {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	if url == "" {
		return false
	}
	last := url
	if i := strings.LastIndex(url, "/"); i >= 0 {
		last = url[i+1:]
	}
	return normalizeDoltDatabaseName(last) == normalizeDoltDatabaseName(db)
}

// DoltCorruptAsideCheck warns while the dolt pack's state dir holds
// corrupt-aside entries — stores start-time journal-corruption recovery
// preserved under <runtime>/packs/dolt/corrupt-aside/<db>.<ts> and never
// deletes. Each entry is postmortem evidence of a replaced store (or, after
// a failed move-back, the only copy left); the warning clears when the
// operator has confirmed the live database and removed the copy. Read-only
// by design: the preserved store is the evidence.
type DoltCorruptAsideCheck struct {
	cityPath        string
	skip            bool
	applicableKnown bool
	applicable      bool
}

// NewDoltCorruptAsideCheck creates a corrupt-aside evidence check.
func NewDoltCorruptAsideCheck(cityPath string, skip bool) *DoltCorruptAsideCheck {
	return &DoltCorruptAsideCheck{cityPath: cityPath, skip: skip}
}

// NewDoltCorruptAsideCheckForConfig creates a corrupt-aside check using
// preloaded city config.
func NewDoltCorruptAsideCheckForConfig(cityPath string, skip bool, cfg *config.City, cfgErr error) *DoltCorruptAsideCheck {
	return &DoltCorruptAsideCheck{
		cityPath:        cityPath,
		skip:            skip,
		applicableKnown: true,
		applicable:      ManagedLocalDoltChecksApplicableForConfig(cityPath, cfg, cfgErr),
	}
}

func (c *DoltCorruptAsideCheck) managedApplicable() bool {
	if c.applicableKnown {
		return c.applicable
	}
	return managedLocalDoltChecksApplicable(c.cityPath)
}

// Name returns the check identifier.
func (c *DoltCorruptAsideCheck) Name() string { return "dolt-corrupt-aside" }

// Run reports the corrupt-aside entries preserved under the dolt pack's
// state dir.
func (c *DoltCorruptAsideCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip || !c.managedApplicable() {
		r.Status = StatusOK
		r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		return r
	}
	entries, err := os.ReadDir(filepath.Join(doctorDoltPackStateDir(c.cityPath), "corrupt-aside"))
	if err != nil {
		if !os.IsNotExist(err) {
			r.Status = StatusWarning
			r.Message = fmt.Sprintf("read corrupt-aside dir: %v", err)
			return r
		}
		entries = nil
	}
	if len(entries) == 0 {
		r.Status = StatusOK
		r.Message = "no corrupt-aside stores preserved"
		return r
	}
	asideDir := filepath.Join(doctorDoltPackStateDir(c.cityPath), "corrupt-aside")
	named := make([]string, 0, len(entries))
	for _, entry := range entries {
		// Start-time recovery preserves stores as <db>.<ts>; database names
		// cannot contain a dot, so the first one splits db from timestamp.
		db, ts, hasTS := strings.Cut(entry.Name(), ".")
		path := filepath.Join(asideDir, entry.Name())
		if !hasTS || db == "" || ts == "" {
			named = append(named, fmt.Sprintf("%s (unrecognized entry name) at %s", entry.Name(), path))
			continue
		}
		named = append(named, fmt.Sprintf("%s (as of %s) at %s", db, ts, path))
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("corrupt-aside holds %d preserved store(s) from start-time recovery: %s", len(entries), strings.Join(named, "; "))
	r.FixHint = "confirm the live database (gc dolt logs), then remove the aside copy — see examples/bd/dolt/docs/journal-corruption-recovery.md"
	return r
}

// CanFix returns false. The preserved store is the evidence; only the
// operator can decide the live database is confirmed and remove it.
func (c *DoltCorruptAsideCheck) CanFix() bool { return false }

// Fix is a no-op. See CanFix.
func (c *DoltCorruptAsideCheck) Fix(_ *CheckContext) error { return nil }
