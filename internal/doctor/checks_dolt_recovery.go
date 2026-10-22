package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// doltRecoveryChecksSkippedMessage is reported where gc does not manage the
// local Dolt data dir, so neither recovery check applies.
const doltRecoveryChecksSkippedMessage = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"

// DoltUnboundDatabasesCheck warns about databases in the managed Dolt data
// dir that no city or rig scope binds and that have no usable local backup.
// mol-dog-backup and jsonl-export cover only bound scopes, so such a database
// has no restore source: start-time journal recovery fails closed for it, and
// anything that replaces the shared data dir loses it (#1979).
type DoltUnboundDatabasesCheck struct {
	cityPath   string
	skip       bool
	applicable bool
	scopeRoots []string
}

// NewDoltUnboundDatabasesCheckForConfig creates the unbound-database check
// using preloaded city config.
func NewDoltUnboundDatabasesCheckForConfig(cityPath string, skip bool, cfg *config.City, cfgErr error) *DoltUnboundDatabasesCheck {
	return &DoltUnboundDatabasesCheck{
		cityPath:   cityPath,
		skip:       skip,
		applicable: ManagedLocalDoltChecksApplicableForConfig(cityPath, cfg, cfgErr),
		scopeRoots: managedDoltScopeRootsForConfig(cityPath, cfg, cfgErr),
	}
}

// Name returns the check identifier.
func (c *DoltUnboundDatabasesCheck) Name() string { return "dolt-unbound-databases" }

// Run lists committed databases in the managed data dir that no scope binds
// and that no usable local backup covers.
func (c *DoltUnboundDatabasesCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip || !c.applicable {
		r.Status = StatusOK
		r.Message = doltRecoveryChecksSkippedMessage
		return r
	}
	targets, unresolved := managedLocalDoltScanTargetsForScopeRoots(c.cityPath, c.scopeRoots)
	if unresolved {
		// A scope whose database cannot be read would make its own database
		// look unbound; the beads-store checks report the resolution error.
		r.Status = StatusOK
		r.Message = "skipped (dolt target unresolved)"
		return r
	}

	bound := map[string]bool{}
	for _, target := range targets {
		if !target.Orphan {
			bound[normalizeDoltDatabaseName(target.Database)] = true
		}
	}
	dataDir := resolveManagedDoltDataDir(c.cityPath)
	var unbacked []string
	for _, target := range targets {
		db := target.Database
		if !target.Orphan || bound[normalizeDoltDatabaseName(db)] || !isManagedDoltUserDatabase(db) {
			continue
		}
		if _, err := os.Stat(filepath.Join(target.ScanRoot, "noms", "manifest")); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			r.Status = StatusWarning
			r.Message = fmt.Sprintf("inspect unbound database %s: %v", db, err)
			return r
		}
		name, err := usableLocalDoltBackup(dataDir, db)
		if err != nil {
			r.Status = StatusWarning
			r.Message = fmt.Sprintf("inspect backups of unbound database %s: %v", db, err)
			return r
		}
		if name == "" {
			unbacked = append(unbacked, db)
		}
	}
	if len(unbacked) == 0 {
		r.Status = StatusOK
		r.Message = "every database in the managed data dir is bound to a scope or has a local backup"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("unbound databases with no usable local backup in %s: %s (no backup or JSONL export covers them; journal corruption or loss of the data dir loses them)",
		dataDir, strings.Join(unbacked, ", "))
	r.FixHint = "bind each database to a rig, remove it if it is unused, or keep a local backup of it; to " +
		doltBackupFixHint("<db>", "<backup-dir>/<db>")
	return r
}

// CanFix returns false — binding, backing up or removing a database is the
// operator's call.
func (c *DoltUnboundDatabasesCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *DoltUnboundDatabasesCheck) Fix(_ *CheckContext) error { return nil }

// normalizeDoltDatabaseName maps a database name to the form Dolt stores on
// disk, which replaces '-' with '_'.
func normalizeDoltDatabaseName(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), "-", "_")
}

// usableLocalDoltBackup returns the name of a backup registered in
// <dataDir>/<db>/.dolt/repo_state.json that start-time recovery can restore
// db from: bd's "default" backup when its URL names db, else the legacy
// "<db>-backup", each a file:// URL of a non-empty directory. It returns ""
// when neither is usable; a missing repo_state.json is not an error.
func usableLocalDoltBackup(dataDir, db string) (string, error) {
	statePath := filepath.Join(dataDir, db, ".dolt", "repo_state.json")
	data, err := os.ReadFile(statePath) //nolint:gosec // path is derived from managed city layout
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	var state struct {
		Backups map[string]struct {
			URL string `json:"url"`
		} `json:"backups"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return "", fmt.Errorf("parse %s: %w", statePath, err)
	}
	for _, name := range []string{"default", db + "-backup"} {
		backup, ok := state.Backups[name]
		if !ok || !strings.HasPrefix(backup.URL, "file://") {
			continue
		}
		dir := strings.TrimPrefix(backup.URL, "file://")
		if name == "default" && normalizeDoltDatabaseName(filepath.Base(dir)) != normalizeDoltDatabaseName(db) {
			continue
		}
		populated, err := dirHasEntries(dir)
		if err != nil {
			return "", err
		}
		if populated {
			return name, nil
		}
	}
	return "", nil
}

// DoltCorruptAsideCheck reports the pre-corruption stores start-time journal
// recovery kept under <dolt pack state>/corrupt-aside/<db>.<timestamp>. Each
// one marks a database that was restored from a backup and is missing the
// writes made after that backup's last sync.
type DoltCorruptAsideCheck struct {
	cityPath   string
	skip       bool
	applicable bool
}

// NewDoltCorruptAsideCheckForConfig creates the corrupt-aside check using
// preloaded city config.
func NewDoltCorruptAsideCheckForConfig(cityPath string, skip bool, cfg *config.City, cfgErr error) *DoltCorruptAsideCheck {
	return &DoltCorruptAsideCheck{
		cityPath:   cityPath,
		skip:       skip,
		applicable: ManagedLocalDoltChecksApplicableForConfig(cityPath, cfg, cfgErr),
	}
}

// Name returns the check identifier.
func (c *DoltCorruptAsideCheck) Name() string { return "dolt-corrupt-aside" }

// Run lists the stores kept aside by journal recovery.
func (c *DoltCorruptAsideCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip || !c.applicable {
		r.Status = StatusOK
		r.Message = doltRecoveryChecksSkippedMessage
		return r
	}
	asideRoot := filepath.Join(doctorDoltPackStateDir(c.cityPath), "corrupt-aside")
	entries, err := os.ReadDir(asideRoot)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("read %s: %v", asideRoot, err)
		return r
	}
	var copies []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		db, ts, _ := strings.Cut(entry.Name(), ".")
		copies = append(copies, fmt.Sprintf("%s (moved aside %s) at %s", db, ts, filepath.Join(asideRoot, entry.Name())))
	}
	if len(copies) == 0 {
		r.Status = StatusOK
		r.Message = "no corrupt stores kept aside by journal recovery"
		return r
	}
	r.Status = StatusWarning
	r.Message = "journal recovery restored databases from backup; writes after each backup's last sync are not in the restored copy: " +
		strings.Join(copies, "; ")
	r.FixHint = "review each kept copy (the pre-corruption store) and recover any writes the restored database lacks, then remove the copy; see `gc dolt logs` for the RESTORED line"
	return r
}

// CanFix returns false — whether a kept copy still holds needed writes is the
// operator's call.
func (c *DoltCorruptAsideCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *DoltCorruptAsideCheck) Fix(_ *CheckContext) error { return nil }
