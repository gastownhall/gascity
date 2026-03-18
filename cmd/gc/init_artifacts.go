package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

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
		if strings.TrimSpace(include) == "packs/gastown" {
			return true
		}
	}
	return false
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
