package beads

import (
	"encoding/json"
	"maps"
	"testing"
)

// TestMetadataReadingsAreTheBdCLIAndNativeReadings: MetadataReadings is the
// bd CLI's StringMap decode and the native store's metadataMapFromNative,
// exactly, and a JSON null is where they part.
func TestMetadataReadingsAreTheBdCLIAndNativeReadings(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"gc.root_bead_id":"gc-1","note":"x"}`,
		`{"gc.root_bead_id":null,"session_id":null}`,
		`{"n":1.50,"e":1e3,"b":true,"o":{"b":1, "a":2},"a":[1, 2]}`,
	} {
		readings, err := MetadataReadings([]byte(raw))
		if err != nil || len(readings) != 2 {
			t.Fatalf("MetadataReadings(%s) = %v, %v; want two readings", raw, readings, err)
		}
		var cli StringMap
		if err := json.Unmarshal([]byte(raw), &cli); err != nil {
			t.Fatal(err)
		}
		native, err := metadataMapFromNative(json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(readings[0], cli) {
			t.Errorf("%s: first reading %q, want the bd CLI's StringMap reading %q", raw, readings[0], cli)
		}
		if !maps.Equal(readings[1], native) {
			t.Errorf("%s: second reading %q, want the native store's reading %q", raw, readings[1], native)
		}
	}
	readings, err := MetadataReadings([]byte(`{"gc.root_bead_id":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := readings[0]["gc.root_bead_id"]; got != "" {
		t.Errorf("the bd CLI reads a stored null as %q, want empty", got)
	}
	if got := readings[1]["gc.root_bead_id"]; got != "null" {
		t.Errorf("the native store reads a stored null as %q, want the text null", got)
	}
	for _, raw := range []string{`[1]`, `"x"`, `1`, `{`} {
		if _, err := MetadataReadings([]byte(raw)); err == nil {
			t.Errorf("MetadataReadings(%s) read a body that is not a JSON object", raw)
		}
	}
}
