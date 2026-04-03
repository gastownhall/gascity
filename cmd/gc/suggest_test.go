package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"mayor", "mayer", 1},   // single substitution
		{"claude", "cloude", 1}, // single substitution
		{"codex", "codex", 0},
		{"cat", "car", 1},
		{"kitten", "sitting", 3},
	}
	for _, tt := range tests {
		got := levenshtein(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestSuggestSimilar(t *testing.T) {
	candidates := []string{"mayor", "deacon", "worker", "polecat"}

	tests := []struct {
		input    string
		wantHas  string // substring the result should contain, or "" for empty
		wantNone bool   // true if we expect no suggestion
	}{
		{"mayer", "mayor", false},          // distance 1
		{"worke", "worker", false},         // distance 1
		{"deecon", "deacon", false},        // distance 1
		{"completely-different", "", true}, // too far
		{"xyz", "", true},                  // too far
		{"Mayor", "mayor", false},          // case-insensitive
	}
	for _, tt := range tests {
		got := suggestSimilar(tt.input, candidates)
		if tt.wantNone {
			if got != "" {
				t.Errorf("suggestSimilar(%q) = %q, want empty", tt.input, got)
			}
			continue
		}
		if !strings.Contains(got, tt.wantHas) {
			t.Errorf("suggestSimilar(%q) = %q, want to contain %q", tt.input, got, tt.wantHas)
		}
	}
}

func TestSuggestSimilarEmptyCandidates(t *testing.T) {
	got := suggestSimilar("mayor", nil)
	if got != "" {
		t.Errorf("suggestSimilar with nil candidates = %q, want empty", got)
	}
}

func TestFormatAvailable(t *testing.T) {
	tests := []struct {
		label string
		names []string
		want  string
	}{
		{"agents", nil, ""},
		{"agents", []string{}, ""},
		{"agents", []string{"mayor"}, "; available agents: mayor"},
		{"agents", []string{"a", "b", "c"}, "; available agents: a, b, c"},
		{"rigs", []string{"a", "b", "c", "d", "e", "f"}, "; available rigs: a, b, c, d, e, ..."},
	}
	for _, tt := range tests {
		got := formatAvailable(tt.label, tt.names)
		if got != tt.want {
			t.Errorf("formatAvailable(%q, %v) = %q, want %q", tt.label, tt.names, got, tt.want)
		}
	}
}

func TestAgentNotFoundMsg(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "worker"},
		},
	}

	// Close match → should suggest.
	msg := agentNotFoundMsg("gc agent attach", "mayer", cfg)
	if !strings.Contains(msg, "did you mean") {
		t.Errorf("should suggest similar: %q", msg)
	}
	if !strings.Contains(msg, "mayor") {
		t.Errorf("should mention mayor: %q", msg)
	}

	// No close match → should list available.
	msg = agentNotFoundMsg("gc agent attach", "xyz", cfg)
	if !strings.Contains(msg, "available agents") {
		t.Errorf("should list available: %q", msg)
	}
}

func TestRigNotFoundMsg(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "hello-world", Path: "/tmp/hw"},
			{Name: "tower-of-hanoi", Path: "/tmp/toh"},
		},
	}

	msg := rigNotFoundMsg("gc rig status", "hello-wolrd", cfg)
	if !strings.Contains(msg, "did you mean") {
		t.Errorf("should suggest similar: %q", msg)
	}
}

func TestAvailableAgentNames(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "polecat", Dir: "hw"},
		},
	}
	names := availableAgentNames(cfg)
	if len(names) != 2 {
		t.Fatalf("expected 2, got %d", len(names))
	}
	if names[0] != "mayor" {
		t.Errorf("names[0] = %q, want mayor", names[0])
	}
	if names[1] != "hw/polecat" {
		t.Errorf("names[1] = %q, want hw/polecat", names[1])
	}
}
