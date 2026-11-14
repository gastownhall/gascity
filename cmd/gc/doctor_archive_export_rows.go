package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// archiveExportRowsFor returns a lookup that reports how many rows a rig's
// durable archived export holds. It is injected into RigDataPresenceCheck so
// that check does not have to learn where a pack keeps its state.
//
// Why a second oracle exists at all: on a gc-managed scope the local
// .beads/issues.jsonl is deliberately removed (reapStaleBdExportJSONL, and
// again after EnsureCanonicalConfig writes export.auto=false), because the
// re-import cycle stalls bd writes on large datasets. That is the exact
// configuration the data-presence check targets, so without a durable source
// its blocking branch can never fire.
//
// The archive repo is maintained by the core pack's jsonl-export script as a
// git working tree, one directory per database, and is not reaped. Reading the
// checked-out file is enough — no git invocation, so a doctor run stays cheap
// and cannot hang on a repo lock.
func archiveExportRowsFor(cityPath string) func(rigPath string) (int, bool, error) {
	return func(rigPath string) (int, bool, error) {
		metadataPath := scopeMetadataJSONPath(rigPath)
		db, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, metadataPath)
		if err != nil {
			return 0, false, fmt.Errorf("reading the rig's database name from %s: %w", metadataPath, err)
		}
		if !ok || strings.TrimSpace(db) == "" {
			return 0, false, nil
		}
		repo := activeJSONLArchiveRepo(os.Getenv, cityPath)
		if repo == "" {
			return 0, false, nil
		}
		// The per-database directory first, then the flattened mirror the
		// export script also writes. The first file that exists is the
		// evidence, read or not: falling through to the other on a read error
		// would hide the error.
		for _, candidate := range []string{
			filepath.Join(repo, db, "issues.jsonl"),
			filepath.Join(repo, db+".jsonl"),
		} {
			n, err := doctor.CountJSONLLines(candidate)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return 0, false, fmt.Errorf("reading %s: %w", candidate, err)
			}
			return n, true, nil
		}
		return 0, false, nil
	}
}

// activeJSONLArchiveRepo returns the archive repo the core pack's
// jsonl-export script writes to, or "" when there is none yet: the
// GC_JSONL_ARCHIVE_REPO override if set, otherwise the first candidate that is
// a git repository. Only that repo is evidence. A stale legacy archive beside
// the current one must not supply rows the exporter no longer writes.
func activeJSONLArchiveRepo(getenv func(string) string, cityPath string) string {
	if override := strings.TrimSpace(getenv("GC_JSONL_ARCHIVE_REPO")); override != "" {
		return override
	}
	for _, dir := range jsonlArchiveRepoCandidates(getenv, cityPath) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
	}
	return ""
}

// jsonlArchiveRepoCandidates mirrors the archive-repo precedence in the core
// pack's jsonl-export script, most current first. An explicit
// GC_JSONL_ARCHIVE_REPO override replaces the search entirely. Kept in this
// package because it encodes pack layout, which internal/doctor must not
// depend on, and shared with jsonlArchiveDoctorCheck.resolveArchiveRepo so the
// two readers of the archive cannot drift from the script or each other.
//
// getenv is passed in rather than read directly so callers that already own an
// env seam (the doctor check) keep using it.
func jsonlArchiveRepoCandidates(getenv func(string) string, cityPath string) []string {
	if override := strings.TrimSpace(getenv("GC_JSONL_ARCHIVE_REPO")); override != "" {
		return []string{override}
	}
	runtime := strings.TrimSpace(getenv("GC_CITY_RUNTIME_DIR"))
	if runtime == "" {
		runtime = filepath.Join(cityPath, ".gc", "runtime")
	}
	base := strings.TrimSpace(getenv("GC_PACK_STATE_DIR"))
	if base == "" {
		base = filepath.Join(runtime, "packs", "core")
	}
	return []string{
		filepath.Join(base, "jsonl-archive"),
		// Legacy locations, still present on cities that predate the core pack
		// split. Checked after the current path so a live archive always wins.
		filepath.Join(runtime, "packs", "maintenance", "jsonl-archive"),
		filepath.Join(cityPath, ".gc", "jsonl-archive"),
	}
}
