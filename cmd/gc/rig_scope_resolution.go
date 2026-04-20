package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// resolveRigScopeFromFlagOrCwd returns the rig a command should target,
// preferring an explicit --rig flag value over cwd-based detection. A nil
// result means no rig was resolved (caller should fall back to city scope).
//
// This is the shared primitive for commands whose only rig signals are the
// --rig flag and the working directory (e.g. gc formula cook).
//
// Commands that can also discover a rig from other signals — such as gc bd
// (bead-ID prefix in args) or gc sling (agent target, bead prefix) — have
// their own richer resolvers. They may delegate the flag/cwd leg here if
// their precedence rules permit it, but those with intermediate signals
// (like bd's bead-ID sniffing between flag and cwd) must layer their own
// logic explicitly.
func resolveRigScopeFromFlagOrCwd(cfg *config.City, cityPath, rigName string) (*config.Rig, error) {
	if strings.TrimSpace(rigName) != "" {
		rig, ok := rigByName(cfg, rigName)
		if !ok {
			return nil, fmt.Errorf("rig %q not found", rigName)
		}
		if strings.TrimSpace(rig.Path) == "" {
			return nil, fmt.Errorf("rig %q is declared but has no path binding — run `gc rig add <dir> --name %s` to bind it", rig.Name, rig.Name)
		}
		return &rig, nil
	}
	rig, ok, err := bdRigFromCwd(cfg, cityPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return &rig, nil
}

func rigForDir(cfg *config.City, cityPath, dir string) (config.Rig, bool) {
	rig, ok, _ := resolveRigForDir(cfg, cityPath, dir)
	return rig, ok
}

func resolveRigForDir(cfg *config.City, cityPath, dir string) (config.Rig, bool, error) {
	dir = normalizePathForCompare(dir)
	resolveRigPaths(cityPath, cfg.Rigs)
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		rigPath := normalizePathForCompare(resolveStoreScopeRoot(cityPath, rig.Path))
		if pathWithinScope(dir, rigPath) {
			return rig, true, nil
		}
	}
	return rigFromRedirectedBeadsDir(cfg, cityPath, dir)
}

func rigFromRedirectedBeadsDir(cfg *config.City, cityPath, dir string) (config.Rig, bool, error) {
	for current := dir; current != "" && current != filepath.Dir(current); current = filepath.Dir(current) {
		redirectPath := filepath.Join(current, ".beads", "redirect")
		redirectTarget, err := os.ReadFile(redirectPath)
		if err != nil {
			continue
		}
		targetBeadsDir := normalizePathForCompare(strings.TrimSpace(string(redirectTarget)))
		if targetBeadsDir == "" {
			continue
		}
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			rigBeadsDir := normalizePathForCompare(filepath.Join(resolveStoreScopeRoot(cityPath, rig.Path), ".beads"))
			if targetBeadsDir == rigBeadsDir {
				return rig, true, nil
			}
		}
		return config.Rig{}, false, fmt.Errorf("cwd redirect %s points outside declared city rigs", redirectPath)
	}
	return config.Rig{}, false, nil
}

func pathWithinScope(path, scopeRoot string) bool {
	if scopeRoot == "" {
		return false
	}
	if path == scopeRoot {
		return true
	}
	return len(path) > len(scopeRoot) && strings.HasPrefix(path, scopeRoot) && path[len(scopeRoot)] == '/'
}
