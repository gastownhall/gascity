// Package materialize installs pack-defined skill catalogs into each
// agent's provider-specific skill sink. The package is the v0.15.1
// hotfix per engdocs/proposals/skill-materialization.md — no agent
// previously received pack skills, despite the v0.15.0 catalog walk
// surfacing them in `gc skill list`.
//
// Two callers are expected:
//
//   - The supervisor (gc start, every supervisor tick) walks all agents
//     and materializes into each agent's scope-root sink.
//   - `gc internal materialize-skills`, injected as a PreStart entry for
//     stage-2-eligible runtimes whose session WorkDir differs from the
//     scope root, materializes into the per-session worktree sink.
//
// Both callers funnel through MaterializeAgent. Materialization is
// idempotent — repeated passes converge on the same on-disk shape.
//
// The package owns three responsibilities:
//
//  1. Source discovery — enumerate the union of (city pack skills) ∪
//     (each bootstrap implicit-import pack's skills) ∪ (the agent's
//     local skills) and resolve the precedence rule (city > bootstrap;
//     agent-local wins on collision with the shared catalog).
//  2. Cleanup by ownership-by-target-prefix — symlinks under the sink
//     whose target lives under a known gc-managed root are owned and
//     pruned/replaced; everything else is left alone.
//  3. Legacy stub migration — v0.15.0 wrote regular directories at the
//     same names the v0.15.1 core pack now ships; the materializer
//     removes those exact-shape stubs once before its first symlink
//     pass.
//
// Per the spec, this package does not perform fingerprint hashing or
// PreStart injection — those land in Phase 3 callers.
package materialize

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/bootstrap"
	"github.com/gastownhall/gascity/internal/config"
)

// vendorSinks maps an agent provider to the relative directory under the
// agent's scope-root or session WorkDir where skills are materialized.
//
// Only the four providers with verified skill-reading behavior are
// included. The other four providers recognized by hooks.go (copilot,
// cursor, pi, omp) intentionally have no entry — VendorSink returns
// ok=false so the caller can log a single skip line per session.
var vendorSinks = map[string]string{
	"claude":   ".claude/skills",
	"codex":    ".codex/skills",
	"gemini":   ".gemini/skills",
	"opencode": ".opencode/skills",
}

// VendorSink returns the sink subdirectory for a provider, relative to
// an agent's workdir. Returns ok=false when the provider has no v0.15.1
// sink mapping — callers should skip materialization and log once per
// session.
func VendorSink(provider string) (string, bool) {
	sink, ok := vendorSinks[provider]
	return sink, ok
}

// SupportedVendors returns the set of providers with materialization
// sinks. Stable across calls; callers should not mutate the returned
// slice.
func SupportedVendors() []string {
	out := make([]string, 0, len(vendorSinks))
	for v := range vendorSinks {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// SkillEntry is a single skill source: the directory containing SKILL.md
// that a sink symlink will target.
type SkillEntry struct {
	// Name is the skill name (sink leaf). Matches the source directory
	// basename, e.g. "gc-work" or "plan".
	Name string
	// Source is the absolute filesystem path of the source directory.
	// The materialized symlink targets this path.
	Source string
	// Origin is a diagnostic label describing which catalog this entry
	// came from. One of: "city", "<bootstrap-pack-name>", "agent".
	Origin string
	// Description is the one-line summary pulled from the SKILL.md YAML
	// frontmatter `description:` field, or "" when the frontmatter is
	// absent/malformed. Used by prompt-rendering surfaces that want to
	// show each skill's purpose alongside its name.
	Description string
}

// ShadowedEntry records a name that was provided by more than one source
// in the shared catalog. The winner kept its place; the loser was
// silently replaced. Surfaced for diagnostic logging.
type ShadowedEntry struct {
	// Name is the skill name involved in the collision.
	Name string
	// Winner is the origin label of the entry kept.
	Winner string
	// Loser is the origin label of the entry shadowed.
	Loser string
}

// CityCatalog is the city-wide shared skill catalog: the union of the
// current city pack's skills and every bootstrap implicit-import pack's
// skills, with city-pack entries winning on name collision.
//
// CityCatalog is independent of any specific agent and may be reused
// across all agents in the same city.
type CityCatalog struct {
	// Entries is the deduplicated, precedence-resolved list of shared
	// skills. Sorted by Name for deterministic output.
	Entries []SkillEntry
	// OwnedRoots is the set of absolute path prefixes that mark
	// gc-managed shared-skill targets. Cleanup uses this list to decide
	// whether an existing sink symlink is "ours" and therefore eligible
	// for prune/replace.
	OwnedRoots []string
	// Shadowed records every name that was provided by more than one
	// source. The winning entry appears in Entries; this list is purely
	// diagnostic.
	Shadowed []ShadowedEntry
}

// AgentCatalog is one agent's private skill catalog
// (agents/<name>/skills/). It overlays the CityCatalog at materialization
// time, with agent-local entries winning on name collision.
type AgentCatalog struct {
	// Entries is the agent's local skill list, sorted by Name.
	Entries []SkillEntry
	// OwnedRoot is the absolute path of the agent's local skills
	// directory, or empty when the agent has no local catalog. Used
	// alongside CityCatalog.OwnedRoots so cleanup can prune symlinks
	// pointing at this agent's old skill dir.
	OwnedRoot string
}

// LoadCityCatalog discovers the shared skill catalog for a city.
//
// packSkillsDir is the city pack's `skills/` directory (typically
// cfg.PackSkillsDir from the loaded config). Pass "" if the city pack
// has no skills/ subdirectory.
//
// imported catalogs are binding-qualified shared skills roots composed
// from pack imports. Each catalog contributes `<binding>.<name>`
// entries to the shared city catalog.
//
// Bootstrap implicit-import packs are read from
// ~/.gc/implicit-import.toml via config.ReadImplicitImports. Each
// pack's skills/ subdirectory is enumerated and added to the catalog.
// City-pack entries win on name collision with bootstrap entries.
func LoadCityCatalog(packSkillsDir string, imported ...config.DiscoveredSkillCatalog) (CityCatalog, error) {
	var (
		cat       CityCatalog
		nameOwner = make(map[string]int) // name → index into cat.Entries
		ownedSet  = make(map[string]struct{})
	)

	addEntry := func(entry SkillEntry) {
		if existing, dup := nameOwner[entry.Name]; dup {
			cat.Shadowed = append(cat.Shadowed, ShadowedEntry{
				Name:   entry.Name,
				Winner: cat.Entries[existing].Origin,
				Loser:  entry.Origin,
			})
			return
		}
		nameOwner[entry.Name] = len(cat.Entries)
		cat.Entries = append(cat.Entries, entry)
	}

	addRoot := func(root string) {
		if root == "" {
			return
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return
		}
		if _, dup := ownedSet[abs]; dup {
			return
		}
		ownedSet[abs] = struct{}{}
		cat.OwnedRoots = append(cat.OwnedRoots, abs)
	}

	if packSkillsDir != "" {
		addRoot(packSkillsDir)
	}
	for _, catalog := range imported {
		addRoot(catalog.SourceDir)
	}
	bootstrapDirs, err := bootstrapSkillDirs()
	if err != nil {
		return cat, err
	}
	for _, bd := range bootstrapDirs {
		addRoot(bd.Dir)
	}

	// City pack first — wins precedence over bootstrap entries.
	if packSkillsDir != "" {
		entries, err := readSkillDir(packSkillsDir, "city")
		if err != nil {
			return cat, fmt.Errorf("reading city pack skills %q: %w", packSkillsDir, err)
		}
		for _, e := range entries {
			addEntry(e)
		}
	}

	for _, catalog := range imported {
		origin := strings.TrimSpace(catalog.BindingName)
		if origin == "" {
			origin = strings.TrimSpace(catalog.PackName)
		}
		if origin == "" {
			origin = "import"
		}
		entries, err := readSkillDir(catalog.SourceDir, origin)
		if err != nil {
			return cat, fmt.Errorf("reading imported pack %q skills %q: %w", origin, catalog.SourceDir, err)
		}
		for _, e := range entries {
			if catalog.BindingName != "" {
				e.Name = catalog.BindingName + "." + e.Name
				e.Origin = catalog.BindingName
			}
			addEntry(e)
		}
	}

	for _, bd := range bootstrapDirs {
		entries, err := readSkillDir(bd.Dir, bd.Name)
		if err != nil {
			return cat, fmt.Errorf("reading bootstrap pack %q skills %q: %w", bd.Name, bd.Dir, err)
		}
		for _, e := range entries {
			addEntry(e)
		}
	}

	sort.Slice(cat.Entries, func(i, j int) bool { return cat.Entries[i].Name < cat.Entries[j].Name })
	sort.Slice(cat.Shadowed, func(i, j int) bool { return cat.Shadowed[i].Name < cat.Shadowed[j].Name })
	sort.Strings(cat.OwnedRoots)
	return cat, nil
}

// LoadAgentCatalog reads the agent's local skills directory, returning
// an AgentCatalog with one SkillEntry per `<dir>/<name>/SKILL.md`.
// Pass "" when the agent has no local skills; the result is a
// zero-value AgentCatalog.
func LoadAgentCatalog(agentSkillsDir string) (AgentCatalog, error) {
	if agentSkillsDir == "" {
		return AgentCatalog{}, nil
	}
	entries, err := readSkillDir(agentSkillsDir, "agent")
	if err != nil {
		return AgentCatalog{}, fmt.Errorf("reading agent skills %q: %w", agentSkillsDir, err)
	}
	abs, err := filepath.Abs(agentSkillsDir)
	if err != nil {
		abs = agentSkillsDir
	}
	return AgentCatalog{Entries: entries, OwnedRoot: abs}, nil
}

// EffectiveSet merges the shared city catalog and an agent's local
// catalog into the final desired symlink set. Agent-local entries win
// on name collision with shared entries. Returns a stable, sorted
// slice; never returns nil for a non-zero input.
func EffectiveSet(city CityCatalog, agent AgentCatalog) []SkillEntry {
	byName := make(map[string]SkillEntry, len(city.Entries)+len(agent.Entries))
	for _, e := range city.Entries {
		byName[e.Name] = e
	}
	for _, e := range agent.Entries {
		byName[e.Name] = e // agent-local wins
	}
	out := make([]SkillEntry, 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Request specifies one materialization pass into a single
// vendor sink.
type Request struct {
	// SinkDir is the absolute path of the sink directory, typically
	// `<workdir>/<vendor-sink-relative>`. The materializer creates this
	// directory if it does not exist.
	SinkDir string
	// Desired is the post-precedence list of entries to materialize.
	Desired []SkillEntry
	// OwnedRoots is the union of every gc-managed source root the
	// materializer is allowed to prune. Typically the CityCatalog's
	// OwnedRoots concatenated with the AgentCatalog's OwnedRoot.
	OwnedRoots []string
	// LegacyNames lists names whose v0.15.0 stub-shape directories
	// should be migrated (removed) before this pass creates new
	// symlinks. Pass nil to skip legacy migration. Use LegacyStubNames()
	// for the canonical list.
	LegacyNames []string
}

// SkippedConflict records a name in the desired set that could not be
// materialized because the destination is occupied by user-owned
// content (a regular file or directory the materializer must not
// touch).
type SkippedConflict struct {
	// Name is the desired skill name.
	Name string
	// Path is the absolute sink path that conflicts.
	Path string
	// Reason is a short human-readable explanation suitable for log output.
	Reason string
}

// Result records the outcome of a single MaterializeAgent
// invocation. Lists are sorted for deterministic test/log output.
type Result struct {
	// Materialized is the list of names whose symlinks now point at the
	// desired source (created or already correct).
	Materialized []string
	// Skipped lists desired names that were not materialized because
	// user content occupies the target path.
	Skipped []SkippedConflict
	// LegacyMigrated lists legacy-stub names whose v0.15.0 regular
	// directories were removed during this pass. Empty after the first
	// post-upgrade pass converges.
	LegacyMigrated []string
	// Warnings lists non-fatal issues encountered during cleanup
	// (typically I/O errors on individual entries). The pass continued
	// past each warning.
	Warnings []string
}

// Run runs one materialization pass for an agent's sink.
//
// Pass order:
//
//  1. Ensure SinkDir exists (mkdir -p).
//  2. Legacy-stub migration: for each name in req.LegacyNames, if a
//     regular directory at <SinkDir>/<name> matches the v0.15.0 stub
//     shape exactly, remove it.
//  3. Cleanup walk: for each existing entry under SinkDir, apply the
//     safety matrix (delete dangling/orphaned symlinks owned by us,
//     atomic-replace symlinks whose target drifted, leave regular
//     files/dirs and external-target symlinks alone).
//  4. For each desired entry not yet present and not blocked by user
//     content: atomic-create the symlink via tmp-then-rename.
//
// MaterializeAgent is idempotent: a second invocation with the same
// request observes a converged sink and creates nothing new. Errors
// returned are sink-level fatal errors; per-entry errors are recorded
// in Result.Warnings and do not abort the pass.
func Run(req Request) (Result, error) {
	if req.SinkDir == "" {
		return Result{}, errors.New("materialize: SinkDir is required")
	}
	absSink, err := filepath.Abs(req.SinkDir)
	if err != nil {
		return Result{}, fmt.Errorf("resolving sink dir %q: %w", req.SinkDir, err)
	}
	if err := os.MkdirAll(absSink, 0o755); err != nil {
		return Result{}, fmt.Errorf("creating sink dir %q: %w", absSink, err)
	}

	desiredByName := make(map[string]SkillEntry, len(req.Desired))
	for _, e := range req.Desired {
		desiredByName[e.Name] = e
	}

	var result Result

	owned := make([]string, 0, len(req.OwnedRoots))
	for _, root := range req.OwnedRoots {
		if root == "" {
			continue
		}
		canon, err := canonicalizePath(root)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("canonicalize owned root %q: %v", root, err))
			continue
		}
		owned = append(owned, canon)
	}

	// Step 2: legacy stub migration.
	for _, name := range req.LegacyNames {
		path := filepath.Join(absSink, name)
		if removed, warn := tryRemoveLegacyStub(path, name); removed {
			result.LegacyMigrated = append(result.LegacyMigrated, name)
		} else if warn != "" {
			result.Warnings = append(result.Warnings, warn)
		}
	}

	// Step 3: cleanup walk.
	dirEntries, err := os.ReadDir(absSink)
	if err != nil {
		return result, fmt.Errorf("reading sink dir %q: %w", absSink, err)
	}
	for _, de := range dirEntries {
		name := de.Name()
		path := filepath.Join(absSink, name)
		info, lerr := os.Lstat(path)
		if lerr != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("lstat %q: %v", path, lerr))
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			// Regular file or directory — not ours, leave alone.
			continue
		}
		target, rerr := os.Readlink(path)
		if rerr != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("readlink %q: %v", path, rerr))
			continue
		}
		// MaterializeAgent always writes absolute targets, so a
		// relative-target symlink is by definition not ours. Skip
		// canonicalisation entirely; otherwise filepath.Abs would
		// resolve the relative path against the process working
		// directory (a misleading base) and may misclassify a
		// user-placed link as gc-owned.
		if !filepath.IsAbs(target) {
			continue
		}
		canonTarget, terr := canonicalizePath(target)
		if terr != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("canonicalize target %q: %v", target, terr))
			continue
		}
		if !targetUnderOwnedRoot(canonTarget, owned) {
			// External target — symlink the user placed themselves.
			continue
		}
		desired, want := desiredByName[name]
		if !want {
			// Owned but not desired — delete (covers dangling and orphaned).
			if rmErr := os.Remove(path); rmErr != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("removing orphan symlink %q: %v", path, rmErr))
			}
			continue
		}
		desiredAbs, terr := filepath.Abs(desired.Source)
		if terr != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("resolving desired target %q: %v", desired.Source, terr))
			continue
		}
		canonDesired, derr := canonicalizePath(desiredAbs)
		if derr != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("canonicalize desired target %q: %v", desiredAbs, derr))
			continue
		}
		if canonTarget == canonDesired {
			// Already correct — record and move on. The create loop
			// will see this name has been satisfied via desiredByName
			// removal below.
			result.Materialized = append(result.Materialized, name)
			delete(desiredByName, name)
			continue
		}
		// Drifted target — atomic replace. Use the lexical desiredAbs
		// (not canonicalized) so the on-disk symlink target remains the
		// caller-intended path; canonicalization is comparison-only.
		if rerr := atomicSymlink(desiredAbs, path); rerr != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("replacing symlink %q: %v", path, rerr))
			continue
		}
		result.Materialized = append(result.Materialized, name)
		delete(desiredByName, name)
	}

	// Step 4: create remaining desired entries. Iterate in sorted order
	// so partial-failure recovery is deterministic across runs.
	remaining := make([]string, 0, len(desiredByName))
	for name := range desiredByName {
		remaining = append(remaining, name)
	}
	sort.Strings(remaining)
	for _, name := range remaining {
		desired := desiredByName[name]
		path := filepath.Join(absSink, name)
		desiredAbs, terr := filepath.Abs(desired.Source)
		if terr != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("resolving desired target %q: %v", desired.Source, terr))
			continue
		}
		info, lerr := os.Lstat(path)
		if lerr == nil {
			// Something exists. If it's a symlink we already handled it
			// in step 3 (otherwise the desiredByName entry would have
			// been deleted). It's user content — skip and report.
			if info.Mode()&os.ModeSymlink != 0 {
				// Symlink still exists despite the cleanup pass — likely
				// an external-target symlink the user owns.
				result.Skipped = append(result.Skipped, SkippedConflict{
					Name:   name,
					Path:   path,
					Reason: "user-owned symlink at sink path",
				})
				continue
			}
			result.Skipped = append(result.Skipped, SkippedConflict{
				Name:   name,
				Path:   path,
				Reason: "user content at sink path (regular file or directory)",
			})
			continue
		}
		if !os.IsNotExist(lerr) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("lstat %q: %v", path, lerr))
			continue
		}
		if cerr := atomicSymlink(desiredAbs, path); cerr != nil {
			return result, fmt.Errorf("creating symlink %q -> %q: %w", path, desiredAbs, cerr)
		}
		result.Materialized = append(result.Materialized, name)
	}

	sort.Strings(result.Materialized)
	sort.Slice(result.Skipped, func(i, j int) bool { return result.Skipped[i].Name < result.Skipped[j].Name })
	sort.Strings(result.LegacyMigrated)
	sort.Strings(result.Warnings)
	return result, nil
}

// LegacyStubNames returns the canonical list of v0.15.0 stub names that
// the materializer migrates on the first post-upgrade pass. These are
// the gc-<topic> stubs the old materializeSkillStubs wrote into every
// .claude/skills sink and which the v0.15.1 core bootstrap pack now
// supplies as real skills.
func LegacyStubNames() []string {
	out := make([]string, 0, len(legacyStubBodies))
	for name := range legacyStubBodies {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// readSkillDescription parses the SKILL.md YAML frontmatter and returns
// the `description:` value. Returns "" when:
//   - the file is unreadable
//   - the file has no `---` frontmatter delimiters
//   - the frontmatter lacks a description key
//
// Only the first 64 lines of the file are scanned — description lives
// in frontmatter near the top; this caps the I/O cost of rendering
// every skill's description alongside the catalog without pulling
// large skill bodies into memory.
//
// This is a minimal YAML consumer rather than a real parser. It
// recognizes `description: <value>` (single-line), strips optional
// surrounding quotes, and stops at the closing `---` delimiter.
// Multi-line block scalars (`description: >\n  ...`) are not
// supported in v0.15.1 and return "" — same end state as a missing
// description, which the caller handles gracefully.
func readSkillDescription(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// Strip optional UTF-8 BOM before the frontmatter check. Editors
	// on Windows and some export pipelines prepend EF BB BF; without
	// this the prefix check would silently fail and every exported
	// SKILL.md would surface with a blank description.
	text := strings.TrimPrefix(string(data), "\ufeff")
	// Require frontmatter to begin on the first line.
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return ""
	}
	// Find the closing `---` on a line by itself.
	lines := strings.Split(text, "\n")
	const maxScan = 64
	end := len(lines)
	if end > maxScan {
		end = maxScan
	}
	for i := 1; i < end; i++ {
		line := strings.TrimRight(lines[i], "\r")
		if line == "---" {
			break
		}
		trimmed := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trimmed, "description:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "description:"))
		// Strip a matching pair of surrounding quotes; no escape handling
		// needed for the typical single-line description.
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		return value
	}
	return ""
}

// readSkillDir enumerates skill subdirectories of root. A subdirectory
// counts as a skill iff it contains a SKILL.md file (case-sensitive,
// matching the vendor convention). Returns nil if root does not exist.
func readSkillDir(root, origin string) ([]SkillEntry, error) {
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	var out []SkillEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(abs, e.Name())
		skillMD := filepath.Join(dir, "SKILL.md")
		if _, statErr := os.Stat(skillMD); statErr != nil {
			continue
		}
		out = append(out, SkillEntry{
			Name:        e.Name(),
			Source:      dir,
			Origin:      origin,
			Description: readSkillDescription(skillMD),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// namedSkillsDir pairs a bootstrap pack name with its resolved skills/
// directory on disk. Used internally by LoadCityCatalog.
type namedSkillsDir struct {
	Name string
	Dir  string
}

// bootstrapSkillDirs resolves each bootstrap implicit-import pack to its
// `<resolved-cache-root>/skills/` directory if that directory exists.
// Bootstrap packs missing from implicit-import.toml are silently
// skipped — the bootstrap entry is the source of truth for "is this
// pack installed".
func bootstrapSkillDirs() ([]namedSkillsDir, error) {
	imports, _, err := config.ReadImplicitImports()
	if err != nil {
		return nil, fmt.Errorf("reading implicit imports: %w", err)
	}
	if len(imports) == 0 {
		return nil, nil
	}
	bootstrapNames := make(map[string]struct{}, len(bootstrap.PackNames()))
	for _, name := range bootstrap.PackNames() {
		bootstrapNames[name] = struct{}{}
	}
	gcHome := config.ImplicitGCHome()
	if gcHome == "" {
		return nil, nil
	}
	out := make([]namedSkillsDir, 0, len(imports))
	for name, imp := range imports {
		if _, ok := bootstrapNames[name]; !ok {
			continue
		}
		if imp.Commit == "" {
			continue
		}
		skillsDir := filepath.Join(config.GlobalRepoCachePath(gcHome, imp.Source, imp.Commit), "skills")
		if info, err := os.Stat(skillsDir); err != nil || !info.IsDir() {
			continue
		}
		out = append(out, namedSkillsDir{Name: name, Dir: skillsDir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// targetUnderOwnedRoot reports whether the given canonical symlink
// target falls under one of the canonical gc-managed source root
// prefixes. Both the target and the roots must already be passed
// through canonicalizePath so /var ↔ /private/var aliases compare
// equal; otherwise a self-written symlink can be mis-classified as
// external (the failure mode the macOS path-alias regression catches).
func targetUnderOwnedRoot(target string, ownedRoots []string) bool {
	if !filepath.IsAbs(target) {
		// Materializer always writes absolute targets, so a relative
		// link is by definition not ours.
		return false
	}
	for _, root := range ownedRoots {
		if target == root {
			return true
		}
		if strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// canonicalizePath returns a path with all leading symlinks resolved
// (via filepath.EvalSymlinks). When the path itself does not exist
// (e.g., a dangling symlink target or a not-yet-created sink entry),
// the function walks up to find the deepest ancestor that does exist,
// canonicalizes that, and re-appends the missing tail. This handles
// platforms where common roots are symlinks (macOS /tmp →
// /private/tmp; certain Linux distros where /var symlinks elsewhere)
// without breaking comparisons against materializer-written targets
// that may have been recorded with the unresolved prefix.
//
// Returns an error only when filepath.Abs fails on a relative input.
// All EvalSymlinks errors are absorbed by the walk-up fallback.
func canonicalizePath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	abs := path
	if !filepath.IsAbs(abs) {
		a, err := filepath.Abs(abs)
		if err != nil {
			return "", err
		}
		abs = a
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	// Walk up until an ancestor exists; canonicalize it, then re-append
	// the missing suffix. Falls back to the cleaned absolute path when
	// nothing along the way exists (e.g., entirely-fictional path
	// supplied by a test).
	var suffix []string
	cur := abs
	for {
		parent := filepath.Dir(cur)
		suffix = append([]string{filepath.Base(cur)}, suffix...)
		if parent == cur {
			return abs, nil
		}
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Join(parts...), nil
		}
		cur = parent
	}
}

// atomicSymlink creates or replaces a symlink at path pointing to
// target via tmp-then-rename. POSIX rename(2) is atomic, so observers
// never see a missing or partially-written symlink during replacement.
func atomicSymlink(target, path string) error {
	dir := filepath.Dir(path)
	tmp, err := tempSymlinkPath(dir, filepath.Base(path))
	if err != nil {
		return fmt.Errorf("allocating temp symlink path: %w", err)
	}
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("creating temp symlink %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("renaming temp symlink into place: %w", err)
	}
	return nil
}

// tempSymlinkPath produces a unique temporary path next to the target
// path. We cannot use os.CreateTemp because Symlink will refuse to
// create over an existing file.
func tempSymlinkPath(dir, base string) (string, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, "."+base+".tmp."+hex.EncodeToString(nonce[:])), nil
}

// tryRemoveLegacyStub removes a v0.15.0 stub directory at path if its
// contents match the recorded stub shape exactly. Returns (true, "")
// when the directory was removed, (false, "") when the directory does
// not match (left alone — typical case once converged), and
// (false, warning) when an unexpected I/O error blocks the decision.
func tryRemoveLegacyStub(path, name string) (bool, string) {
	body, ok := legacyStubBodies[name]
	if !ok {
		return false, ""
	}
	info, err := os.Lstat(path)
	if err != nil {
		// Missing is the converged state; not a warning.
		return false, ""
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Already a symlink — converged.
		return false, ""
	}
	if !info.IsDir() {
		// User has placed a regular file at this name. Leave alone.
		return false, ""
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, fmt.Sprintf("reading legacy stub dir %q: %v", path, err)
	}
	if len(entries) != 1 || entries[0].Name() != "SKILL.md" || entries[0].IsDir() {
		// User content — leave alone.
		return false, ""
	}
	got, err := os.ReadFile(filepath.Join(path, "SKILL.md"))
	if err != nil {
		return false, fmt.Sprintf("reading legacy stub file %q: %v", path, err)
	}
	if string(got) != body {
		// User-edited stub — preserve.
		return false, ""
	}
	if err := os.RemoveAll(path); err != nil {
		return false, fmt.Sprintf("removing legacy stub dir %q: %v", path, err)
	}
	return true, ""
}

// legacyStubBodies maps each v0.15.0 stub name to the exact SKILL.md
// content the old cmd/gc/skill_stubs.go materializer wrote. The
// migration step matches by full byte equality so user-edited stubs
// (which would not match the canonical text) are preserved.
//
// The map mirrors cmd/gc/cmd_skills.go's skillTopics table verbatim;
// it is duplicated here so the materializer remains independent of
// cmd/gc after Phase 2B deletes that file.
var legacyStubBodies = map[string]string{
	"gc-work":      "---\nname: gc-work\ndescription: Finding, creating, claiming, and closing work items (beads)\n---\n!`gc skills work`\n",
	"gc-dispatch":  "---\nname: gc-dispatch\ndescription: Routing work to agents with gc sling and formulas\n---\n!`gc skills dispatch`\n",
	"gc-agents":    "---\nname: gc-agents\ndescription: Managing agents — list, peek, nudge, suspend, drain\n---\n!`gc skills agents`\n",
	"gc-rigs":      "---\nname: gc-rigs\ndescription: Managing rigs — add, list, status, suspend, resume\n---\n!`gc skills rigs`\n",
	"gc-mail":      "---\nname: gc-mail\ndescription: Sending and reading messages between agents\n---\n!`gc skills mail`\n",
	"gc-city":      "---\nname: gc-city\ndescription: City lifecycle — status, start, stop, init\n---\n!`gc skills city`\n",
	"gc-dashboard": "---\nname: gc-dashboard\ndescription: API server and web dashboard — config, start, monitor\n---\n!`gc skills dashboard`\n",
}
