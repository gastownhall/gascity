package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
)

type codexHooksDriftCheck struct {
	dirs []string
}

func newCodexHooksDriftCheck(dirs []string) *codexHooksDriftCheck {
	seen := map[string]struct{}{}
	var cleaned []string
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		dir = filepath.Clean(dir)
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		cleaned = append(cleaned, dir)
	}
	sort.Strings(cleaned)
	return &codexHooksDriftCheck{dirs: cleaned}
}

func codexHookWorkDirs(cityPath string, cfg *config.City) []string {
	dirs := []string{cityPath}
	if cfg == nil {
		return dirs
	}
	for _, rig := range cfg.Rigs {
		if rig.Suspended || strings.TrimSpace(rig.Path) == "" {
			continue
		}
		dirs = append(dirs, rig.Path)
	}
	return dirs
}

func (c *codexHooksDriftCheck) Name() string { return "codex-hooks-drift" }

func (c *codexHooksDriftCheck) CanFix() bool { return true }

func (c *codexHooksDriftCheck) Fix(_ *doctor.CheckContext) error {
	for _, dir := range c.dirs {
		if !codexHooksMissingPreCompact(filepath.Join(dir, ".codex", "hooks.json")) {
			continue
		}
		if err := hooks.Install(fsys.OSFS{}, dir, dir, []string{"codex"}); err != nil {
			return fmt.Errorf("upgrading Codex hooks in %s: %w", dir, err)
		}
	}
	return nil
}

func (c *codexHooksDriftCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	var stale []string
	for _, dir := range c.dirs {
		path := filepath.Join(dir, ".codex", "hooks.json")
		if codexHooksMissingPreCompact(path) {
			stale = append(stale, path)
		}
	}
	if len(stale) == 0 {
		return okCheck(c.Name(), "Codex hooks are current or user-owned")
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d managed Codex hook file(s) missing PreCompact handoff", len(stale)),
		"run `gc doctor --fix` or restart the city to upgrade managed Codex hooks",
		stale)
}

func codexHooksMissingPreCompact(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return false
	}
	hooksMap, ok := doc["hooks"].(map[string]any)
	if !ok {
		return false
	}
	if _, ok := hooksMap["PreCompact"]; ok {
		return false
	}
	return codexHookDocHasManagedCommand(doc)
}

func codexHookDocHasManagedCommand(v any) bool {
	switch node := v.(type) {
	case map[string]any:
		if command, ok := node["command"].(string); ok && codexHookCommandLooksManaged(command) {
			return true
		}
		for _, val := range node {
			if codexHookDocHasManagedCommand(val) {
				return true
			}
		}
	case []any:
		for _, val := range node {
			if codexHookDocHasManagedCommand(val) {
				return true
			}
		}
	}
	return false
}

func codexHookCommandLooksManaged(command string) bool {
	for _, needle := range []string{
		"gc prime --hook",
		"gc nudge drain --inject",
		"gc mail check --inject",
		"gc hook --inject",
		"gc handoff --auto",
	} {
		if strings.Contains(command, needle) {
			return true
		}
	}
	return false
}
