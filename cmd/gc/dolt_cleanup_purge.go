package main

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"path/filepath"
	"strconv"
	"strings"
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
	if opts.Force && hasRigProtectionError(report) {
		return
	}

	var totalBytes int64
	bytesByRigDB := map[string]int64{}
	eligibleRigDBs := map[string]bool{}
	for _, rig := range opts.Rigs {
		if !rigSharesResolvedDoltServer(rig, opts) {
			continue
		}
		root := filepath.Join(rig.Path, droppedDatabasesDir)
		bytes, err := sumBytesUnder(opts.FS, root)
		if err != nil {
			recordCleanupError(report, "purge", root, err)
			continue
		}
		totalBytes += bytes
		dbName := rigDoltDatabaseName(rig, opts.FS)
		bytesByRigDB[dbName] += bytes
		eligibleRigDBs[dbName] = true
	}

	if !opts.Force {
		report.Purge.BytesReclaimed = totalBytes
		return
	}
	if opts.DoltClient == nil {
		if opts.DoltClientOpenErr != nil {
			recordCleanupError(report, "purge", "", opts.DoltClientOpenErr)
		}
		return
	}

	listCtx, listCancel := context.WithTimeout(context.Background(), cleanupListTimeout)
	liveDBs, err := opts.DoltClient.ListDatabases(listCtx)
	listCancel()
	if err != nil {
		report.Errors = append(report.Errors, CleanupError{Stage: "purge", Error: err.Error()})
		report.Summary.ErrorsTotal++
		return
	}
	live := make(map[string]bool, len(liveDBs))
	for _, name := range liveDBs {
		live[name] = true
	}

	allOK := true
	var reclaimedBytes int64
	for _, rp := range report.RigsProtected {
		if !eligibleRigDBs[rp.DB] {
			continue
		}
		if !live[rp.DB] {
			if bytesByRigDB[rp.DB] > 0 {
				allOK = false
				recordCleanupError(
					report,
					"purge",
					rp.DB,
					fmt.Errorf("database not live with %d reclaimable dropped-database bytes", bytesByRigDB[rp.DB]),
				)
			}
			continue
		}
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
			continue
		}
		reclaimedBytes += bytesByRigDB[rp.DB]
	}
	report.Purge.BytesReclaimed = reclaimedBytes
	report.Purge.OK = allOK
}

func rigSharesResolvedDoltServer(rig resolverRig, opts cleanupOptions) bool {
	if opts.PortResolution.Port <= 0 || opts.FS == nil {
		return true
	}
	port, state := rigPortFileValue(rig, opts.FS)
	switch state {
	case rigPortFileValid:
		return port == opts.PortResolution.Port
	case rigPortFileMissing:
		return opts.PortResolution.Fallback || cityConfigPortSelectsRig(rig, opts)
	default:
		return false
	}
}

func cityConfigPortSelectsRig(_ resolverRig, opts cleanupOptions) bool {
	return opts.PortResolution.Source == cityConfigDoltPortSource &&
		opts.CityPort == opts.PortResolution.Port
}

type rigPortFileState int

const (
	rigPortFileMissing rigPortFileState = iota
	rigPortFileInvalid
	rigPortFileValid
)

func rigPortFileValue(rig resolverRig, fs fsys.FS) (int, rigPortFileState) {
	data, err := fs.ReadFile(filepath.Join(rig.Path, ".beads", "dolt-server.port"))
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return 0, rigPortFileMissing
		}
		return 0, rigPortFileInvalid
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return 0, rigPortFileInvalid
	}
	port, err := strconv.Atoi(text)
	if err != nil || !validDoltPort(port) {
		return 0, rigPortFileInvalid
	}
	return port, rigPortFileValid
}

// sumBytesUnder walks the given root recursively and returns the total
// bytes of every regular file underneath. Returns 0, nil when the root
// doesn't exist (callers treat this as "nothing to reclaim"). Symlinks
// are followed via Stat (the dolt dropped-databases directory does not
// contain symlinks in normal operation).
func sumBytesUnder(fs fsys.FS, root string) (int64, error) {
	return sumBytesUnderPath(fs, root, true)
}

func sumBytesUnderPath(fs fsys.FS, root string, allowMissingRoot bool) (int64, error) {
	entries, err := fs.ReadDir(root)
	if err != nil {
		if allowMissingRoot && errors.Is(err, iofs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", root, err)
	}
	var total int64
	for _, e := range entries {
		full := filepath.Join(root, e.Name())
		if e.IsDir() {
			sub, err := sumBytesUnderPath(fs, full, false)
			if err != nil {
				return 0, err
			}
			total += sub
			continue
		}
		info, err := fs.Stat(full)
		if err != nil {
			return 0, fmt.Errorf("stat %s: %w", full, err)
		}
		total += info.Size()
	}
	return total, nil
}
