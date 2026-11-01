package main

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestParseBeadFormat(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantFmt  string
		wantRest []string
	}{
		{"nil args", nil, "text", nil},
		{"empty args", []string{}, "text", nil},
		{"no format flag", []string{"gc-1"}, "text", []string{"gc-1"}},
		{"--format json", []string{"--format", "json", "gc-1"}, "json", []string{"gc-1"}},
		{"--format toon", []string{"--format", "toon"}, "toon", nil},
		{"--format=json", []string{"--format=json", "gc-1"}, "json", []string{"gc-1"}},
		{"--format=toon", []string{"--format=toon"}, "toon", nil},
		{"--format text", []string{"--format", "text"}, "text", nil},
		{"--json shorthand", []string{"--json", "gc-1"}, "json", []string{"gc-1"}},
		{"format after positional", []string{"gc-1", "--format", "json"}, "json", []string{"gc-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotFmt, gotRest := parseBeadFormat(tt.args)
			if gotFmt != tt.wantFmt {
				t.Errorf("format = %q, want %q", gotFmt, tt.wantFmt)
			}
			if len(gotRest) != len(tt.wantRest) {
				t.Errorf("rest = %v, want %v", gotRest, tt.wantRest)
			} else {
				for i := range gotRest {
					if gotRest[i] != tt.wantRest[i] {
						t.Errorf("rest[%d] = %q, want %q", i, gotRest[i], tt.wantRest[i])
					}
				}
			}
		})
	}
}

func TestToonVal(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"simple", "simple"},
		{"has,comma", `"has,comma"`},
		{`has"quote`, `"has""quote"`},
		{"has\nnewline", `"has` + "\n" + `newline"`},
		{"", ""},
	}
	for _, tt := range tests {
		got := toonVal(tt.in)
		if got != tt.want {
			t.Errorf("toonVal(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestFilterBeadsWithNoMatchIsEmptyNotNil proves a --label/--status filter
// that matches nothing yields an empty slice that prints [] like the direct
// store read, never null.
func TestFilterBeadsWithNoMatchIsEmptyNotNil(t *testing.T) {
	in := []beads.Bead{{ID: "gc-1", Status: "open", Labels: []string{"x"}}}
	for name, f := range map[string]beadFilters{
		"label":  {label: "nope"},
		"status": {status: "closed"},
	} {
		t.Run(name, func(t *testing.T) {
			got := filterBeads(in, f)
			if got == nil || len(got) != 0 {
				t.Fatalf("filterBeads = %#v, want empty non-nil slice", got)
			}
			data, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(data) != "[]" {
				t.Fatalf("json = %s, want []", data)
			}
		})
	}
}
