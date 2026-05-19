package overlay

import (
	"encoding/json"
	"testing"
)

func TestIsMergeablePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{".claude/settings.json", true},
		{".gemini/settings.json", true},
		{".codex/hooks.json", true},
		{".cursor/hooks.json", true},
		{".github/hooks/gascity.json", true},
		// Negative cases.
		{".claude/settings.local.json", false},
		{".opencode/config.js", false},
		{"settings.json", false},
		{".claude/hooks.json", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsMergeablePath(tt.path); got != tt.want {
			t.Errorf("IsMergeablePath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestMergeSettingsJSON_UnionHookCategories(t *testing.T) {
	base := `{
		"hooks": {
			"SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "start"}]}],
			"Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "stop"}]}]
		}
	}`
	over := `{
		"hooks": {
			"PreToolUse": [{"matcher": "Bash(*foo*)", "hooks": [{"type": "command", "command": "guard"}]}]
		}
	}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	hooks := doc["hooks"].(map[string]any)

	// All three categories must be present.
	for _, cat := range []string{"SessionStart", "Stop", "PreToolUse"} {
		if _, ok := hooks[cat]; !ok {
			t.Errorf("missing hook category %q after merge", cat)
		}
	}
}

func TestMergeSettingsJSON_SameMatcherReplacement(t *testing.T) {
	// Crew scenario: overlay changes PreCompact catch-all command.
	base := `{
		"hooks": {
			"PreCompact": [{"matcher": "", "hooks": [{"type": "command", "command": "gc prime"}]}]
		}
	}`
	over := `{
		"hooks": {
			"PreCompact": [{"matcher": "", "hooks": [{"type": "command", "command": "gc handoff --auto \"context cycle\""}]}]
		}
	}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	hooks := doc["hooks"].(map[string]any)
	arr := hooks["PreCompact"].([]any)
	if len(arr) != 1 {
		t.Fatalf("PreCompact entries = %d, want 1", len(arr))
	}
	entry := arr[0].(map[string]any)
	innerHooks := entry["hooks"].([]any)
	cmd := innerHooks[0].(map[string]any)["command"].(string)
	if cmd != `gc handoff --auto "context cycle"` {
		t.Errorf("PreCompact command = %q, want gc handoff", cmd)
	}
}

func TestMergeSettingsJSON_AppendNewMatcher(t *testing.T) {
	// Witness scenario: overlay adds PreToolUse guards to base that has none.
	base := `{
		"hooks": {
			"SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "gc prime"}]}]
		}
	}`
	over := `{
		"hooks": {
			"PreToolUse": [
				{"matcher": "Bash(*foo*)", "hooks": [{"type": "command", "command": "guard1"}]},
				{"matcher": "Bash(*bar*)", "hooks": [{"type": "command", "command": "guard2"}]}
			]
		}
	}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	hooks := doc["hooks"].(map[string]any)

	// SessionStart preserved from base.
	if _, ok := hooks["SessionStart"]; !ok {
		t.Error("SessionStart missing from base")
	}
	// PreToolUse from overlay.
	arr := hooks["PreToolUse"].([]any)
	if len(arr) != 2 {
		t.Errorf("PreToolUse entries = %d, want 2", len(arr))
	}
}

func TestMergeSettingsJSON_NonHookKeysOverride(t *testing.T) {
	base := `{"version": "1.0", "editorMode": "vim"}`
	over := `{"version": "2.0", "newKey": true}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if doc["version"] != "2.0" {
		t.Errorf("version = %v, want 2.0", doc["version"])
	}
	if doc["editorMode"] != "vim" {
		t.Errorf("editorMode = %v, want vim (preserved from base)", doc["editorMode"])
	}
	if doc["newKey"] != true {
		t.Errorf("newKey = %v, want true", doc["newKey"])
	}
}

func TestMergeSettingsJSON_CursorFormat_CommandIdentity(t *testing.T) {
	base := `{
		"hooks": {
			"PreToolUse": [{"command": "lint.sh", "on": "save"}]
		}
	}`
	over := `{
		"hooks": {
			"PreToolUse": [
				{"command": "lint.sh", "on": "always"},
				{"command": "format.sh", "on": "save"}
			]
		}
	}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	arr := doc["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(arr) != 2 {
		t.Fatalf("PreToolUse entries = %d, want 2 (replace + append)", len(arr))
	}
	// lint.sh replaced in-place.
	first := arr[0].(map[string]any)
	if first["on"] != "always" {
		t.Errorf("lint.sh 'on' = %v, want 'always'", first["on"])
	}
	// format.sh appended.
	second := arr[1].(map[string]any)
	if second["command"] != "format.sh" {
		t.Errorf("second entry command = %v, want format.sh", second["command"])
	}
}

func TestMergeSettingsJSON_BashIdentity(t *testing.T) {
	base := `{
		"hooks": {
			"Stop": [{"bash": "cleanup.sh"}]
		}
	}`
	over := `{
		"hooks": {
			"Stop": [{"bash": "cleanup.sh", "timeout": 30}]
		}
	}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	arr := doc["hooks"].(map[string]any)["Stop"].([]any)
	if len(arr) != 1 {
		t.Fatalf("Stop entries = %d, want 1 (replaced)", len(arr))
	}
	entry := arr[0].(map[string]any)
	if entry["timeout"] != float64(30) {
		t.Errorf("timeout = %v, want 30", entry["timeout"])
	}
}

func TestMergeSettingsJSON_EmptyBase(t *testing.T) {
	over := `{"hooks": {"Stop": [{"matcher": "", "hooks": []}]}}`
	result, err := MergeSettingsJSON([]byte(`{}`), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := doc["hooks"].(map[string]any)["Stop"]; !ok {
		t.Error("Stop hook missing")
	}
}

func TestMergeSettingsJSON_EmptyOverlay(t *testing.T) {
	base := `{"hooks": {"Stop": [{"matcher": "", "hooks": []}]}}`
	result, err := MergeSettingsJSON([]byte(base), []byte(`{}`))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := doc["hooks"].(map[string]any)["Stop"]; !ok {
		t.Error("Stop hook from base missing after empty overlay")
	}
}

func TestMergeSettingsJSON_InvalidBase(t *testing.T) {
	_, err := MergeSettingsJSON([]byte(`not json`), []byte(`{}`))
	if err == nil {
		t.Error("expected error for invalid base JSON")
	}
}

func TestMergeSettingsJSON_InvalidOverlay(t *testing.T) {
	_, err := MergeSettingsJSON([]byte(`{}`), []byte(`not json`))
	if err == nil {
		t.Error("expected error for invalid overlay JSON")
	}
}

func TestMergeSettingsJSON_WitnessScenario(t *testing.T) {
	// Full witness scenario: base has 4 default hooks, overlay adds PreToolUse only.
	base := `{
		"hooks": {
			"SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "gc prime"}]}],
			"PreCompact": [{"matcher": "", "hooks": [{"type": "command", "command": "gc prime"}]}],
			"UserPromptSubmit": [{"matcher": "", "hooks": [{"type": "command", "command": "gc mail check --inject"}]}],
			"Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "gc hook --inject"}]}]
		}
	}`
	over := `{
		"hooks": {
			"PreToolUse": [
				{"matcher": "Bash(*bd mol pour*patrol*)", "hooks": [{"type": "command", "command": "echo BLOCKED && exit 2"}]},
				{"matcher": "Bash(*bd mol pour *mol-witness*)", "hooks": [{"type": "command", "command": "echo BLOCKED && exit 2"}]}
			]
		}
	}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	hooks := doc["hooks"].(map[string]any)

	// All 5 categories present.
	for _, cat := range []string{"SessionStart", "PreCompact", "UserPromptSubmit", "Stop", "PreToolUse"} {
		if _, ok := hooks[cat]; !ok {
			t.Errorf("missing category %q", cat)
		}
	}
	// PreToolUse has 2 entries.
	arr := hooks["PreToolUse"].([]any)
	if len(arr) != 2 {
		t.Errorf("PreToolUse entries = %d, want 2", len(arr))
	}
}

func TestMergeSettingsJSON_CrewScenario(t *testing.T) {
	// Full crew scenario: base has 4 hooks, overlay overrides PreCompact only.
	base := `{
		"hooks": {
			"SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "gc prime"}]}],
			"PreCompact": [{"matcher": "", "hooks": [{"type": "command", "command": "gc prime"}]}],
			"UserPromptSubmit": [{"matcher": "", "hooks": [{"type": "command", "command": "gc mail check --inject"}]}],
			"Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "gc hook --inject"}]}]
		}
	}`
	over := `{
		"hooks": {
			"PreCompact": [{"matcher": "", "hooks": [{"type": "command", "command": "gc handoff --auto \"context cycle\""}]}]
		}
	}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	hooks := doc["hooks"].(map[string]any)

	// All 4 categories still present.
	for _, cat := range []string{"SessionStart", "PreCompact", "UserPromptSubmit", "Stop"} {
		if _, ok := hooks[cat]; !ok {
			t.Errorf("missing category %q", cat)
		}
	}
	// PreCompact replaced.
	arr := hooks["PreCompact"].([]any)
	if len(arr) != 1 {
		t.Fatalf("PreCompact entries = %d, want 1", len(arr))
	}
	entry := arr[0].(map[string]any)
	innerHooks := entry["hooks"].([]any)
	cmd := innerHooks[0].(map[string]any)["command"].(string)
	if cmd != `gc handoff --auto "context cycle"` {
		t.Errorf("PreCompact command = %q, want gc handoff", cmd)
	}
}

func TestMergeSettingsJSON_BackwardCompat_FullOverlay(t *testing.T) {
	// When overlay contains all hooks (legacy full copy), result equals overlay content.
	full := `{
		"hooks": {
			"SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "gc prime"}]}],
			"PreCompact": [{"matcher": "", "hooks": [{"type": "command", "command": "gc handoff --auto \"context cycle\""}]}],
			"UserPromptSubmit": [{"matcher": "", "hooks": [{"type": "command", "command": "gc mail check --inject"}]}],
			"Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "gc hook --inject"}]}]
		}
	}`
	base := `{
		"hooks": {
			"SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "gc prime"}]}],
			"PreCompact": [{"matcher": "", "hooks": [{"type": "command", "command": "gc prime"}]}],
			"UserPromptSubmit": [{"matcher": "", "hooks": [{"type": "command", "command": "gc mail check --inject"}]}],
			"Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "gc hook --inject"}]}]
		}
	}`

	result, err := MergeSettingsJSON([]byte(base), []byte(full))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	// Parse both and compare structurally.
	var resultDoc, fullDoc map[string]any
	if err := json.Unmarshal(result, &resultDoc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if err := json.Unmarshal([]byte(full), &fullDoc); err != nil {
		t.Fatalf("unmarshal full: %v", err)
	}

	// Re-marshal both for string comparison (normalized).
	resultNorm, _ := json.Marshal(resultDoc)
	fullNorm, _ := json.Marshal(fullDoc)
	if string(resultNorm) != string(fullNorm) {
		t.Errorf("full overlay merge produced different result:\ngot:  %s\nwant: %s", resultNorm, fullNorm)
	}
}

func TestMergeSettingsJSON_NoIdentityAlwaysAppends(t *testing.T) {
	base := `{
		"hooks": {
			"Stop": [{"custom": "field1"}]
		}
	}`
	over := `{
		"hooks": {
			"Stop": [{"custom": "field2"}]
		}
	}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	arr := doc["hooks"].(map[string]any)["Stop"].([]any)
	if len(arr) != 2 {
		t.Errorf("Stop entries = %d, want 2 (both appended since no identity)", len(arr))
	}
}

func TestMergeSettingsJSON_EmptyArrayPreservesBase(t *testing.T) {
	// Union-only semantics: an empty overlay array does NOT remove base entries.
	base := `{
		"hooks": {
			"SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "gc prime"}]}]
		}
	}`
	over := `{
		"hooks": {
			"SessionStart": []
		}
	}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	arr := doc["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(arr) != 1 {
		t.Errorf("SessionStart entries = %d, want 1 (base preserved with empty overlay)", len(arr))
	}
}
