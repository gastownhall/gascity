package cipolicy

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestComposeActionsResolveGoVersionFromGoMod guards against the two
// gascity setup composite actions pinning a literal go-version that can
// drift from go.mod's required version. actions/setup-go must resolve the
// toolchain from go.mod (via go-version-file) whenever the action's own
// go-version input is left at its default, matching the 13+ direct
// setup-go callers in ci.yml. See ga-1qp5qo.
func TestComposeActionsResolveGoVersionFromGoMod(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	tests := []struct {
		name string
		path string
	}{
		{name: "ubuntu", path: filepath.Join(root, ".github", "actions", "setup-gascity-ubuntu", "action.yml")},
		{name: "macos", path: filepath.Join(root, ".github", "actions", "setup-gascity-macos", "action.yml")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action := readYAMLMap(t, tt.path)

			versionInput := input(t, action, "go-version")
			if def, _ := versionInput["default"].(string); def != "" {
				t.Fatalf("go-version input default = %q, want empty so go.mod stays authoritative", def)
			}

			steps := actionSteps(t, action)
			setupIndex := -1
			for i, raw := range steps {
				step, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if uses, _ := step["uses"].(string); strings.HasPrefix(uses, "actions/setup-go@") {
					setupIndex = i
					break
				}
			}
			if setupIndex < 0 {
				t.Fatal("no actions/setup-go step found")
			}

			setup := actionStep(t, action, setupIndex)
			with, ok := setup["with"].(map[string]any)
			if !ok {
				t.Fatal("actions/setup-go step has no with mapping")
			}
			versionFile, ok := with["go-version-file"].(string)
			if !ok || !strings.Contains(versionFile, "go.mod") {
				t.Fatalf("actions/setup-go with.go-version-file = %v, want an expression that falls back to go.mod when go-version is empty", with["go-version-file"])
			}
		})
	}
}
