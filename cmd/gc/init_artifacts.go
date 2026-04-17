package main

import (
	"fmt"
	"io"
	"strings"

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
}

func usesGastownPack(cfg *config.City) bool {
	for _, include := range append(cfg.Workspace.Includes, cfg.Workspace.DefaultRigIncludes...) {
		if strings.TrimSpace(include) == "packs/gastown" {
			return true
		}
	}
	return false
}
