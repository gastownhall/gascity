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

// Validate checks that all provider names are supported for hook installation.
// Returns an error listing any unsupported names.
func Validate(providers []string) error {
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
		if !sup[p] {
			if noHook[p] {
				bad = append(bad, fmt.Sprintf("%s (no hook mechanism)", p))
			} else {
				bad = append(bad, fmt.Sprintf("%s (unknown)", p))
			}
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
	for _, p := range providers {
		var err error
		switch p {
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
// Upgrades stale managed files that are missing newer hook events.
func installCodex(fs fsys.FS, workDir string) error {
	dst := filepath.Join(workDir, ".codex", "hooks.json")
	data, err := readEmbedded("config/codex.json")
	if err != nil {
		return err
	}
	return writeEmbeddedManaged(fs, dst, data, codexFileNeedsUpgrade)
}

// installOpenCode writes .opencode/plugins/gascity.js in the working directory.
func installOpenCode(fs fsys.FS, workDir string) error {
	dst := filepath.Join(workDir, ".opencode", "plugins", "gascity.js")
	return writeEmbedded(fs, "config/opencode.js", dst)
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

func codexFileNeedsUpgrade(existing []byte) bool {
	current, err := readEmbedded("config/codex.json")
	if err != nil {
		return false
	}
	// Construct the exact pre-UserPromptSubmit managed file by removing
	// that block from the current embed. Only exact matches are upgraded,
	// protecting user-customized files (even those with only SessionStart + Stop
	// but modified commands within those events).
	currentStr := string(current)
	upsStart := strings.Index(currentStr, `    "UserPromptSubmit"`)
	stopStart := strings.Index(currentStr, `    "Stop"`)
	if upsStart < 0 || stopStart < 0 || upsStart >= stopStart {
		return false
	}
	stale := currentStr[:upsStart] + currentStr[stopStart:]
	return string(existing) == stale
}

func claudeFileNeedsUpgrade(existing []byte) bool {
	return matchesStaleManagedFile(existing, "config/claude.json")
}

func cursorFileNeedsUpgrade(existing []byte) bool {
	return matchesStaleManagedFile(existing, "config/cursor.json")
}

// StaleCityHooks checks city-level hook files and returns provider names
// whose managed files need upgrading. Currently only Claude has city-level hooks.
// Both the source (hooks/claude.json) and runtime (.gc/settings.json) paths
// are checked — either being stale triggers an upgrade.
func StaleCityHooks(fs fsys.FS, cityDir string) []string {
	paths := []string{
		filepath.Join(cityDir, citylayout.ClaudeHookFile),
		filepath.Join(cityDir, ".gc", "settings.json"),
	}
	for _, path := range paths {
		data, err := fs.ReadFile(path)
		if err != nil {
			continue
		}
		if claudeFileNeedsUpgrade(data) {
			return []string{"claude"}
		}
	}
	return nil
}

// StaleWorkDirHooks checks workdir-level hook files and returns provider names
// whose managed files need upgrading (codex, gemini, cursor).
func StaleWorkDirHooks(fs fsys.FS, workDir string) []string {
	checks := []struct {
		name string
		path string
		fn   func([]byte) bool
	}{
		{"codex", filepath.Join(workDir, ".codex", "hooks.json"), codexFileNeedsUpgrade},
		{"gemini", filepath.Join(workDir, ".gemini", "settings.json"), geminiFileNeedsUpgrade},
		{"cursor", filepath.Join(workDir, ".cursor", "hooks.json"), cursorFileNeedsUpgrade},
	}
	var stale []string
	for _, c := range checks {
		data, err := fs.ReadFile(c.path)
		if err != nil {
			continue
		}
		if c.fn(data) {
			stale = append(stale, c.name)
		}
	}
	return stale
}

// StaleProviderHooks checks all hook files (city-level and workdir-level) and
// returns provider names whose managed files need upgrading.
func StaleProviderHooks(fs fsys.FS, cityDir, workDir string) []string {
	stale := StaleCityHooks(fs, cityDir)
	stale = append(stale, StaleWorkDirHooks(fs, workDir)...)
	return stale
}

func matchesStaleManagedFile(existing []byte, embedPath string) bool {
	current, err := readEmbedded(embedPath)
	if err != nil {
		return false
	}
	stale := strings.Replace(string(current), `gc handoff "context cycle"`, `gc prime --hook`, 1)
	return string(existing) == stale
}
