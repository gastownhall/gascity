package schemacontract

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

// mustExtract extracts one schema document and fails the test on error.
func mustExtract(t *testing.T, doc string) Entry {
	t.Helper()
	entry, err := Extract([]byte(doc))
	if err != nil {
		t.Fatalf("Extract(%s): %v", doc, err)
	}
	return entry
}

// field returns the record at path in the named variant, failing if absent.
func field(t *testing.T, entry Entry, variant, path string) Field {
	t.Helper()
	for _, f := range entry.Variants[variant] {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("variant %q has no record for %s; records: %v", variant, path, entry.Variants[variant])
	return Field{}
}

func hasField(entry Entry, variant, path string) bool {
	for _, f := range entry.Variants[variant] {
		if f.Path == path {
			return true
		}
	}
	return false
}

func TestExtractVersionAndFlatObject(t *testing.T) {
	entry := mustExtract(t, `{
		"type":"object","required":["schema_version","a"],
		"properties":{
			"schema_version":{"const":"1"},
			"a":{"type":"string"},
			"b":{"type":"integer"}
		}}`)
	if entry.SchemaVersion != "1" {
		t.Fatalf("SchemaVersion = %q, want 1", entry.SchemaVersion)
	}
	root := field(t, entry, "", "$")
	if got := strings.Join(root.Types, ","); got != "object" || !root.Required {
		t.Fatalf("root = %+v, want required object", root)
	}
	if f := field(t, entry, "", "$.a"); !f.Required || strings.Join(f.Types, ",") != "string" {
		t.Fatalf("$.a = %+v, want required string", f)
	}
	if f := field(t, entry, "", "$.b"); f.Required || strings.Join(f.Types, ",") != "integer" {
		t.Fatalf("$.b = %+v, want optional integer", f)
	}
	v := field(t, entry, "", "$.schema_version")
	if string(v.Const) != `"1"` || strings.Join(v.Types, ",") != "string" {
		t.Fatalf("$.schema_version = %+v, want const \"1\" typed from the const", v)
	}
}

func TestExtractUnversionedSchemaHasEmptyVersion(t *testing.T) {
	for name, doc := range map[string]string{
		"absent":      `{"type":"object","properties":{"a":{"type":"string"}}}`,
		"bare string": `{"type":"object","properties":{"schema_version":{"type":"string"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := mustExtract(t, doc).SchemaVersion; got != "" {
				t.Fatalf("SchemaVersion = %q, want empty (unversioned)", got)
			}
		})
	}
}

func TestExtractFullDepthPaths(t *testing.T) {
	entry := mustExtract(t, `{"type":"object","properties":{
		"summary":{"type":"object","required":["closed"],"properties":{"closed":{"type":"integer"}}},
		"sessions":{"type":"array","items":{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}}
	}}`)
	if f := field(t, entry, "", "$.summary.closed"); !f.Required {
		t.Fatalf("$.summary.closed = %+v, want required", f)
	}
	if f := field(t, entry, "", "$.sessions"); strings.Join(f.Types, ",") != "array" {
		t.Fatalf("$.sessions = %+v, want array", f)
	}
	if f := field(t, entry, "", "$.sessions[].id"); !f.Required {
		t.Fatalf("$.sessions[].id = %+v, want required", f)
	}
}

func TestExtractInlinesLocalRefsWithoutRecordingDefNames(t *testing.T) {
	entry := mustExtract(t, `{"type":"object","properties":{
		"row":{"$ref":"#/$defs/rowDef"},
		"rows":{"type":"array","items":{"$ref":"#/$defs/rowDef"}}
	},"$defs":{"rowDef":{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}}}`)
	if f := field(t, entry, "", "$.row"); strings.Join(f.Types, ",") != "object" {
		t.Fatalf("$.row = %+v, want object resolved through $ref", f)
	}
	field(t, entry, "", "$.row.id")
	field(t, entry, "", "$.rows[].id")
	for _, f := range entry.Variants[""] {
		if strings.Contains(f.Path, "rowDef") {
			t.Fatalf("helper def name leaked into record %q", f.Path)
		}
	}
}

func TestExtractAllOfMergesPropertiesAndRequired(t *testing.T) {
	entry := mustExtract(t, `{"allOf":[
		{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}},
		{"properties":{"b":{"type":"boolean"}},"required":["b"]}
	]}`)
	if !field(t, entry, "", "$.a").Required || !field(t, entry, "", "$.b").Required {
		t.Fatalf("allOf members were not merged: %v", entry.Variants[""])
	}
}

func TestExtractAllOfRequiredWithoutPropertyIsAny(t *testing.T) {
	entry := mustExtract(t, `{"type":"object","allOf":[{"required":["ghost"]}]}`)
	if f := field(t, entry, "", "$.ghost"); strings.Join(f.Types, ",") != "any" || !f.Required {
		t.Fatalf("$.ghost = %+v, want required any", f)
	}
}

func TestExtractRootOneOfBecomesKeyedVariants(t *testing.T) {
	entry := mustExtract(t, `{
		"type":"object","required":["ok"],
		"properties":{"ok":{"const":true}},
		"oneOf":[{"$ref":"#/$defs/local"},{"$ref":"#/$defs/api"}],
		"$defs":{
			"local":{"type":"object","required":["l"],"properties":{"l":{"type":"string"}}},
			"api":{"type":"object","required":["a"],"properties":{"a":{"type":"integer"}}}}}`)
	if len(entry.Variants) != 2 {
		t.Fatalf("variants = %v, want local and api", entry.Variants)
	}
	field(t, entry, "local", "$.l")
	field(t, entry, "api", "$.a")
	if !hasField(entry, "local", "$.ok") || !hasField(entry, "api", "$.ok") {
		t.Fatalf("root properties must apply to every variant: %v", entry.Variants)
	}
	if hasField(entry, "local", "$.a") {
		t.Fatalf("variant local leaked api's field")
	}
}

func TestExtractInlineOneOfBranchesAreKeyedByIndex(t *testing.T) {
	entry := mustExtract(t, `{"oneOf":[
		{"type":"object","properties":{"x":{"type":"string"}}},
		{"type":"object","properties":{"y":{"type":"string"}}}]}`)
	field(t, entry, "0", "$.x")
	field(t, entry, "1", "$.y")
}

func TestExtractScalarUnionBecomesTypeUnion(t *testing.T) {
	entry := mustExtract(t, `{"type":"object","properties":{
		"n":{"oneOf":[{"type":"integer"},{"type":"null"}]},
		"s":{"anyOf":[{"const":"a"},{"const":"b"}]},
		"arr":{"type":["integer","null"]}
	}}`)
	if got := strings.Join(field(t, entry, "", "$.n").Types, ","); got != "integer,null" {
		t.Fatalf("$.n types = %q, want integer,null", got)
	}
	if got := strings.Join(field(t, entry, "", "$.s").Types, ","); got != "string" {
		t.Fatalf("$.s types = %q, want string (const-only branches take the const type)", got)
	}
	if got := strings.Join(field(t, entry, "", "$.arr").Types, ","); got != "integer,null" {
		t.Fatalf("$.arr types = %q, want integer,null", got)
	}
}

func TestExtractBooleanSchemaProperties(t *testing.T) {
	entry := mustExtract(t, `{"type":"object","properties":{"open":true,"never":false}}`)
	if got := strings.Join(field(t, entry, "", "$.open").Types, ","); got != "any" {
		t.Fatalf("$.open types = %q, want any", got)
	}
	if hasField(entry, "", "$.never") {
		t.Fatalf("a false schema must contribute no record")
	}
}

func TestExtractUntypedRootYieldsAny(t *testing.T) {
	entry := mustExtract(t, `{}`)
	if f := field(t, entry, "", "$"); strings.Join(f.Types, ",") != "any" {
		t.Fatalf("root = %+v, want any", f)
	}
	if len(entry.Variants[""]) != 1 {
		t.Fatalf("untyped root must yield no fields, got %v", entry.Variants[""])
	}
}

func TestExtractArrayRoot(t *testing.T) {
	entry := mustExtract(t, `{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"}}}}`)
	field(t, entry, "", "$[].id")
}

func TestExtractIgnoresDocumentedKeywords(t *testing.T) {
	doc := `{"$schema":"x","$id":"y","title":"t","description":"d","$comment":"c",
		"type":"object","additionalProperties":false,
		"properties":{"a":{"type":"string","format":"date-time","pattern":"^a","enum":["a"],
			"minLength":1,"maxLength":2,"minimum":0,"maximum":3,"minItems":1,"maxItems":2}},
		"if":{"properties":{"a":{"const":"a"}}},"then":{"required":["a"]},"else":{}}`
	entry := mustExtract(t, doc)
	if f := field(t, entry, "", "$.a"); f.Required || len(f.Const) != 0 {
		t.Fatalf("if/then/enum must not change the record: %+v", f)
	}
}

func TestExtractFramingFlags(t *testing.T) {
	cases := map[string]struct{ jsonl, raw bool }{
		`{"type":"object"}`:                                      {false, false},
		`{"type":"object","x-gc-jsonl":{}}`:                      {true, false},
		`{"type":"object","x-gc-jsonl":true}`:                    {true, false},
		`{"type":"object","x-gc-raw-json":true}`:                 {false, true},
		`{"type":"object","x-gc-raw-json":false}`:                {false, false},
		`{"type":"object","x-gc-jsonl":{},"x-gc-raw-json":true}`: {true, true},
	}
	for doc, want := range cases {
		entry := mustExtract(t, doc)
		if entry.JSONL != want.jsonl || entry.RawJSON != want.raw {
			t.Errorf("%s: jsonl=%v raw=%v, want jsonl=%v raw=%v", doc, entry.JSONL, entry.RawJSON, want.jsonl, want.raw)
		}
	}
}

func TestExtractFailsLoudly(t *testing.T) {
	cases := map[string]struct{ doc, want string }{
		"unknown keyword":     {`{"type":"object","patternProperties":{}}`, "patternProperties"},
		"unknown x- keyword":  {`{"type":"object","x-other":1}`, "x-other"},
		"remote ref":          {`{"type":"object","properties":{"a":{"$ref":"https://example.com/a.json"}}}`, "$ref"},
		"missing def":         {`{"type":"object","properties":{"a":{"$ref":"#/$defs/nope"}}}`, "nope"},
		"ref cycle":           {`{"type":"object","properties":{"a":{"$ref":"#/$defs/n"}},"$defs":{"n":{"type":"object","properties":{"c":{"$ref":"#/$defs/n"}}}}}`, "cycle"},
		"nested object union": {`{"type":"object","properties":{"a":{"oneOf":[{"type":"object"},{"type":"object"}]}}}`, "union"},
		"nested array union":  {`{"type":"object","properties":{"a":{"anyOf":[{"type":"array"},{"type":"null"}]}}}`, "union"},
		"reserved dot":        {`{"type":"object","properties":{"a.b":{"type":"string"}}}`, "a.b"},
		"reserved bracket":    {`{"type":"object","properties":{"a[]":{"type":"string"}}}`, "a[]"},
		"framing below root":  {`{"type":"object","properties":{"a":{"type":"object","x-gc-jsonl":{}}}}`, "x-gc-jsonl"},
		"not json":            {`{`, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Extract([]byte(tc.doc))
			if err == nil {
				t.Fatalf("Extract(%s) succeeded, want an error naming %q", tc.doc, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestExtractAllWalksSchemaFilesOnly(t *testing.T) {
	fsys := fstest.MapFS{
		"a/result.schema.json":   {Data: []byte(`{"type":"object","properties":{"schema_version":{"const":"1"}}}`)},
		"b/c/result.schema.json": {Data: []byte(`{"type":"object"}`)},
		"failure.schema.json":    {Data: []byte(`{"type":"object"}`)},
		"a/validate.sh":          {Data: []byte("#!/bin/sh\n")},
		"a/notes.json":           {Data: []byte(`{"not":"a schema"}`)},
	}
	got, err := ExtractAll(fsys)
	if err != nil {
		t.Fatalf("ExtractAll: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ExtractAll returned %d schemas, want 3: %v", len(got), got)
	}
	if got["a/result.schema.json"].SchemaVersion != "1" {
		t.Fatalf("a/result.schema.json version = %q, want 1", got["a/result.schema.json"].SchemaVersion)
	}
}

func TestExtractAllNamesTheBadSchema(t *testing.T) {
	fsys := fstest.MapFS{"bad/result.schema.json": {Data: []byte(`{"patternProperties":{}}`)}}
	_, err := ExtractAll(fsys)
	if err == nil || !strings.Contains(err.Error(), "bad/result.schema.json") {
		t.Fatalf("ExtractAll error = %v, want one naming bad/result.schema.json", err)
	}
}

func TestFieldJSONShape(t *testing.T) {
	data, err := json.Marshal(Field{Path: "$.a", Types: []string{"string"}, Const: json.RawMessage(`"x"`), Required: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"path":"$.a","types":["string"],"const":"x","required":true}`; string(data) != want {
		t.Fatalf("Field JSON = %s, want %s", data, want)
	}
}
