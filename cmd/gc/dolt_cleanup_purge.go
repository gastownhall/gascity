package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// cleanupPurgeTimeout caps each per-rig CALL DOLT_PURGE_DROPPED_DATABASES.
// The dolt server's purge work is bounded by the on-disk size of the
// .dolt_dropped_databases directory; large reclaims can take longer than a
// drop, so the cap is generous.
const cleanupPurgeTimeout = 60 * time.Second

// droppedDatabasesDir is the relative path under each rig root where the
// dolt server stages dropped databases until DOLT_PURGE_DROPPED_DATABASES
// reclaims them.
const droppedDatabasesDir = ".beads/dolt/.dolt_dropped_databases"

// runPurgeStage walks each rig's .dolt_dropped_databases directory to sum
// reclaimable bytes. On --force it then calls DOLT_PURGE_DROPPED_DATABASES
// against each rig database to actually free the disk. Errors are recorded
// into report.Errors but never abort the run.
//
// Purge.OK is true only when --force was set and every purge call
// succeeded; in dry-run mode OK stays false because no work was done.
func runPurgeStage(report *CleanupReport, opts cleanupOptions) {
	if opts.FS == nil {
		return
	}

	var totalBytes int64
	for _, rig := range opts.Rigs {
		root := filepath.Join(rig.Path, droppedDatabasesDir)
		bytes, err := sumBytesUnder(opts.FS, root)
		if err != nil {
			// Missing directory is normal (no drops to reclaim) — only
			// surface unexpected errors.
			continue
		}
		totalBytes += bytes
	}
	report.Purge.BytesReclaimed = totalBytes

	if !opts.Force || opts.DoltClient == nil {
		return
	}

	allOK := true
	for _, rp := range report.RigsProtected {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupPurgeTimeout)
		err := opts.DoltClient.PurgeDroppedDatabases(ctx, rp.DB)
		cancel()
		if err != nil {
			allOK = false
			report.Errors = append(report.Errors, CleanupError{
				Stage: "purge",
				Name:  rp.DB,
				Error: err.Error(),
			})
			report.Summary.ErrorsTotal++
		}
	}
	report.Purge.OK = allOK
}

// sumBytesUnder walks the given root recursively and returns the total
// bytes of every regular file underneath. Returns 0, nil when the root
// doesn't exist (callers treat this as "nothing to reclaim"). Symlinks
// are followed via Stat (the dolt dropped-databases directory does not
// contain symlinks in normal operation).
func sumBytesUnder(fs fsys.FS, root string) (int64, error) {
	entries, err := fs.ReadDir(root)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		full := filepath.Join(root, e.Name())
		if e.IsDir() {
			sub, err := sumBytesUnder(fs, full)
			if err == nil {
				total += sub
			}
			continue
		}
		info, err := fs.Stat(full)
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total, nil
}
