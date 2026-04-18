// Package hooks installs provider-specific agent hook files into working
// directories. Each provider (Claude, Codex, Gemini, OpenCode, Copilot, etc.)
// has its own file format and install location. Hook files are embedded at build time
// and written idempotently — existing files are never overwritten.
package hooks

import (
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

//go:embed config/*
var configFS embed.FS

var resolveGCBinary = func() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	if path, err := exec.LookPath("gc"); err == nil && path != "" {
		return path
	}
	return "gc"
}

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
		case "codex":
			err = installCodex(fs, workDir)
		case "gemini":
			err = installGemini(fs, workDir)
		case "opencode":
			err = installOpenCode(fs, workDir)
		case "copilot":
			err = installCopilot(fs, workDir)
		case "cursor":
			err = installCursor(fs, workDir)
		case "pi":
			err = installPi(fs, workDir)
		case "omp":
			err = installOmp(fs, workDir)
		default:
			return fmt.Errorf("unsupported hook provider %q", p)
		}
		if err != nil {
			return fmt.Errorf("installing %s hooks: %w", p, err)
		}
	}
	return nil
}

// installClaude writes both the source hook file (hooks/claude.json) and the
// runtime settings file (.gc/settings.json) in the city directory.
//
// The session command path always points at .gc/settings.json, but older code
// and tests still treat hooks/claude.json as the canonical source file. When
// either file already exists, use its content to seed the missing counterpart
// so existing custom hook settings are preserved.
func installClaude(fs fsys.FS, cityDir string) error {
	hookDst := filepath.Join(cityDir, citylayout.ClaudeHookFile)
	runtimeDst := filepath.Join(cityDir, ".gc", "settings.json")
	embedded, err := readEmbedded("config/claude.json")
	if err != nil {
		return err
	}

	data, err := fs.ReadFile(hookDst)
	if err != nil {
		data, err = fs.ReadFile(runtimeDst)
		if err != nil {
			data = embedded
		} else if claudeFileNeedsUpgrade(data) {
			data = embedded
		}
	} else if claudeFileNeedsUpgrade(data) {
		data = embedded
	}

	if err := writeEmbeddedManaged(fs, hookDst, data, claudeFileNeedsUpgrade); err != nil {
		return err
	}
	return writeEmbeddedManaged(fs, runtimeDst, data, claudeFileNeedsUpgrade)
}

// installGemini writes .gemini/settings.json in the working directory.
func installGemini(fs fsys.FS, workDir string) error {
	dst := filepath.Join(workDir, ".gemini", "settings.json")
	data, err := readEmbedded("config/gemini.json")
	if err != nil {
		return err
	}
	data = []byte(strings.ReplaceAll(string(data), "{{GC_BIN}}", resolveGCBinary()))
	return writeEmbeddedManaged(fs, dst, data, geminiFileNeedsUpgrade)
}

// installCodex writes .codex/hooks.json in the working directory.
func installCodex(fs fsys.FS, workDir string) error {
	dst := filepath.Join(workDir, ".codex", "hooks.json")
	return writeEmbedded(fs, "config/codex.json", dst)
}

// installOpenCode writes .opencode/plugins/gascity.js in the working directory.
func installOpenCode(fs fsys.FS, workDir string) error {
	dst := filepath.Join(workDir, ".opencode", "plugins", "gascity.js")
	data, err := readEmbedded("config/opencode.js")
	if err != nil {
		return err
	}
	return writeEmbeddedManaged(fs, dst, data, opencodeFileNeedsUpgrade)
}

// installCopilot writes executable Copilot hooks plus a markdown companion file.
func installCopilot(fs fsys.FS, workDir string) error {
	hooksPath := filepath.Join(workDir, ".github", "hooks", "gascity.json")
	if err := writeEmbedded(fs, "config/copilot.json", hooksPath); err != nil {
		return err
	}
	instructionsPath := filepath.Join(workDir, ".github", "copilot-instructions.md")
	return writeEmbedded(fs, "config/copilot.md", instructionsPath)
}

// installCursor writes .cursor/hooks.json in the working directory.
func installCursor(fs fsys.FS, workDir string) error {
	dst := filepath.Join(workDir, ".cursor", "hooks.json")
	data, err := readEmbedded("config/cursor.json")
	if err != nil {
		return err
	}
	return writeEmbeddedManaged(fs, dst, data, cursorFileNeedsUpgrade)
}

// installPi writes .pi/extensions/gc-hooks.js in the working directory.
func installPi(fs fsys.FS, workDir string) error {
	dst := filepath.Join(workDir, ".pi", "extensions", "gc-hooks.js")
	return writeEmbedded(fs, "config/pi.js", dst)
}

// installOmp writes .omp/hooks/gc-hook.ts in the working directory.
func installOmp(fs fsys.FS, workDir string) error {
	dst := filepath.Join(workDir, ".omp", "hooks", "gc-hook.ts")
	return writeEmbedded(fs, "config/omp.ts", dst)
}

// writeEmbedded reads an embedded file and writes it to dst, creating parent
// directories as needed. Skips if dst already exists.
func writeEmbedded(fs fsys.FS, embedPath, dst string) error {
	data, err := readEmbedded(embedPath)
	if err != nil {
		return err
	}
	return writeEmbeddedManaged(fs, dst, data, nil)
}

func readEmbedded(embedPath string) ([]byte, error) {
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

func geminiFileNeedsUpgrade(existing []byte) bool {
	content := string(existing)
	if !strings.Contains(content, `export PATH=`) {
		return false
	}
	return strings.Contains(content, `gc prime --hook`) ||
		strings.Contains(content, `gc nudge drain --inject`) ||
		strings.Contains(content, `gc mail check --inject`) ||
		strings.Contains(content, `gc hook --inject`)
}

func opencodeFileNeedsUpgrade(existing []byte) bool {
	content := string(existing)
	if strings.Contains(content, "module.exports = {") &&
		strings.Contains(content, "gc prime --hook") &&
		strings.Contains(content, "experimental.chat.system.transform") {
		return true
	}
	if strings.Contains(content, "experimental.chat.system.transform") &&
		strings.Contains(content, "gc prime --hook") &&
		!strings.Contains(content, `"chat.message"`) {
		return true
	}
	if strings.Contains(content, `const prime = await run(directory, "prime", "--hook");`) {
		return true
	}
	if strings.Contains(content, "export default async function") &&
		strings.Contains(content, `let cachedPrime = "";`) &&
		strings.Contains(content, `cachedPrime === ""`) {
		return true
	}
	if strings.Contains(content, "output.system[1] = prefix + \"\\n\\n\" + output.system[1]") {
		return true
	}
	if strings.Contains(content, "export default async function") &&
		strings.Contains(content, "gc prime --hook") &&
		(!strings.Contains(content, `"session.deleted"`) ||
			!strings.Contains(content, "gc hook --inject")) {
		return true
	}
	return strings.Contains(content, "output.system.push(") &&
		strings.Contains(content, "gc prime --hook") &&
		strings.Contains(content, "experimental.chat.system.transform")
}

func claudeFileNeedsUpgrade(existing []byte) bool {
	return matchesStaleManagedFile(existing, "config/claude.json")
}

func cursorFileNeedsUpgrade(existing []byte) bool {
	return matchesStaleManagedFile(existing, "config/cursor.json")
}

func matchesStaleManagedFile(existing []byte, embedPath string) bool {
	current, err := readEmbedded(embedPath)
	if err != nil {
		return false
	}
	stale := strings.Replace(string(current), `gc handoff "context cycle"`, `gc prime --hook`, 1)
	return string(existing) == stale
}
