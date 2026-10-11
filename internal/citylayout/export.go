package citylayout

import (
	"os"
	"path/filepath"
)

// BeadsRoot is the per-scope beads directory (city root or rig root).
const BeadsRoot = ".beads"

// defaultWorkDirExportExcludes lists controller-local state that a runtime
// must not copy out of a session's work dir: secrets and credentials,
// controller sockets, locks and pid files, the event and usage logs, the
// nudge queue, relocated class stores, managed Dolt/SQLite ledger data,
// controller runtime state, other sessions' worktrees, and bd's server and
// proxy runtime files. Deployment artifacts sessions read at start
// (.gc/settings.json, .gc/site.toml, .gc/cache, .gc/system, .gc/scripts,
// .gc/tmp snapshots, .beads/metadata.json, .beads/config.yaml,
// .beads/http_target.json) are deliberately absent.
var defaultWorkDirExportExcludes = []string{
	".gc/services/**",
	".gc/secrets/**",
	".gc/controller.token",
	".gc/credentials.toml",
	".gc/**/*.sock",
	".gc/**/*.pid",
	".gc/*.lock",
	".gc/events.jsonl*",
	".gc/usage.jsonl*",
	".gc/nudges/**",
	".gc/store/**",
	".gc/storage/**",
	".gc/infra/**",
	".gc/dolt-data/**",
	".gc/runtime/**",
	".gc/session-mcp/**",
	".gc/session-name-locks/**",
	".gc/supervisor/**",
	".gc/worktrees/**",
	".gc/storage-served-binding.json",
	".gc/storage-infra-cleared.json",
	"**/.beads/dolt/**",
	"**/.beads/embeddeddolt/**",
	"**/.beads/backup/**",
	"**/.beads/*.db*",
	"**/.beads/*.sqlite3*",
	"**/.beads/*.lock",
	"**/.beads/dolt-server*",
	"**/.beads/proxy*",
	"**/.beads/proxied*",
	"**/.beads/daemon.*",
	"**/.beads/bd.sock*",
	"**/.beads/.env",
	"**/.beads/.beads-credential-key",
}

// WorkDirExportExcludes returns the paths, relative to workDir, that a
// runtime which copies the work dir somewhere else (a remote sandbox, a
// container image) must leave behind. It returns nil unless workDir holds a
// .gc/ runtime root or a .beads/ directory, so an ordinary checkout gets no
// list.
//
// Patterns are slash-separated and relative to workDir. A segment of exactly
// "**" matches zero or more whole path segments; every other segment is a
// path.Match glob that never crosses a "/" ("*" matches any run of
// characters in one segment, leading dots included). A path is excluded when
// it, or any directory above it, matches a pattern, so a trailing "/**"
// spells out that a whole subtree stays behind.
//
// The returned slice is a fresh copy the caller may modify.
func WorkDirExportExcludes(workDir string) []string {
	if !HasRuntimeRoot(workDir) && !hasDir(workDir, BeadsRoot) {
		return nil
	}
	return append([]string(nil), defaultWorkDirExportExcludes...)
}

func hasDir(dir, name string) bool {
	if dir == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, name))
	return err == nil && fi.IsDir()
}
