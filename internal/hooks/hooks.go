// Package hooks installs provider-specific agent hook files into working
// directories. Each provider (Claude, Codex, Gemini, Antigravity, OpenCode, Copilot, etc.)
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
	"regexp"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/shellquote"
)

//go:embed config/*
var configFS embed.FS

// supported lists provider names that have hook support wired into
// Gas Town's installer.
var supported = []string{"claude", "codex", "gemini", "antigravity", "kiro", "opencode", "mimocode", "groq", "cerebras", "copilot", "cursor", "pi", "omp", "kimi"}

const (
	managedPiHookVersion       = 7
	managedOpenCodeHookVersion = 5
	managedMimoCodeHookVersion = 2
	managedOmpHookVersion      = 2
)

var (
	piHookVersionPattern       = regexp.MustCompile(`\bGC_PI_HOOK_VERSION\s*=\s*([0-9]+)\b`)
	opencodeHookVersionPattern = regexp.MustCompile(`\bGC_OPENCODE_HOOK_VERSION\s*=\s*([0-9]+)\b`)
	mimocodeHookVersionPattern = regexp.MustCompile(`\bGC_MIMOCODE_HOOK_VERSION\s*=\s*([0-9]+)\b`)
	ompHookVersionPattern      = regexp.MustCompile(`\bGC_OMP_HOOK_VERSION\s*=\s*([0-9]+)\b`)
)

// unwiredHookProviders lists provider names whose own CLIs do expose a
// hook mechanism (per upstream documentation) but for which Gas Town
// has not yet wired hook installation. Tracked as gap 4 of the
// non-Claude provider parity audit (gastownhall/gascity#672):
//
//   - amp: Sourcegraph Amp CLI exposes a plugin system with
//     session.start and tool.call events
//     (https://ampcode.com/manual, Plugin events).
//   - auggie: Augment Auggie CLI exposes SessionStart, SessionEnd,
//     Stop, PreToolUse, PostToolUse hooks configured globally in
//     ~/.augment/settings.json (https://docs.augmentcode.com/cli/overview).
//
// Listing them here lets Validate emit an accurate "hooks not yet
// wired" error rather than the historical "no hook mechanism" claim,
// which is factually wrong against current provider docs.
//
// Nudge delivery is unaffected: the supervisor-hosted dispatcher and
// the legacy per-session poller (cmd/gc/cmd_nudge.go) both deliver
// queued nudges via the worker.Handle abstraction without requiring
// provider hooks, so amp and auggie sessions still drain queued
// nudges. The remaining work is event-driven coordination
// (session-start priming, pre-compaction handoff).
var unwiredHookProviders = []string{"amp", "auggie"}

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
	unwired := make(map[string]bool, len(unwiredHookProviders))
	for _, u := range unwiredHookProviders {
		unwired[u] = true
	}
	var bad []string
	for _, p := range providers {
		family := resolveFamily(resolve, p)
		if sup[family] {
			continue
		}
		if unwired[family] {
			bad = append(bad, fmt.Sprintf("%s (hooks not yet wired; see gastownhall/gascity#672 gap 4)", p))
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
		case "codex", "gemini", "antigravity", "kiro", "opencode", "mimocode", "copilot", "cursor", "pi", "omp", "kimi":
			err = installOverlayManaged(fs, cityDir, workDir, family)
		case "groq", "cerebras":
			err = installOverlayManaged(fs, cityDir, workDir, "opencode")
		default:
			return fmt.Errorf("unsupported hook provider %q", p)
		}
		if err != nil {
			return fmt.Errorf("installing %s hooks: %w", p, err)
		}
	}
	return nil
}

func installOverlayManaged(fs fsys.FS, cityDir, workDir, provider string) error {
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
		if provider == "antigravity" && rel == path.Join(".agents", "hooks.json") {
			return writeJSONOverlayManaged(fs, dst, data)
		}
		if provider == "codex" && rel == path.Join(".codex", "hooks.json") {
			return writeCodexHooksManaged(fs, cityDir, dst, data)
		}
		if overlay.IsMergeablePath(filepath.FromSlash(rel)) {
			if normalized, normErr := overlay.CanonicalJSON(data); normErr == nil {
				data = normalized
			}
		}
		return writeEmbeddedManaged(fs, dst, data, overlayManagedNeedsUpgrade(provider, rel))
	})
}

func writeJSONOverlayManaged(fs fsys.FS, dst string, data []byte) error {
	if existing, err := fs.ReadFile(dst); err == nil {
		merged, mergeErr := overlay.MergeSettingsJSON(existing, data)
		if mergeErr != nil {
			return fmt.Errorf("merging %s: %w", dst, mergeErr)
		}
		if bytes.Equal(merged, existing) {
			return nil
		}
		return writeManagedData(fs, dst, merged)
	} else if _, statErr := fs.Stat(dst); statErr == nil {
		return nil
	}
	if normalized, err := overlay.CanonicalJSON(data); err == nil {
		data = normalized
	}
	return writeManagedData(fs, dst, data)
}

func overlayManagedNeedsUpgrade(provider, rel string) func([]byte) bool {
	if provider == "pi" && rel == path.Join(".pi", "extensions", "gc-hooks.js") {
		return piHookNeedsUpgrade
	}
	if provider == "opencode" && rel == path.Join(".opencode", "plugins", "gascity.js") {
		return opencodeHookNeedsUpgrade
	}
	if provider == "mimocode" && rel == path.Join(".mimocode", "plugin", "gascity.js") {
		return mimocodeHookNeedsUpgrade
	}
	if provider == "omp" && rel == path.Join(".omp", "hooks", "gc-hook.ts") {
		return ompHookNeedsUpgrade
	}
	return nil
}

func piHookNeedsUpgrade(existing []byte) bool {
	content := string(existing)
	if !strings.Contains(content, "Gas City hooks for Pi Coding Agent") {
		return false
	}
	if piHookVersion(content) < managedPiHookVersion ||
		!strings.Contains(content, "gc prime --hook") ||
		!strings.Contains(content, "gc hook --inject") ||
		!strings.Contains(content, "gc handoff --auto") ||
		!strings.Contains(content, "mirrorTempCounter") ||
		!strings.Contains(content, "GC_PROVIDER_SESSION_ID") ||
		!strings.Contains(content, "GC_PROVIDER_SESSION_ID_REQUIRED") ||
		!strings.Contains(content, `stdio: ["ignore", "pipe", "inherit"]`) {
		return true
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

func piHookVersion(content string) int {
	match := piHookVersionPattern.FindStringSubmatch(content)
	if len(match) != 2 {
		return 0
	}
	version, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return version
}

func opencodeHookNeedsUpgrade(existing []byte) bool {
	content := string(existing)
	if !strings.Contains(content, "Gas City hooks for OpenCode.") {
		return false
	}
	if opencodeHookVersion(content) < managedOpenCodeHookVersion ||
		!strings.Contains(content, `process.env.GC_BIN || "gc"`) ||
		!strings.Contains(content, `/opt/homebrew/bin:/usr/local/bin:${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`) ||
		!strings.Contains(content, `"experimental.session.compacting"`) ||
		!strings.Contains(content, `runWithWarning(directory, "handoff", "--auto", "context cycle")`) ||
		!strings.Contains(content, "output.context.push(handoff)") ||
		!strings.Contains(content, "logRunFailure") ||
		!strings.Contains(content, "logRunStderr(stderr);") ||
		!strings.Contains(content, "GC_PROVIDER_SESSION_ID") ||
		!strings.Contains(content, "GC_PROVIDER_SESSION_ID_REQUIRED") {
		return true
	}
	for _, marker := range []string{
		`run(directory, "handoff", "context cycle")`,
		`"session", "reset"`,
		`"session.deleted"`,
	} {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

func opencodeHookVersion(content string) int {
	match := opencodeHookVersionPattern.FindStringSubmatch(content)
	if len(match) != 2 {
		return 0
	}
	version, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return version
}

// mimocodeHookNeedsUpgrade reports whether an existing managed MiMo Code
// plugin predates the current managed version. Files without the managed
// header are user-authored and never upgraded.
func mimocodeHookNeedsUpgrade(existing []byte) bool {
	content := string(existing)
	if !strings.Contains(content, "Gas City hooks for MiMo Code.") {
		return false
	}
	return mimocodeHookVersion(content) < managedMimoCodeHookVersion
}

func mimocodeHookVersion(content string) int {
	match := mimocodeHookVersionPattern.FindStringSubmatch(content)
	if len(match) != 2 {
		return 0
	}
	version, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return version
}

func ompHookNeedsUpgrade(existing []byte) bool {
	content := string(existing)
	if !strings.Contains(content, "Gas City hooks for Oh My Pi (OMP).") {
		return false
	}
	if ompHookVersion(content) < managedOmpHookVersion ||
		!strings.Contains(content, "gascityOmpExtension") ||
		!strings.Contains(content, "GC_PROVIDER_SESSION_ID") ||
		!strings.Contains(content, "GC_PROVIDER_SESSION_ID_REQUIRED") ||
		!strings.Contains(content, `pi.on("session_start"`) ||
		!strings.Contains(content, `pi.on("session_compact"`) ||
		!strings.Contains(content, `pi.on("before_agent_start"`) ||
		!strings.Contains(content, "logRunFailure") ||
		!strings.Contains(content, `stdio: ["ignore", "pipe", "inherit"]`) {
		return true
	}
	for _, marker := range []string{
		"export default {",
		`"session.created"`,
		`"session.compacted"`,
		`"experimental.chat.system.transform"`,
	} {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

func ompHookVersion(content string) int {
	match := ompHookVersionPattern.FindStringSubmatch(content)
	if len(match) != 2 {
		return 0
	}
	version, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return version
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
	var backup []byte
	if existing, err := fs.ReadFile(dst); err == nil {
		if needsUpgrade == nil || !needsUpgrade(existing) {
			return nil
		}
		backup = append([]byte(nil), existing...)
	} else if _, statErr := fs.Stat(dst); statErr == nil {
		// File exists but isn't readable. Preserve it rather than clobbering it.
		return nil
	}

	dir := filepath.Dir(dst)
	if err := fs.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if backup != nil {
		backupPath, err := nextManagedBackupPath(fs, dst)
		if err != nil {
			return err
		}
		if err := fs.WriteFile(backupPath, backup, 0o644); err != nil {
			return fmt.Errorf("backing up %s to %s: %w", dst, backupPath, err)
		}
	}

	if err := fs.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	return nil
}

func nextManagedBackupPath(fs fsys.FS, dst string) (string, error) {
	base := dst + ".bak"
	for i := 0; ; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s.%d", base, i)
		}
		if _, err := fs.Stat(candidate); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return candidate, nil
			}
			return "", fmt.Errorf("checking backup %s: %w", candidate, err)
		}
	}
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

	// Apply targeted in-place upgrades to legacy forms of managed gascity
	// hook commands and matchers in the user's override before merging
	// with the embedded base. Custom hook events and custom commands are
	// preserved semantically: command strings and hook entries are not
	// modified, though MarshalCanonicalJSON may re-order keys or arrays
	// when an upgrade rewrite is applied. The previous "use base instead"
	// path discarded user customizations along with stale managed-hook
	// bytes; this path patches the managed bytes while keeping
	// customizations intact.
	upgradedOverride, _, upgradeErr := upgradeClaudeFile(overrideData)
	if upgradeErr != nil {
		// Distinguish a malformed user file from a gascity-side
		// MarshalCanonicalJSON failure. JSON parse errors point at the
		// user's override; the canonical recovery is to skip the merge
		// and surface a clear, actionable error that names the file —
		// previously this path silently re-assigned the malformed bytes
		// and crashed downstream with a cryptic "merging ... : invalid
		// character" error from MergeSettingsJSON. Marshal failures
		// shouldn't happen on user data (we already parsed it
		// successfully above) so they indicate a gascity bug worth
		// surfacing too. See gastownhall/gascity#2109.
		var syntaxErr *json.SyntaxError
		if errors.As(upgradeErr, &syntaxErr) {
			return nil, claudeSettingsSourceNone, fmt.Errorf("invalid Claude settings override at %s: invalid JSON; fix or remove the file to proceed with install: %w", overridePath, upgradeErr)
		}
		return nil, claudeSettingsSourceNone, fmt.Errorf("upgrading Claude settings from %s: %w", overridePath, upgradeErr)
	}

	merged, err := overlay.MergeSettingsJSON(base, upgradedOverride, overlay.WithWrapBareHooks())
	if err != nil {
		if overlay.IsOverlayObjectShapeError(err) {
			return nil, claudeSettingsSourceNone, fmt.Errorf("invalid Claude settings override at %s: Claude settings override is not a JSON object; expected a JSON object; fix or remove the file to proceed with install: %w", overridePath, err)
		}
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
	// The previous !claudeFileNeedsUpgrade gates here forced cities whose
	// settings.json had stale managed-hook commands AND user customizations
	// to fall through to the "use base" branch, silently discarding their
	// customizations. desiredClaudeSettings now patches stale managed
	// commands in-place via upgradeClaudeFile before merging with base, so
	// customizations survive while managed commands get upgraded.
	if hookExists && (!runtimeExists || !bytes.Equal(hookData, runtimeData)) {
		return hookPath, hookData, claudeSettingsSourceLegacyHook, nil
	}
	if runtimeExists && !bytes.Equal(runtimeData, base) {
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

func writeCodexHooksManaged(fs fsys.FS, cityDir, dst string, data []byte) error {
	if normalized, _, err := normalizeCodexHookCommands(data, cityDir); err == nil {
		data = normalized
	}
	if existing, err := fs.ReadFile(dst); err == nil {
		upgraded, changed, upgradeErr := upgradeCodexHooks(existing, data, cityDir)
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

func upgradeCodexHooks(existing, desired []byte, cityDir string) ([]byte, bool, error) {
	var root any
	if err := json.Unmarshal(existing, &root); err != nil {
		return nil, false, err
	}
	hasManagedCommand := codexHookValueHasManagedCommand(root, "")
	needsPreCompact := codexHookDocCanAddPreCompact(root)
	changed := upgradeCodexHookValue(root, "", cityDir)
	if desiredCodexPreCompactHook(desired) != nil && normalizeCodexManagedHookEntries(root, cityDir) {
		changed = true
	}
	if addCodexPreCompactHook(root, desired) {
		changed = true
	}
	data, err := overlay.MarshalCanonicalJSON(root)
	if err != nil {
		return nil, false, err
	}
	if hasManagedCommand && !needsPreCompact && !bytes.Equal(data, existing) {
		changed = true
	}
	return data, changed, nil
}

func normalizeCodexHookCommands(existing []byte, cityDir string) ([]byte, bool, error) {
	var root any
	if err := json.Unmarshal(existing, &root); err != nil {
		return nil, false, err
	}
	hasManagedCommand := codexHookValueHasManagedCommand(root, "")
	changed := upgradeCodexHookValue(root, "", cityDir)
	if normalizeCodexManagedHookEntries(root, cityDir) {
		changed = true
	}
	data, err := overlay.MarshalCanonicalJSON(root)
	if err != nil {
		return nil, false, err
	}
	if hasManagedCommand && !bytes.Equal(data, existing) {
		changed = true
	}
	return data, changed, nil
}

// CodexHooksMissingManagedPreCompact reports whether data is a Gas City
// managed Codex hooks document that can be upgraded with a PreCompact hook.
func CodexHooksMissingManagedPreCompact(data []byte) bool {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return false
	}
	return codexHookDocCanAddPreCompact(root)
}

// CodexHooksNeedManagedUpgrade reports whether data is a recognizable Gas City
// managed Codex hooks document that would be upgraded to current managed form
// for cityDir, including explicit --city rebinding and missing PreCompact.
func CodexHooksNeedManagedUpgrade(data []byte, cityDir string) bool {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return false
	}
	return applyCodexManagedHookUpgrade(root, nil, cityDir)
}

func applyCodexManagedHookUpgrade(root any, desired []byte, cityDir string) bool {
	changed := upgradeCodexHookValue(root, "", cityDir)
	if addCodexPreCompactHook(root, desired) {
		changed = true
	}
	return changed
}

func codexHookValueHasManagedCommand(v any, event string) bool {
	switch node := v.(type) {
	case map[string]any:
		for key, val := range node {
			if key == "hooks" {
				if hooksMap, ok := val.(map[string]any); ok {
					for eventName, eventVal := range hooksMap {
						if codexHookValueHasManagedCommand(eventVal, eventName) {
							return true
						}
					}
					continue
				}
			}
			if key == "command" {
				if command, ok := val.(string); ok && codexHookCommandLooksManaged(event, command) {
					return true
				}
				continue
			}
			if codexHookValueHasManagedCommand(val, event) {
				return true
			}
		}
	case []any:
		for _, elem := range node {
			if codexHookValueHasManagedCommand(elem, event) {
				return true
			}
		}
	}
	return false
}

func upgradeCodexHookValue(v any, event, cityDir string) bool {
	switch node := v.(type) {
	case map[string]any:
		changed := false
		for key, val := range node {
			if key == "hooks" {
				if hooksMap, ok := val.(map[string]any); ok {
					for eventName, eventVal := range hooksMap {
						if upgradeCodexHookValue(eventVal, eventName, cityDir) {
							changed = true
						}
					}
					continue
				}
			}
			if key == "command" {
				if command, ok := val.(string); ok {
					if upgraded, didUpgrade := upgradeCodexHookCommand(event, command, cityDir); didUpgrade {
						node[key] = upgraded
						changed = true
					}
				}
				continue
			}
			if upgradeCodexHookValue(val, event, cityDir) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, elem := range node {
			if upgradeCodexHookValue(elem, event, cityDir) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

func normalizeCodexManagedHookEntries(root any, cityDir string) bool {
	doc, ok := root.(map[string]any)
	if !ok {
		return false
	}
	hooksMap, ok := doc["hooks"].(map[string]any)
	if !ok {
		return false
	}
	changed := false
	for event, entriesValue := range hooksMap {
		entries, ok := entriesValue.([]any)
		if !ok {
			continue
		}
		normalized := entries[:0]
		seenManaged := map[string]bool{}
		for _, entry := range entries {
			if event == "SessionStart" {
				if normalizeCodexManagedSessionStartEntry(entry, cityDir) {
					changed = true
				}
			}
			if codexHookValueHasManagedCommand(entry, event) {
				keyData, err := overlay.MarshalCanonicalJSON(entry)
				if err == nil {
					key := string(keyData)
					if seenManaged[key] {
						changed = true
						continue
					}
					seenManaged[key] = true
				}
			}
			normalized = append(normalized, entry)
		}
		if len(normalized) != len(entries) {
			hooksMap[event] = normalized
		}
	}
	return changed
}

func normalizeCodexManagedSessionStartEntry(entry any, cityDir string) bool {
	entryMap, ok := entry.(map[string]any)
	if !ok || !codexHookEntryHasCommandBody(entryMap, sessionStartCurrentFormBody(cityDir)) {
		return false
	}
	if matcher, ok := entryMap["matcher"].(string); !ok || matcher != "startup" {
		entryMap["matcher"] = "startup"
		return true
	}
	return false
}

func codexHookEntryHasCommandBody(entry map[string]any, body string) bool {
	hooksValue, ok := entry["hooks"].([]any)
	if !ok {
		return false
	}
	for _, hookValue := range hooksValue {
		hookMap, ok := hookValue.(map[string]any)
		if !ok {
			continue
		}
		command, ok := hookMap["command"].(string)
		if !ok {
			continue
		}
		if commandBodyAfterCanonicalPrefix(command) == body {
			return true
		}
	}
	return false
}

func codexHookCommandLooksManaged(event, command string) bool {
	_, env, args, ok := parseManagedGCCommand(command)
	if !ok {
		return false
	}
	switch event {
	case "SessionStart":
		return codexSessionStartArgsMatch(env, args) || codexLegacySessionStartRunArgsMatch(args)
	case "PreCompact":
		return codexPreCompactArgsMatch(args)
	case "UserPromptSubmit":
		return codexManagedPromptArgsMatch(args, "codex")
	default:
		return codexSessionStartArgsMatch(env, args) ||
			codexLegacySessionStartRunArgsMatch(args) ||
			codexPreCompactArgsMatch(args) ||
			codexManagedPromptArgsMatch(args, "codex")
	}
}

func upgradeCodexHookCommand(event, command, cityDir string) (string, bool) {
	prefix, env, args, ok := parseManagedGCCommand(command)
	if !ok {
		return "", false
	}
	switch event {
	case "SessionStart":
		if !codexSessionStartArgsMatch(env, args) && !codexLegacySessionStartRunArgsMatch(args) {
			return "", false
		}
		desired := sessionStartCurrentFormBody(cityDir)
		return prefix + desired, strings.TrimPrefix(command, prefix) != desired
	case "PreCompact":
		if !codexPreCompactArgsMatch(args) {
			return "", false
		}
		desired := preCompactCurrentFormBody(cityDir)
		return prefix + desired, strings.TrimPrefix(command, prefix) != desired
	case "UserPromptSubmit":
		return upgradeManagedPromptHookCommand(command, "codex", cityDir)
	default:
		if upgraded, ok := upgradeManagedPromptHookCommand(command, "codex", cityDir); ok {
			return upgraded, true
		}
		if codexSessionStartArgsMatch(env, args) || codexLegacySessionStartRunArgsMatch(args) {
			desired := sessionStartCurrentFormBody(cityDir)
			return prefix + desired, strings.TrimPrefix(command, prefix) != desired
		}
		if codexPreCompactArgsMatch(args) {
			desired := preCompactCurrentFormBody(cityDir)
			return prefix + desired, strings.TrimPrefix(command, prefix) != desired
		}
		return "", false
	}
}

func managedPromptHookRunPrefix(cityDir string) string {
	return `gc ` + codexCityFlag(cityDir) + `hook run --timeout 15s --timeout-exit-code 0 -- `
}

func upgradeManagedPromptHookCommand(command, hookFormat, cityDir string) (string, bool) {
	prefix, _, args, ok := parseManagedGCCommand(command)
	if !ok {
		return "", false
	}
	target, ok := codexManagedPromptTargetArgs(args, hookFormat)
	if !ok {
		return "", false
	}
	desired := managedPromptHookRunPrefix(cityDir) + target
	return prefix + desired, strings.TrimPrefix(command, prefix) != desired
}

func codexCityFlag(cityDir string) string {
	cityDir = strings.TrimSpace(cityDir)
	if cityDir == "" {
		return ""
	}
	return `--city ` + shellquote.Quote(cityDir) + ` `
}

func isCodexSessionStartCommandBody(body string) bool {
	env, args, ok := parseGCCommandBody(body)
	if !ok {
		return false
	}
	if event, ok := env["GC_HOOK_EVENT_NAME"]; ok && event != "SessionStart" {
		return false
	}
	if len(args) == 2 && args[0] == "prime" && args[1] == "--hook" {
		return true
	}
	return len(args) == 4 && args[0] == "prime" && args[1] == "--hook" && args[2] == "--hook-format" && args[3] == "codex"
}

func isCodexPreCompactCommandBody(body string) bool {
	_, args, ok := parseGCCommandBody(body)
	if !ok || len(args) < 2 || args[0] != "handoff" {
		return false
	}
	switch {
	case len(args) == 2 && args[1] == "context cycle":
		return true
	case len(args) == 3 && args[1] == "--auto" && args[2] == "context cycle":
		return true
	case len(args) == 5 && args[1] == "--auto" && args[2] == "--hook-format" && args[3] == "codex" && args[4] == "context cycle":
		return true
	default:
		return false
	}
}

func codexManagedPromptTarget(body, hookFormat string) bool {
	_, args, ok := parseGCCommandBody(body)
	if !ok {
		return false
	}
	if len(args) >= 3 && args[0] == "nudge" && args[1] == "drain" && args[2] == "--inject" {
		_, ok := managedPromptTarget("nudge drain --inject", args[3:], hookFormat)
		return ok
	}
	if len(args) >= 3 && args[0] == "mail" && args[1] == "check" && args[2] == "--inject" {
		_, ok := managedPromptTarget("mail check --inject", args[3:], hookFormat)
		return ok
	}
	if len(args) < 8 || args[0] != "hook" || args[1] != "run" {
		return false
	}
	if args[2] != "--timeout" || args[3] != "15s" || args[4] != "--timeout-exit-code" || args[5] != "0" || args[6] != "--" {
		return false
	}
	targetArgs := args[7:]
	switch {
	case len(targetArgs) >= 3 && targetArgs[0] == "nudge" && targetArgs[1] == "drain" && targetArgs[2] == "--inject":
		_, ok := managedPromptTarget("nudge drain --inject", targetArgs[3:], hookFormat)
		return ok
	case len(targetArgs) >= 3 && targetArgs[0] == "mail" && targetArgs[1] == "check" && targetArgs[2] == "--inject":
		_, ok := managedPromptTarget("mail check --inject", targetArgs[3:], hookFormat)
		return ok
	default:
		return false
	}
}

func managedPromptTarget(base string, rest []string, hookFormat string) (string, bool) {
	if len(rest) == 0 {
		if hookFormat == "" {
			return base, true
		}
		return base + ` --hook-format ` + hookFormat, true
	}
	if hookFormat == "" {
		return "", false
	}
	if len(rest) == 2 && rest[0] == "--hook-format" && rest[1] == hookFormat {
		return base + ` --hook-format ` + hookFormat, true
	}
	return "", false
}

func parseGCCommandBody(body string) (map[string]string, []string, bool) {
	tokens := shellquote.Split(body)
	if len(tokens) == 0 {
		return nil, nil, false
	}
	env := map[string]string{}
	i := 0
	for i < len(tokens) && strings.Contains(tokens[i], "=") && !strings.HasPrefix(tokens[i], "=") {
		key, value, ok := strings.Cut(tokens[i], "=")
		if !ok || key == "" {
			break
		}
		if !isManagedGCCommandEnvKey(key) {
			return nil, nil, false
		}
		env[key] = value
		i++
	}
	if i >= len(tokens) || tokens[i] != "gc" {
		return nil, nil, false
	}
	args := tokens[i+1:]
	if len(args) >= 2 && args[0] == "--city" {
		args = args[2:]
	} else if len(args) >= 1 && strings.HasPrefix(args[0], "--city=") {
		args = args[1:]
	}
	return env, args, true
}

func isManagedGCCommandEnvKey(key string) bool {
	switch key {
	case "GC_MANAGED_SESSION_HOOK", "GC_HOOK_EVENT_NAME":
		return true
	default:
		return false
	}
}

func parseManagedGCCommand(command string) (string, map[string]string, []string, bool) {
	prefix := ""
	body := command
	if strings.HasPrefix(body, canonicalGCPathPrefix) {
		prefix = canonicalGCPathPrefix
		body = strings.TrimPrefix(body, canonicalGCPathPrefix)
	}
	tokens := shellquote.Split(body)
	if len(tokens) == 0 {
		return "", nil, nil, false
	}
	env := map[string]string{}
	var envTokens []string
	var extraEnvTokens []string
	i := 0
	hasManagedEnv := false
	for i < len(tokens) && strings.Contains(tokens[i], "=") && !strings.HasPrefix(tokens[i], "=") {
		key, value, ok := strings.Cut(tokens[i], "=")
		if !ok || key == "" {
			break
		}
		if isManagedGCCommandEnvKey(key) {
			hasManagedEnv = true
		} else {
			extraEnvTokens = append(extraEnvTokens, tokens[i])
		}
		env[key] = value
		envTokens = append(envTokens, tokens[i])
		i++
	}
	if i >= len(tokens) || tokens[i] != "gc" {
		return "", nil, nil, false
	}
	if len(envTokens) > 0 && prefix == "" && !hasManagedEnv {
		return "", nil, nil, false
	}
	if len(extraEnvTokens) > 0 {
		prefix += shellquote.Join(extraEnvTokens) + " "
	}
	args := tokens[i+1:]
	if len(args) >= 2 && args[0] == "--city" {
		args = args[2:]
	} else if len(args) >= 1 && strings.HasPrefix(args[0], "--city=") {
		args = args[1:]
	}
	return prefix, env, args, true
}

func codexSessionStartArgsMatch(env map[string]string, args []string) bool {
	if event, ok := env["GC_HOOK_EVENT_NAME"]; ok && event != "SessionStart" {
		return false
	}
	if len(args) == 2 && args[0] == "prime" && args[1] == "--hook" {
		return true
	}
	return len(args) == 4 && args[0] == "prime" && args[1] == "--hook" && args[2] == "--hook-format" && args[3] == "codex"
}

func codexLegacySessionStartRunArgsMatch(args []string) bool {
	if len(args) < 8 || args[0] != "hook" || args[1] != "run" {
		return false
	}
	if args[2] != "--timeout" || args[3] != "15s" || args[4] != "--timeout-exit-code" || args[5] != "0" || args[6] != "--" {
		return false
	}
	targetArgs := args[7:]
	return len(targetArgs) == 2 && targetArgs[0] == "prime" && targetArgs[1] == "--hook" ||
		(len(targetArgs) == 4 && targetArgs[0] == "prime" && targetArgs[1] == "--hook" && targetArgs[2] == "--hook-format" && targetArgs[3] == "codex")
}

func codexPreCompactArgsMatch(args []string) bool {
	if len(args) < 2 || args[0] != "handoff" {
		return false
	}
	switch {
	case len(args) == 2 && args[1] == "context cycle":
		return true
	case len(args) == 3 && args[1] == "--auto" && args[2] == "context cycle":
		return true
	case len(args) == 5 && args[1] == "--auto" && args[2] == "--hook-format" && args[3] == "codex" && args[4] == "context cycle":
		return true
	default:
		return false
	}
}

func codexManagedPromptArgsMatch(args []string, hookFormat string) bool {
	_, ok := codexManagedPromptTargetArgs(args, hookFormat)
	if ok {
		return true
	}
	if hookFormat != "" {
		_, ok = codexManagedPromptTargetArgs(args, "")
	}
	return ok
}

func codexManagedPromptTargetArgs(args []string, hookFormat string) (string, bool) {
	if len(args) >= 3 && args[0] == "nudge" && args[1] == "drain" && args[2] == "--inject" {
		return managedPromptTarget("nudge drain --inject", args[3:], hookFormat)
	}
	if len(args) >= 3 && args[0] == "mail" && args[1] == "check" && args[2] == "--inject" {
		return managedPromptTarget("mail check --inject", args[3:], hookFormat)
	}
	if len(args) < 8 || args[0] != "hook" || args[1] != "run" {
		return "", false
	}
	if args[2] != "--timeout" || args[3] != "15s" || args[4] != "--timeout-exit-code" || args[5] != "0" || args[6] != "--" {
		return "", false
	}
	targetArgs := args[7:]
	switch {
	case len(targetArgs) >= 3 && targetArgs[0] == "nudge" && targetArgs[1] == "drain" && targetArgs[2] == "--inject":
		return managedPromptTarget("nudge drain --inject", targetArgs[3:], hookFormat)
	case len(targetArgs) >= 3 && targetArgs[0] == "mail" && targetArgs[1] == "check" && targetArgs[2] == "--inject":
		return managedPromptTarget("mail check --inject", targetArgs[3:], hookFormat)
	default:
		return "", false
	}
}

func addCodexPreCompactHook(root any, desired []byte) bool {
	if !codexHookDocCanAddPreCompact(root) {
		return false
	}
	doc := root.(map[string]any)
	hooksMap := doc["hooks"].(map[string]any)
	preCompact := desiredCodexPreCompactHook(desired)
	if preCompact == nil {
		return false
	}
	hooksMap["PreCompact"] = preCompact
	return true
}

func codexHookDocCanAddPreCompact(root any) bool {
	doc, ok := root.(map[string]any)
	if !ok || !codexHookDocLooksManaged(doc) {
		return false
	}
	hooksMap, ok := doc["hooks"].(map[string]any)
	if !ok {
		return false
	}
	if _, exists := hooksMap["PreCompact"]; exists {
		return false
	}
	return true
}

func codexHookDocLooksManaged(doc map[string]any) bool {
	var found bool
	var walk func(any)
	walk = func(v any) {
		if found {
			return
		}
		switch node := v.(type) {
		case map[string]any:
			if hooksMap, ok := node["hooks"].(map[string]any); ok {
				for eventName, val := range hooksMap {
					if codexHookValueHasManagedCommand(val, eventName) {
						found = true
						return
					}
				}
			}
			for _, val := range node {
				walk(val)
			}
		case []any:
			for _, val := range node {
				walk(val)
			}
		}
	}
	walk(doc)
	return found
}

func desiredCodexPreCompactHook(desired []byte) any {
	if len(desired) == 0 {
		var err error
		desired, err = iofs.ReadFile(core.PackFS, path.Join("overlay", "per-provider", "codex", ".codex", "hooks.json"))
		if err != nil {
			return nil
		}
	}
	var doc struct {
		Hooks map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(desired, &doc); err != nil {
		return nil
	}
	return doc.Hooks["PreCompact"]
}

func writeManagedFile(fs fsys.FS, dst string, data []byte, policy writeManagedFilePolicy) error {
	if normalized, err := overlay.CanonicalJSON(data); err == nil {
		data = normalized
	}
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

// claudeFileNeedsUpgrade reports whether the existing settings.json contains
// known legacy forms of managed gascity hook commands or matchers that would
// be patched by upgradeClaudeFile. Used by isStaleHookFile to decide whether
// to overwrite the legacy hook-file path; readClaudeSettingsOverride no
// longer gates on this since desiredClaudeSettings applies the upgrade
// in-place before merge.
//
// The previous implementation enumerated 16 byte-exact transforms of the
// embedded template and matched the user's bytes against that set. Any
// custom addition (e.g. an extra Stop hook entry) defeated every variant
// match, so cities with customizations never received upstream fixes —
// most notably the PreCompact `--auto` patch from commit 7b3b913a, which
// landed weeks before this rewrite but never propagated to cities like
// pipex-city that had drifted from the canonical embedded shape.
func claudeFileNeedsUpgrade(existing []byte) bool {
	_, changed, err := upgradeClaudeFile(existing)
	if err != nil {
		return false
	}
	return changed
}

// upgradeClaudeFile parses the existing Claude settings.json and patches
// known legacy forms of managed gascity hook commands and matchers to their
// current shape. Walks the hook events so upgrades can be event-aware
// (e.g. SessionStart matcher upgrade, PreCompact command upgrade); custom
// hook events and custom commands are preserved semantically — their
// command strings and entry contents are untouched, though
// MarshalCanonicalJSON may reorder keys or arrays when an upgrade
// rewrite is applied.
//
// Returns the (possibly re-marshaled) JSON bytes and whether any patch
// was applied.
func upgradeClaudeFile(existing []byte) ([]byte, bool, error) {
	var root any
	if err := json.Unmarshal(existing, &root); err != nil {
		return nil, false, err
	}
	rootMap, ok := root.(map[string]any)
	if !ok {
		return existing, false, nil
	}
	hooks, ok := rootMap["hooks"].(map[string]any)
	if !ok {
		return existing, false, nil
	}
	changed := false
	for event, entries := range hooks {
		entriesArr, ok := entries.([]any)
		if !ok {
			continue
		}
		for _, entry := range entriesArr {
			entryMap, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if upgradeClaudeHookEntry(event, entryMap) {
				changed = true
			}
		}
	}
	if !changed {
		return existing, false, nil
	}
	data, err := overlay.MarshalCanonicalJSON(root)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// upgradeClaudeHookEntry applies event-aware upgrades to a single
// {matcher, hooks: [...]} entry under one of the hook event arrays.
//
// Upgrade applies only when the entry is identifiable as a GC-managed
// legacy entry — at least one hook command must match a known legacy
// form via isLegacyGCManagedCommand. User-authored entries that happen
// to share an empty matcher or a wrapper that prefixes "gc prime --hook"
// are left untouched.
func upgradeClaudeHookEntry(event string, entry map[string]any) bool {
	hookCmds, ok := entry["hooks"].([]any)
	if !ok {
		return false
	}

	// First pass: identify whether this entry has the GC-managed legacy
	// shape (via at least one recognizable legacy command body), and
	// upgrade any commands that match a known legacy form.
	changed := false
	hasManagedCommand := false
	for _, h := range hookCmds {
		hMap, ok := h.(map[string]any)
		if !ok {
			continue
		}
		cmd, ok := hMap["command"].(string)
		if !ok {
			continue
		}
		if isLegacyGCManagedCommand(event, cmd) {
			hasManagedCommand = true
		}
		if upgraded, didUpgrade := upgradeClaudeHookCommand(event, cmd); didUpgrade {
			hMap["command"] = upgraded
			changed = true
		}
	}

	// Second pass: normalize matcher only when the entry is identifiably
	// GC-managed. Blocks user-authored SessionStart entries with
	// matcher:"" from being silently rewritten to "startup".
	if event == "SessionStart" && hasManagedCommand {
		if matcher, ok := entry["matcher"].(string); ok && matcher == "" {
			entry["matcher"] = "startup"
			changed = true
		}
	}
	return changed
}

// canonicalGCPathPrefix is the env-setup prefix gc prepends to every
// managed hook command. Legacy command bodies always appear either bare
// or with this prefix; user-wrapped variants never have this exact prefix.
const canonicalGCPathPrefix = `export PATH="$HOME/go/bin:$HOME/.local/bin:$PATH" && `

// commandBodyAfterCanonicalPrefix returns the portion of command following
// the canonical gc PATH-export prefix if present, else returns command
// unchanged. Used to anchor legacy-form matching against the post-prefix
// body without matching arbitrary user-authored prefixes.
func commandBodyAfterCanonicalPrefix(command string) string {
	return strings.TrimPrefix(command, canonicalGCPathPrefix)
}

// isLegacyGCManagedCommand reports whether a hook command body matches a
// known legacy form (or the already-upgraded current SessionStart form)
// that gc previously generated. Used to gate matcher normalization in
// upgradeClaudeHookEntry — user-authored commands that wrap,
// suffix-append, or otherwise extend the legacy form (e.g.
// "my-wrapper gc prime --hook --foo", "gc prime --hook --foo",
// `gc prime --hook && my-extra-step`, or the current-form preamble
// with extra trailing args appended) return false and are left alone.
// All recognition paths require exact-body match — gc has only ever
// emitted these tokens as the complete command body, never with
// trailing args.
func isLegacyGCManagedCommand(event, command string) bool {
	body := commandBodyAfterCanonicalPrefix(command)
	switch event {
	case "PreCompact":
		return equalsLegacyCommandBody(body, "gc prime --hook") ||
			equalsLegacyCommandBody(body, `gc handoff "context cycle"`) ||
			equalsLegacyCommandBody(body, `gc handoff --auto "context cycle"`) ||
			isCodexPreCompactCommandBody(body)
	case "SessionStart":
		return equalsLegacyCommandBody(body, "gc prime --hook") ||
			equalsLegacyCommandBody(body, "gc prime --hook --hook-format codex") ||
			equalsLegacyCommandBody(body, sessionStartPreviousManagedFormBody) ||
			isCodexSessionStartCommandBody(body)
	case "UserPromptSubmit":
		return equalsLegacyCommandBody(body, `gc nudge drain --inject`) ||
			equalsLegacyCommandBody(body, `gc mail check --inject`) ||
			codexManagedPromptTarget(body, "")
	}
	return false
}

// sessionStartCurrentFormBody is the canonical current-form managed
// SessionStart command body (post-canonical-PATH-prefix). Recognized
// via exact-body match in isLegacyGCManagedCommand so an already-upgraded
// entry still gates matcher normalization, without matching user
// commands that prefix-collide with the GC_MANAGED_SESSION_HOOK= or
// full env-var preamble. If gc ever extends the current-form command
// with additional arguments, update this constant alongside the
// emission site so legacy detection remains tight.
func sessionStartCurrentFormBody(cityDir string) string {
	return `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc ` + codexCityFlag(cityDir) + `prime --hook --hook-format codex`
}

const sessionStartPreviousManagedFormBody = `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc prime --hook`

// preCompactCurrentFormBody is the canonical current-form managed PreCompact
// command body (post-canonical-PATH-prefix). If gc ever extends this command
// with additional arguments, update this constant alongside the emission site.
func preCompactCurrentFormBody(cityDir string) string {
	return `gc ` + codexCityFlag(cityDir) + `handoff --auto --hook-format codex "context cycle"`
}

// equalsLegacyCommandBody reports whether the command body is exactly the
// legacy token. gc historically emitted these tokens as the complete
// command body (possibly with the canonical PATH-export prefix), never
// with trailing arguments or chained commands. Treating any deviation —
// wrappers, suffix-appended flags, "&&" chains, suffix-token collisions
// like "gc prime --hookable" — as user authorship and leaving the
// command alone is the only safe classification for an upgrade pass that
// silently rewrites managed entries.
func equalsLegacyCommandBody(command, token string) bool {
	return command == token
}

// upgradeClaudeHookCommand returns the upgraded form of an event-scoped
// command if it matches a known legacy shape via exact-body match.
// Returns ("", false) when no upgrade applies.
//
// The match anchors against the command body following the canonical
// gc PATH-export prefix (or against the bare body if there is no
// prefix), and requires that body to equal a known legacy form
// verbatim. This permits gc's own legacy commands (which always carry
// the canonical PATH prefix and have no trailing args) to upgrade,
// while blocking wrapped variants ("my-wrapper gc prime --hook --foo")
// and suffix-appended variants ("gc prime --hook --foo",
// `gc prime --hook && my-step`) from matching and being silently
// rewritten.
func upgradeClaudeHookCommand(event, command string) (string, bool) {
	body := commandBodyAfterCanonicalPrefix(command)
	switch event {
	case "PreCompact":
		// Older legacy: PreCompact used `gc prime --hook` before
		// `gc handoff` was introduced. Upgrade to the current
		// `gc handoff --auto "context cycle"` form. Tested first
		// because it changes the same trailing token the bare-handoff
		// form would otherwise patch.
		if equalsLegacyCommandBody(body, `gc prime --hook`) {
			return strings.Replace(command, `gc prime --hook`, `gc handoff --auto "context cycle"`, 1), true
		}
		// Legacy: bare `gc handoff "context cycle"` (no --auto)
		// requests a controller restart on every Claude Code
		// compaction event, killing the session (gc-flp1). Upstream
		// fix landed in commit 7b3b913a; this patches existing cities.
		if equalsLegacyCommandBody(body, `gc handoff "context cycle"`) {
			return strings.Replace(command, `gc handoff "context cycle"`, `gc handoff --auto "context cycle"`, 1), true
		}
	case "SessionStart":
		// Legacy: bare `gc prime --hook` without the
		// GC_MANAGED_SESSION_HOOK / GC_HOOK_EVENT_NAME env vars the
		// current managed form expects.
		if equalsLegacyCommandBody(body, `gc prime --hook`) ||
			equalsLegacyCommandBody(body, `gc prime --hook --hook-format codex`) ||
			equalsLegacyCommandBody(body, sessionStartPreviousManagedFormBody) {
			prefix := strings.TrimSuffix(command, body)
			return prefix + sessionStartCurrentFormBody(""), true
		}
	case "UserPromptSubmit":
		return upgradeManagedPromptHookCommand(command, "", "")
	}
	return "", false
}
