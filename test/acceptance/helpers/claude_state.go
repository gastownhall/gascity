package acceptancehelpers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// EnsureClaudeStateFile creates or updates HOME/.claude.json with the minimum
// global onboarding state Claude Code needs to avoid first-run onboarding UI.
// If configDir is non-empty it is used as the Claude config directory;
// otherwise it defaults to HOME/.claude.
func EnsureClaudeStateFile(home string, configDir ...string) error {
	home = strings.TrimSpace(home)
	if home == "" {
		return nil
	}
	cd := filepath.Join(home, ".claude")
	if len(configDir) > 0 {
		if v := strings.TrimSpace(configDir[0]); v != "" {
			cd = v
		}
	}
	for _, statePath := range claudeStatePaths(home, cd) {
		root, err := loadClaudeState(statePath)
		if err != nil {
			return err
		}
		root["hasCompletedOnboarding"] = true
		if err := saveClaudeState(statePath, root); err != nil {
			return err
		}
	}
	return nil
}

// EnsureClaudeProjectState marks a project path as trusted/onboarded in the
// isolated Claude state file rooted at env HOME.
func EnsureClaudeProjectState(env *Env, projectPath string) error {
	if env == nil {
		return nil
	}
	projectPath = strings.TrimSpace(projectPath)
	if projectPath == "" {
		return nil
	}
	if !filepath.IsAbs(projectPath) {
		abs, err := filepath.Abs(projectPath)
		if err != nil {
			return err
		}
		projectPath = abs
	}
	home := strings.TrimSpace(env.Get("HOME"))
	if home == "" {
		return nil
	}
	if err := EnsureClaudeStateFile(home); err != nil {
		return err
	}
	configDir := filepath.Join(home, ".claude")
	if env != nil {
		if v := strings.TrimSpace(env.Get("CLAUDE_CONFIG_DIR")); v != "" {
			configDir = v
		}
	}
	for _, statePath := range claudeStatePaths(home, configDir) {
		root, err := loadClaudeState(statePath)
		if err != nil {
			return err
		}
		projects, _ := root["projects"].(map[string]any)
		if projects == nil {
			projects = map[string]any{}
			root["projects"] = projects
		}
		entry, _ := projects[projectPath].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		entry["hasCompletedProjectOnboarding"] = true
		entry["hasTrustDialogAccepted"] = true
		if _, ok := entry["projectOnboardingSeenCount"]; !ok {
			entry["projectOnboardingSeenCount"] = 1
		}
		projects[projectPath] = entry
		if err := saveClaudeState(statePath, root); err != nil {
			return err
		}
	}
	return nil
}

func claudeStatePaths(home, configDir string) []string {
	seen := make(map[string]struct{}, 2)
	var paths []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	add(filepath.Join(home, ".claude.json"))
	add(filepath.Join(configDir, ".claude.json"))
	return paths
}

func loadClaudeState(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, nil
}

func saveClaudeState(path string, root map[string]any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}
