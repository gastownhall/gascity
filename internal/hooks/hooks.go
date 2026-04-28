// Package hooks installs provider-specific agent hook files into working
// directories. Each provider (Claude, Codex, Gemini, OpenCode, Copilot, etc.)
// has its own file format and install location. Hook files are embedded at build time
// and written idempotently — existing files are never overwritten.
package hooks

import (
	"bytes"
	"embed"
	"encoding/json"
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

//go:embed config/*
var configFS embed.FS

// supported lists provider names that have hook support.
var supported = []string{"claude", "codex", "gemini", "opencode", "copilot", "cursor", "pi", "omp"}

// unsupported lists provider names that have no hook mechanism.
var unsupported = []string{"amp", "auggie"}

// SupportedProviders returns the list of provider names with hook support.
func SupportedProviders() []string {
	out := make([]string, len(supported))
	copy(out, supported)
	return out
}

// FamilyResolver maps a raw provider name (which may be a custom wrapper
// alias like "my-fast-claude") to its built-in family name (e.g. "claude").
// A nil resolver (or one that returns "") is treated as identity: the raw
// name is used verbatim for the switch lookup. Provided so callers holding
// a city-providers map can route wrapped aliases to their ancestor's hook
// format without pulling the config package into hooks.
type FamilyResolver func(name string) string

// resolveFamily applies fn to name, falling back to name itself when fn
// is nil or returns "". The identity fallback preserves Install/Validate's
// existing contract for callers that pass raw built-in names directly.
func resolveFamily(fn FamilyResolver, name string) string {
	if fn == nil {
		return name
	}
	if family := fn(name); family != "" {
		return family
	}
	return name
}

// Validate checks that all provider names are supported for hook installation.
// Returns an error listing any unsupported names.
func Validate(providers []string) error {
	return ValidateWithResolver(providers, nil)
}

// ValidateWithResolver is Validate with a FamilyResolver so callers that
// hold city-provider inheritance context can validate wrapped custom
// aliases against the resolved built-in family (e.g. a custom
// "my-fast-claude" with base = "builtin:claude" validates as claude-
// family). Passing a nil resolver is equivalent to Validate.
func ValidateWithResolver(providers []string, resolve FamilyResolver) error {
	sup := make(map[string]bool, len(supported))
	for _, s := range supported {
		sup[s] = true
	}
	noHook := make(map[string]bool, len(unsupported))
	for _, u := range unsupported {
		noHook[u] = true
	}
	var bad []string
	for _, p := range providers {
		family := resolveFamily(resolve, p)
		if sup[family] {
			continue
		}
		if noHook[family] {
			bad = append(bad, fmt.Sprintf("%s (no hook mechanism)", p))
		} else {
			bad = append(bad, fmt.Sprintf("%s (unknown)", p))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("unsupported install_agent_hooks: %s; supported: %s",
			strings.Join(bad, ", "), strings.Join(supported, ", "))
	}
	return nil
}

// Install writes hook files for the given providers. cityDir is the city root
// (used for city-wide files like Claude settings). workDir is the agent's
// working directory (used for per-project files like Gemini, OpenCode, Copilot).
// Idempotent — existing files are not overwritten.
func Install(fs fsys.FS, cityDir, workDir string, providers []string) error {
	return InstallWithResolver(fs, cityDir, workDir, providers, nil)
}

// InstallWithResolver is Install with a FamilyResolver so callers that
// hold city-provider inheritance context can route wrapped custom
// aliases to their resolved built-in hook handler (e.g. "my-fast-claude"
// with base = "builtin:claude" installs claude-style hooks). Passing a
// nil resolver is equivalent to Install.
func InstallWithResolver(fs fsys.FS, cityDir, workDir string, providers []string, resolve FamilyResolver) error {
	for _, p := range providers {
		family := resolveFamily(resolve, p)
		var err error
		switch family {
		case "claude":
			err = installClaude(fs, cityDir)
		case "codex", "gemini", "opencode", "copilot", "cursor", "pi", "omp":
			err = installOverlayManaged(fs, workDir, family)
		default:
			return fmt.Errorf("unsupported hook provider %q", p)
		}
		if err != nil {
			return fmt.Errorf("installing %s hooks: %w", p, err)
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
		if provider == "codex" && rel == path.Join(".codex", "hooks.json") {
			return writeCodexHooksManaged(fs, dst, data)
		}
		return writeEmbeddedManaged(fs, dst, data, overlayManagedNeedsUpgrade(provider, rel))
	})
}

func overlayManagedNeedsUpgrade(provider, rel string) func([]byte) bool {
	if provider == "pi" && rel == path.Join(".pi", "extensions", "gc-hooks.js") {
		return piHookNeedsUpgrade
	}
	return nil
}

func piHookNeedsUpgrade(existing []byte) bool {
	content := string(existing)
	if !strings.Contains(content, "Gas City hooks for Pi Coding Agent") {
		return false
	}
	for _, marker := range []string{
		"module.exports = {",
		`"session.created"`,
		`"session.compacted"`,
		`"session.deleted"`,
		`"experimental.chat.system.transform"`,
	} {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

// installClaude writes the runtime settings file (.gc/settings.json) in the
// city directory. The legacy hooks/claude.json file remains user-owned unless
// gc can prove it is safe to update a stale generated copy.
//
// Source precedence for user-authored Claude settings:
//  1. <city>/.claude/settings.json
//  2. <city>/hooks/claude.json
//  3. <city>/.gc/settings.json
//
// The selected source is merged over embedded defaults so new default hooks
// still land for users with custom settings.
func installClaude(fs fsys.FS, cityDir string) error {
	hookDst := filepath.Join(cityDir, citylayout.ClaudeHookFile)
	runtimeDst := filepath.Join(cityDir, ".gc", "settings.json")
	data, sourceKind, err := desiredClaudeSettings(fs, cityDir)
	if err != nil {
		return err
	}

	if sourceKind == claudeSettingsSourceLegacyHook || isStaleHookFile(fs, hookDst) {
		if err := writeManagedFile(fs, hookDst, data, preserveUnreadable); err != nil {
			return err
		}
	}
	return writeManagedFile(fs, runtimeDst, data, forceOverwrite)
}

type writeManagedFilePolicy int

const (
	preserveUnreadable writeManagedFilePolicy = iota
	forceOverwrite
)

func isStaleHookFile(fs fsys.FS, hookDst string) bool {
	data, err := fs.ReadFile(hookDst)
	if err != nil {
		return false
	}
	return claudeFileNeedsUpgrade(data)
}

func readEmbedded(embedPath ...string) ([]byte, error) {
	path := "config/claude.json"
	if len(embedPath) > 0 && embedPath[0] != "" {
		path = embedPath[0]
	}
	data, err := configFS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading embedded %s: %w", path, err)
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

func desiredClaudeSettings(fs fsys.FS, cityDir string) ([]byte, claudeSettingsSourceKind, error) {
	base, err := readEmbedded("config/claude.json")
	if err != nil {
		return nil, claudeSettingsSourceNone, err
	}

	overridePath, overrideData, sourceKind, err := readClaudeSettingsOverride(fs, cityDir, base)
	if err != nil {
		return nil, claudeSettingsSourceNone, err
	}
	if sourceKind == claudeSettingsSourceNone {
		return base, claudeSettingsSourceNone, nil
	}
	if len(overrideData) == 0 {
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
	preferredPath := citylayout.ClaudeSettingsPath(cityDir)
	preferredState, preferredData, preferredErr := readClaudeSettingsCandidate(fs, preferredPath)
	switch preferredState {
	case candidateFound:
		return preferredPath, preferredData, claudeSettingsSourceCityDotClaude, nil
	case candidateUnreadable:
		return "", nil, claudeSettingsSourceNone, fmt.Errorf("reading %s: %w", preferredPath, preferredErr)
	}

	hookPath := citylayout.ClaudeHookFilePath(cityDir)
	runtimePath := filepath.Join(cityDir, ".gc", "settings.json")
	hookState, hookData, _ := readClaudeSettingsCandidate(fs, hookPath)
	runtimeState, runtimeData, _ := readClaudeSettingsCandidate(fs, runtimePath)

	if hookState == candidateUnreadable {
		return "", nil, claudeSettingsSourceNone, nil
	}

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

func writeCodexHooksManaged(fs fsys.FS, dst string, data []byte) error {
	if existing, err := fs.ReadFile(dst); err == nil {
		upgraded, changed, upgradeErr := upgradeCodexHookCommands(existing)
		if upgradeErr != nil || !changed {
			return nil
		}
		return writeManagedData(fs, dst, upgraded)
	} else if _, statErr := fs.Stat(dst); statErr == nil {
		return nil
	}
	return writeManagedData(fs, dst, data)
}

func writeManagedData(fs fsys.FS, dst string, data []byte) error {
	dir := filepath.Dir(dst)
	if err := fs.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := fs.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	return nil
}

func upgradeCodexHookCommands(existing []byte) ([]byte, bool, error) {
	var root any
	if err := json.Unmarshal(existing, &root); err != nil {
		return nil, false, err
	}
	if !upgradeCodexHookValue(root) {
		return nil, false, nil
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(data, '\n'), true, nil
}

func upgradeCodexHookValue(v any) bool {
	switch node := v.(type) {
	case map[string]any:
		changed := false
		for key, val := range node {
			if key == "command" {
				if command, ok := val.(string); ok {
					if upgraded, didUpgrade := upgradeCodexHookCommand(command); didUpgrade {
						node[key] = upgraded
						changed = true
					}
				}
				continue
			}
			if upgradeCodexHookValue(val) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, elem := range node {
			if upgradeCodexHookValue(elem) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

func upgradeCodexHookCommand(command string) (string, bool) {
	if strings.Contains(command, `--hook-format codex`) {
		return "", false
	}
	for _, needle := range []string{
		`gc prime --hook`,
		`gc nudge drain --inject`,
		`gc mail check --inject`,
		`gc hook --inject`,
	} {
		if strings.Contains(command, needle) {
			return strings.Replace(command, needle, needle+` --hook-format codex`, 1), true
		}
	}
	return "", false
}

func writeManagedFile(fs fsys.FS, dst string, data []byte, policy writeManagedFilePolicy) error {
	existing, readErr := fs.ReadFile(dst)
	if readErr == nil && bytes.Equal(existing, data) {
		return nil
	}
	if readErr != nil {
		if _, statErr := fs.Stat(dst); statErr == nil && policy == preserveUnreadable {
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
	current, err := readEmbedded("config/claude.json")
	if err != nil {
		return false
	}
	transforms := []func(string) string{
		func(s string) string {
			return strings.Replace(s, `gc handoff \"context cycle\"`, `gc prime --hook`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc prime --hook`, `gc prime --hook`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"matcher": "startup"`, `"matcher": ""`, 1)
		},
	}

	known := make(map[string]struct{})
	var enumerate func(int, string, bool)
	enumerate = func(idx int, candidate string, changed bool) {
		if idx == len(transforms) {
			if changed {
				known[candidate] = struct{}{}
			}
			return
		}
		enumerate(idx+1, candidate, changed)
		next := transforms[idx](candidate)
		enumerate(idx+1, next, changed || next != candidate)
	}
	enumerate(0, string(current), false)

	_, ok := known[string(existing)]
	return ok
}
