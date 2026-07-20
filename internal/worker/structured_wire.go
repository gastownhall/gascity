package worker

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// providerNativeForbiddenTokens is the canonical set of provider-native JSON
// keys (and key-like tokens) that must never appear on the provider-neutral
// structured wire. The structured contract maps every provider fact onto typed
// snake_case neutral fields, so the presence of any of these tokens means a
// provider-native shape leaked through normalization.
//
// Every token is a camelCase or underscore-prefixed provider key, chosen so it
// can never be a substring of a legitimate snake_case neutral key (for example
// the native "exitCode" cannot collide with the neutral "exit_code", and the
// native "tool_use_id" cannot collide with the neutral "tool_call_id"). This is
// the single source of truth for leakage assertions across the API projection
// tests and the worker-conformance suite.
var providerNativeForbiddenTokens = []string{
	// Claude tool-result envelope, edit, and result-display shapes.
	"toolUseResult", "resultDisplay", "structuredPatch",
	"_diffHtml", "_highlightedContentHtml", "_renderedHtml",
	"oldString", "newString", "originalFile", "replaceAll", "userModified",
	"filePath", "fileText", "fileSize", "linesCreated", "totalLines",
	"totalChars", "appliedLimit", "multiSelect", "oldTodos", "newTodos",
	"activeForm", "readToolCall", "writeToolCall",
	// Provider-native tool-call identifiers.
	"tool_use_id", "toolCallId", "callID", "provider_result",
	// Codex / function-call native event and result shapes.
	"functionResponse", "event_msg", "codex_error_info",
	"shellId", "sessionId", "stdoutLines", "stderrLines", "exitCode",
	"taskId", "taskType", "totalDurationMs", "totalToolUseCount",
}

// ProviderNativeForbiddenTokens returns a copy of the canonical denylist of
// provider-native tokens that must never cross the structured wire.
func ProviderNativeForbiddenTokens() []string {
	return append([]string(nil), providerNativeForbiddenTokens...)
}

// ScanForbiddenTokens reports which provider-native tokens (the canonical
// denylist plus any extra case-specific tokens) appear as substrings of the
// serialized structured wire. An empty result means no known provider-native
// shape leaked. Results are de-duplicated and sorted for stable assertions.
func ScanForbiddenTokens(wire []byte, extra ...string) []string {
	haystack := string(wire)
	hits := map[string]struct{}{}
	for _, token := range providerNativeForbiddenTokens {
		if token != "" && strings.Contains(haystack, token) {
			hits[token] = struct{}{}
		}
	}
	for _, token := range extra {
		if token != "" && strings.Contains(haystack, token) {
			hits[token] = struct{}{}
		}
	}
	return sortedStringSet(hits)
}

// NeutralWireKeys recursively collects every JSON object key that values of the
// given type can legitimately serialize, following structs, pointers, slices,
// and arrays. The result is the allowlist of neutral wire keys for that type
// and is the future-proof complement to ScanForbiddenTokens: it detects
// provider-native keys the denylist does not yet name.
//
// Map-typed fields contribute no keys, because a map's keys are data rather
// than schema. A caller that intentionally serializes dynamic map keys onto the
// wire must therefore exclude that subtree before calling UnexpectedWireKeys.
func NeutralWireKeys(t reflect.Type) map[string]struct{} {
	allowed := map[string]struct{}{}
	collectNeutralWireKeys(t, allowed, map[reflect.Type]struct{}{})
	return allowed
}

func collectNeutralWireKeys(t reflect.Type, out map[string]struct{}, seen map[reflect.Type]struct{}) {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	if _, ok := seen[t]; ok {
		return
	}
	seen[t] = struct{}{}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" { // unexported field
			continue
		}
		tag := field.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "-" {
			continue
		}
		// Anonymous fields without an explicit name are promoted: their own
		// fields appear inline with no wrapper key.
		if !field.Anonymous || name != "" {
			if name == "" {
				name = field.Name
			}
			out[name] = struct{}{}
		}
		collectNeutralWireKeys(field.Type, out, seen)
	}
}

// UnexpectedWireKeys returns the JSON object keys present in the serialized wire
// that are not in allowed, de-duplicated and sorted. It also descends into
// stringified JSON object/array values in generic argument carriers. Other
// strings stay opaque because typed text, command, code, and patch fields may
// legitimately contain JSON. A non-empty result means a key the typed schema
// does not define crossed the wire — typically a leaked provider-native key.
func UnexpectedWireKeys(wire []byte, allowed map[string]struct{}) ([]string, error) {
	var decoded any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		return nil, err
	}
	unexpected := map[string]struct{}{}
	collectUnexpectedWireKeys(decoded, allowed, unexpected, false)
	return sortedStringSet(unexpected), nil
}

func collectUnexpectedWireKeys(value any, allowed, unexpected map[string]struct{}, inspectString bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if _, ok := allowed[key]; !ok {
				unexpected[key] = struct{}{}
			}
			collectUnexpectedWireKeys(child, allowed, unexpected, key == "value")
		}
	case []any:
		for _, child := range typed {
			collectUnexpectedWireKeys(child, allowed, unexpected, inspectString)
		}
	case string:
		if inspectString {
			if nested, ok := decodeJSONStringContainer(typed); ok {
				collectUnexpectedWireKeys(nested, allowed, unexpected, false)
			}
		}
	}
}

func decodeJSONStringContainer(value string) (any, bool) {
	value = strings.TrimSpace(value)
	if value == "" || (value[0] != '{' && value[0] != '[') {
		return nil, false
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil, false
	}
	switch decoded.(type) {
	case map[string]any, []any:
		return decoded, true
	default:
		return nil, false
	}
}

func sortedStringSet(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
