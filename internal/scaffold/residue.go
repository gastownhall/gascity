// Package scaffold answers one question about a gc-managed worktree: of the
// files git reports as dirty, which ones represent actual agent work?
//
// gc injects its own files into every worktree it creates — provider-native
// MCP config, materialized skill symlinks, and an appended ".gitignore"
// block. All three land on tracked-or-visible paths, so a plain
// "git status --porcelain" in a healthy, untouched gc worktree is never
// empty. Any disposal gate written as `[ -z "$DIRTY" ]` therefore refuses
// forever, and the one signal that should mean "real uncommitted work is
// about to be destroyed" fires on 100% of worktrees (gas-wr8).
//
// Residue subtracts only scaffolding gc can PROVE it owns, from records gc
// itself wrote at injection time:
//
//   - skill symlinks — named in the sink's ".gc-skill-ownership.json"
//   - MCP config — marked in ".gc/mcp-managed/<provider>.json", and only
//     when the file still hashes to what gc last wrote
//   - ".gitignore" — only when removing gc's own "# Gas City" blocks
//     restores the committed content exactly
//
// Everything else is residue. The subtraction is deliberately one-directional:
// an unprovable case stays residue, so the failure mode is a worktree that
// lives too long, never one that is deleted with work in it.
package scaffold

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/reviewquorum"
)

// gasCityIgnoreMarker is the header ensureGitignoreEntries writes above each
// block of gc-managed ignore entries. The block's CONTENTS vary per worktree
// (the city block lists MCP targets, a rig block lists ".beads/*"), so the
// marker is matched and the block parsed — never compared against fixed
// expected content, which would silently stop matching and quietly restore
// the gas-wr8 bug.
const gasCityIgnoreMarker = "# Gas City"

// ownershipManifestFile mirrors the materializer's per-sink bookkeeping file.
const ownershipManifestFile = ".gc-skill-ownership.json"

// HeadFileFunc returns the committed bytes of a repo-relative path at HEAD.
// It returns an error when the path is absent from HEAD; callers treat any
// error as "cannot prove", which keeps the entry in the residue.
type HeadFileFunc func(rel string) ([]byte, error)

// Residue returns the subset of entries that is not provably gc-injected
// scaffolding. An empty result means the worktree holds nothing but gc's own
// files and is safe to discard.
//
// worktree is the absolute path to the working tree the entries came from.
// headFile reads committed content for the ".gitignore" comparison; a nil
// headFile disables that one subtraction rather than failing the whole pass.
func Residue(worktree string, entries []reviewquorum.StatusEntry, headFile HeadFileFunc) []reviewquorum.StatusEntry {
	managed := managedMCPHashes(worktree)
	manifests := newManifestCache(worktree)

	var residue []reviewquorum.StatusEntry
	for _, entry := range entries {
		if isScaffolding(worktree, entry, managed, manifests, headFile) {
			continue
		}
		residue = append(residue, entry)
	}
	return residue
}

// HasRealWork reports whether porcelain output from worktree contains any
// change that is not gc scaffolding. It is the drop-in replacement for a bare
// `strings.TrimSpace(porcelain) != ""` disposal gate.
func HasRealWork(worktree, porcelain string, headFile HeadFileFunc) bool {
	return len(Residue(worktree, reviewquorum.ParseStatusPorcelain(porcelain), headFile)) > 0
}

func isScaffolding(
	worktree string,
	entry reviewquorum.StatusEntry,
	managed map[string]string,
	manifests *manifestCache,
	headFile HeadFileFunc,
) bool {
	// git reports untracked directories with a trailing slash; the skill sink
	// holds symlinks, but normalize so either shape matches its manifest key.
	rel := strings.TrimSuffix(entry.Path, "/")
	if rel == "" {
		return false
	}

	// A deletion is never scaffolding: gc only ever writes these paths, so a
	// missing one is a change gc did not make.
	if strings.Contains(entry.Status, "D") {
		return false
	}

	if manifests.owns(rel) {
		return true
	}
	if want, ok := managed[rel]; ok {
		return mcpTargetUnmodified(filepath.Join(worktree, filepath.FromSlash(rel)), want)
	}
	if rel == ".gitignore" {
		return gitignoreHoldsOnlyGasCityBlocks(worktree, headFile)
	}
	return false
}

// mcpTargetUnmodified reports whether the on-disk MCP config still hashes to
// the content gc recorded when it wrote the file. A mismatch means something
// edited it after gc, so the file is real work. An empty want (a marker
// written before gc recorded hashes) proves nothing and fails closed.
func mcpTargetUnmodified(absPath, want string) bool {
	if want == "" {
		return false
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == want
}

// managedMCPHashes maps each slash-relative MCP target gc has adopted in this
// worktree to the content hash gc last wrote there.
func managedMCPHashes(worktree string) map[string]string {
	targets := materialize.ManagedMCPTargets(worktree)
	if len(targets) == 0 {
		return nil
	}
	out := make(map[string]string, len(targets))
	for _, t := range targets {
		out[t.RelPath] = t.SHA256
	}
	return out
}

// manifestCache resolves skill-sink ownership lazily: a path is gc's if the
// sink directory holding it carries an ownership manifest that names it. No
// sink location is hardcoded, so a pack materializing into ".codex/skills"
// (or anywhere else) is recognized on the same evidence as ".claude/skills".
type manifestCache struct {
	worktree string
	byDir    map[string]map[string]string
}

func newManifestCache(worktree string) *manifestCache {
	return &manifestCache{worktree: worktree, byDir: map[string]map[string]string{}}
}

func (c *manifestCache) owns(rel string) bool {
	dir := path.Dir(rel)
	if dir == "." {
		return false
	}
	targets, ok := c.byDir[dir]
	if !ok {
		targets = loadOwnershipTargets(filepath.Join(c.worktree, filepath.FromSlash(dir), ownershipManifestFile))
		c.byDir[dir] = targets
	}
	if targets == nil {
		return false
	}
	base := path.Base(rel)
	// The manifest is gc's own bookkeeping file and never agent work, but it
	// only counts as scaffolding inside a directory that actually is a sink.
	if base == ownershipManifestFile {
		return true
	}
	_, owned := targets[base]
	return owned
}

// loadOwnershipTargets returns the manifest's entry names, or nil when the
// directory is not a gc skill sink. A corrupt manifest yields nil so the
// entries under it stay in the residue.
func loadOwnershipTargets(manifestPath string) map[string]string {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}
	var m struct {
		Targets map[string]string `json:"targets"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return m.Targets
}

// gitignoreHoldsOnlyGasCityBlocks reports whether the working copy of
// ".gitignore" differs from HEAD by nothing except gc's own appended blocks.
//
// The block is subtracted by PARSING it — from the "# Gas City" marker to the
// next blank line — rather than by ignoring the ".gitignore" path outright.
// Path-ignoring would drop an agent's legitimate edit to a shared, tracked
// file and classify a worktree holding only that edit as disposable, which is
// the destructive direction this whole gate exists to prevent.
func gitignoreHoldsOnlyGasCityBlocks(worktree string, headFile HeadFileFunc) bool {
	if headFile == nil {
		return false
	}
	current, err := os.ReadFile(filepath.Join(worktree, ".gitignore"))
	if err != nil {
		return false
	}
	head, err := headFile(".gitignore")
	if err != nil {
		return false
	}
	stripped := stripGasCityBlocks(string(current))
	// Trailing newlines are normalized: the injector inserts a blank
	// separator line before its block, and restoring that exactly would mean
	// guessing how many newlines HEAD ended with.
	return strings.TrimRight(stripped, "\n") == strings.TrimRight(string(head), "\n")
}

// stripGasCityBlocks removes every "# Gas City" section from a .gitignore.
// A section runs from its marker line to the next blank line or EOF, matching
// how ensureGitignoreEntries appends one (and it may append more than one, as
// later calls add newly-managed paths).
func stripGasCityBlocks(content string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != gasCityIgnoreMarker {
			out = append(out, lines[i])
			continue
		}
		// Skip the marker and its contiguous entries.
		for i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" {
			i++
		}
	}
	return strings.Join(out, "\n")
}
