package main

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

func TestWaitForManagedDoltProcessExit(t *testing.T) {
	const pid = 4242
	tests := []struct {
		name         string
		timeout      time.Duration
		aliveThrough int
		wantCalls    int
		wantElapsed  time.Duration
	}{
		{name: "already dead", timeout: 45 * time.Millisecond, aliveThrough: 0, wantCalls: 1},
		{name: "exits after two polls", timeout: time.Second, aliveThrough: 2, wantCalls: 3, wantElapsed: 40 * time.Millisecond},
		{name: "never exits uses exact remainder", timeout: 45 * time.Millisecond, aliveThrough: -1, wantCalls: 4, wantElapsed: 45 * time.Millisecond},
		{name: "nonpositive bound probes once", timeout: 0, aliveThrough: -1, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				started := time.Now()
				waitForManagedDoltProcessExit(pid, tt.timeout, func(gotPID int) bool {
					if gotPID != pid {
						t.Fatalf("alive pid = %d, want %d", gotPID, pid)
					}
					calls++
					return tt.aliveThrough < 0 || calls <= tt.aliveThrough
				})

				if calls != tt.wantCalls {
					t.Errorf("alive calls = %d, want %d", calls, tt.wantCalls)
				}
				if elapsed := time.Since(started); elapsed != tt.wantElapsed {
					t.Errorf("elapsed = %v, want %v", elapsed, tt.wantElapsed)
				}
			})
		})
	}
}

func TestStopManagedDoltExpectedIdentityRefusalPreservesStalePIDFile(t *testing.T) {
	city := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(city)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatal(err)
	}
	stalePID := []byte("not-a-pid\n")
	if err := os.WriteFile(layout.PIDFile, stalePID, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := stopManagedDoltProcessWithExpectedIdentity(city, "1", true, &handoffProtocolIdentity{PID: -1})
	if err == nil {
		t.Fatal("strict stop accepted an impossible expected identity")
	}
	if report.Mutated {
		t.Fatalf("strict refusal reports mutation: %+v", report)
	}
	got, err := os.ReadFile(layout.PIDFile)
	if err != nil || string(got) != string(stalePID) {
		t.Fatalf("strict refusal changed stale PID file: got %q, err=%v", got, err)
	}
}

func TestClearManagedDoltRuntimeReportsPartialMutation(t *testing.T) {
	city := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(city)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	// A nonempty directory at the PID-file path makes cleanup fail after the
	// atomic runtime-state rewrite has committed.
	if err := os.MkdirAll(layout.PIDFile, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.PIDFile, "keep"), []byte("evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	mutated, err := clearManagedDoltRuntimeWithMutation(layout, "3307")
	if err == nil || !mutated {
		t.Fatalf("clear runtime = mutated %t, err %v; want partial mutation and error", mutated, err)
	}
	state, err := readDoltRuntimeStateFile(layout.StateFile)
	if err != nil || state.Running || state.PID != 0 {
		t.Fatalf("runtime state was not committed before cleanup error: state=%+v err=%v", state, err)
	}
}

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
