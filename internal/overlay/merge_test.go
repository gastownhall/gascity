package overlay

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestIsMergeablePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{".agents/hooks.json", true},
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

func TestMergeSettingsJSON_CanonicalizesCommandsWithoutHTMLEscaping(t *testing.T) {
	base := `{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"export PATH=\"$HOME/bin:$PATH\" && gc prime"}]}]}}`
	over := `{}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}

	if bytes.Contains(result, []byte(`\u0026`)) {
		t.Fatalf("merged JSON escaped command operator:\n%s", result)
	}
	if !bytes.Contains(result, []byte(` && gc prime`)) {
		t.Fatalf("merged JSON missing literal command operator:\n%s", result)
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

func TestMergeSettingsJSON_NullOverlayIsNotObject(t *testing.T) {
	_, err := MergeSettingsJSON([]byte(`{}`), []byte(`null`))
	if err == nil {
		t.Fatal("expected error for null overlay JSON")
	}
	if !IsOverlayObjectShapeError(err) {
		t.Fatalf("expected overlay object-shape error, got %v", err)
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

// maHookEntry is the wrapper shape used by packs like model-advisor: a Claude
// hook entry with NO top-level "matcher" and the command nested inside an inner
// "hooks" array. Before the dedup fix this shape had no identity key and was
// appended unconditionally on every re-projection, accumulating without bound.
const maHookEntry = `{"hooks":[{"type":"command","command":"bash $CLAUDE_PROJECT_DIR/packs/model-advisor/hooks/capture-invocation.sh"}]}`

func stopEntries(t *testing.T, merged []byte) []any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("result has no hooks object: %s", merged)
	}
	arr, ok := hooks["Stop"].([]any)
	if !ok {
		t.Fatalf("result has no Stop array: %s", merged)
	}
	return arr
}

func TestMergeSettingsJSON_MatcherlessWrapper_DedupsIdenticalCommand(t *testing.T) {
	// Regression: overlay re-projects a Stop hook that already exists in the
	// target. The matcherless wrapper must be deduped by its inner command(s),
	// not appended again. (Bug: 60-1000+ duplicate capture-invocation.sh hooks.)
	base := `{"hooks":{"Stop":[` + maHookEntry + `]}}`
	over := `{"hooks":{"Stop":[` + maHookEntry + `]}}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	if got := len(stopEntries(t, result)); got != 1 {
		t.Errorf("Stop entries = %d, want 1 (identical command must not duplicate)", got)
	}
}

func TestMergeSettingsJSON_MatcherlessWrapper_IdempotentAcrossManyMerges(t *testing.T) {
	// N re-projections of the same overlay must converge to exactly 1 copy,
	// not N copies. This is the session-start accumulation that broke prod.
	doc := []byte(`{"hooks":{"Stop":[` + maHookEntry + `]}}`)
	merged := doc
	for i := 0; i < 25; i++ {
		var err error
		merged, err = MergeSettingsJSON(merged, doc)
		if err != nil {
			t.Fatalf("merge iteration %d: %v", i, err)
		}
	}
	if got := len(stopEntries(t, merged)); got != 1 {
		t.Errorf("Stop entries after 25 merges = %d, want 1 (idempotent)", got)
	}
}

func TestMergeSettingsJSON_MatcherlessWrapper_DistinctCommandsCoexist(t *testing.T) {
	// A genuinely different matcherless command must still be added, and two
	// distinct commands must coexist (dedup keys on command content, not shape).
	other := `{"hooks":[{"type":"command","command":"bash $CLAUDE_PROJECT_DIR/packs/other/hooks/run.sh"}]}`
	base := `{"hooks":{"Stop":[` + maHookEntry + `]}}`
	over := `{"hooks":{"Stop":[` + other + `]}}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	if got := len(stopEntries(t, result)); got != 2 {
		t.Errorf("Stop entries = %d, want 2 (distinct commands coexist)", got)
	}
}

func TestMergeSettingsJSON_MatcherlessWrapper_PreservesCoreHook(t *testing.T) {
	// Core (non-overlay) hooks projected by gc itself use a "matcher" wrapper.
	// Merging a matcherless pack hook must preserve the core entry and append
	// the pack entry exactly once (ordering: core first).
	core := `{"matcher":"","hooks":[{"type":"command","command":"gc hook --inject"}]}`
	base := `{"hooks":{"Stop":[` + core + `]}}`
	over := `{"hooks":{"Stop":[` + maHookEntry + `]}}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	arr := stopEntries(t, result)
	if len(arr) != 2 {
		t.Fatalf("Stop entries = %d, want 2 (core preserved + pack appended)", len(arr))
	}
	first, _ := arr[0].(map[string]any)
	if _, hasMatcher := first["matcher"]; !hasMatcher {
		t.Errorf("core hook (with matcher) must remain first; got %v", arr[0])
	}
	// Re-merging the overlay again must not add a third entry.
	result2, err := MergeSettingsJSON(result, []byte(over))
	if err != nil {
		t.Fatalf("second MergeSettingsJSON: %v", err)
	}
	if got := len(stopEntries(t, result2)); got != 2 {
		t.Errorf("Stop entries after re-merge = %d, want 2 (still idempotent with core present)", got)
	}
}

func TestMergeSettingsJSON_MatcherlessWrapper_RealWorldRecovery(t *testing.T) {
	// Reproduce the observed corrupt state: a Stop array already bloated with
	// many identical matcherless entries. One merge must collapse the
	// byte-identical duplicates back to a single entry (self-heal, #3862).
	// The pack overlay carrying this shape projects onto .claude/settings.json,
	// so production always merges it with WithWrapBareHooks (overlay.go,
	// hooks.go) — the self-heal is scoped to that path and leaves the additive
	// Codex/Cursor merge alone.
	bloat := make([]string, 50)
	for i := range bloat {
		bloat[i] = maHookEntry
	}
	base := `{"hooks":{"Stop":[` + joinJSON(bloat) + `]}}`
	over := `{"hooks":{"Stop":[` + maHookEntry + `]}}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over), WithWrapBareHooks())
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	if got := len(stopEntries(t, result)); got != 1 {
		t.Errorf("Stop entries = %d, want 1 (byte-identical bloat must collapse)", got)
	}
}

func joinJSON(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
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

func TestWrapsBareHooks(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{".claude/settings.json", true},
		// Other providers keep bare entries verbatim.
		{".gemini/settings.json", false},
		{".codex/hooks.json", false},
		{".cursor/hooks.json", false},
		{".github/hooks/gascity.json", false},
		{"settings.json", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := WrapsBareHooks(tt.path); got != tt.want {
			t.Errorf("WrapsBareHooks(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// preToolUse is a small helper to pull the PreToolUse array out of a merged
// settings document.
func preToolUse(t *testing.T, merged []byte) []any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("no hooks map in result: %s", merged)
	}
	arr, ok := hooks["PreToolUse"].([]any)
	if !ok {
		t.Fatalf("no PreToolUse array in result: %s", merged)
	}
	return arr
}

func innerCommand(t *testing.T, entry any) string {
	t.Helper()
	m, ok := entry.(map[string]any)
	if !ok {
		t.Fatalf("entry is not an object: %v", entry)
	}
	inner, ok := m["hooks"].([]any)
	if !ok || len(inner) == 0 {
		t.Fatalf("entry has no wrapped hooks array: %v", m)
	}
	return inner[0].(map[string]any)["command"].(string)
}

func TestMergeSettingsJSON_WrapBareHooks_NormalizesOverlayBareEntry(t *testing.T) {
	// A bare {type,command} entry from the overlay is the exact shape the ubs
	// pack shipped; with WithWrapBareHooks it must become valid Claude form.
	base := `{}`
	over := `{"hooks":{"PreToolUse":[{"type":"command","command":"scan"}]}}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over), WithWrapBareHooks())
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	arr := preToolUse(t, result)
	if len(arr) != 1 {
		t.Fatalf("PreToolUse entries = %d, want 1", len(arr))
	}
	entry := arr[0].(map[string]any)
	if _, hasMatcher := entry["matcher"]; hasMatcher {
		// The wrapped form omits "matcher" so the entry keys on its inner
		// command content — a matcher-"" stamp would collide with the core
		// hooks' matcher-"" identity slot and with other bare pack hooks.
		t.Errorf("wrapped entry carries matcher %v, want matcherless wrapper", entry["matcher"])
	}
	if got := innerCommand(t, entry); got != "scan" {
		t.Errorf("inner command = %q, want scan", got)
	}
}

func TestMergeSettingsJSON_WrapBareHooks_NormalizesBaseBareEntry(t *testing.T) {
	// Models the accumulated agent file: a stale bare entry already in the
	// destination (base) plus the overlay's wrapped entry. After merge both
	// must be valid (carry a "hooks" array) — no /doctor error.
	base := `{"hooks":{"PreToolUse":[{"type":"command","command":"scan"}]}}`
	over := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"scan"}]}]}}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over), WithWrapBareHooks())
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	arr := preToolUse(t, result)
	if len(arr) != 2 {
		t.Fatalf("PreToolUse entries = %d, want 2", len(arr))
	}
	for i, e := range arr {
		m := e.(map[string]any)
		if _, ok := m["hooks"]; !ok {
			t.Errorf("entry[%d] = %v lacks a hooks array (invalid Claude shape)", i, m)
		}
	}
}

func TestMergeSettingsJSON_WrapBareHooks_PreservesDistinctBareEntries(t *testing.T) {
	// Two distinct bare commands must both survive (no collapse / data loss).
	base := `{"hooks":{"PreToolUse":[{"type":"command","command":"a"}]}}`
	over := `{"hooks":{"PreToolUse":[{"type":"command","command":"b"}]}}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over), WithWrapBareHooks())
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	arr := preToolUse(t, result)
	if len(arr) != 2 {
		t.Fatalf("PreToolUse entries = %d, want 2 (no data loss)", len(arr))
	}
	seen := map[string]bool{}
	for _, e := range arr {
		seen[innerCommand(t, e)] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("expected both commands a and b preserved, got %v", seen)
	}
}

func TestMergeSettingsJSON_WrapBareHooks_LeavesWrappedUnchanged(t *testing.T) {
	base := `{}`
	over := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"scan"}]}]}}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over), WithWrapBareHooks())
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	arr := preToolUse(t, result)
	if len(arr) != 1 {
		t.Fatalf("PreToolUse entries = %d, want 1", len(arr))
	}
	if arr[0].(map[string]any)["matcher"] != "Bash" {
		t.Errorf("matcher = %v, want Bash (already-wrapped entry must be unchanged)", arr[0].(map[string]any)["matcher"])
	}
}

func TestMergeSettingsJSON_NoWrap_LeavesBareEntries(t *testing.T) {
	// Without the option, Codex/Cursor-style bare entries are preserved verbatim
	// (merged by command identity), never wrapped.
	base := `{"hooks":{"PreToolUse":[{"command":"lint.sh"}]}}`
	over := `{"hooks":{"PreToolUse":[{"command":"lint.sh","on":"always"}]}}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	arr := preToolUse(t, result)
	if len(arr) != 1 {
		t.Fatalf("PreToolUse entries = %d, want 1 (replaced by identity)", len(arr))
	}
	if _, wrapped := arr[0].(map[string]any)["hooks"]; wrapped {
		t.Errorf("bare entry was wrapped without WithWrapBareHooks: %v", arr[0])
	}
}

// ubsBareEntry is the bare {type,command} shape the ubs pack ships. Reconcile
// re-projects it into .claude/settings.json on every tick (#3862).
const ubsBareEntry = `{"type":"command","command":"if echo \"$TOOL_INPUT\" | grep -q 'git commit'; then ubs --staged --format=json 2>/dev/null || true; fi"}`

func TestMergeSettingsJSON_WrapBareHooks_IdempotentAcrossReconciles(t *testing.T) {
	// Issue #3862: the ubs pack overlay ships a bare PreToolUse entry. Each
	// reconcile merges that overlay into the already-projected (wrapped)
	// .claude/settings.json; the hook must stay at exactly one matcherless
	// entry instead of growing by one copy per reconcile.
	over := []byte(`{"hooks":{"PreToolUse":[` + ubsBareEntry + `]}}`)
	merged := []byte(`{}`)
	for i := 0; i < 25; i++ {
		var err error
		merged, err = MergeSettingsJSON(merged, over, WithWrapBareHooks())
		if err != nil {
			t.Fatalf("merge tick %d: %v", i, err)
		}
	}
	arr := preToolUse(t, merged)
	if len(arr) != 1 {
		t.Fatalf("PreToolUse entries after 25 reconciles = %d, want 1 (idempotent)", len(arr))
	}
	assertCanonicalEqual(t, arr[0], `{"hooks":[`+ubsBareEntry+`]}`)
}

// assertCanonicalEqual fails unless got marshals to the same canonical JSON as
// the want document.
func assertCanonicalEqual(t *testing.T, got any, want string) {
	t.Helper()
	gotJSON, err := MarshalCanonicalJSON(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	var wantDoc any
	if err := json.Unmarshal([]byte(want), &wantDoc); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	wantJSON, err := MarshalCanonicalJSON(wantDoc)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("entry = %s, want %s", gotJSON, wantJSON)
	}
}

func TestMergeSettingsJSON_WrapBareHooks_CoreHookSurvivesBarePackHook(t *testing.T) {
	// A category can hold both a core gc hook (matcher "") and a pack's bare
	// hook. The bare entry must not claim the matcher-"" identity slot: the
	// core hook survives, the pack hook lands exactly once across re-projection,
	// and a later core upgrade still replaces the core entry in place.
	base := []byte(`{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"gc hook --inject"}]}]}}`)
	pack := []byte(`{"hooks":{"Stop":[{"type":"command","command":"pack-cleanup.sh"}]}}`)

	result := base
	for i := 0; i < 4; i++ {
		var err error
		result, err = MergeSettingsJSON(result, pack, WithWrapBareHooks())
		if err != nil {
			t.Fatalf("pack merge %d: %v", i, err)
		}
	}
	arr := stopEntries(t, result)
	if len(arr) != 2 {
		t.Fatalf("Stop entries after 4 pack merges = %d, want 2 (core + pack):\n%s", len(arr), result)
	}
	assertCanonicalEqual(t, arr[0], `{"matcher":"","hooks":[{"type":"command","command":"gc hook --inject"}]}`)
	assertCanonicalEqual(t, arr[1], `{"hooks":[{"type":"command","command":"pack-cleanup.sh"}]}`)

	upgrade := []byte(`{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"gc hook --inject --v2"}]}]}}`)
	result, err := MergeSettingsJSON(result, upgrade, WithWrapBareHooks())
	if err != nil {
		t.Fatalf("upgrade merge: %v", err)
	}
	arr = stopEntries(t, result)
	if len(arr) != 2 {
		t.Fatalf("Stop entries after core upgrade = %d, want 2 (upgraded core + pack):\n%s", len(arr), result)
	}
	assertCanonicalEqual(t, arr[0], `{"matcher":"","hooks":[{"type":"command","command":"gc hook --inject --v2"}]}`)
	assertCanonicalEqual(t, arr[1], `{"hooks":[{"type":"command","command":"pack-cleanup.sh"}]}`)
}

func TestMergeSettingsJSON_WrapBareHooks_CoreMatcherReplacedOnUpgrade(t *testing.T) {
	// The core gc hooks in internal/hooks/config/claude.json occupy the
	// matcher-"" slot (PreCompact, UserPromptSubmit) and reach the merge with
	// WithWrapBareHooks via installClaude. When a release changes the command,
	// the new entry must replace the old one rather than append beside it.
	// TestMergeSettingsJSON_SameMatcherReplacement covers the no-wrap path only.
	base := `{"hooks":{"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":"gc nudge drain --old"}]}]}}`
	over := `{"hooks":{"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":"gc nudge drain --new"}]}]}}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over), WithWrapBareHooks())
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	arr := doc["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	if len(arr) != 1 {
		t.Fatalf("UserPromptSubmit entries = %d, want 1 (stale core hook must not linger)", len(arr))
	}
	if got := innerCommand(t, arr[0]); got != "gc nudge drain --new" {
		t.Errorf("command = %q, want gc nudge drain --new", got)
	}
}

func TestMergeSettingsJSON_WrapBareHooks_DistinctBareHooksCoexistAcrossTicks(t *testing.T) {
	// Two distinct bare hooks in one event each key on their own command
	// content, so re-projection keeps both — once each, in overlay order —
	// instead of appending copies or collapsing them into one slot.
	over := []byte(`{"hooks":{"PreToolUse":[` +
		`{"type":"command","command":"hook-x"},` +
		`{"type":"command","command":"hook-z"}` +
		`]}}`)
	merged := []byte(`{}`)
	for i := 0; i < 10; i++ {
		var err error
		merged, err = MergeSettingsJSON(merged, over, WithWrapBareHooks())
		if err != nil {
			t.Fatalf("merge tick %d: %v", i, err)
		}
	}
	arr := preToolUse(t, merged)
	if len(arr) != 2 {
		t.Fatalf("PreToolUse entries after 10 ticks = %d, want 2 (X then Z):\n%s", len(arr), merged)
	}
	assertCanonicalEqual(t, arr[0], `{"hooks":[{"type":"command","command":"hook-x"}]}`)
	assertCanonicalEqual(t, arr[1], `{"hooks":[{"type":"command","command":"hook-z"}]}`)
}

func TestMergeSettingsJSON_WrapBareHooks_LegacyWrappedBloatSelfHeals(t *testing.T) {
	// A file already bloated by the pre-fix append loop holds N byte-identical
	// {"matcher":"","hooks":[...]} copies of the pack hook. One merge tick must
	// converge the category back to a single matcherless entry: duplicates
	// collapse, and the surviving legacy twin migrates to the content-keyed
	// shape of the re-projected bare entry.
	legacy := `{"matcher":"","hooks":[` + ubsBareEntry + `]}`
	bloat := make([]string, 40)
	for i := range bloat {
		bloat[i] = legacy
	}
	base := `{"hooks":{"PreToolUse":[` + joinJSON(bloat) + `]}}`
	over := `{"hooks":{"PreToolUse":[` + ubsBareEntry + `]}}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over), WithWrapBareHooks())
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	arr := preToolUse(t, result)
	if len(arr) != 1 {
		t.Fatalf("PreToolUse entries = %d, want 1 (bloat must self-heal)", len(arr))
	}
	assertCanonicalEqual(t, arr[0], `{"hooks":[`+ubsBareEntry+`]}`)
}

func TestMergeSettingsJSON_DedupKeepsDistinctSameKeyEntries(t *testing.T) {
	// Exact-duplicate collapse must not touch entries that share an identity
	// key but differ in content (e.g. a hand-added second Bash-matcher hook),
	// nor entries without an identity key.
	base := `{"hooks":{"PreToolUse":[
		{"matcher":"Bash","hooks":[{"type":"command","command":"a"}]},
		{"matcher":"Bash","hooks":[{"type":"command","command":"b"}]},
		{"hooks":"x"},
		"not-an-object"
	]}}`
	over := `{"hooks":{"PreToolUse":[]}}`
	result, err := MergeSettingsJSON([]byte(base), []byte(over), WithWrapBareHooks())
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	if got := len(preToolUse(t, result)); got != 4 {
		t.Errorf("PreToolUse entries = %d, want 4 (distinct content and identity-less entries must survive dedup)", got)
	}
}

func TestMergeSettingsJSON_WrapBareHooks_HealSkipsCategoriesOverlayOmits(t *testing.T) {
	// The heal runs only for categories the overlay projects; other categories
	// are carried over verbatim, duplicates included.
	dup := `{"matcher":"","hooks":[{"type":"command","command":"same"}]}`
	base := `{"hooks":{"Stop":[` + dup + `,` + dup + `]}}`
	over := `{"hooks":{"PreToolUse":[{"type":"command","command":"scan"}]}}`
	result, err := MergeSettingsJSON([]byte(base), []byte(over), WithWrapBareHooks())
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	if got := len(stopEntries(t, result)); got != 2 {
		t.Errorf("Stop entries = %d, want 2 (unprojected category must be untouched)", got)
	}
}

func TestMergeSettingsJSON_WrapBareHooks_TwinMigratesWhenNotInMatcherSlot(t *testing.T) {
	// A category can hold several matcher-"" entries, but the identity index
	// tracks only the last one. When the legacy twin is not the entry holding
	// that slot, it must still be found by content — otherwise the pack hook
	// migrates alongside its own legacy copy and fires twice on every event.
	base := `{"hooks":{"UserPromptSubmit":[` +
		`{"matcher":"","hooks":[{"type":"command","command":"packcmd"}]},` +
		`{"matcher":"","hooks":[{"type":"command","command":"gc hook --inject"}]}` +
		`]}}`
	over := []byte(`{"hooks":{"UserPromptSubmit":[{"type":"command","command":"packcmd"}]}}`)

	result, err := MergeSettingsJSON([]byte(base), over, WithWrapBareHooks())
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	arr := doc["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	if len(arr) != 2 {
		t.Fatalf("UserPromptSubmit entries = %d, want 2 (pack hook + core hook):\n%s", len(arr), result)
	}
	assertCanonicalEqual(t, arr[0], `{"hooks":[{"type":"command","command":"packcmd"}]}`)
	assertCanonicalEqual(t, arr[1], `{"matcher":"","hooks":[{"type":"command","command":"gc hook --inject"}]}`)
}

func TestMergeSettingsJSON_WrapBareHooks_TwinDropsWhenMatcherlessCopyExists(t *testing.T) {
	// A staged rollout can leave both shapes on disk: a new binary writes the
	// matcherless entry, an older one re-appends the legacy matcher-"" twin.
	// The twin is redundant with the matcherless copy and must not survive, or
	// the pack hook fires twice on every event forever.
	base := `{"hooks":{"Stop":[` +
		`{"matcher":"","hooks":[{"type":"command","command":"pack"}]},` +
		`{"hooks":[{"type":"command","command":"pack"}]}` +
		`]}}`
	over := []byte(`{"hooks":{"Stop":[{"type":"command","command":"pack"}]}}`)

	result, err := MergeSettingsJSON([]byte(base), over, WithWrapBareHooks())
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	arr := stopEntries(t, result)
	if len(arr) != 1 {
		t.Fatalf("Stop entries = %d, want 1 (legacy twin must not survive beside its matcherless copy)\n%s", len(arr), result)
	}
	assertCanonicalEqual(t, arr[0], `{"hooks":[{"type":"command","command":"pack"}]}`)
}

func TestMergeSettingsJSON_NoWrap_KeepsDuplicateBaseEntries(t *testing.T) {
	// The duplicate collapse and bare-entry wrap are Claude-settings behavior
	// for #3862. Codex and Cursor hooks.json must keep the merge's additive
	// behavior: without WithWrapBareHooks byte-identical base entries survive
	// and bare overlay entries stay bare.
	base := `{"hooks":{"PreToolUse":[{"command":"same"},{"command":"same"}]}}`
	over := `{"hooks":{"PreToolUse":[{"command":"new"},{"bash":"also-new"}]}}`

	result, err := MergeSettingsJSON([]byte(base), []byte(over))
	if err != nil {
		t.Fatalf("MergeSettingsJSON: %v", err)
	}
	arr := preToolUse(t, result)
	if len(arr) != 4 {
		t.Fatalf("PreToolUse entries = %d, want 4 (no-wrap path must be unchanged):\n%s", len(arr), result)
	}
	assertCanonicalEqual(t, arr[0], `{"command":"same"}`)
	assertCanonicalEqual(t, arr[1], `{"command":"same"}`)
	assertCanonicalEqual(t, arr[2], `{"command":"new"}`)
	assertCanonicalEqual(t, arr[3], `{"bash":"also-new"}`)
}
