package session

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestParseTemplateOverrides pins the shared template_overrides parse contract
// every read site relies on (manager resume replay, API runtime projections,
// cmd/gc launch and config-drift paths): absent, blank, JSON-null, and empty
// objects normalize to nil with no error; malformed payloads surface an error
// naming the metadata key; valid objects round-trip unchanged.
func TestParseTemplateOverrides(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]string
		want     map[string]string
		wantErr  bool
	}{
		{name: "nil metadata", metadata: nil},
		{name: "missing key", metadata: map[string]string{"state": "asleep"}},
		{name: "empty value", metadata: map[string]string{"template_overrides": ""}},
		{name: "whitespace only", metadata: map[string]string{"template_overrides": " \n\t "}},
		{name: "json null", metadata: map[string]string{"template_overrides": "null"}},
		{name: "empty object", metadata: map[string]string{"template_overrides": "{}"}},
		{name: "invalid json", metadata: map[string]string{"template_overrides": "{not json"}, wantErr: true},
		{name: "non-string value", metadata: map[string]string{"template_overrides": `{"model":1}`}, wantErr: true},
		{
			name:     "valid object",
			metadata: map[string]string{"template_overrides": `{"model":"sonnet","initial_message":"hi"}`},
			want:     map[string]string{"model": "sonnet", "initial_message": "hi"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTemplateOverrides(tt.metadata)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseTemplateOverrides() = %v, want error", got)
				}
				if !strings.Contains(err.Error(), "template_overrides") {
					t.Fatalf("error %q does not name the metadata key", err)
				}
				if got != nil {
					t.Fatalf("ParseTemplateOverrides() = %v on error, want nil", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTemplateOverrides(): %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseTemplateOverrides() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestParseTemplateOverridesFromInfoMatchesRaw proves the Info front-door decode
// (ParseTemplateOverridesFromInfo, used by the reconciler config-drift hash path
// and the launch path) is byte-identical to the raw ParseTemplateOverrides for
// the same bead across the full absent/blank/null/empty/malformed/valid contract.
func TestParseTemplateOverridesFromInfoMatchesRaw(t *testing.T) {
	metas := []map[string]string{
		nil,
		{"state": "asleep"},
		{"template_overrides": ""},
		{"template_overrides": " \n\t "},
		{"template_overrides": "null"},
		{"template_overrides": "{}"},
		{"template_overrides": "{not json"},
		{"template_overrides": `{"model":1}`},
		{"template_overrides": `{"model":"sonnet","initial_message":"hi"}`},
	}
	for _, meta := range metas {
		info := infoFromPersistedBead(beads.Bead{Metadata: meta})
		rawOut, rawErr := ParseTemplateOverrides(meta)
		infoOut, infoErr := ParseTemplateOverridesFromInfo(info)
		if (rawErr == nil) != (infoErr == nil) {
			t.Fatalf("meta=%v: raw err=%v info err=%v", meta, rawErr, infoErr)
		}
		if !reflect.DeepEqual(rawOut, infoOut) {
			t.Fatalf("meta=%v: raw=%v info=%v", meta, rawOut, infoOut)
		}
	}
}
