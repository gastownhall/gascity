package schemacontract

import (
	"bytes"
	"strings"
	"testing"
)

const goldenBaseline = `{
  "format": 1,
  "schemas": {
    "x/result.schema.json": [
      {
        "schema_version": "1",
        "jsonl": false,
        "raw_json": false,
        "variants": {
          "": [
            {"path":"$","types":["object"],"required":true},
            {"path":"$.a","types":["string"],"required":true},
            {"path":"$.schema_version","types":["string"],"const":"1","required":false}
          ]
        }
      },
      {
        "schema_version": "2",
        "note": "renamed a to b",
        "issue": "#8453",
        "jsonl": true,
        "raw_json": false,
        "variants": {
          "api": [
            {"path":"$","types":["object"],"required":true}
          ],
          "local": [
            {"path":"$","types":["object"],"required":true}
          ]
        }
      }
    ]
  }
}
`

func goldenValue() Baseline {
	return Baseline{Format: 1, Schemas: map[string][]Entry{
		"x/result.schema.json": {
			{
				SchemaVersion: "1",
				Variants: map[string][]Field{"": {
					{Path: "$.schema_version", Types: []string{"string"}, Const: []byte(`"1"`)},
					{Path: "$.a", Types: []string{"string"}, Required: true},
					{Path: "$", Types: []string{"object"}, Required: true},
				}},
			},
			{
				SchemaVersion: "2", Note: "renamed a to b", Issue: "#8453", JSONL: true,
				Variants: map[string][]Field{
					"local": {{Path: "$", Types: []string{"object"}, Required: true}},
					"api":   {{Path: "$", Types: []string{"object"}, Required: true}},
				},
			},
		},
	}}
}

func TestMarshalIsDeterministicAndByteExact(t *testing.T) {
	got, err := Marshal(goldenValue())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != goldenBaseline {
		t.Fatalf("Marshal bytes differ from the golden.\ngot:\n%s\nwant:\n%s", got, goldenBaseline)
	}
	again, err := Marshal(goldenValue())
	if err != nil || !bytes.Equal(got, again) {
		t.Fatalf("Marshal is not deterministic: %v", err)
	}
}

func TestParseBaselineRoundTrip(t *testing.T) {
	b, err := ParseBaseline([]byte(goldenBaseline))
	if err != nil {
		t.Fatalf("ParseBaseline: %v", err)
	}
	if got := b.Schemas["x/result.schema.json"][1].Issue; got != "#8453" {
		t.Fatalf("Issue = %q, want #8453", got)
	}
	out, err := Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != goldenBaseline {
		t.Fatalf("round trip changed the bytes:\n%s", out)
	}
}

func TestParseBaselineRejectsMalformedInput(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"unknown top-level field": {`{"format":1,"schemas":{},"extra":1}`, "extra"},
		"unknown entry field":     {`{"format":1,"schemas":{"a":[{"schema_version":"1","bogus":1,"variants":{}}]}}`, "bogus"},
		"wrong format":            {`{"format":2,"schemas":{}}`, "format"},
		"missing format":          {`{"schemas":{}}`, "format"},
		"schema with no entries":  {`{"format":1,"schemas":{"a":[]}}`, "a"},
		"trailing data":           {`{"format":1,"schemas":{}} {}`, "trailing"},
		"not json":                {`nope`, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseBaseline([]byte(tc.in))
			if err == nil {
				t.Fatalf("ParseBaseline(%s) succeeded, want error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestMarshalEmptyBaseline(t *testing.T) {
	got, err := Marshal(Baseline{Format: 1})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := "{\n  \"format\": 1,\n  \"schemas\": {}\n}\n"; string(got) != want {
		t.Fatalf("Marshal(empty) = %q, want %q", got, want)
	}
}
