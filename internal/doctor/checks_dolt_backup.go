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

// DoltBackupCheck verifies that a rig's Dolt database has a backup remote
// configured. `gc rig add` provisions the rig but does not register a
// backup; without a backup remote, mol-dog backup automation silently treats
// the database as ineligible. This check catches that gap up-front in
// `gc doctor`.
//
// Two signals satisfy the check; either is sufficient:
//
//   - Filesystem: <city>/.dolt-backup/<db>/ exists with backup contents.
//     mol-dog-backup syncs here, so populated contents are evidence that
//     a sync has run.
//   - Repo state: <managed-dolt-data-dir>/<db>/.dolt/repo_state.json
//     contains a backup entry named <db>-backup. This is the
//     post-registration, pre-sync state.
//
// When both signals are absent the check emits StatusWarning with the
// exact copy-pasteable invocation needed to register and sync the
// backup. We deliberately do NOT auto-fix: backup destination is
// operator policy (local fs vs S3 vs B2 etc.) and a one-way door.
//
// The check is intended to be registered per non-suspended rig; the
// caller in cmd_doctor.go skips suspended rigs before constructing this
// check.
type DoltBackupCheck struct {
	cityPath    string
	rig         config.Rig
	doltDataDir string
}

// NewDoltBackupCheck creates a per-rig dolt-backup registration check.
func NewDoltBackupCheck(cityPath string, rig config.Rig, doltDataDir string) *DoltBackupCheck {
	if strings.TrimSpace(doltDataDir) == "" {
		doltDataDir = filepath.Join(cityPath, ".beads", "dolt")
	}
	return &DoltBackupCheck{cityPath: cityPath, rig: rig, doltDataDir: doltDataDir}
}

// Name returns the check identifier ("rig:<name>:dolt-backup").
func (c *DoltBackupCheck) Name() string {
	return "rig:" + c.rig.Name + ":dolt-backup"
}

// Run executes the check.
func (c *DoltBackupCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	rigPath := c.normalizedRigPath()
	dbName, details := c.resolveDBName(rigPath)
	r.Details = append(r.Details, details...)
	backupDir := filepath.Join(c.cityPath, ".dolt-backup", dbName)

	// Signal 1: backup directory exists on disk with contents.
	backupDirHasContent, err := dirHasEntries(backupDir)
	switch {
	case err != nil:
		r.Details = append(r.Details, fmt.Sprintf("read backup dir: %v", err))
	case backupDirHasContent:
		r.Status = StatusOK
		r.Message = fmt.Sprintf("backup artifact dir present (assumed previously synced): %s", backupDir)
		return r
	}

	// Signal 2: backup remote is registered in repo_state.json.
	registered, err := backupRemoteRegistered(c.doltDataDir, dbName)
	switch {
	case err != nil:
		// Treat read errors as "not registered" but record the cause in
		// Details for verbose runs. We still want the warning + fix
		// command to reach the operator.
		r.Details = append(r.Details, fmt.Sprintf("read repo_state.json: %v", err))
	case registered:
		r.Status = StatusOK
		r.Message = fmt.Sprintf("backup remote %q registered (sync pending)", dbName+"-backup")
		return r
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("rig %q: no dolt backup registered (expected %s)", c.rig.Name, backupDir)
	r.FixHint = doltBackupFixHint(dbName, backupDir)
	return r
}

// CanFix returns false. Registering a backup destination is operator
// policy (local fs vs cloud bucket vs offsite); auto-creating a local
// backup would silently bypass that decision.
func (c *DoltBackupCheck) CanFix() bool { return false }

// Fix is a no-op. See CanFix.
func (c *DoltBackupCheck) Fix(_ *CheckContext) error { return nil }

func (c *DoltBackupCheck) normalizedRigPath() string {
	rigPath := c.rig.Path
	if !filepath.IsAbs(rigPath) {
		rigPath = filepath.Join(c.cityPath, rigPath)
	}
	return rigPath
}

// resolveDBName returns the rig's Dolt database name from
// .beads/metadata.json, falling back to rig.Name when the metadata is
// missing or unreadable. Falling back preserves a useful warning even
// for rigs whose metadata never landed — the operator can correct the
// db name in the suggested command if needed.
func (c *DoltBackupCheck) resolveDBName(rigPath string) (string, []string) {
	metadataPath := filepath.Join(rigPath, ".beads", "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return c.rig.Name, nil
		}
		return c.rig.Name, []string{fmt.Sprintf("read metadata.json: %v; using rig name %q", err, c.rig.Name)}
	}
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return c.rig.Name, []string{fmt.Sprintf("parse metadata.json: %v; using rig name %q", err, c.rig.Name)}
	}
	if s := strings.TrimSpace(meta.DoltDatabase); s != "" {
		return s, nil
	}
	return c.rig.Name, nil
}

// backupRemoteRegistered reports whether
// <managed-dolt-data-dir>/<db>/.dolt/repo_state.json declares a backup remote
// named "<db>-backup". A missing file returns (false, nil) — that is the
// expected state for a freshly-provisioned rig and not itself an error.
func backupRemoteRegistered(doltDataDir, dbName string) (bool, error) {
	statePath := filepath.Join(doltDataDir, dbName, ".dolt", "repo_state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	var state struct {
		Backups map[string]json.RawMessage `json:"backups"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return false, fmt.Errorf("parse %s: %w", statePath, err)
	}
	_, ok := state.Backups[dbName+"-backup"]
	return ok, nil
}

func dirHasEntries(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// doltBackupFixHint returns the multi-line DOLT_BACKUP add+sync
// invocation as a copy-pasteable shell command. The command targets the
// running managed Dolt server (port comes from $GC_DOLT_PORT, which
// `gc dolt status` surfaces); it does not assume the operator has
// stopped the server.
func doltBackupFixHint(dbName, backupDir string) string {
	return fmt.Sprintf(
		"register the backup remote (requires GC_DOLT_PORT from `gc dolt status`):\n"+
			"  DOLT_CLI_PASSWORD='' dolt --host 127.0.0.1 --port ${GC_DOLT_PORT:?set this via gc dolt status} --user root --no-tls sql -q \\\n"+
			"    \"USE \\`%s\\`; \\\n"+
			"     CALL DOLT_BACKUP('add', '%s-backup', 'file://%s'); \\\n"+
			"     CALL DOLT_BACKUP('sync', '%s-backup');\"",
		dbName, dbName, backupDir, dbName,
	)
}
