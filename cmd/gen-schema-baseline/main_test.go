package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/schemacontract"
)

// newRoot makes a fake repository root holding the given schemas under
// schemas/ and an optional existing baseline.
func newRoot(t *testing.T, schemas map[string]string, baseline string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range schemas {
		path := filepath.Join(root, "schemas", rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if baseline != "" {
		path := filepath.Join(root, schemacontract.BaselinePath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(baseline), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func baselineFile(root string) string {
	return filepath.Join(root, schemacontract.BaselinePath)
}

const versioned = `{"type":"object","properties":{"schema_version":{"const":"1"},"a":{"type":"string"}}}`

func noEnv(string) string { return "" }

func TestRunRequiresARoot(t *testing.T) {
	var out bytes.Buffer
	err := run(nil, noEnv, &out)
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("run without a root = %v, want an error naming the root", err)
	}
}

func TestRunSeedsBaselineFromRootFlag(t *testing.T) {
	root := newRoot(t, map[string]string{"a/result.schema.json": versioned}, "")
	var out bytes.Buffer
	if err := run([]string{"-root", root}, noEnv, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	data, err := os.ReadFile(baselineFile(root))
	if err != nil {
		t.Fatalf("baseline not written: %v", err)
	}
	baseline, err := schemacontract.ParseBaseline(data)
	if err != nil {
		t.Fatalf("written baseline does not parse: %v", err)
	}
	if got := len(baseline.Schemas["a/result.schema.json"]); got != 1 {
		t.Fatalf("entries for a/result.schema.json = %d, want 1", got)
	}
	if !strings.Contains(out.String(), "seeded") {
		t.Fatalf("summary %q does not mention seeded schemas", out.String())
	}
}

func TestRunUsesBuildWorkspaceDirectory(t *testing.T) {
	root := newRoot(t, map[string]string{"a/result.schema.json": versioned}, "")
	env := func(key string) string {
		if key == "BUILD_WORKSPACE_DIRECTORY" {
			return root
		}
		return ""
	}
	if err := run(nil, env, &bytes.Buffer{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(baselineFile(root)); err != nil {
		t.Fatalf("baseline not written under BUILD_WORKSPACE_DIRECTORY: %v", err)
	}
}

func TestRunSecondRunIsByteIdentical(t *testing.T) {
	root := newRoot(t, map[string]string{"a/result.schema.json": versioned}, "")
	if err := run([]string{"-root", root}, noEnv, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(baselineFile(root))
	if err := run([]string{"-root", root}, noEnv, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(baselineFile(root))
	if !bytes.Equal(first, second) {
		t.Fatalf("second run changed the baseline")
	}
}

func TestRunExtractorErrorWritesNothing(t *testing.T) {
	root := newRoot(t, map[string]string{
		"a/result.schema.json":   versioned,
		"bad/result.schema.json": `{"patternProperties":{}}`,
	}, "")
	err := run([]string{"-root", root}, noEnv, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "bad/result.schema.json") {
		t.Fatalf("run error = %v, want one naming the bad schema", err)
	}
	if _, statErr := os.Stat(baselineFile(root)); !os.IsNotExist(statErr) {
		t.Fatalf("baseline was written despite the extractor error: %v", statErr)
	}
}

func TestRunBackwardsVersionLeavesBaselineUnchanged(t *testing.T) {
	existing := `{
  "format": 1,
  "schemas": {
    "a/result.schema.json": [
      {"schema_version": "1", "jsonl": false, "raw_json": false, "variants": {"": [{"path":"$","types":["object"],"required":false}]}},
      {"schema_version": "2", "note": "n", "issue": "#1", "jsonl": false, "raw_json": false, "variants": {"": [{"path":"$","types":["object"],"required":false}]}}
    ]
  }
}
`
	root := newRoot(t, map[string]string{"a/result.schema.json": versioned}, existing)
	err := run([]string{"-root", root}, noEnv, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "went backwards") {
		t.Fatalf("run error = %v, want went-backwards", err)
	}
	after, _ := os.ReadFile(baselineFile(root))
	if string(after) != existing {
		t.Fatalf("baseline changed on a refused run:\n%s", after)
	}
}
