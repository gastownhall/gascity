// Package hooks installs provider hook files needed before runtime startup.
// Claude still uses a city-level settings file, while the other providers use
// files sourced from the embedded core pack overlay/per-provider tree and
// materialized into the session workdir.
package hooks

import (
	"embed"
	"fmt"
	iofs "io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
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
