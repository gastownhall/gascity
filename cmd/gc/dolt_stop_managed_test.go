package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

func TestManagedDoltStopPollInterval(t *testing.T) {
	cases := []struct {
		name  string
		grace time.Duration
		want  time.Duration
	}{
		{"default grace keeps 500ms", 30 * time.Second, 500 * time.Millisecond},
		{"exactly 500ms keeps 500ms", 500 * time.Millisecond, 500 * time.Millisecond},
		{"sub-poll grace shrinks to grace", 200 * time.Millisecond, 200 * time.Millisecond},
		{"tiny grace shrinks to grace", 100 * time.Millisecond, 100 * time.Millisecond},
		{"zero grace keeps 500ms", 0, 500 * time.Millisecond},
		{"negative grace keeps 500ms", -1 * time.Second, 500 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltStopPollInterval(tc.grace); got != tc.want {
				t.Errorf("managedDoltStopPollInterval(%v) = %v, want %v", tc.grace, got, tc.want)
			}
		})
	}
}

func TestResolveManagedDoltStopTimeoutDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test"

[[agent]]
name = "mayor"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	got := resolveManagedDoltStopTimeout(dir)
	if got != config.DefaultDoltStopTimeout {
		t.Errorf("resolveManagedDoltStopTimeout() = %v, want %v (default)", got, config.DefaultDoltStopTimeout)
	}
}

func TestResolveManagedDoltStopTimeoutCustom(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test"

[daemon]
dolt_stop_timeout = "1m"

[[agent]]
name = "mayor"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	got := resolveManagedDoltStopTimeout(dir)
	if got != time.Minute {
		t.Errorf("resolveManagedDoltStopTimeout() = %v, want 1m", got)
	}
}

func TestResolveManagedDoltStopTimeoutMissingCityFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	// No city.toml — loadCityConfig should fail and we should fall back.
	got := resolveManagedDoltStopTimeout(dir)
	if got != config.DefaultDoltStopTimeout {
		t.Errorf("resolveManagedDoltStopTimeout() with no city.toml = %v, want %v (default)", got, config.DefaultDoltStopTimeout)
	}
}

func TestResolveManagedDoltStopTimeoutEmptyCityPathReturnsDefault(t *testing.T) {
	// An empty cityPath must NOT trigger loadCityConfig("", …), which would
	// resolve "city.toml" relative to cwd and materialize builtin packs
	// there. Plant a stray ./city.toml with a non-default dolt_stop_timeout;
	// resolveManagedDoltStopTimeout("") must ignore it and return the default.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "stray"

[daemon]
dolt_stop_timeout = "1m"

[[agent]]
name = "mayor"
`), 0o644); err != nil {
		t.Fatalf("write stray city.toml: %v", err)
	}
	t.Chdir(dir)

	got := resolveManagedDoltStopTimeout("")
	if got != config.DefaultDoltStopTimeout {
		t.Errorf("resolveManagedDoltStopTimeout(\"\") = %v, want %v (default — must not read stray ./city.toml)", got, config.DefaultDoltStopTimeout)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gc")); err == nil {
		t.Error("resolveManagedDoltStopTimeout(\"\") materialized .gc/ under cwd; empty cityPath must not load config")
	}
}

func TestResolveManagedDoltStopTimeoutInvalidValueFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test"

[daemon]
dolt_stop_timeout = "not-a-duration"

[[agent]]
name = "mayor"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	got := resolveManagedDoltStopTimeout(dir)
	if got != config.DefaultDoltStopTimeout {
		t.Errorf("resolveManagedDoltStopTimeout() with invalid duration = %v, want %v (default)", got, config.DefaultDoltStopTimeout)
	}
}
