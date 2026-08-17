package dashboardbff

import (
	"strings"
	"testing"
)

func TestRunDetailPath(t *testing.T) {
	tests := []struct {
		name  string
		city  string
		runID string
		want  string
	}{
		{name: "plain", city: "alpha", runID: "gcg-abc123", want: "/city/alpha/runs/gcg-abc123"},
		{name: "dotted run id", city: "alpha", runID: "run1.2", want: "/city/alpha/runs/run1.2"},
		{
			name:  "run id needing escaping",
			city:  "alpha",
			runID: "a/b c%",
			want:  "/city/alpha/runs/a%2Fb%20c%25",
		},
		{
			name:  "city needing escaping",
			city:  "a/b",
			runID: "run1",
			want:  "/city/a%2Fb/runs/run1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RunDetailPath(tt.city, tt.runID); got != tt.want {
				t.Errorf("RunDetailPath(%q, %q) = %q, want %q", tt.city, tt.runID, got, tt.want)
			}
		})
	}
}

func TestRunsListPath(t *testing.T) {
	if got, want := RunsListPath("alpha"), "/city/alpha/runs"; got != want {
		t.Errorf("RunsListPath(alpha) = %q, want %q", got, want)
	}
	if got, want := RunsListPath("a b"), "/city/a%20b/runs"; got != want {
		t.Errorf("RunsListPath(a b) = %q, want %q", got, want)
	}
}

func TestValidCityName(t *testing.T) {
	valid := []string{"a", "alpha", "alpha-1", "A1-b2-C3", strings.Repeat("a", 64)}
	for _, name := range valid {
		if !ValidCityName(name) {
			t.Errorf("ValidCityName(%q) = false, want true", name)
		}
	}
	invalid := []string{"", "-a", "a-", "a_b", "a b", "a/b", "a.b", strings.Repeat("a", 65)}
	for _, name := range invalid {
		if ValidCityName(name) {
			t.Errorf("ValidCityName(%q) = true, want false", name)
		}
	}
}
