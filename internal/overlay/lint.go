package overlay

import (
	"encoding/json"
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
