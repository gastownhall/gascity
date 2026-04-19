// Package hooks installs provider hook files needed before runtime startup.
// Claude still uses a city-level settings file, while the other providers use
// files sourced from the embedded core pack overlay/per-provider tree and
// materialized into the session workdir.
package hooks

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/overlay"
)

//go:embed config/claude.json
var configFS embed.FS

// supported lists provider names that Install recognizes.
var supported = []string{"claude"}

// overlayManaged lists provider names whose hooks ship via the core pack
// overlay instead of this package. Included in Validate's accept set so
// existing install_agent_hooks entries stay valid without extra config churn.
var overlayManaged = []string{"codex", "gemini", "opencode", "copilot", "cursor", "pi", "omp"}

// unsupported lists provider names that have no hook mechanism.
var unsupported = []string{"amp", "auggie"}

// SupportedProviders returns the list of provider names with hook support —
// including the overlay-managed ones so callers can surface them in docs.
func SupportedProviders() []string {
	out := make([]string, 0, len(supported)+len(overlayManaged))
	out = append(out, supported...)
	out = append(out, overlayManaged...)
	return out
}

// Validate checks that all provider names are supported for hook installation.
// Returns an error listing any unsupported names.
func Validate(providers []string) error {
	accept := make(map[string]bool, len(supported)+len(overlayManaged))
	for _, s := range supported {
		accept[s] = true
	}
	for _, s := range overlayManaged {
		accept[s] = true
	}
	noHook := make(map[string]bool, len(unsupported))
	for _, u := range unsupported {
		noHook[u] = true
	}
	var bad []string
	for _, p := range providers {
		if !accept[p] {
			if noHook[p] {
				bad = append(bad, fmt.Sprintf("%s (no hook mechanism)", p))
			} else {
				bad = append(bad, fmt.Sprintf("%s (unknown)", p))
			}
		}
	}
	if len(bad) > 0 {
		all := append(append([]string{}, supported...), overlayManaged...)
		return fmt.Errorf("unsupported install_agent_hooks: %s; supported: %s",
			strings.Join(bad, ", "), strings.Join(all, ", "))
	}
	return nil
}

// Install writes hook files for the requested providers. Claude still uses a
// city-level file; the overlay-managed providers are copied from the embedded
// core pack overlay into the target workdir so desired-state fingerprinting
// and direct runtimes see the same files before startup.
func Install(fs fsys.FS, cityDir, workDir string, providers []string) error {
	for _, p := range providers {
		switch p {
		case "claude":
			if err := installClaude(fs, cityDir); err != nil {
				return fmt.Errorf("installing %s hooks: %w", p, err)
			}
		case "codex", "gemini", "opencode", "copilot", "cursor", "pi", "omp":
			if err := installOverlayManaged(fs, workDir, p); err != nil {
				return fmt.Errorf("installing %s hooks: %w", p, err)
			}
		default:
			return fmt.Errorf("unsupported hook provider %q", p)
		}
	}
	return nil
}

func installOverlayManaged(fs fsys.FS, workDir, provider string) error {
	if strings.TrimSpace(workDir) == "" {
		return nil
	}
	base := path.Join("overlay", "per-provider", provider)
	if _, err := iofs.Stat(core.PackFS, base); err != nil {
		return fmt.Errorf("provider overlay %q: %w", provider, err)
	}
	return iofs.WalkDir(core.PackFS, base, func(name string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == base || d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(name, base+"/")
		data, err := iofs.ReadFile(core.PackFS, name)
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		dst := filepath.Join(workDir, filepath.FromSlash(rel))
		return writeEmbeddedManaged(fs, dst, data, nil)
	})
}

// installClaude writes both the source hook file (hooks/claude.json) and the
// runtime settings file (.gc/settings.json) in the city directory. The
// runtime file is the path gc passes to Claude via --settings, while the
// legacy hooks/claude.json file remains user-owned unless gc can prove it is
// safe to seed or rewrite during migration/upgrade.
//
// Source precedence for user-authored Claude settings:
//  1. <city>/.claude/settings.json
//  2. <city>/hooks/claude.json
//  3. <city>/.gc/settings.json
//
// The selected source (or embedded defaults, if no override exists) is merged
// onto the embedded default Claude settings so new default hooks added in
// future releases land for users on every source, not just .claude/settings.json.
func installClaude(fs fsys.FS, cityDir string) error {
	hookDst := filepath.Join(cityDir, citylayout.ClaudeHookFile)
	runtimeDst := filepath.Join(cityDir, ".gc", "settings.json")
	data, sourceKind, err := desiredClaudeSettings(fs, cityDir)
	if err != nil {
		return err
	}

	// Write hooks/claude.json when:
	//  (a) it's the explicitly selected source (claudeSettingsSourceLegacyHook),
	//      so the user's merged settings land back in the file they own; or
	//  (b) the existing hook file is a known-stale gc-generated pattern and
	//      needs the in-place upgrade (old embedded bytes → new embedded bytes).
	//
	// Previous revisions also seeded the hook file on FRESH installs
	// whenever it was absent — that behavior created a stale-mirror bug:
	// a user who started with .claude/settings.json got a mirrored
	// hooks/claude.json written on first install, then if they later
	// removed .claude/settings.json desiredClaudeSettings would fall
	// back to the mirror as "legacy hook source" and ship the previous
	// generation's settings instead of the current embedded defaults.
	// Fresh installs now leave hooks/claude.json untouched; the
	// gc-managed .gc/settings.json is what gc passes to Claude via
	// --settings.
	if sourceKind == claudeSettingsSourceLegacyHook || isStaleHookFile(fs, hookDst) {
		if err := writeManagedFile(fs, hookDst, data, preserveUnreadable); err != nil {
			return err
		}
	}
	// The runtime file is gc-owned: if existing content is unreadable (bad
	// perms, i/o error), force an overwrite rather than silently preserving
	// a stale blob Claude can't parse. If the write itself fails, surface
	// the error so the caller can fail agent creation loudly instead of
	// launching with a broken --settings path.
	return writeManagedFile(fs, runtimeDst, data, forceOverwrite)
}

type writeManagedFilePolicy int

const (
	// preserveUnreadable leaves a stat-ok-but-read-fails file in place.
	// Used for user-owned paths (hooks/claude.json) where clobbering an
	// unreadable file could lose user-authored content.
	preserveUnreadable writeManagedFilePolicy = iota
	// forceOverwrite attempts to write the new content even when the
	// existing file is unreadable. Used for gc-managed paths (.gc/settings.json)
	// where the file's content is gc's responsibility.
	forceOverwrite
)

// isStaleHookFile reports whether hooks/claude.json exists AND matches a
// known stale gc-generated pattern. Only true for files we can prove gc
// wrote: user-authored content and the current-embedded-defaults case
// both return false so they are preserved in place.
func isStaleHookFile(fs fsys.FS, hookDst string) bool {
	data, err := fs.ReadFile(hookDst)
	if err != nil {
		return false
	}
	return claudeFileNeedsUpgrade(data)
}

// readEmbedded returns the embedded Claude defaults (config/claude.json).
// The path is fixed — the embed directive only captures that one file —
// so the parameter would be dead weight (and tripped up the unparam
// linter). All callers read the same file.
func readEmbedded() ([]byte, error) {
	const embedPath = "config/claude.json"
	data, err := configFS.ReadFile(embedPath)
	if err != nil {
		return nil, fmt.Errorf("reading embedded %s: %w", embedPath, err)
	}
	return data, nil
}

func writeEmbeddedManaged(fs fsys.FS, dst string, data []byte, needsUpgrade func([]byte) bool) error {
	if existing, err := fs.ReadFile(dst); err == nil {
		if needsUpgrade == nil || !needsUpgrade(existing) {
			return nil
		}
	} else if _, statErr := fs.Stat(dst); statErr == nil {
		// File exists but isn't readable. Preserve it rather than clobbering it.
		return nil
	}

	dir := filepath.Dir(dst)
	if err := fs.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	if err := fs.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	return nil
}

type claudeSettingsSourceKind int

const (
	claudeSettingsSourceNone claudeSettingsSourceKind = iota
	claudeSettingsSourceCityDotClaude
	claudeSettingsSourceLegacyHook
	claudeSettingsSourceLegacyRuntime
)

// desiredClaudeSettings returns the bytes that should land in the managed
// runtime file (.gc/settings.json) and the source kind that was chosen.
//
// All override sources — including legacy ones — are merged against the
// embedded base. The hooks array in overlay.MergeSettingsJSON uses
// union-by-identity semantics (duplicate entries collapse), so merging is
// safe and gives legacy users the future-base-hook-additions path back: any
// new default hook added to config/claude.json in a future release lands
// for users whose source is hooks/claude.json or .gc/settings.json, not
// just users on .claude/settings.json.
func desiredClaudeSettings(fs fsys.FS, cityDir string) ([]byte, claudeSettingsSourceKind, error) {
	base, err := readEmbedded()
	if err != nil {
		return nil, claudeSettingsSourceNone, err
	}

	overridePath, overrideData, sourceKind, err := readClaudeSettingsOverride(fs, cityDir, base)
	if err != nil {
		return nil, claudeSettingsSourceNone, err
	}

	if sourceKind == claudeSettingsSourceNone {
		// No override found. Return embedded base as-is.
		return base, claudeSettingsSourceNone, nil
	}
	if len(overrideData) == 0 {
		// An override source was located but its content is empty. For the
		// preferred source (.claude/settings.json), that contradicts the
		// strict contract — an intentionally-empty file is indistinguishable
		// from a truncated write and must not silently degrade to defaults.
		// For legacy sources, an empty file is unusual but not worth failing
		// the entire agent over; fall back to embedded base.
		if sourceKind == claudeSettingsSourceCityDotClaude {
			return nil, claudeSettingsSourceNone, fmt.Errorf("empty Claude settings from %s (file present but zero bytes)", overridePath)
		}
		return base, claudeSettingsSourceNone, nil
	}

	merged, err := overlay.MergeSettingsJSON(base, overrideData)
	if err != nil {
		return nil, claudeSettingsSourceNone, fmt.Errorf("merging Claude settings from %s: %w", overridePath, err)
	}
	return merged, sourceKind, nil
}

func readClaudeSettingsOverride(fs fsys.FS, cityDir string, base []byte) (string, []byte, claudeSettingsSourceKind, error) {
	// Preferred source (.claude/settings.json): a present-but-unreadable
	// file is a hard error. Falling back silently to a legacy source the
	// user did not intend would ship the wrong --settings.
	preferredPath := citylayout.ClaudeSettingsPath(cityDir)
	preferredState, preferredData, preferredErr := readClaudeSettingsCandidate(fs, preferredPath)
	switch preferredState {
	case candidateFound:
		return preferredPath, preferredData, claudeSettingsSourceCityDotClaude, nil
	case candidateUnreadable:
		return "", nil, claudeSettingsSourceNone, fmt.Errorf("reading %s: %w", preferredPath, preferredErr)
	}

	// Legacy candidates. A genuinely missing file is fine — fall through.
	// An exists-but-unreadable hooks/claude.json must NOT silently demote
	// to .gc/settings.json, or a stale runtime file could override a
	// user-owned hook file that gc simply couldn't read this tick. Fall
	// back to embedded base defaults instead.
	//
	// An unreadable .gc/settings.json does NOT block hook precedence —
	// the runtime file is gc-managed, not user-owned, so treating it as
	// "missing" when unreadable is equivalent to "gc will overwrite
	// whatever's there." A valid hooks/claude.json should still win.
	hookPath := citylayout.ClaudeHookFilePath(cityDir)
	runtimePath := filepath.Join(cityDir, ".gc", "settings.json")
	hookState, hookData, _ := readClaudeSettingsCandidate(fs, hookPath)
	runtimeState, runtimeData, _ := readClaudeSettingsCandidate(fs, runtimePath)

	if hookState == candidateUnreadable {
		return "", nil, claudeSettingsSourceNone, nil
	}

	// hooks/claude.json is authoritative when it exists, is not a known
	// stale auto-generated file, and differs from the managed runtime file
	// (the redundant-mirror case). We deliberately do NOT disqualify a
	// hook file whose bytes equal the embedded base: a user may pin
	// hooks/claude.json to exactly the embedded defaults as their
	// authoritative source and still expect it to outrank .gc/settings.json
	// per the documented precedence. Stale-pattern detection alone
	// distinguishes gc-generated from user-authored.
	hookExists := hookState == candidateFound
	runtimeExists := runtimeState == candidateFound
	if hookExists &&
		(!runtimeExists || !bytes.Equal(hookData, runtimeData)) &&
		!claudeFileNeedsUpgrade(hookData) {
		return hookPath, hookData, claudeSettingsSourceLegacyHook, nil
	}
	if runtimeExists &&
		!bytes.Equal(runtimeData, base) &&
		!claudeFileNeedsUpgrade(runtimeData) {
		return runtimePath, runtimeData, claudeSettingsSourceLegacyRuntime, nil
	}
	return "", nil, claudeSettingsSourceNone, nil
}

type claudeCandidateState int

const (
	candidateMissing claudeCandidateState = iota
	candidateFound
	candidateUnreadable
)

// readClaudeSettingsCandidate reads a candidate settings file and reports
// one of three states. Callers decide strictness: the preferred source
// surfaces candidateUnreadable as a hard error; legacy sources use it to
// block silent fallback to a lower-priority source.
//
// A read error that wraps os.ErrNotExist reports candidateMissing (matches
// both real OS filesystems and the test Fake). Any other read error
// reports candidateUnreadable with the original error returned.
func readClaudeSettingsCandidate(fs fsys.FS, path string) (claudeCandidateState, []byte, error) {
	data, err := fs.ReadFile(path)
	if err == nil {
		return candidateFound, data, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return candidateMissing, nil, nil
	}
	return candidateUnreadable, nil, err
}

func writeManagedFile(fs fsys.FS, dst string, data []byte, policy writeManagedFilePolicy) error {
	existing, readErr := fs.ReadFile(dst)
	if readErr == nil && bytes.Equal(existing, data) {
		return nil
	}
	if readErr != nil {
		if _, statErr := fs.Stat(dst); statErr == nil && policy == preserveUnreadable {
			// File exists but isn't readable. For user-owned paths, preserve
			// rather than clobbering. gc-owned paths fall through and attempt
			// the write (a write failure surfaces an error to the caller).
			return nil
		}
	}

	dir := filepath.Dir(dst)
	if err := fs.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	if err := fs.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}

	// If we just force-overwrote a previously-unreadable gc-owned file,
	// os.WriteFile preserved its restrictive mode and Claude still can't
	// open --settings. Add ONLY the owner-read bit to the existing mode,
	// preserving any user-tightened permissions (e.g. 0o600 for privacy)
	// and leaving fresh-install files at whatever perm-&-umask produced.
	if policy == forceOverwrite && readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		info, err := fs.Stat(dst)
		if err != nil {
			return fmt.Errorf("stat %s: %w", dst, err)
		}
		currentMode := info.Mode().Perm()
		if currentMode&0o400 == 0 {
			if err := fs.Chmod(dst, currentMode|0o400); err != nil {
				return fmt.Errorf("chmod %s: %w", dst, err)
			}
		}
	}
	return nil
}

func claudeFileNeedsUpgrade(existing []byte) bool {
	current, err := readEmbedded()
	if err != nil {
		return false
	}
	// The pattern uses JSON-escaped quotes to match how the string appears
	// in the embedded file bytes. Without the escapes, strings.Replace
	// finds nothing and stale == current — which silently flags every
	// base-equal file as "needs upgrade" and masks any precedence logic
	// that depends on this predicate.
	stale := strings.Replace(string(current), `gc handoff \"context cycle\"`, `gc prime --hook`, 1)
	return string(existing) == stale
}
