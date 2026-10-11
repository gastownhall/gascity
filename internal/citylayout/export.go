package citylayout

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// BeadsRoot is the per-scope beads directory (city root or rig root).
const BeadsRoot = ".beads"

// defaultWorkDirExportExcludes lists orchestrator-local state that a runtime
// must not copy out of a session's work dir. Deployment artifacts sessions
// read at start (.gc/settings.json, .gc/site.toml, .gc/cache, .gc/system,
// .gc/scripts, .gc/tmp snapshots, .beads/metadata.json, .beads/config.yaml,
// .beads/http_target.json, .beads/redirect) are deliberately absent.
//
// Roots this package names are built from its own constants, and
// TestEveryCitylayoutRuntimeRootIsClassified fails when a new .gc/ root here
// is neither excluded nor deliberately kept.
var defaultWorkDirExportExcludes = []string{
	// Orchestrator runtime roots named by this package.
	RuntimeDataRoot + "/**",
	RuntimeServicesRoot + "/**",
	RuntimeRoot + "/" + SessionDiagnosticsRoot + "/**",
	path.Dir(ACPTranscriptsRoot) + "/**",
	RuntimeRoot + "/" + sessionNameLocksRoot + "/**",
	RuntimeRoot + "/" + gateSlotsRoot + "/**",

	// Secrets and credentials.
	".gc/secrets/**",
	".gc/controller.token",
	".gc/credentials.toml",
	".gc/mcp-adopted/**",

	// Sockets, pid files, and locks.
	".gc/**/*.sock",
	".gc/**/*.pid",
	".gc/*.lock",

	// Orchestrator logs, queues, stores, and per-session state.
	".gc/events.jsonl*",
	".gc/usage.jsonl*",
	".gc/nudges/**",
	".gc/store/**",
	".gc/storage/**",
	".gc/infra/**",
	".gc/dolt-data/**",
	".gc/session-mcp/**",
	".gc/supervisor/**",
	".gc/worktrees/**",
	".gc/emergency/**",
	".gc/storage-served-binding.json",
	".gc/storage-infra-cleared.json",

	// Ledger copies: the file provider's store in every scope and the
	// legacy JSONL archive.
	"**/.gc/beads.json",
	".gc/jsonl-archive/**",
	".gc/jsonl-export-state.json",

	// bd stores, backups, and copies of them, in every scope.
	"**/.beads/dolt/**",
	"**/.beads/doltlite/**",
	"**/.beads/embeddeddolt/**",
	"**/.beads/dolt-backups/**",
	"**/.beads/*.corrupt.backup/**",
	"**/.beads/backup/**",
	"**/.beads/export-state*",
	"**/.beads/dolt-pprof/**",
	"**/.beads/*.db*",
	"**/.beads/db.sqlite",
	"**/.beads/*.sqlite3*",

	// bd server, proxy, daemon, lock, and per-machine sync state.
	"**/.beads/*.lock",
	"**/.beads/.exclusive-lock",
	"**/.beads/dolt-server*",
	"**/.beads/proxy*",
	"**/.beads/proxied*",
	"**/.beads/daemon.*",
	"**/.beads/bd.sock*",
	"**/.beads/.env",
	"**/.beads/.beads-credential-key",
	"**/.beads/sync-state.json",
	"**/.beads/push-state.json",
	"**/.beads/last-touched",
	"**/.beads/.local_version",
	"**/.beads/last_pull",

	// Managed-Dolt ownership handoff state.
	"**/.beads/ownership-handoff.json",
	"**/.beads/dolt-handoff-*",
}

// WorkDirExportExcludes returns the paths, relative to workDir, that a
// runtime which copies the work dir somewhere else (a remote sandbox, a
// container image) must leave behind. It returns nil unless workDir holds a
// .gc/ runtime root or a .beads/ directory, so an ordinary checkout gets no
// list.
//
// The list is a floor, not the complete set: a consumer must skip every
// matching path and may skip more. [MatchWorkDirExclude] documents the
// pattern grammar.
//
// The returned slice is a fresh copy the caller may modify.
func WorkDirExportExcludes(workDir string) []string {
	if !HasRuntimeRoot(workDir) && !hasDir(workDir, BeadsRoot) {
		return nil
	}
	return append([]string(nil), defaultWorkDirExportExcludes...)
}

// MatchWorkDirExclude reports whether the work-dir-relative path rel is
// excluded by pattern.
//
// Patterns and paths are slash-separated and relative to the work dir. A
// segment of exactly "**" matches zero or more whole path segments; every
// other segment is a [path.Match] glob that never crosses a "/" ("*" matches
// any run of characters in one segment, leading dots included). A path is
// excluded when it, or any directory above it, matches, so a trailing "/**"
// spells out that a whole subtree stays behind. This is not gitignore, rsync,
// or tar syntax: a pattern with no "/" matches only at the work-dir root.
//
// It returns an error for a malformed pattern segment.
func MatchWorkDirExclude(pattern, rel string) (bool, error) {
	pat := strings.Split(pattern, "/")
	segs := strings.Split(rel, "/")
	for n := 1; n <= len(segs); n++ {
		ok, err := matchExcludeSegments(pat, segs[:n])
		if err != nil {
			return false, fmt.Errorf("work-dir exclude pattern %q: %w", pattern, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func matchExcludeSegments(pat, segs []string) (bool, error) {
	if len(pat) == 0 {
		return len(segs) == 0, nil
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			ok, err := matchExcludeSegments(pat[1:], segs[i:])
			if err != nil || ok {
				return ok, err
			}
		}
		return false, nil
	}
	if len(segs) == 0 {
		// Still validate the remaining segments so a malformed pattern is
		// reported regardless of which path it is first tried against.
		for _, p := range pat {
			if _, err := path.Match(p, ""); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false, err
	}
	return matchExcludeSegments(pat[1:], segs[1:])
}

// WorkDirExportExcluded reports whether rel is excluded by any of patterns.
func WorkDirExportExcluded(patterns []string, rel string) (bool, error) {
	for _, p := range patterns {
		ok, err := MatchWorkDirExclude(p, rel)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// WorkDirExportEntries walks workDir and returns, in lexical order, the
// slash-separated work-dir-relative path of every file, directory, and
// symlink that survives [WorkDirExportExcludes]. An excluded directory is
// pruned with everything below it. Symlinks are listed, never followed.
//
// Directories are listed as well as their contents so empty directories
// survive; a consumer must therefore not recurse into listed directories
// (tar --no-recursion, or rsync --files-from without -r).
func WorkDirExportEntries(workDir string) ([]string, error) {
	if workDir == "" {
		return nil, fmt.Errorf("work-dir export: empty work dir")
	}
	fi, err := os.Stat(workDir)
	if err != nil {
		return nil, fmt.Errorf("work-dir export: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("work-dir export: %s is not a directory", workDir)
	}
	excludes := WorkDirExportExcludes(workDir)
	var entries []string
	err = filepath.WalkDir(workDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == workDir {
			return nil
		}
		rel, err := filepath.Rel(workDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		excluded, err := WorkDirExportExcluded(excludes, rel)
		if err != nil {
			return err
		}
		if excluded {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entries = append(entries, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("work-dir export %s: %w", workDir, err)
	}
	return entries, nil
}

func hasDir(dir, name string) bool {
	if dir == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, name))
	return err == nil && fi.IsDir()
}
