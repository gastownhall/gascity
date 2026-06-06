package gastown_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestPreCompactHandoffHooksUseAuto guards shipped PreCompact hooks against
// the destructive-eviction regression documented in gc-flp1.
//
// `gc handoff` (bare) sends mail AND requests a controller restart, which kills
// the running session. For PreCompact — which fires automatically on provider
// context cycles inside sessions running these overlays — restart-mode turns
// every compaction into a session kill.
//
// `gc handoff --auto` is the documented mode for this scenario: send mail,
// skip restart, return immediately. The internal SDK hook config
// (internal/hooks/config/claude.json) was switched to --auto in commit
// 7b3b913a ("fix: add auto handoff for precompact"); the gastown pack overlay
// must match.
func TestPreCompactHandoffHooksUseAuto(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join(exampleDir(), "..", ".."))
	paths := preCompactHookConfigPaths(t, repoRoot)
	if len(paths) == 0 {
		t.Fatalf("expected shipped PreCompact hook configs; got none")
	}

	var sawHandoff bool
	for _, path := range paths {
		path := path
		t.Run(relPath(t, repoRoot, path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading hook config: %v", err)
			}

			var cfg map[string]any
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatalf("parsing hook config as JSON: %v", err)
			}

			for _, command := range preCompactCommands(cfg) {
				if !containsGCHandoff(command) {
					continue
				}
				sawHandoff = true
				if !hasAutoFlag(command) {
					t.Errorf("PreCompact hook invokes 'gc handoff' without --auto; bare gc handoff requests a restart and kills the session on every compaction (gc-flp1).\n  command: %q\n  fix: insert --auto, e.g. 'gc handoff --auto \"context cycle\"'", command)
				}
			}
		})
	}
	if !sawHandoff {
		t.Errorf("shipped PreCompact hooks do not call 'gc handoff' at all; expected 'gc handoff --auto \"context cycle\"'")
	}
}

func preCompactHookConfigPaths(t *testing.T, repoRoot string) []string {
	t.Helper()

	var paths []string
	for _, root := range []string{"examples", "internal/bootstrap/packs"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, root), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}

			dir := filepath.Base(filepath.Dir(path))
			name := filepath.Base(path)
			if (dir == ".claude" && name == "settings.json") || (dir == ".cursor" && name == "hooks.json") {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	sort.Strings(paths)
	return paths
}

func preCompactCommands(cfg map[string]any) []string {
	hooks, ok := cfg["hooks"].(map[string]any)
	if !ok {
		return nil
	}

	var commands []string
	for _, key := range []string{"PreCompact", "preCompact"} {
		commands = append(commands, hookCommands(hooks[key])...)
	}
	return commands
}

func hookCommands(raw any) []string {
	var commands []string
	items, ok := raw.([]any)
	if !ok {
		return commands
	}

	for _, item := range items {
		hook, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if command, ok := hook["command"].(string); ok {
			commands = append(commands, command)
		}
		commands = append(commands, hookCommands(hook["hooks"])...)
	}
	return commands
}

func containsGCHandoff(command string) bool {
	fields := strings.Fields(command)
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] == "gc" && fields[i+1] == "handoff" {
			return true
		}
	}
	return false
}

func hasAutoFlag(command string) bool {
	for _, field := range strings.Fields(command) {
		if field == "--auto" {
			return true
		}
	}
	return false
}

func relPath(t *testing.T, base, path string) string {
	t.Helper()

	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return rel
}
