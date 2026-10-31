package schemacontract

import (
	"io/fs"
	"strings"
	"testing"

	gascity "github.com/gastownhall/gascity"
)

// liveSchemas extracts every embedded built-in schema.
func liveSchemas(t *testing.T) map[string]Entry {
	t.Helper()
	sub, err := fs.Sub(gascity.BuiltinSchemas, "schemas")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	live, err := ExtractAll(sub)
	if err != nil {
		t.Fatalf("ExtractAll(built-in schemas): %v", err)
	}
	return live
}

// TestSchemaContractBaseline is the CI gate: the committed baseline must
// describe every built-in schema, and no schema may have changed shape
// without the version bump and entry the baseline requires.
func TestSchemaContractBaseline(t *testing.T) {
	baseline, err := ParseBaseline(EmbeddedBaseline)
	if err != nil {
		t.Fatalf("parse %s: %v", BaselinePath, err)
	}
	if problems := Check(baseline, liveSchemas(t)); len(problems) != 0 {
		t.Fatalf("schema contract violations (see json-cli-support.md):\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestEveryBuiltinSchemaIsExtractable pins that the extractor accepts every
// shipped schema, so a new keyword fails here rather than silently skipping.
func TestEveryBuiltinSchemaIsExtractable(t *testing.T) {
	if got := len(liveSchemas(t)); got < 133 {
		t.Fatalf("extracted %d schemas, want at least 133", got)
	}
}

// TestBaselineIsCanonical pins that the committed file is what the generator
// would write, so hand edits that reorder or reformat are caught.
func TestBaselineIsCanonical(t *testing.T) {
	baseline, err := ParseBaseline(EmbeddedBaseline)
	if err != nil {
		t.Fatalf("parse %s: %v", BaselinePath, err)
	}
	out, err := Marshal(baseline)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(out) != string(EmbeddedBaseline) {
		t.Fatalf("%s is not in canonical form; run bazel run //cmd/gen-schema-baseline", BaselinePath)
	}
}

// TestFixedResultSchemasDeclareVersion pins that the commands with a fixed
// result shape carry a non-empty schema_version const in the baseline.
func TestFixedResultSchemasDeclareVersion(t *testing.T) {
	live := liveSchemas(t)
	for _, path := range []string{
		"rig/list/result.schema.json",
		"status/result.schema.json",
		"sling/result.schema.json",
		"config/show/result.schema.json",
	} {
		entry, ok := live[path]
		if !ok {
			t.Fatalf("no schema at %s", path)
		}
		if entry.SchemaVersion == "" {
			t.Errorf("%s does not declare a schema_version const", path)
		}
	}
}
