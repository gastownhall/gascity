// Package overlay — merge-aware copy for provider hook/settings files.
package overlay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
)

// mergeablePaths is the set of relative paths that get JSON-level merge
// instead of file-level overwrite when both base and overlay exist.
var mergeablePaths = map[string]bool{
	filepath.Join(".agents", "hooks.json"):            true,
	filepath.Join(".claude", "settings.json"):         true,
	filepath.Join(".gemini", "settings.json"):         true,
	filepath.Join(".codex", "hooks.json"):             true,
	filepath.Join(".cursor", "hooks.json"):            true,
	filepath.Join(".github", "hooks", "gascity.json"): true,
}

// wrapBareHookPaths is the set of settings files whose top-level hook entries
// must carry a "hooks" array. For these files a bare entry such as
// {"type": "command", "command": "..."} is schema-invalid at the top level, so
// it is normalized into wrapped form during merge. The "matcher" key is
// optional — omitting it activates the group on every occurrence of the event —
// and the normalized form deliberately leaves it out so the entry keys on its
// command content rather than claiming the shared matcher-"" identity slot.
//
// Only Claude Code's .claude/settings.json is included here. Codex and Cursor
// hooks.json legitimately use bare {"command": ...}/{"bash": ...} entries and
// must NOT be wrapped.
var wrapBareHookPaths = map[string]bool{
	filepath.Join(".claude", "settings.json"): true,
}

var (
	errBaseNotObject    = errors.New("base JSON is not an object")
	errOverlayNotObject = errors.New("overlay JSON is not an object")
)

// IsOverlayObjectShapeError reports whether err indicates an overlay document
// was syntactically valid JSON but not a top-level object.
func IsOverlayObjectShapeError(err error) bool {
	return errors.Is(err, errOverlayNotObject)
}

// IsMergeablePath reports whether relPath is a known settings/hooks file
// that should be JSON-merged rather than overwritten.
func IsMergeablePath(relPath string) bool {
	return mergeablePaths[filepath.Clean(relPath)]
}

// WrapsBareHooks reports whether relPath is a settings file that requires
// wrapped hook entries, so bare/flat entries should be normalized into
// {"hooks": [entry]} form during merge.
func WrapsBareHooks(relPath string) bool {
	return wrapBareHookPaths[filepath.Clean(relPath)]
}

// MergeOption configures MergeSettingsJSON.
type MergeOption func(*mergeConfig)

type mergeConfig struct {
	wrapBareHooks bool
}

// WithWrapBareHooks normalizes bare/flat hook entries (e.g.
// {"type": "command", "command": "..."}) into the wrapped
// {"hooks": [entry]} shape that Claude settings require. Pass it
// when merging a .claude/settings.json (see WrapsBareHooks). Without it the
// merge preserves entry shapes verbatim, which is correct for Codex/Cursor
// hooks.json.
func WithWrapBareHooks() MergeOption {
	return func(c *mergeConfig) { c.wrapBareHooks = true }
}

// MergeSettingsJSON performs a deep merge of base and overlay JSON documents.
// Both documents must be top-level JSON objects.
//
// Merge semantics:
//   - Non-hook top-level keys: last writer (overlay) wins.
//   - Hook categories (keys under "hooks"): union across layers.
//   - Entries within a hook category: merged by identity key.
//     Same identity → overlay replaces base entry. New identity → appended.
//   - Identity key extraction:
//     1. "matcher" key → identity is the matcher value
//     2. "command" key → identity is "cmd:<value>"
//     3. "bash" key → identity is "bash:<value>"
//     4. nested "hooks" array (Claude/Gemini wrapper shape with no top-level
//     matcher/command) → identity is "inner:<canonical inner hooks>", so an
//     overlay re-projecting an already-present command is a no-op instead of
//     an unbounded append.
//     5. else → no identity, always append
//   - With WithWrapBareHooks, bare entries (those with neither a "matcher" nor
//     a "hooks" key) in BOTH documents are normalized into {"hooks": [entry]}
//     BEFORE the keyed merge. Pre-merge wrapping means a bare overlay entry and
//     its wrapped on-disk form share the same content identity (rule 4), so a
//     re-projected pack hook replaces its prior copy instead of appending a new
//     one on every reconcile tick (#3862). The matcherless target shape keeps
//     bare pack hooks out of the matcher-"" identity slot, which core gc hooks
//     use for same-matcher replacement.
//
// Returns pretty-printed JSON.
func MergeSettingsJSON(base, overlay []byte, opts ...MergeOption) ([]byte, error) {
	var cfg mergeConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	baseDoc, err := parseSettingsObject("base", base, errBaseNotObject)
	if err != nil {
		return nil, err
	}
	overDoc, err := parseSettingsObject("overlay", overlay, errOverlayNotObject)
	if err != nil {
		return nil, err
	}

	// For wrap-style providers, normalize bare hook entries in both documents
	// before merging so a bare overlay entry keys identically to its wrapped
	// on-disk form. Wrapping after the merge left the two shapes with distinct
	// identities, and every re-projection appended another copy (#3862).
	if cfg.wrapBareHooks {
		if h, ok := baseDoc["hooks"].(map[string]any); ok {
			baseDoc["hooks"] = wrapBareHookEntries(h)
		}
		if h, ok := overDoc["hooks"].(map[string]any); ok {
			overDoc["hooks"] = wrapBareHookEntries(h)
		}
	}

	// Start with a copy of base, then apply overlay on top.
	result := make(map[string]any, len(baseDoc)+len(overDoc))
	for k, v := range baseDoc {
		result[k] = v
	}

	for k, v := range overDoc {
		if k == "hooks" {
			baseHooks := toMapStringAny(baseDoc["hooks"])
			overHooks := toMapStringAny(v)
			result["hooks"] = mergeHooksMap(baseHooks, overHooks)
		} else {
			// Non-hook keys: last writer wins.
			result[k] = v
		}
	}

	out, err := MarshalCanonicalJSON(result)
	if err != nil {
		return nil, fmt.Errorf("merge: marshaling result: %w", err)
	}
	return out, nil
}

func parseSettingsObject(label string, data []byte, shapeErr error) (map[string]any, error) {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("merge: parsing %s: %w", label, err)
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("merge: parsing %s: expected JSON object: %w", label, shapeErr)
	}
	return obj, nil
}

// CanonicalJSON parses and re-emits a JSON document with stable formatting.
func CanonicalJSON(data []byte) ([]byte, error) {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return MarshalCanonicalJSON(doc)
}

// MarshalCanonicalJSON emits JSON with deterministic indentation, no HTML
// escaping, and a trailing newline.
func MarshalCanonicalJSON(doc any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mergeHooksMap unions hook categories from base and overlay.
// Categories present in only one side are preserved as-is.
// Categories present in both get entry-level merge.
func mergeHooksMap(base, over map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range over {
		overArr, okOver := toSliceAny(v)
		baseArr, okBase := toSliceAny(result[k])
		if okOver && okBase {
			result[k] = mergeHookArray(baseArr, overArr)
		} else {
			result[k] = v
		}
	}
	return result
}

// mergeHookArray merges two arrays of hook entries by identity key.
// Entries with the same identity → overlay replaces base in-place.
// New entries → appended.
func mergeHookArray(base, over []any) []any {
	// Build ordered result starting from base entries.
	result := make([]any, len(base))
	copy(result, base)

	// Index base entries by identity for in-place replacement.
	baseIdx := make(map[string]int) // identity → index in result
	for i, entry := range result {
		if m, ok := entry.(map[string]any); ok {
			if key, hasKey := hookEntryKey(m); hasKey {
				baseIdx[key] = i
			}
		}
	}

	for _, entry := range over {
		m, ok := entry.(map[string]any)
		if !ok {
			result = append(result, entry)
			continue
		}
		key, hasKey := hookEntryKey(m)
		if !hasKey {
			// No identity → always append.
			result = append(result, entry)
			continue
		}
		if idx, found := baseIdx[key]; found {
			// Same identity → replace in-place.
			result[idx] = entry
		} else {
			// New identity → append.
			result = append(result, entry)
			baseIdx[key] = len(result) - 1
		}
	}
	return result
}

// hookEntryKey extracts the identity key from a hook entry.
// Returns the key string and true if an identity was found.
func hookEntryKey(entry map[string]any) (string, bool) {
	if v, ok := entry["matcher"]; ok {
		s, sok := v.(string)
		if !sok {
			return "", false
		}
		return s, true
	}
	if v, ok := entry["command"]; ok {
		s, sok := v.(string)
		if !sok {
			return "", false
		}
		return "cmd:" + s, true
	}
	if v, ok := entry["bash"]; ok {
		s, sok := v.(string)
		if !sok {
			return "", false
		}
		return "bash:" + s, true
	}
	// Claude/Gemini wrapper shape: { "hooks": [ {type, command}, ... ] } with
	// no top-level matcher/command. Pack overlays (e.g. model-advisor's
	// Stop/SubagentStop) use this shape, and without an identity key every
	// re-projection appended another copy — accumulating unbounded across
	// session starts. Key on the canonicalized inner hooks so that re-merging
	// the same command(s) is idempotent (dedup by inner command content).
	if v, ok := entry["hooks"]; ok {
		if key, kok := innerHooksKey(v); kok {
			return key, true
		}
	}
	return "", false
}

// innerHooksKey derives a stable identity from the inner "hooks" array of a
// wrapper-shape entry. The key is the canonical (sorted-key, HTML-unescaped)
// JSON of the inner array, so two entries carrying identical command(s) collapse
// to one regardless of key ordering or whitespace. Returns false if the value
// is not a JSON array (leave such entries to the always-append fallback).
func innerHooksKey(inner any) (string, bool) {
	if _, ok := inner.([]any); !ok {
		return "", false
	}
	canon, err := MarshalCanonicalJSON(inner)
	if err != nil {
		return "", false
	}
	return "inner:" + string(bytes.TrimRight(canon, "\n")), true
}

// wrapBareHookEntries returns a copy of a hooks map in which every bare
// top-level entry — one with neither a "matcher" nor a "hooks" key, e.g.
// {"type": "command", "command": "..."} — is normalized into the wrapped
// {"hooks": [entry]} shape that Claude settings require.
// Already-wrapped entries are left unchanged. No entries are added or removed.
func wrapBareHookEntries(hooks map[string]any) map[string]any {
	out := make(map[string]any, len(hooks))
	for category, v := range hooks {
		arr, ok := toSliceAny(v)
		if !ok {
			out[category] = v
			continue
		}
		normalized := make([]any, len(arr))
		for i, entry := range arr {
			normalized[i] = normalizeHookEntry(entry)
		}
		out[category] = normalized
	}
	return out
}

// normalizeHookEntry wraps a bare hook entry into {"hooks": [entry]} form.
// Entries that already carry a "matcher" or "hooks" key (or are not JSON
// objects) are returned unchanged.
//
// The wrapped form deliberately omits "matcher" (Claude treats an absent
// matcher and matcher "" identically — both match everything): a matcherless
// wrapper keys on its inner command content (see hookEntryKey), so identical
// re-projections replace instead of append and distinct bare hooks coexist.
// Stamping matcher "" instead would put every bare pack hook in the single
// matcher-"" identity slot that core gc hooks use for same-matcher
// replacement — colliding with them and with each other.
func normalizeHookEntry(entry any) any {
	m, ok := entry.(map[string]any)
	if !ok {
		return entry
	}
	if _, hasHooks := m["hooks"]; hasHooks {
		return entry
	}
	if _, hasMatcher := m["matcher"]; hasMatcher {
		return entry
	}
	return map[string]any{
		"hooks": []any{entry},
	}
}

// toMapStringAny attempts to convert v to map[string]any.
// Returns nil if v is nil or not the expected type.
func toMapStringAny(v any) map[string]any {
	if v == nil {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// toSliceAny attempts to convert v to []any.
func toSliceAny(v any) ([]any, bool) {
	if v == nil {
		return nil, false
	}
	s, ok := v.([]any)
	return s, ok
}
