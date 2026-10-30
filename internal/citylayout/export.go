package citylayout

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ControlPlaneDescriptorPath is the worker-site descriptor `gc connect` writes,
// relative to the city root. Its presence makes the directory a worker site, so
// it never leaves the box it was written in: a work dir exported from a box
// must not turn its destination into one.
const ControlPlaneDescriptorPath = RuntimeRoot + "/control-plane.toml"

// defaultWorkDirExportExcludes lists orchestrator-local state that a runtime
// must not copy out of a session's work dir. Deployment artifacts sessions
// read at start (.gc/settings.json, .gc/site.toml, .gc/cache, .gc/system,
// .gc/scripts, .gc/tmp snapshots, .beads/metadata.json, .beads/config.yaml,
// .beads/http_target.json, .beads/redirect) are deliberately absent.
//
// Roots this package names are built from its own constants.
// TestEveryCitylayoutRuntimeRootIsClassified fails when a new .gc/ root here
// is neither excluded nor deliberately kept, and
// TestEveryRepoRuntimeEntryIsClassified does the same for every top-level
// .gc/ entry the module's Go source names.
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
	".gc/credentials.json",
	".gc/mcp-adopted/**",

	// Sockets, pid files, and locks. Lock files are matched only directly
	// under .gc/, because pack sources under .gc/cache ship their packs.lock.
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
	".gc/agents/**",
	".gc/emergency/**",
	".gc/storage-served-binding.json",
	".gc/storage-infra-cleared.json",
	".gc/scope-ownership.json",
	// gc bd's trust state for this host's bd binaries: the verdict cache,
	// its lock, the walks' scratch dirs, and the operator's vouches.
	".gc/bd-inventory-vouch.json",
	".gc/cache/bd-inventory.json",
	".gc/cache/bd-inventory.json.lock",
	".gc/cache/tmp/**",
	".gc/mcp-locks/**",
	".gc/dispatch-wake",
	ControlPlaneDescriptorPath,

	// Ledger copies: the file provider's store in every scope and the
	// legacy JSONL archive.
	"**/.gc/beads.json",
	"**/.gc/beads.json.bak",
	".gc/jsonl-archive/**",
	".gc/jsonl-export-state.json",

	// SQLite ledger stores, matched by file name (WAL and SHM included)
	// wherever they sit: the legacy .gc/beads.sqlite, copies of a store, and
	// a binding root config places inside the work dir. The list cannot see
	// where a binding root is, only what its store file is called.
	"**/beads.sqlite*",

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

// WorkDirExportExcludes returns the paths, relative to a session's work dir,
// that a runtime which copies the work dir somewhere else (a remote sandbox, a
// container image) must leave behind. The list does not depend on the work
// dir: patterns that match nothing cost nothing, and patterns starting with
// "**/" reach nested rig scopes under any work dir.
//
// The list is a floor, not the complete set: a consumer must skip every
// matching path and may skip more. [MatchWorkDirExclude] documents the
// pattern grammar.
//
// The returned slice is a fresh copy the caller may modify.
func WorkDirExportExcludes() []string {
	return append([]string(nil), defaultWorkDirExportExcludes...)
}

// excludePattern is a work-dir exclude pattern split into its segments, each
// validated once at compile time.
type excludePattern struct {
	raw  string
	segs []string
}

func compileWorkDirExcludes(patterns []string) ([]excludePattern, error) {
	compiled := make([]excludePattern, 0, len(patterns))
	for _, p := range patterns {
		segs := strings.Split(p, "/")
		for _, seg := range segs {
			if _, err := path.Match(seg, ""); err != nil {
				return nil, fmt.Errorf("work-dir exclude pattern %q: %w", p, err)
			}
		}
		compiled = append(compiled, excludePattern{raw: p, segs: segs})
	}
	return compiled, nil
}

// matchesExactly reports whether the pattern matches the path segments
// themselves, ignoring the directories above them.
func (p excludePattern) matchesExactly(segs []string) (bool, error) {
	ok, err := matchExcludeSegments(p.segs, segs)
	if err != nil {
		return false, fmt.Errorf("work-dir exclude pattern %q: %w", p.raw, err)
	}
	return ok, nil
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
// It returns an error for a malformed pattern segment, whatever rel is.
func MatchWorkDirExclude(pattern, rel string) (bool, error) {
	return WorkDirExportExcluded([]string{pattern}, rel)
}

// WorkDirExportExcluded reports whether rel, or any directory above it, is
// excluded by any of patterns. It returns an error when any pattern is
// malformed, whatever rel is.
func WorkDirExportExcluded(patterns []string, rel string) (bool, error) {
	compiled, err := compileWorkDirExcludes(patterns)
	if err != nil {
		return false, err
	}
	segs := strings.Split(rel, "/")
	for n := 1; n <= len(segs); n++ {
		excluded, err := matchesAny(compiled, segs[:n])
		if err != nil || excluded {
			return excluded, err
		}
	}
	return false, nil
}

func matchesAny(compiled []excludePattern, segs []string) (bool, error) {
	for _, p := range compiled {
		ok, err := p.matchesExactly(segs)
		if err != nil || ok {
			return ok, err
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
		return false, nil
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false, err
	}
	return matchExcludeSegments(pat[1:], segs[1:])
}

// WorkDirExportEntries walks workDir and returns the slash-separated
// work-dir-relative paths a runtime may copy elsewhere: every file, symlink,
// and other non-directory that survives [WorkDirExportExcludes], plus every
// surviving directory that is empty on disk. An excluded directory is pruned
// with everything below it. Symlinks are listed, never followed; a workDir
// that is itself a symlink is resolved first.
//
// A non-empty directory is never listed, so a consumer that recurses into
// listed directories still adds nothing excluded; one that does not recurse
// recreates parents from the paths below them. A directory whose every entry
// is excluded is therefore absent from the list.
//
// Entries come in depth-first walk order, the entries of each directory
// sorted by name.
func WorkDirExportEntries(workDir string) ([]string, error) {
	if workDir == "" {
		return nil, fmt.Errorf("work-dir export: empty work dir")
	}
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return nil, fmt.Errorf("work-dir export: %w", err)
	}
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("work-dir export: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("work-dir export: %s is not a directory", workDir)
	}
	excludes, err := compileWorkDirExcludes(defaultWorkDirExportExcludes)
	if err != nil {
		return nil, err
	}
	w := exportWalker{excludes: excludes}
	if err := w.walk(root, nil); err != nil {
		return nil, fmt.Errorf("work-dir export %s: %w", workDir, err)
	}
	return w.entries, nil
}

type exportWalker struct {
	excludes []excludePattern
	entries  []string
}

// walk lists the surviving entries below the directory abs, whose
// work-dir-relative segments are segs. Its caller has already checked abs
// and every directory above it, so each entry is matched by its own path
// only.
func (w *exportWalker) walk(abs string, segs []string) error {
	children, err := os.ReadDir(abs)
	if err != nil {
		return err
	}
	if len(children) == 0 && len(segs) > 0 {
		w.entries = append(w.entries, strings.Join(segs, "/"))
		return nil
	}
	for _, child := range children {
		childSegs := make([]string, len(segs)+1)
		copy(childSegs, segs)
		childSegs[len(segs)] = child.Name()
		excluded, err := matchesAny(w.excludes, childSegs)
		if err != nil {
			return err
		}
		if excluded {
			continue
		}
		if child.IsDir() {
			if err := w.walk(filepath.Join(abs, child.Name()), childSegs); err != nil {
				return err
			}
			continue
		}
		w.entries = append(w.entries, strings.Join(childSegs, "/"))
	}
	return nil
}
