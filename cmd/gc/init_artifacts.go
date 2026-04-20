package main

import (
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/examples/bd"
	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

var defaultFormulas = core.PackFS

func ensureInitArtifacts(cityPath string, cfg *config.City, stderr io.Writer, commandName string) {
	if commandName == "" {
		commandName = "gc start"
	}
	if code := installClaudeHooks(fsys.OSFS{}, cityPath, stderr); code != 0 {
		fmt.Fprintf(stderr, "%s: installing claude hooks: exit %d\n", commandName, code) //nolint:errcheck // best-effort stderr
	}
	if cfg != nil && usesGastownPack(cfg) {
		if err := MaterializeGastownPacks(cityPath); err != nil {
			fmt.Fprintf(stderr, "%s: materializing gastown packs: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		}
	}
	if err := ensureInitFormulas(cityPath); err != nil {
		fmt.Fprintf(stderr, "%s: init formulas: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
	}
}

func usesGastownPack(cfg *config.City) bool {
	for _, include := range append(cfg.Workspace.Includes, cfg.Workspace.DefaultRigIncludes...) {
		if isGastownPackSource(include) {
			return true
		}
	}
	for _, imp := range cfg.Imports {
		if isGastownPackSource(imp.Source) {
			return true
		}
	}
	for _, imp := range cfg.DefaultRigImports {
		if isGastownPackSource(imp.Source) {
			return true
		}
	}
	return false
}

func isGastownPackSource(source string) bool {
	source = strings.TrimSpace(source)
	if source == "" {
		return false
	}
	clean := filepath.Clean(source)
	if clean == filepath.Clean("packs/gastown") || clean == filepath.Clean(".gc/system/packs/gastown") {
		return true
	}
	return strings.HasSuffix(clean, filepath.Join("packs", "gastown"))
}

func ensureInitFormulas(cityPath string) error {
	return writeInitFormulas(fsys.OSFS{}, cityPath, false)
}

func writeDefaultFormulas(fs fsys.FS, cityPath string, stderr io.Writer) int {
	if err := writeInitFormulas(fs, cityPath, false); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func MaterializeBeadsBdScript(cityPath string) error {
	scriptsFS, err := fs.Sub(bd.PackFS, "assets/scripts")
	if err != nil {
		return err
	}
	dstDir := filepath.Join(cityPath, citylayout.SystemPacksRoot, "bd", "assets", "scripts")
	return materializeFS(scriptsFS, ".", dstDir)
}

func writeInitFormulas(fs fsys.FS, cityPath string, overwrite bool) error {
	entries, err := defaultFormulas.ReadDir("formulas")
	if err != nil {
		return err
	}
	formulasDir := filepath.Join(cityPath, citylayout.FormulasRoot)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		dst := filepath.Join(formulasDir, e.Name())
		if !overwrite {
			if _, err := fs.Stat(dst); err == nil {
				continue
			}
		}
		data, err := defaultFormulas.ReadFile(filepath.Join("formulas", e.Name()))
		if err != nil {
			return err
		}
		if err := fs.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := fs.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
