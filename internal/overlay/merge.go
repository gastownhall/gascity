// Package overlay — merge-aware copy for provider hook/settings files.
package overlay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
//   - With WithWrapBareHooks, the base array is also healed of the residue the
//     pre-fix append loop left behind, so bloated files converge instead of
//     needing a manual dedup — see healBaseEntries. Without the option the merge
//     stays additive, which is correct for Codex/Cursor hooks.json.
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
			result["hooks"] = mergeHooksMap(baseHooks, overHooks, cfg.wrapBareHooks)
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
func mergeHooksMap(base, over map[string]any, wrapBareHooks bool) map[string]any {
	result := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range over {
		overArr, okOver := toSliceAny(v)
		baseArr, okBase := toSliceAny(result[k])
		if okOver && okBase {
			result[k] = mergeHookArray(baseArr, overArr, wrapBareHooks)
		} else {
			result[k] = v
		}
	}
	return result
}

// mergeHookArray merges two arrays of hook entries by identity key.
// Entries with the same identity → overlay replaces base in-place.
// New entries → appended.
//
// For wrap-style (Claude) merges, base is first healed of the residue the
// pre-fix append loop left behind — see healBaseEntries.
//
// Known limitation, unchanged from before #3862: baseIdx maps an identity to a
// single index, so when a category holds several entries sharing one identity
// (e.g. two matcher-"" hooks with different commands) only the last is
// replaceable; an overlay entry with that identity replaces it and leaves the
// earlier one untouched. Bare pack hooks are kept out of the matcher-"" slot
// precisely so the fix does not add traffic to that ambiguity.
func mergeHookArray(base, over []any, wrapBareHooks bool) []any {
	result := base
	if wrapBareHooks {
		result = healBaseEntries(base)
	}

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
		idx, found := baseIdx[key]
		if !found && strings.HasPrefix(key, "inner:") {
			// A content-keyed wrapper also unifies with a legacy
			// {"matcher": "", "hooks": [...]} twin carrying the same inner
			// hooks: matcher "" and no matcher are equivalent (both match
			// everything), and the pre-#3862 wrap normalization stamped
			// matcher "" onto bare pack entries. Replacing the twin migrates
			// it to the content-keyed shape.
			if tidx, ok := emptyMatcherTwin(result, key); ok {
				idx, found = tidx, true
				// The migrated entry no longer carries matcher "". Drop the
				// matcher-"" index only when it pointed at that entry, so a
				// core hook holding the slot elsewhere in this category still
				// replaces in place instead of appending a stale duplicate.
				if slot, ok := baseIdx[""]; ok && slot == tidx {
					delete(baseIdx, "")
				}
			}
		}
		if found {
			// Same identity → replace in-place.
			result[idx] = entry
			baseIdx[key] = idx
		} else {
			// New identity → append.
			result = append(result, entry)
			baseIdx[key] = len(result) - 1
		}
	}
	return result
}

// healBaseEntries removes redundant copies of a hook from an existing Claude
// settings array, so a file already bloated by the #3862 append loop converges
// on the next merge instead of needing a manual dedup. Two kinds of residue go:
//
//   - byte-identical duplicates of an identity-keyed entry, which collapse to
//     their first occurrence: same identity already means "one entry", so exact
//     copies carry no information.
//   - a legacy {"matcher": "", "hooks": [X]} twin that sits beside a matcherless
//     {"hooks": [X]} entry for the same command. Both fire on every event, so the
//     twin is pure duplication — a staged rollout leaves this shape behind when a
//     newer binary writes the matcherless form and an older one re-appends the
//     wrapped one. The matcherless entry is kept because it carries the content
//     identity the merge keys on.
//
// Entries without an identity key keep their documented always-append semantics
// and are never removed. Only wrap-style (Claude) merges call this; Codex and
// Cursor hooks.json keep the merge's additive behavior.
func healBaseEntries(base []any) []any {
	matcherless := make(map[string]bool) // inner-hooks key → a matcherless entry exists
	for _, entry := range base {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if _, hasMatcher := m["matcher"]; hasMatcher {
			continue
		}
		if inner, ok := m["hooks"]; ok {
			if key, ok := innerHooksKey(inner); ok {
				matcherless[key] = true
			}
		}
	}

	result := make([]any, 0, len(base))
	seen := make(map[string]bool) // canonical JSON of identity-keyed entries
	for _, entry := range base {
		m, ok := entry.(map[string]any)
		if !ok {
			result = append(result, entry)
			continue
		}
		if _, hasKey := hookEntryKey(m); !hasKey {
			result = append(result, entry)
			continue
		}
		if isRedundantEmptyMatcherTwin(m, matcherless) {
			continue
		}
		// An entry that cannot be canonicalized has no comparable form, so it
		// cannot be proven a duplicate. Keep it: dropping an entry we failed to
		// read would lose a hook, and collapsing is only ever an optimization
		// over the correct always-keep behavior. Unreachable in practice —
		// these entries were just unmarshaled from JSON.
		if canon, err := MarshalCanonicalJSON(m); err == nil {
			c := string(canon)
			if seen[c] {
				continue
			}
			seen[c] = true
		}
		result = append(result, entry)
	}
	return result
}

// isRedundantEmptyMatcherTwin reports whether entry is a {"matcher": "",
// "hooks": [X]} wrapper whose command content is already carried by a
// matcherless entry in the same category.
func isRedundantEmptyMatcherTwin(entry map[string]any, matcherless map[string]bool) bool {
	if matcher, ok := entry["matcher"].(string); !ok || matcher != "" {
		return false
	}
	inner, ok := entry["hooks"]
	if !ok {
		return false
	}
	key, ok := innerHooksKey(inner)
	return ok && matcherless[key]
}

// emptyMatcherTwin locates an entry carrying matcher "" whose inner hooks match
// innerKey. Such an entry is the same hook as a content-keyed wrapper — only its
// shape differs. Entries are scanned by content rather than through the
// matcher-"" index, which holds just one entry per category and may point at an
// unrelated core hook.
func emptyMatcherTwin(result []any, innerKey string) (int, bool) {
	for i, entry := range result {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if matcher, ok := m["matcher"].(string); !ok || matcher != "" {
			continue
		}
		inner, ok := m["hooks"]
		if !ok {
			continue
		}
		if key, ok := innerHooksKey(inner); ok && key == innerKey {
			return i, true
		}
	}
	return 0, false
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
