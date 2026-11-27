package schemacontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// interpretedKeywords are the schema keywords that shape the contract.
var interpretedKeywords = map[string]bool{
	"type": true, "const": true, "properties": true, "required": true,
	"items": true, "$ref": true, "$defs": true, "oneOf": true, "anyOf": true,
	"allOf": true,
}

// ignoredKeywords are accepted and deliberately carry no contract: they
// describe documentation or value-level constraints, not shape.
var ignoredKeywords = map[string]bool{
	"additionalProperties": true, "description": true, "title": true,
	"$schema": true, "$id": true, "$comment": true, "format": true,
	"pattern": true, "enum": true, "minLength": true, "maxLength": true,
	"minimum": true, "maximum": true, "minItems": true, "maxItems": true,
	"if": true, "then": true, "else": true,
}

const (
	keywordJSONL   = "x-gc-jsonl"
	keywordRawJSON = "x-gc-raw-json"
	defsPrefix     = "#/$defs/"
	schemaFileExt  = ".schema.json"
	versionField   = "schema_version"
)

// node is the resolved shape of one schema position.
type node struct {
	types    map[string]bool
	constVal json.RawMessage
	props    map[string]*node
	required map[string]bool
	items    *node
}

func newNode() *node {
	return &node{types: map[string]bool{}, props: map[string]*node{}, required: map[string]bool{}}
}

// merge returns the conjunction of a and b without mutating either.
func merge(a, b *node) *node {
	out := newNode()
	for _, n := range []*node{a, b} {
		for t := range n.types {
			out.types[t] = true
		}
		if len(n.constVal) != 0 {
			out.constVal = n.constVal
		}
		for name := range n.required {
			out.required[name] = true
		}
		for name, child := range n.props {
			if prev, ok := out.props[name]; ok {
				out.props[name] = merge(prev, child)
			} else {
				out.props[name] = child
			}
		}
		if n.items != nil {
			if out.items != nil {
				out.items = merge(out.items, n.items)
			} else {
				out.items = n.items
			}
		}
	}
	return out
}

// extractor resolves one schema document.
type extractor struct {
	defs map[string]json.RawMessage
}

// Extract derives the contract entry of one schema document.
func Extract(raw []byte) (Entry, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return Entry{}, fmt.Errorf("parsing schema: %w", err)
	}
	ex := &extractor{defs: map[string]json.RawMessage{}}
	if defs, ok := top["$defs"]; ok {
		if err := json.Unmarshal(defs, &ex.defs); err != nil {
			return Entry{}, fmt.Errorf("parsing $defs: %w", err)
		}
	}
	for _, name := range sortedKeys(ex.defs) {
		if _, err := ex.build(ex.defs[name], false, []string{name}); err != nil {
			return Entry{}, fmt.Errorf("$defs/%s: %w", name, err)
		}
	}
	entry := Entry{Variants: map[string][]Field{}}
	if _, ok := top[keywordJSONL]; ok {
		entry.JSONL = true
	}
	if rawJSON, ok := top[keywordRawJSON]; ok {
		if err := json.Unmarshal(rawJSON, &entry.RawJSON); err != nil {
			return Entry{}, fmt.Errorf("%s must be a boolean: %w", keywordRawJSON, err)
		}
	}
	base, branches, err := ex.buildRoot(top)
	if err != nil {
		return Entry{}, err
	}
	if len(branches) == 0 {
		entry.Variants[""] = flatten("$", base, true)
	}
	for key, branch := range branches {
		entry.Variants[key] = flatten("$", merge(base, branch), true)
	}
	entry.SchemaVersion = versionOf(base)
	return entry, nil
}

// versionOf reads the declared schema_version const, or "" when unversioned.
func versionOf(root *node) string {
	child, ok := root.props[versionField]
	if !ok || len(child.constVal) == 0 {
		return ""
	}
	var version string
	if err := json.Unmarshal(child.constVal, &version); err != nil {
		return ""
	}
	return version
}

// buildRoot resolves the root object, splitting a root oneOf/anyOf into
// keyed variant branches.
func (ex *extractor) buildRoot(top map[string]json.RawMessage) (*node, map[string]*node, error) {
	var unions []string
	for _, kw := range []string{"oneOf", "anyOf"} {
		if _, ok := top[kw]; ok {
			unions = append(unions, kw)
		}
	}
	if len(unions) > 1 {
		return nil, nil, fmt.Errorf("root declares both oneOf and anyOf")
	}
	rest := map[string]json.RawMessage{}
	for k, v := range top {
		if len(unions) == 1 && k == unions[0] {
			continue
		}
		rest[k] = v
	}
	restRaw, err := json.Marshal(rest)
	if err != nil {
		return nil, nil, err
	}
	base, err := ex.build(restRaw, true, nil)
	if err != nil {
		return nil, nil, err
	}
	if len(unions) == 0 {
		return base, nil, nil
	}
	var members []json.RawMessage
	if err := json.Unmarshal(top[unions[0]], &members); err != nil {
		return nil, nil, fmt.Errorf("parsing root %s: %w", unions[0], err)
	}
	branches := map[string]*node{}
	for i, member := range members {
		key := fmt.Sprint(i)
		var probe struct {
			Ref string `json:"$ref"`
		}
		if json.Unmarshal(member, &probe) == nil && strings.HasPrefix(probe.Ref, defsPrefix) {
			key = strings.TrimPrefix(probe.Ref, defsPrefix)
		}
		branch, err := ex.build(member, false, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("root %s[%d]: %w", unions[0], i, err)
		}
		if branch == nil {
			continue
		}
		branches[key] = branch
	}
	return base, branches, nil
}

// build resolves one schema position. It returns nil for the false schema.
func (ex *extractor) build(raw json.RawMessage, root bool, refStack []string) (*node, error) {
	trimmed := bytes.TrimSpace(raw)
	switch string(trimmed) {
	case "true":
		return newNode(), nil
	case "false":
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("schema must be an object or boolean: %w", err)
	}
	for _, kw := range sortedKeys(obj) {
		switch {
		case interpretedKeywords[kw], ignoredKeywords[kw]:
		case kw == keywordJSONL || kw == keywordRawJSON:
			if !root {
				return nil, fmt.Errorf("framing annotation %s is only allowed at the schema root", kw)
			}
		default:
			return nil, fmt.Errorf("unsupported keyword %q: teach the extractor before using it", kw)
		}
	}
	n := newNode()
	if err := ex.applyTypes(n, obj); err != nil {
		return nil, err
	}
	if ref, ok := obj["$ref"]; ok {
		target, err := ex.resolveRef(ref, refStack)
		if err != nil {
			return nil, err
		}
		if target != nil {
			n = merge(n, target)
		}
	}
	n, err := ex.applyAllOf(n, obj, refStack)
	if err != nil {
		return nil, err
	}
	if err := ex.applyObject(n, obj, refStack); err != nil {
		return nil, err
	}
	for _, kw := range []string{"oneOf", "anyOf"} {
		if members, ok := obj[kw]; ok {
			if err := ex.applyScalarUnion(n, kw, members, refStack); err != nil {
				return nil, err
			}
		}
	}
	return n, nil
}

func (ex *extractor) applyTypes(n *node, obj map[string]json.RawMessage) error {
	if raw, ok := obj["type"]; ok {
		var one string
		var many []string
		switch {
		case json.Unmarshal(raw, &one) == nil:
			many = []string{one}
		case json.Unmarshal(raw, &many) == nil:
		default:
			return fmt.Errorf("type must be a string or an array of strings")
		}
		for _, t := range many {
			n.types[t] = true
		}
	}
	if raw, ok := obj["const"]; ok {
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return fmt.Errorf("const: %w", err)
		}
		n.constVal = json.RawMessage(compact.Bytes())
		if _, typed := obj["type"]; !typed {
			n.types[jsonType(compact.Bytes())] = true
		}
	}
	return nil
}

// jsonType names the JSON type of a compact literal.
func jsonType(literal []byte) string {
	switch literal[0] {
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	case '{':
		return "object"
	case '[':
		return "array"
	}
	if bytes.ContainsAny(literal, ".eE") {
		return "number"
	}
	return "integer"
}

func (ex *extractor) resolveRef(raw json.RawMessage, refStack []string) (*node, error) {
	var ref string
	if err := json.Unmarshal(raw, &ref); err != nil {
		return nil, fmt.Errorf("$ref must be a string: %w", err)
	}
	if !strings.HasPrefix(ref, defsPrefix) {
		return nil, fmt.Errorf("$ref %q is not a local #/$defs/ reference", ref)
	}
	name := strings.TrimPrefix(ref, defsPrefix)
	target, ok := ex.defs[name]
	if !ok {
		return nil, fmt.Errorf("$ref %q: no such def %q", ref, name)
	}
	for _, seen := range refStack {
		if seen == name {
			return nil, fmt.Errorf("$ref cycle through %q", name)
		}
	}
	return ex.build(target, false, append(append([]string(nil), refStack...), name))
}

func (ex *extractor) applyAllOf(n *node, obj map[string]json.RawMessage, refStack []string) (*node, error) {
	raw, ok := obj["allOf"]
	if !ok {
		return n, nil
	}
	var members []json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, fmt.Errorf("parsing allOf: %w", err)
	}
	for i, member := range members {
		m, err := ex.build(member, false, refStack)
		if err != nil {
			return nil, fmt.Errorf("allOf[%d]: %w", i, err)
		}
		if m != nil {
			n = merge(n, m)
		}
	}
	return n, nil
}

func (ex *extractor) applyObject(n *node, obj map[string]json.RawMessage, refStack []string) error {
	if raw, ok := obj["properties"]; ok {
		var props map[string]json.RawMessage
		if err := json.Unmarshal(raw, &props); err != nil {
			return fmt.Errorf("parsing properties: %w", err)
		}
		for _, name := range sortedKeys(props) {
			if strings.ContainsAny(name, ".[]\\") {
				return fmt.Errorf("property name %q contains a reserved path character", name)
			}
			child, err := ex.build(props[name], false, refStack)
			if err != nil {
				return fmt.Errorf("property %q: %w", name, err)
			}
			if child == nil {
				continue
			}
			if prev, ok := n.props[name]; ok {
				child = merge(prev, child)
			}
			n.props[name] = child
		}
	}
	if raw, ok := obj["required"]; ok {
		var names []string
		if err := json.Unmarshal(raw, &names); err != nil {
			return fmt.Errorf("parsing required: %w", err)
		}
		for _, name := range names {
			n.required[name] = true
		}
	}
	if raw, ok := obj["items"]; ok {
		items, err := ex.build(raw, false, refStack)
		if err != nil {
			return fmt.Errorf("items: %w", err)
		}
		if items != nil {
			if n.items != nil {
				items = merge(n.items, items)
			}
			n.items = items
		}
	}
	return nil
}

// applyScalarUnion folds a nested oneOf/anyOf of scalar branches into one
// type union; a branch that is an object or array is an error.
func (ex *extractor) applyScalarUnion(n *node, kw string, raw json.RawMessage, refStack []string) error {
	var members []json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return fmt.Errorf("parsing %s: %w", kw, err)
	}
	union := map[string]bool{}
	for i, member := range members {
		m, err := ex.build(member, false, refStack)
		if err != nil {
			return fmt.Errorf("%s[%d]: %w", kw, i, err)
		}
		if m == nil {
			continue
		}
		if len(m.props) != 0 || len(m.required) != 0 || m.items != nil || m.types["object"] || m.types["array"] {
			return fmt.Errorf("%s[%d]: a nested union of objects or arrays is not supported; use a root-level union", kw, i)
		}
		if len(m.types) == 0 {
			union["any"] = true
		}
		for t := range m.types {
			union[t] = true
		}
	}
	if len(n.types) == 0 {
		for t := range union {
			n.types[t] = true
		}
	}
	return nil
}

// flatten emits one record per field path, depth first, sorted by path.
func flatten(path string, n *node, required bool) []Field {
	types := make([]string, 0, len(n.types))
	for t := range n.types {
		types = append(types, t)
	}
	sort.Strings(types)
	if len(types) == 0 {
		types = []string{"any"}
	}
	fields := []Field{{Path: path, Types: types, Const: n.constVal, Required: required}}
	names := map[string]bool{}
	for name := range n.props {
		names[name] = true
	}
	for name := range n.required {
		names[name] = true
	}
	for _, name := range sortedKeys(names) {
		child, ok := n.props[name]
		if !ok {
			child = newNode()
		}
		fields = append(fields, flatten(path+"."+name, child, n.required[name])...)
	}
	if n.items != nil {
		fields = append(fields, flatten(path+"[]", n.items, false)...)
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Path < fields[j].Path })
	return fields
}

// ExtractAll extracts every *.schema.json under fsys, keyed by its
// slash-separated path relative to fsys.
func ExtractAll(fsys fs.FS) (map[string]Entry, error) {
	out := map[string]Entry{}
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, schemaFileExt) {
			return nil
		}
		raw, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		entry, err := Extract(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out[path] = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// sortedKeys returns the keys of m in ascending order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
