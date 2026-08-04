package main

import (
	"runtime/debug"
	"testing"
)

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "v0.13.0", want: "0.13.0"},
		{in: "0.13.0", want: "0.13.0"},
		{in: "v0.13.0-rc2.0.20260317225312-41a12e4914cb+dirty", want: "0.13.0-rc2"},
		{in: "v0.0.0-20260317225312-41a12e4914cb", want: "dev"},
		{in: "(devel)", want: "dev"},
		{in: "", want: "dev"},
		// SemVer build metadata must be preserved.
		{in: "1.3.5+ra.1", want: "1.3.5+ra.1"},
		{in: "1.3.5", want: "1.3.5"},
		// Pseudo-version with a newer timestamp still collapses.
		{in: "v0.0.0-20260719191849-4c2927134266", want: "dev"},
		// +incompatible is the one Go-specific suffix we strip.
		{in: "1.2.3+incompatible", want: "1.2.3"},
	}
	for _, tt := range tests {
		if got := normalizeVersion(tt.in); got != tt.want {
			t.Fatalf("normalizeVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResolveBuildMetadataUsesModuleVersion(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{
			Version: "v0.13.0",
		},
	}
	version, commit, date := resolveBuildMetadata("dev", "unknown", "unknown", true, info)
	if version != "0.13.0" {
		t.Fatalf("version = %q, want %q", version, "0.13.0")
	}
	if commit != "unknown" {
		t.Fatalf("commit = %q, want unknown", commit)
	}
	if date != "unknown" {
		t.Fatalf("date = %q, want unknown", date)
	}
}

func TestResolveBuildMetadataUsesVCSSettings(t *testing.T) {
	info := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
			{Key: "vcs.time", Value: "2026-03-17T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	version, commit, date := resolveBuildMetadata("dev", "unknown", "unknown", true, info)
	if version != "dev" {
		t.Fatalf("version = %q, want dev", version)
	}
	if commit != "abc123-dirty" {
		t.Fatalf("commit = %q, want %q", commit, "abc123-dirty")
	}
	if date != "2026-03-17T00:00:00Z" {
		t.Fatalf("date = %q, want %q", date, "2026-03-17T00:00:00Z")
	}
}
