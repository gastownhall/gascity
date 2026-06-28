package beadmeta

import "testing"

// TestJSONPath pins the MySQL JSON-path rendering used by direct-SQL readers.
// The dotted gc.* key must be wrapped in double quotes inside the path, or
// MySQL would interpret the dots as path separators and the lookup would
// silently match nothing.
func TestJSONPath(t *testing.T) {
	cases := map[string]string{
		KindMetadataKey:       `$."gc.kind"`,
		RootBeadIDMetadataKey: `$."gc.root_bead_id"`,
		WorkflowIDMetadataKey: `$."gc.workflow_id"`,
	}
	for key, want := range cases {
		if got := JSONPath(key); got != want {
			t.Errorf("JSONPath(%q) = %q, want %q", key, got, want)
		}
	}
}
