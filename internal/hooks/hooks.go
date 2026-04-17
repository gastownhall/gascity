// Package hooks installs the Claude city-level settings file that gc passes
// via --settings on session start. All other provider hook files ship from
// the core bootstrap pack's overlay/per-provider/<provider>/ tree and flow
// through the normal overlay copy+merge pipeline.
package hooks

import (
	"embed"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

//go:embed config/claude.json
var configFS embed.FS

// supported lists provider names that Install recognizes. Only Claude has a
// city-level file; every other provider's hooks arrive via overlay copy.
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

// Install writes hook files that require Go-side wiring. Currently that is
// only Claude's city-level settings file — other providers flow through the
// core pack's overlay/per-provider/<provider>/ tree at session start.
// Entries for overlay-managed providers are accepted and silently no-op.
func Install(fs fsys.FS, cityDir, workDir string, providers []string) error {
	_ = workDir // reserved for future per-workdir installs
	for _, p := range providers {
		switch p {
		case "claude":
			if err := installClaude(fs, cityDir); err != nil {
				return fmt.Errorf("installing %s hooks: %w", p, err)
			}
		case "codex", "gemini", "opencode", "copilot", "cursor", "pi", "omp":
			// Shipped via core pack overlay — no Go-side work needed.
		default:
			return fmt.Errorf("unsupported hook provider %q", p)
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

func claudeFileNeedsUpgrade(existing []byte) bool {
	current, err := readEmbedded("config/claude.json")
	if err != nil {
		return false
	}
	stale := strings.Replace(string(current), `gc handoff "context cycle"`, `gc prime --hook`, 1)
	return string(existing) == stale
}
