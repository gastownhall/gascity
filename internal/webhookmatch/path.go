package webhookmatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ParseBody decodes a raw webhook body into a generic tree suitable for path
// resolution. It uses json.Number so numeric payload fields round-trip to their
// exact source text (e.g. a PR number "1347" stays "1347"), which the extraction
// coercion relies on. A body whose top-level value is not a JSON object is
// rejected: every webhook provider this receiver targets posts an object.
func ParseBody(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("webhookmatch: parse body: %w", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("webhookmatch: parse body: top-level JSON value is %T, want object", v)
	}
	return obj, nil
}

// resolvePath walks a dotted path into a decoded JSON tree. Object segments
// index by key; array segments index by a non-negative decimal position. It
// reports (value, true) when the whole path resolves — including a resolved
// JSON null, which is (nil, true) — and (nil, false) when any segment is
// missing, out of range, or lands on a scalar mid-path. It never panics.
func resolvePath(root any, path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	cur := root
	for _, part := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[part]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(part)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			// A scalar (or nil) with path segments remaining cannot be indexed.
			return nil, false
		}
	}
	return cur, true
}

// coerceToString renders a resolved JSON value as a deterministic string.
// Scalars map to their natural text; JSON null maps to the empty string; a
// nested object or array maps to its compact JSON encoding. The result is always
// an inert string — never re-interpreted as a path or command by this package.
func coerceToString(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case json.Number:
		return t.String(), nil
	case float64:
		// Only reached if a caller parsed the body without UseNumber; keep the
		// shortest exact decimal so integral values don't grow a ".000000" tail.
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return "", fmt.Errorf("webhookmatch: encode value of type %T: %w", v, err)
		}
		return string(b), nil
	}
}
