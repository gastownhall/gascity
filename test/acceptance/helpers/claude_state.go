package acceptancehelpers

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// realUserHome returns the home directory of the user running the tests, from
// the passwd database rather than $HOME: an acceptance env rewrites HOME, so
// HOME cannot tell a test home from the real one. Overridden by the canary
// tests.
var realUserHome = func() string {
	lu, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(lu.HomeDir)
}

// underRealUserHome reports whether path is the real user home or inside it.
func underRealUserHome(path string) bool {
	home := realUserHome()
	if home == "" || strings.TrimSpace(path) == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// EnvAllowHostClaude opts a run in to Claude state writes under the real user
// home, for a developer who has decided their own ~/.claude.json may take the
// test's trust entries. CI runners need no opt-in: their home is discarded
// with the runner (GITHUB_ACTIONS=true).
const EnvAllowHostClaude = "GC_TEST_ALLOW_HOST_CLAUDE"

// HostClaudeStateAllowed reports whether this run may write Claude state under
// the real user home at all: on a GitHub Actions runner, or with
// GC_TEST_ALLOW_HOST_CLAUDE=1. Everywhere else — a developer box — the refusal
// stands even for an Env that asked with WithHostClaudeState.
func HostClaudeStateAllowed() bool {
	return strings.TrimSpace(os.Getenv("GITHUB_ACTIONS")) == "true" ||
		strings.TrimSpace(os.Getenv(EnvAllowHostClaude)) == "1"
}

// errRealUserHomeClaudeState is the refusal for a Claude state write that
// would land in the developer's own ~/.claude.json or ~/.claude: a
// truncate-then-write of a file live Claude sessions rewrite constantly, and
// an ever-growing list of trusted temp directories (#6838's class).
func errRealUserHomeClaudeState(paths []string) error {
	return fmt.Errorf("acceptance: refusing to write Claude state under the real user home %s (%s); give the env an isolated HOME or CLAUDE_CONFIG_DIR; a tier that drives the host's own Claude CLI may write there only through Env.WithHostClaudeState on a CI runner (GITHUB_ACTIONS=true) or with %s=1",
		realUserHome(), strings.Join(paths, ", "), EnvAllowHostClaude)
}

// EnsureClaudeStateFile creates or updates HOME/.claude.json with the minimum
// global onboarding state Claude Code needs to avoid first-run onboarding UI.
// If configDir is non-empty it is used as the Claude config directory;
// otherwise it defaults to HOME/.claude.
//
// It never writes under the real user home (see realUserHome): such a path is
// dropped when another target remains, and refused when it is the only one.
func EnsureClaudeStateFile(home string, configDir ...string) error {
	cd := ""
	if len(configDir) > 0 {
		cd = configDir[0]
	}
	return ensureClaudeStateFile(home, cd, false)
}

func ensureClaudeStateFile(home, configDir string, allowHost bool) error {
	home = strings.TrimSpace(home)
	if home == "" {
		return nil
	}
	cd := filepath.Join(home, ".claude")
	if v := strings.TrimSpace(configDir); v != "" {
		cd = v
	}
	paths, err := claudeStateTargets(home, cd, allowHost)
	if err != nil {
		return err
	}
	for _, statePath := range paths {
		root, err := loadClaudeState(statePath)
		if err != nil {
			return err
		}
		root["hasCompletedOnboarding"] = true
		if theme, _ := root["theme"].(string); strings.TrimSpace(theme) == "" {
			root["theme"] = "light"
		}
		if err := saveClaudeState(statePath, root, allowHost); err != nil {
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
	configDir := filepath.Join(home, ".claude")
	if env != nil {
		if v := strings.TrimSpace(env.Get("CLAUDE_CONFIG_DIR")); v != "" {
			configDir = v
		}
	}
	allowHost := env.hostClaudeState && HostClaudeStateAllowed()
	if err := ensureClaudeStateFile(home, configDir, allowHost); err != nil {
		return err
	}
	paths, err := claudeStateTargets(home, configDir, allowHost)
	if err != nil {
		return err
	}
	for _, statePath := range paths {
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
		if err := saveClaudeState(statePath, root, allowHost); err != nil {
			return err
		}
	}
	return nil
}

// claudeStatePaths lists the state files to seed: HOME's and the config dir's.
// Claude reads the config dir's whenever CLAUDE_CONFIG_DIR is set, so when HOME
// cannot be written (Bazel's local sandbox mounts the runner's read-only) HOME's
// is left out rather than failing a run that does not need it. It is never left
// out when it would be the only one, so an unwritable HOME with no config dir of
// its own still fails.
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
	if len(paths) > 1 && !dirWritable(home) {
		paths = paths[1:]
	}
	return paths
}

// claudeStateTargets is claudeStatePaths without any path under the real user
// home, unless allowHost. It refuses when that leaves nothing to write.
func claudeStateTargets(home, configDir string, allowHost bool) ([]string, error) {
	paths := claudeStatePaths(home, configDir)
	if allowHost {
		return paths, nil
	}
	var kept, refused []string
	for _, p := range paths {
		if underRealUserHome(p) {
			refused = append(refused, p)
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 && len(refused) > 0 {
		return nil, errRealUserHomeClaudeState(refused)
	}
	return kept, nil
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

func saveClaudeState(path string, root map[string]any, allowHost bool) error {
	// The last line of defense: whatever chose path, nothing here writes into
	// the developer's own home without the explicit opt-in.
	if !allowHost && underRealUserHome(path) {
		return errRealUserHomeClaudeState([]string{path})
	}
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
