package overlay

import (
	"encoding/json"
	"regexp"
	"sort"
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
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return nil, nil
	}

	categories := make([]string, 0, len(hooks))
	for category := range hooks {
		categories = append(categories, category)
	}
	sort.Strings(categories)

	var bare []BareHookEntry
	for _, category := range categories {
		arr, ok := hooks[category].([]any)
		if !ok {
			continue
		}
		for i, entry := range arr {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if _, hasHooks := m["hooks"]; hasHooks {
				continue
			}
			if _, hasMatcher := m["matcher"]; hasMatcher {
				continue
			}
			bare = append(bare, BareHookEntry{Category: category, Index: i})
		}
	}
	return bare, nil
}

// InvalidHookMatcher locates a top-level hook entry whose "matcher" value
// does not compile as a Claude Code tool-name regular expression.
type InvalidHookMatcher struct {
	Category string
	Index    int
	Matcher  string
	Err      error
}

// FindInvalidHookMatchers parses a .claude/settings.json document and returns
// every top-level hook entry whose "matcher" value fails to compile as a
// regular expression. Claude Code interprets a hook's matcher as a tool-name
// regular expression, not as permission-rule Tool(args) syntax; a matcher
// that cannot compile never fires, so a guard written in permission syntax
// silently never runs while still looking like it does. An empty matcher or
// "*" are Claude Code's own match-all forms and are not flagged, nor is an
// entry with no "matcher" key (a bare entry, reported separately by
// FindBareHookEntries) or a non-string matcher value. Categories are scanned
// in sorted order for deterministic output.
func FindInvalidHookMatchers(data []byte) ([]InvalidHookMatcher, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return nil, nil
	}

	categories := make([]string, 0, len(hooks))
	for category := range hooks {
		categories = append(categories, category)
	}
	sort.Strings(categories)

	var invalid []InvalidHookMatcher
	for _, category := range categories {
		arr, ok := hooks[category].([]any)
		if !ok {
			continue
		}
		for i, entry := range arr {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			rawMatcher, hasMatcher := m["matcher"]
			if !hasMatcher {
				continue
			}
			matcher, ok := rawMatcher.(string)
			if !ok {
				continue
			}
			if matcher == "" || matcher == "*" {
				continue
			}
			if _, err := regexp.Compile(matcher); err != nil {
				invalid = append(invalid, InvalidHookMatcher{Category: category, Index: i, Matcher: matcher, Err: err})
			}
		}
	}
	return invalid, nil
}
