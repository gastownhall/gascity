package overlay

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFindBareHookEntries_FlagsBareEntry(t *testing.T) {
	data := []byte(`{"hooks":{"PreToolUse":[
		{"matcher":"Bash","hooks":[{"type":"command","command":"ok"}]},
		{"type":"command","command":"bare"}
	]}}`)
	bare, err := FindBareHookEntries(data)
	if err != nil {
		t.Fatalf("FindBareHookEntries: %v", err)
	}
	if len(bare) != 1 {
		t.Fatalf("bare entries = %d, want 1", len(bare))
	}
	if bare[0].Category != "PreToolUse" || bare[0].Index != 1 {
		t.Errorf("got %+v, want {PreToolUse 1}", bare[0])
	}
}

func TestFindBareHookEntries_AllWrappedIsClean(t *testing.T) {
	data := []byte(`{"hooks":{"PreToolUse":[{"matcher":"","hooks":[{"type":"command","command":"ok"}]}]}}`)
	bare, err := FindBareHookEntries(data)
	if err != nil {
		t.Fatalf("FindBareHookEntries: %v", err)
	}
	if len(bare) != 0 {
		t.Errorf("bare entries = %d, want 0", len(bare))
	}
}

func TestFindBareHookEntries_NoHooksObject(t *testing.T) {
	bare, err := FindBareHookEntries([]byte(`{"editorMode":"vim"}`))
	if err != nil {
		t.Fatalf("FindBareHookEntries: %v", err)
	}
	if len(bare) != 0 {
		t.Errorf("bare entries = %d, want 0", len(bare))
	}
}

func TestFindBareHookEntries_MultipleCategoriesSorted(t *testing.T) {
	data := []byte(`{"hooks":{
		"PreToolUse":[{"type":"command","command":"a"}],
		"Stop":[{"type":"command","command":"b"}]
	}}`)
	bare, err := FindBareHookEntries(data)
	if err != nil {
		t.Fatalf("FindBareHookEntries: %v", err)
	}
	if len(bare) != 2 {
		t.Fatalf("bare entries = %d, want 2", len(bare))
	}
	// Deterministic, sorted by category: PreToolUse before Stop.
	if bare[0].Category != "PreToolUse" || bare[1].Category != "Stop" {
		t.Errorf("categories not sorted: %+v", bare)
	}
}

func TestFindBareHookEntries_InvalidJSON(t *testing.T) {
	if _, err := FindBareHookEntries([]byte(`not json`)); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// hookDocWithMatcher builds a settings document with one wrapped hook entry
// under event whose matcher is matcher.
func hookDocWithMatcher(t *testing.T, event, matcher string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"hooks": map[string]any{event: []any{
		map[string]any{
			"matcher": matcher,
			"hooks":   []any{map[string]any{"type": "command", "command": "true"}},
		},
	}}})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	return data
}

func TestFindInvalidHookMatchers_Classification(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		matcher string
		wantSev Severity // empty: the matcher must lint clean
		wantMsg string   // substring the finding's message must carry
	}{
		// Permission-rule syntax: the shape that shipped dead in gm-zlf3a.
		{"permission syntax", "PreToolUse", "Bash(*bd mol pour*patrol*)", SeverityError, "permission-rule syntax"},
		{"permission syntax with alternation", "PreToolUse", "Edit|Write(*.go)", SeverityError, "permission-rule syntax"},
		{"permission syntax that compiles under RE2", "PreToolUse", "Bash(git:*)", SeverityError, "permission-rule syntax"},

		// Not a regular expression at all.
		{"unbalanced bracket", "PreToolUse", "[Bash", SeverityError, "does not compile"},
		{"unbalanced group", "PreToolUse", "^(Bash$", SeverityError, "does not compile"},
		{"nested repetition", "PreToolUse", "**", SeverityError, "does not compile"},
		{"invalid regex on a non-tool event", "SessionStart", "[startup", SeverityError, "does not compile"},

		// JavaScript-only syntax Go cannot compile: advisory, not a failure.
		{"JS negative lookahead only warns", "PreToolUse", "^(?!Bash$).*", SeverityWarning, "JavaScript"},
		{"JS positive lookahead only warns", "PreToolUse", "^Read(?=$)", SeverityWarning, "JavaScript"},
		{"JS lookbehind only warns", "PreToolUse", "(?<=Notebook)Edit", SeverityWarning, "JavaScript"},
		{"JS backreference only warns", "PreToolUse", "^(Bash)\\1?$", SeverityWarning, "JavaScript"},
		{"permission-shaped JS lookahead warns, not errors", "PreToolUse", "Bash(?!Output)", SeverityWarning, "JavaScript"},
		{"JS unicode escape only warns", "PreToolUse", `^\u0042ash$`, SeverityWarning, "JavaScript"},
		{"JS braced unicode escape only warns", "PreToolUse", `^\u{42}ash$`, SeverityWarning, "JavaScript"},
		{"JS named backreference only warns", "PreToolUse", `^(?<t>Bash)\k<t>?$`, SeverityWarning, "JavaScript"},
		{"JS any-char class only warns", "PreToolUse", `^Bas[^]$`, SeverityWarning, "JavaScript"},

		// Compiles, but names nothing Claude Code has.
		{"unknown tool", "PreToolUse", "^Frobnicate$", SeverityWarning, "no known Claude Code tool"},
		{"typo of a real tool", "PostToolUse", "^Bahs$", SeverityWarning, "no known Claude Code tool"},
		{"truncated name is an exact name, not a prefix", "PreToolUse", "Bas", SeverityWarning, "no known Claude Code tool"},
		{"unknown tool on PostToolUseFailure", "PostToolUseFailure", "Frobnicate", SeverityWarning, "no known Claude Code tool"},
		{"unknown tools on PermissionRequest", "PermissionRequest", "Frobnicate|Quux", SeverityWarning, "no known Claude Code tool"},

		// Clean: the all-tools forms.
		{"empty matcher means all tools", "PreToolUse", "", "", ""},
		{"star means all tools", "PreToolUse", "*", "", ""},

		// Clean: forms in use today.
		{"anchored Bash is shipped prior art", "PreToolUse", "^Bash$", "", ""},
		{"bare Bash is a valid exact name", "PreToolUse", "Bash", "", ""},
		{"pipe list of exact names", "PreToolUse", "Edit|Write", "", ""},
		{"comma list of exact names", "PreToolUse", "Edit, Write", "", ""},
		{"exact list with one real tool", "PreToolUse", "Edit|Frobnicate", "", ""},
		{"prefix regex", "PreToolUse", "Notebook.*", "", ""},
		{"grouped prefix regex", "PreToolUse", "Notebook(Edit|Read)", "", ""},
		{"grouped prefix regex over Task tools", "PreToolUse", "Task(Create|Update)", "", ""},
		{"regex is searched, not anchored", "PreToolUse", "Bas.*", "", ""},
		{"grouped anchored alternation", "PreToolUse", "^(Read|Glob|Grep)$", "", ""},
		{"word then group-any is a regex", "PreToolUse", "Bash(.*)", "", ""},
		{"word then anchored empty group", "PreToolUse", "Read($)", "", ""},
		{"MCP tool regex", "PreToolUse", "mcp__memory__.*", "", ""},
		{"exact MCP tool name", "PostToolUse", "mcp__memory__create_entities", "", ""},

		// Clean: non-tool events match sources and triggers, not tool names.
		{"SessionStart source", "SessionStart", "startup", "", ""},
		{"SessionStart alternation", "SessionStart", "startup|resume", "", ""},
		{"PreCompact trigger", "PreCompact", "manual|auto", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings, err := FindInvalidHookMatchers(hookDocWithMatcher(t, tt.event, tt.matcher))
			if err != nil {
				t.Fatalf("FindInvalidHookMatchers: %v", err)
			}
			if tt.wantSev == "" {
				if len(findings) != 0 {
					t.Fatalf("matcher %q under %s: findings = %+v, want none", tt.matcher, tt.event, findings)
				}
				return
			}
			if len(findings) != 1 {
				t.Fatalf("matcher %q under %s: findings = %+v, want exactly 1", tt.matcher, tt.event, findings)
			}
			got := findings[0]
			if got.Severity != tt.wantSev {
				t.Errorf("severity = %q, want %q (message: %s)", got.Severity, tt.wantSev, got.Message)
			}
			if !strings.Contains(got.Message, tt.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", got.Message, tt.wantMsg)
			}
			if got.Category != tt.event || got.Index != 0 {
				t.Errorf("location = %s[%d], want %s[0]", got.Category, got.Index, tt.event)
			}
		})
	}
}

// A permission-syntax matcher also fails to compile. The specific message must
// win, so the author is pointed at the tool-name-regex contract instead of at a
// regexp parse error, and is steered to the anchored form rather than bare Bash.
func TestFindInvalidHookMatchers_PermissionSyntaxOutranksCompileError(t *testing.T) {
	findings, err := FindInvalidHookMatchers(hookDocWithMatcher(t, "PreToolUse", "Bash(*bd mol pour*patrol*)"))
	if err != nil {
		t.Fatalf("FindInvalidHookMatchers: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly 1", findings)
	}
	msg := findings[0].Message
	if strings.Contains(msg, "does not compile") {
		t.Errorf("message = %q, want the permission-syntax message, not the generic compile failure", msg)
	}
	for _, want := range []string{"tool name", "^Bash$"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want it to contain %q", msg, want)
		}
	}
}

func TestFindInvalidHookMatchers_OrdersFindingsByCategoryThenIndex(t *testing.T) {
	data := []byte(`{"hooks":{
		"SessionStart":[{"matcher":"[startup","hooks":[]}],
		"PreToolUse":[
			{"matcher":"^Bash$","hooks":[]},
			{"matcher":"Bash(*x*)","hooks":[]},
			{"matcher":"^Frobnicate$","hooks":[]}
		]
	}}`)
	findings, err := FindInvalidHookMatchers(data)
	if err != nil {
		t.Fatalf("FindInvalidHookMatchers: %v", err)
	}
	want := []struct {
		category string
		index    int
		severity Severity
	}{
		{"PreToolUse", 1, SeverityError},
		{"PreToolUse", 2, SeverityWarning},
		{"SessionStart", 0, SeverityError},
	}
	if len(findings) != len(want) {
		t.Fatalf("findings = %+v, want %d", findings, len(want))
	}
	for i, w := range want {
		got := findings[i]
		if got.Category != w.category || got.Index != w.index || got.Severity != w.severity {
			t.Errorf("findings[%d] = %s[%d] %q, want %s[%d] %q", i, got.Category, got.Index, got.Severity, w.category, w.index, w.severity)
		}
	}
}

// An entry with no matcher key is not a matcher problem: a wrapped entry
// without one means "all tools", and a bare entry is FindBareHookEntries's
// finding.
func TestFindInvalidHookMatchers_IgnoresEntriesWithoutMatcher(t *testing.T) {
	data := []byte(`{"hooks":{"PreToolUse":[
		{"hooks":[{"type":"command","command":"ok"}]},
		{"type":"command","command":"ok"}
	]}}`)
	findings, err := FindInvalidHookMatchers(data)
	if err != nil {
		t.Fatalf("FindInvalidHookMatchers: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want none", findings)
	}
}

func TestFindInvalidHookMatchers_NoHooksObject(t *testing.T) {
	findings, err := FindInvalidHookMatchers([]byte(`{"editorMode":"vim"}`))
	if err != nil {
		t.Fatalf("FindInvalidHookMatchers: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want none", findings)
	}
}

func TestFindInvalidHookMatchers_InvalidJSON(t *testing.T) {
	if _, err := FindInvalidHookMatchers([]byte(`not json`)); err == nil {
		t.Error("expected error for invalid JSON")
	}
}
