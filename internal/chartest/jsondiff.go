package chartest

import (
	"encoding/json"
	"fmt"
	"strings"
)

// JSONShapeDiff compares two JSON documents under the locked CLI-unification
// safety bar for JSON surfaces — "exact modulo declared-additive fields" — and
// returns a human-readable diff ("" means match). Rules:
//
//   - every key present in want must be present in got with an equal value
//     (recursively); a missing or changed value is a diff.
//   - got MAY carry extra object keys only if the key name is in additive
//     (Move-1 adds fields like molecule_id to result types); an undeclared
//     extra key is a diff.
//   - arrays compare as MULTISETS (order-insensitive): gc list commands do not
//     contract element order (the convoy-list pilot proved --json order is
//     non-deterministic), so a reorder is not a regression, but a changed or
//     missing element, or a length change, still is.
//
// additive is a set of bare key names allowed to appear extra at any depth.
// Coarser than JSON-path scoping on purpose; tighten if a key must be additive
// in one place but exact in another.
func JSONShapeDiff(want, got []byte, additive []string) string {
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		return fmt.Sprintf("want is not valid JSON: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		return fmt.Sprintf("got is not valid JSON: %v", err)
	}
	allow := make(map[string]bool, len(additive))
	for _, k := range additive {
		allow[k] = true
	}
	if diff := shapeDiff("$", w, g, allow); diff != "" {
		return diff
	}
	return ""
}

func shapeDiff(path string, want, got any, additive map[string]bool) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: want object, got %s", path, typeName(got))
		}
		for k, wv := range w {
			gv, present := g[k]
			if !present {
				return fmt.Sprintf("%s.%s: missing in got", path, k)
			}
			if d := shapeDiff(path+"."+k, wv, gv, additive); d != "" {
				return d
			}
		}
		for k := range g {
			if _, inWant := w[k]; !inWant && !additive[k] {
				return fmt.Sprintf("%s.%s: undeclared extra key in got", path, k)
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok {
			return fmt.Sprintf("%s: want array, got %s", path, typeName(got))
		}
		return multisetDiff(path, w, g, additive)
	default:
		if !scalarEqual(want, got) {
			return fmt.Sprintf("%s: want %v, got %v", path, want, got)
		}
		return ""
	}
}

// multisetDiff matches each want element to a distinct got element, ignoring
// order. O(n^2) — fine for CLI list sizes.
func multisetDiff(path string, want, got []any, additive map[string]bool) string {
	if len(want) != len(got) {
		return fmt.Sprintf("%s: array length want %d, got %d", path, len(want), len(got))
	}
	used := make([]bool, len(got))
	for wi, wv := range want {
		matched := false
		for gi, gv := range got {
			if used[gi] {
				continue
			}
			if shapeDiff(fmt.Sprintf("%s[%d]", path, wi), wv, gv, additive) == "" {
				used[gi] = true
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Sprintf("%s[%d]: no matching element in got (%s)", path, wi, compact(wv))
		}
	}
	return ""
}

func scalarEqual(a, b any) bool {
	// json.Unmarshal yields float64 for all numbers, string/bool/nil otherwise —
	// so == is correct for the leaf types.
	return a == b
}

func typeName(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}

func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return strings.TrimSpace(string(b))
}
