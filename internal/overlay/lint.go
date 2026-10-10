package overlay

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// BareHookEntry locates a top-level hook entry in a Claude settings document
// that uses the invalid bare shape — one with neither a "matcher" nor a
// "hooks" key.
type BareHookEntry struct {
	Category string
	Index    int
}

// FindBareHookEntries parses a .claude/settings.json document and returns every
// top-level hook entry that is bare, e.g. {"type": "command", "command": ...}.
// Claude Code requires the wrapped {"matcher": ..., "hooks": [...]} shape, so a
// bare entry is a pack-authoring mistake that produces an invalid settings file
// once projected. Categories are scanned in sorted order for deterministic
// output. A document that isn't a JSON object yields a parse error; a document
// with no "hooks" object yields no findings.
func FindBareHookEntries(data []byte) ([]BareHookEntry, error) {
	var bare []BareHookEntry
	err := forEachHookEntry(data, func(category string, index int, entry map[string]any) {
		if _, hasHooks := entry["hooks"]; hasHooks {
			return
		}
		if _, hasMatcher := entry["matcher"]; hasMatcher {
			return
		}
		bare = append(bare, BareHookEntry{Category: category, Index: index})
	})
	if err != nil {
		return nil, err
	}
	return bare, nil
}

// forEachHookEntry calls visit for every top-level hook entry in a Claude
// settings document that is a JSON object, categories in sorted order so
// findings are deterministic. A document that isn't a JSON object yields a
// parse error; a document with no "hooks" object visits nothing.
func forEachHookEntry(data []byte, visit func(category string, index int, entry map[string]any)) error {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return nil
	}

	categories := make([]string, 0, len(hooks))
	for category := range hooks {
		categories = append(categories, category)
	}
	sort.Strings(categories)

	for _, category := range categories {
		arr, ok := hooks[category].([]any)
		if !ok {
			continue
		}
		for i, entry := range arr {
			if m, ok := entry.(map[string]any); ok {
				visit(category, i, m)
			}
		}
	}
	return nil
}

// Severity ranks a lint finding: an error fails `gc lint`, a warning is advisory.
type Severity string

const (
	// SeverityError marks a finding that must fail the lint.
	SeverityError Severity = "error"
	// SeverityWarning marks an advisory finding.
	SeverityWarning Severity = "warning"
)

// HookMatcherFinding locates a hook entry whose matcher cannot do what its
// author meant, and says why.
type HookMatcherFinding struct {
	Category string
	Index    int
	Severity Severity
	Message  string
}

// permissionSyntaxMatcher matches a matcher written as a permission rule,
// Tool(args), which is not a regular expression over tool names.
var permissionSyntaxMatcher = regexp.MustCompile(`^[A-Za-z|]+\(`)

// jsOnlyRegexSyntax matches constructs JavaScript regexes support and Go
// RE2 rejects: lookaround, numbered and named backreferences, \u escapes
// and the [^] any-character class. Claude Code evaluates matchers as
// JavaScript regexes, so a matcher using them may work there and lint
// cannot call it broken.
var jsOnlyRegexSyntax = regexp.MustCompile(`\(\?<?[=!]|\\[1-9]|\\k<|\\u[0-9A-Fa-f{]|\[\^\]`)

// exactNameMatcher matches a matcher Claude Code reads as a list of exact tool
// names instead of a regex: only letters, digits, underscores, hyphens, spaces,
// commas and pipes.
var exactNameMatcher = regexp.MustCompile(`^[A-Za-z0-9_\- ,|]+$`)

// mcpToolPrefix starts the name of every MCP tool. Those names are defined by
// their servers, so the linter cannot know them.
const mcpToolPrefix = "mcp__"

// toolMatcherEvents are the hook events whose matcher filters on a tool name.
// The others match something else (SessionStart: how the session began,
// PreCompact: manual or auto), so a tool-name check would misfire on them.
var toolMatcherEvents = map[string]bool{
	"PreToolUse":         true,
	"PostToolUse":        true,
	"PostToolUseFailure": true,
	"PermissionRequest":  true,
}

// knownToolNames are the built-in Claude Code tools a hook matcher can name.
var knownToolNames = []string{
	"Agent", "AskUserQuestion", "Bash", "CronCreate", "CronDelete", "CronList",
	"Edit", "EnterPlanMode", "EnterWorktree", "ExitPlanMode", "ExitWorktree",
	"Glob", "Grep", "ListMcpResourcesTool", "LSP", "Monitor", "NotebookEdit",
	"PowerShell", "PushNotification", "Read", "ReadMcpResourceTool",
	"ScheduleWakeup", "SendMessage", "Skill", "Task", "TaskCreate", "TaskGet",
	"TaskList", "TaskOutput", "TaskStop", "TaskUpdate", "TodoWrite", "WebFetch",
	"WebSearch", "Write",
}

// FindInvalidHookMatchers parses a .claude/settings.json document and returns a
// finding for every wrapped hook entry whose matcher cannot do what its author
// meant:
//
//   - an error when the matcher is permission-rule syntax such as
//     Bash(*bd mol pour*) and either fails to compile or, on an event that
//     matches tool names, selects no known Claude Code tool;
//   - an error when any other matcher does not compile as a regular expression;
//   - a warning when the matcher uses JavaScript-only regex syntax (lookaround
//     or backreferences) that Go cannot compile, so lint cannot check it;
//   - a warning, on events that match tool names, when the matcher compiles but
//     selects no known Claude Code tool.
//
// The empty matcher and "*" mean all tools and are always valid. An entry with
// no matcher is not this function's concern (see FindBareHookEntries). Findings
// are ordered by category, then index.
func FindInvalidHookMatchers(data []byte) ([]HookMatcherFinding, error) {
	var findings []HookMatcherFinding
	err := forEachHookEntry(data, func(category string, index int, entry map[string]any) {
		matcher, ok := entry["matcher"].(string)
		if !ok || matcher == "" || matcher == "*" {
			return
		}
		severity, problem := hookMatcherProblem(category, matcher)
		if severity == "" {
			return
		}
		findings = append(findings, HookMatcherFinding{
			Category: category,
			Index:    index,
			Severity: severity,
			Message:  fmt.Sprintf("hooks.%s[%d] matcher %q %s", category, index, matcher, problem),
		})
	})
	if err != nil {
		return nil, err
	}
	return findings, nil
}

// hookMatcherProblem classifies matcher, found under the given hook event. It
// returns an empty severity when the matcher is usable. A matcher that fails to
// compile only warns when it uses JavaScript-only syntax, since Claude Code may
// accept it; this check runs first, so it also covers a permission-shaped
// matcher such as Bash(?!Output). Otherwise a matcher shaped like a permission
// rule, Tool(args), is flagged as such when it fails to compile or, on a tool
// event, selects no known tool: the author needs that specific explanation, not
// a parse error or a typo warning. A valid grouped regex such as
// Notebook(Edit|Read) selects real tools and lints clean.
func hookMatcherProblem(category, matcher string) (Severity, string) {
	permissionSyntax := permissionSyntaxMatcher.MatchString(matcher)
	re, err := regexp.Compile(matcher)
	if err != nil {
		if jsOnlyRegexSyntax.MatchString(matcher) {
			return SeverityWarning, fmt.Sprintf("uses JavaScript-only regex syntax Go cannot check (%v); Claude Code evaluates matchers as JavaScript regexes, so verify it by hand", err)
		}
		if permissionSyntax {
			return SeverityError, permissionSyntaxProblem
		}
		return SeverityError, fmt.Sprintf("does not compile as a regular expression: %v", err)
	}
	if toolMatcherEvents[category] && !strings.Contains(matcher, mcpToolPrefix) && !selectsKnownTool(matcher, re) {
		if permissionSyntax {
			return SeverityError, permissionSyntaxProblem
		}
		return SeverityWarning, "compiles but matches no known Claude Code tool name; check for a typo"
	}
	return "", ""
}

// permissionSyntaxProblem explains a matcher written as a permission rule.
const permissionSyntaxProblem = "is permission-rule syntax (Tool(args)), not a regex; hook matchers are regexes matched against the tool name only. Use e.g. ^Bash$ and filter on tool_input inside the hook"

// selectsKnownTool reports whether matcher selects at least one known tool,
// reading it the way Claude Code does: a matcher made only of name characters
// is a list of exact names, and anything else is an unanchored regex searched
// in each name.
func selectsKnownTool(matcher string, re *regexp.Regexp) bool {
	if exactNameMatcher.MatchString(matcher) {
		names := strings.FieldsFunc(matcher, func(r rune) bool { return r == '|' || r == ',' || r == ' ' })
		return slices.ContainsFunc(names, func(name string) bool { return slices.Contains(knownToolNames, name) })
	}
	return slices.ContainsFunc(knownToolNames, re.MatchString)
}
