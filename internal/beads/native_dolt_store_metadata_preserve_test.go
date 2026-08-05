package beads

import (
	"context"
	"encoding/json"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// metadataPreserveGetter is a nativeIssueGetter returning a fixed issue.
type metadataPreserveGetter struct {
	issue *beadslib.Issue
}

func (g metadataPreserveGetter) GetIssue(_ context.Context, _ string) (*beadslib.Issue, error) {
	return g.issue, nil
}

// decodeMetadataUpdate pulls the "metadata" entry out of a nativeUpdates result
// and decodes it into typed Go values, so a test can assert on JSON types
// rather than on string spelling.
func decodeMetadataUpdate(t *testing.T, updates map[string]interface{}) map[string]any {
	t.Helper()
	rawAny, ok := updates["metadata"]
	if !ok {
		t.Fatalf("updates has no %q entry: %#v", "metadata", updates)
	}
	raw, ok := rawAny.(json.RawMessage)
	if !ok {
		t.Fatalf("metadata update has type %T, want json.RawMessage", rawAny)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshaling metadata update: %v", err)
	}
	return decoded
}

// TestNativeUpdatesPreservesUntouchedMetadataTypes pins that an update naming
// one metadata key does not rewrite the JSON type of keys it never named.
//
// The metadata column is a single JSON document, so applying one key requires
// a read-modify-write of the whole map. Reading it through map[string]string
// renders every non-string value as JSON text, and writing that back persists
// the rendering: a bead holding {"flag": true, "num": 42} came back as
// {"flag": "true", "num": "42"} after an unrelated key was set.
func TestNativeUpdatesPreservesUntouchedMetadataTypes(t *testing.T) {
	store := &NativeDoltStore{}
	getter := metadataPreserveGetter{issue: &beadslib.Issue{
		ID:       "bd-1",
		Metadata: json.RawMessage(`{"flag": true, "num": 42, "nested": {"a": 1}, "text": "hello"}`),
	}}

	updates, err := store.nativeUpdates(context.Background(), getter, "bd-1", UpdateOpts{
		Metadata: map[string]string{"other": "x"},
	})
	if err != nil {
		t.Fatalf("nativeUpdates: %v", err)
	}
	got := decodeMetadataUpdate(t, updates)

	if flag, ok := got["flag"].(bool); !ok || !flag {
		t.Errorf("flag = %#v (%T), want bool true — an untouched key was retyped", got["flag"], got["flag"])
	}
	if num, ok := got["num"].(float64); !ok || num != 42 {
		t.Errorf("num = %#v (%T), want JSON number 42 — an untouched key was retyped", got["num"], got["num"])
	}
	if nested, ok := got["nested"].(map[string]any); !ok {
		t.Errorf("nested = %#v (%T), want JSON object — an untouched key was retyped", got["nested"], got["nested"])
	} else if a, ok := nested["a"].(float64); !ok || a != 1 {
		t.Errorf("nested.a = %#v, want JSON number 1", nested["a"])
	}
	if text, ok := got["text"].(string); !ok || text != "hello" {
		t.Errorf("text = %#v, want %q unchanged", got["text"], "hello")
	}
	if other, ok := got["other"].(string); !ok || other != "x" {
		t.Errorf("other = %#v, want the named key written as %q", got["other"], "x")
	}
}

// TestNativeUpdatesWritesNamedKeysAsStrings pins the unchanged half of the
// contract: UpdateOpts.Metadata is map[string]string, so a key the caller
// names is written as a JSON string even when it previously held another
// type. Only keys outside the request keep their existing representation.
func TestNativeUpdatesWritesNamedKeysAsStrings(t *testing.T) {
	store := &NativeDoltStore{}
	getter := metadataPreserveGetter{issue: &beadslib.Issue{
		ID:       "bd-1",
		Metadata: json.RawMessage(`{"num": 42}`),
	}}

	updates, err := store.nativeUpdates(context.Background(), getter, "bd-1", UpdateOpts{
		Metadata: map[string]string{"num": "43"},
	})
	if err != nil {
		t.Fatalf("nativeUpdates: %v", err)
	}
	got := decodeMetadataUpdate(t, updates)

	if num, ok := got["num"].(string); !ok || num != "43" {
		t.Errorf("num = %#v (%T), want string %q — a named key takes the caller's value verbatim", got["num"], got["num"], "43")
	}
}

// TestMetadataRawWithOverrides covers the shared merge helper directly. Every
// metadata write path in this store routes through it, so the edge cases are
// pinned once here rather than per call site.
func TestMetadataRawWithOverrides(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		overrides map[string]string
		want      string
		wantNil   bool
	}{{
		name:      "preserves every untouched JSON type",
		raw:       `{"b":true,"n":42,"f":1.5,"o":{"a":1},"arr":[1,"two"],"null":null,"s":"str"}`,
		overrides: map[string]string{"new": "v"},
		want:      `{"arr":[1,"two"],"b":true,"f":1.5,"n":42,"new":"v","null":null,"o":{"a":1},"s":"str"}`,
	}, {
		name:      "a named key is written as a JSON string",
		raw:       `{"n":42}`,
		overrides: map[string]string{"n": "43"},
		want:      `{"n":"43"}`,
	}, {
		name:      "absent metadata takes the overrides",
		raw:       ``,
		overrides: map[string]string{"a": "1"},
		want:      `{"a":"1"}`,
	}, {
		name:      "JSON null metadata takes the overrides",
		raw:       `null`,
		overrides: map[string]string{"a": "1"},
		want:      `{"a":"1"}`,
	}, {
		name:      "empty object plus no overrides stays empty",
		raw:       `{}`,
		overrides: nil,
		wantNil:   true,
	}, {
		name:      "no overrides leaves an existing document byte-identical",
		raw:       `{"n":42}`,
		overrides: nil,
		want:      `{"n":42}`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := metadataRawWithOverrides(json.RawMessage(tc.raw), tc.overrides)
			if err != nil {
				t.Fatalf("metadataRawWithOverrides: %v", err)
			}
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got %s, want nil", got)
				}
				return
			}
			if string(got) != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

// TestMetadataRawWithOverridesRejectsMalformedJSON pins that a corrupt
// metadata document surfaces as an error rather than being silently replaced
// by a document containing only the overrides.
func TestMetadataRawWithOverridesRejectsMalformedJSON(t *testing.T) {
	if _, err := metadataRawWithOverrides(json.RawMessage(`{"a":`), map[string]string{"b": "2"}); err == nil {
		t.Fatal("metadataRawWithOverrides on malformed JSON: got nil error, want failure")
	}
}

// TestNativeUpdatesOnBeadWithoutMetadata pins that a bead with no metadata
// column still accepts a first key.
func TestNativeUpdatesOnBeadWithoutMetadata(t *testing.T) {
	store := &NativeDoltStore{}
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "absent", raw: nil},
		{name: "empty object", raw: json.RawMessage(`{}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getter := metadataPreserveGetter{issue: &beadslib.Issue{ID: "bd-1", Metadata: tc.raw}}
			updates, err := store.nativeUpdates(context.Background(), getter, "bd-1", UpdateOpts{
				Metadata: map[string]string{"first": "1"},
			})
			if err != nil {
				t.Fatalf("nativeUpdates: %v", err)
			}
			got := decodeMetadataUpdate(t, updates)
			if first, ok := got["first"].(string); !ok || first != "1" {
				t.Errorf("first = %#v, want %q", got["first"], "1")
			}
			if len(got) != 1 {
				t.Errorf("metadata = %#v, want exactly the one named key", got)
			}
		})
	}
}
