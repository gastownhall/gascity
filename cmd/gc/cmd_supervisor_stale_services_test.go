package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeStaleSweepLaunchdPlist renders a real supervisor plist for label
// pointing at gcHome and writes it into dir, mirroring what
// installSupervisorLaunchd produces.
func writeStaleSweepLaunchdPlist(t *testing.T, dir, label, gcHome string) string {
	t.Helper()
	content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, &supervisorServiceData{
		GCPath:       "/tmp/gc-test",
		LogPath:      filepath.Join(gcHome, "supervisor.log"),
		GCHome:       gcHome,
		LaunchdLabel: label,
		Path:         "/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, label+".plist")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeStaleSweepSystemdUnit renders a real supervisor unit named unit
// pointing at gcHome and writes it into dir, mirroring what
// installSupervisorSystemd produces.
func writeStaleSweepSystemdUnit(t *testing.T, dir, unit, gcHome string) string {
	t.Helper()
	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, &supervisorServiceData{
		GCPath:  "/tmp/gc-test",
		LogPath: filepath.Join(gcHome, "supervisor.log"),
		GCHome:  gcHome,
		Path:    "/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, unit)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSweepStaleIsolatedSupervisorLaunchdRemovesOnlyStale(t *testing.T) {
	homeDir := t.TempDir()
	ownGCHome := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", ownGCHome)
	oldGOOS := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "darwin"
	t.Cleanup(func() { supervisorRuntimeGOOS = oldGOOS })

	launchAgents := filepath.Join(homeDir, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgents, 0o755); err != nil {
		t.Fatal(err)
	}

	staleHome := filepath.Join(t.TempDir(), "deleted-gc-home")
	liveHome := t.TempDir()

	// The default (unsuffixed) agent must never be swept, even when its
	// GC_HOME is gone.
	defaultPath := writeStaleSweepLaunchdPlist(t, launchAgents, defaultSupervisorLaunchdLabel, staleHome)
	// The current process's own suffixed agent must never be swept.
	ownPath := writeStaleSweepLaunchdPlist(t, launchAgents, supervisorLaunchdLabel(), ownGCHome)
	// A leaked agent whose GC_HOME no longer exists must be removed.
	staleLabel := defaultSupervisorLaunchdLabel + ".gc-home-12345678"
	stalePath := writeStaleSweepLaunchdPlist(t, launchAgents, staleLabel, staleHome)
	// Another isolated agent whose GC_HOME still exists must be kept.
	livePath := writeStaleSweepLaunchdPlist(t, launchAgents, defaultSupervisorLaunchdLabel+".other-abcd1234", liveHome)
	// A suffixed plist that cannot be parsed must be left alone.
	junkPath := filepath.Join(launchAgents, defaultSupervisorLaunchdLabel+".junk-ffffffff.plist")
	if err := os.WriteFile(junkPath, []byte("<not-a-plist>"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An unrelated plist must be ignored entirely.
	otherPath := filepath.Join(launchAgents, "com.example.other.plist")
	if err := os.WriteFile(otherPath, []byte("<not-a-plist>"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldRun := supervisorLaunchctlRun
	var calls []string
	supervisorLaunchctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { supervisorLaunchctlRun = oldRun })

	var stderr bytes.Buffer
	sweepStaleIsolatedSupervisorServices(&stderr)

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale plist %q should be removed; err=%v", stalePath, err)
	}
	for _, keep := range []string{defaultPath, ownPath, livePath, junkPath, otherPath} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("plist %q should be untouched: %v", keep, err)
		}
	}

	joined := strings.Join(calls, "\n")
	target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + staleLabel
	for _, want := range []string{"unload " + stalePath, "disable " + target} {
		if !strings.Contains(joined, want) {
			t.Fatalf("launchctl calls = %v, want %q", calls, want)
		}
	}
	for _, path := range []string{defaultPath, ownPath, livePath, junkPath, otherPath} {
		if strings.Contains(joined, path) {
			t.Fatalf("launchctl calls %v must not reference %q", calls, path)
		}
	}
	if !strings.Contains(stderr.String(), staleLabel) {
		t.Fatalf("stderr = %q, want removal notice for %q", stderr.String(), staleLabel)
	}
}

func TestSweepStaleIsolatedSupervisorSystemdRemovesOnlyStale(t *testing.T) {
	homeDir := t.TempDir()
	ownGCHome := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", ownGCHome)
	oldGOOS := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "linux"
	t.Cleanup(func() { supervisorRuntimeGOOS = oldGOOS })

	unitDir := filepath.Join(homeDir, ".local", "share", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}

	staleHome := filepath.Join(t.TempDir(), "deleted-gc-home")
	liveHome := t.TempDir()

	defaultPath := writeStaleSweepSystemdUnit(t, unitDir, defaultSupervisorSystemdUnit, staleHome)
	ownPath := writeStaleSweepSystemdUnit(t, unitDir, supervisorSystemdServiceName(), ownGCHome)
	staleUnit := "gascity-supervisor-gc-home-12345678.service"
	stalePath := writeStaleSweepSystemdUnit(t, unitDir, staleUnit, staleHome)
	livePath := writeStaleSweepSystemdUnit(t, unitDir, "gascity-supervisor-other-abcd1234.service", liveHome)
	unrelatedPath := filepath.Join(unitDir, "other.service")
	if err := os.WriteFile(unrelatedPath, []byte("[Unit]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldRun := supervisorSystemctlRun
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { supervisorSystemctlRun = oldRun })

	var stderr bytes.Buffer
	sweepStaleIsolatedSupervisorServices(&stderr)

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale unit %q should be removed; err=%v", stalePath, err)
	}
	for _, keep := range []string{defaultPath, ownPath, livePath, unrelatedPath} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("unit %q should be untouched: %v", keep, err)
		}
	}

	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"--user stop " + staleUnit,
		"--user disable " + staleUnit,
		"--user daemon-reload",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("systemctl calls = %v, want %q", calls, want)
		}
	}
	for _, unit := range []string{defaultSupervisorSystemdUnit, supervisorSystemdServiceName(), "gascity-supervisor-other-abcd1234.service"} {
		if strings.Contains(joined, "stop "+unit) || strings.Contains(joined, "disable "+unit) {
			t.Fatalf("systemctl calls %v must not stop/disable %q", calls, unit)
		}
	}
}

func TestSweepStaleIsolatedSupervisorSystemdNoStaleMakesNoSystemctlCalls(t *testing.T) {
	homeDir := t.TempDir()
	ownGCHome := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", ownGCHome)
	oldGOOS := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "linux"
	t.Cleanup(func() { supervisorRuntimeGOOS = oldGOOS })

	unitDir := filepath.Join(homeDir, ".local", "share", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	liveHome := t.TempDir()
	writeStaleSweepSystemdUnit(t, unitDir, "gascity-supervisor-live-abcd1234.service", liveHome)

	oldRun := supervisorSystemctlRun
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { supervisorSystemctlRun = oldRun })

	var stderr bytes.Buffer
	sweepStaleIsolatedSupervisorServices(&stderr)

	if len(calls) != 0 {
		t.Fatalf("systemctl calls = %v, want none when nothing is stale", calls)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty when nothing is stale", stderr.String())
	}
}

func TestSweepStaleIsolatedSupervisorServicesMissingDirIsNoop(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", t.TempDir())
	oldGOOS := supervisorRuntimeGOOS
	t.Cleanup(func() { supervisorRuntimeGOOS = oldGOOS })

	for _, goos := range []string{"darwin", "linux"} {
		supervisorRuntimeGOOS = goos
		var stderr bytes.Buffer
		sweepStaleIsolatedSupervisorServices(&stderr)
		if stderr.Len() != 0 {
			t.Fatalf("goos=%s: stderr = %q, want empty for missing service dir", goos, stderr.String())
		}
	}
}

func TestInstallSupervisorLaunchdSweepsStaleSiblings(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	oldGOOS := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "darwin"
	t.Cleanup(func() { supervisorRuntimeGOOS = oldGOOS })

	launchAgents := filepath.Join(homeDir, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgents, 0o755); err != nil {
		t.Fatal(err)
	}
	staleHome := filepath.Join(t.TempDir(), "deleted-gc-home")
	stalePath := writeStaleSweepLaunchdPlist(t, launchAgents, defaultSupervisorLaunchdLabel+".gc-home-87654321", staleHome)

	oldRun := supervisorLaunchctlRun
	supervisorLaunchctlRun = func(_ ...string) error { return nil }
	t.Cleanup(func() { supervisorLaunchctlRun = oldRun })

	data := &supervisorServiceData{
		GCPath:       "/tmp/gc-new",
		LogPath:      filepath.Join(gcHome, "supervisor.log"),
		GCHome:       gcHome,
		LaunchdLabel: supervisorLaunchdLabel(),
		Path:         "/usr/local/bin:/usr/bin:/bin",
	}

	var stdout, stderr bytes.Buffer
	if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorLaunchd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale sibling plist %q should be swept during install; err=%v", stalePath, err)
	}
	if _, err := os.Stat(supervisorLaunchdPlistPath()); err != nil {
		t.Fatalf("own plist should be installed: %v", err)
	}
}

func TestUninstallSupervisorLaunchdSweepsStaleSiblings(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	oldGOOS := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "darwin"
	t.Cleanup(func() { supervisorRuntimeGOOS = oldGOOS })

	launchAgents := filepath.Join(homeDir, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgents, 0o755); err != nil {
		t.Fatal(err)
	}
	staleHome := filepath.Join(t.TempDir(), "deleted-gc-home")
	stalePath := writeStaleSweepLaunchdPlist(t, launchAgents, defaultSupervisorLaunchdLabel+".gc-home-13572468", staleHome)
	ownPath := writeStaleSweepLaunchdPlist(t, launchAgents, supervisorLaunchdLabel(), gcHome)

	oldRun := supervisorLaunchctlRun
	supervisorLaunchctlRun = func(_ ...string) error { return nil }
	t.Cleanup(func() { supervisorLaunchctlRun = oldRun })
	oldActive := supervisorLaunchdActive
	supervisorLaunchdActive = func(string) bool { return false }
	t.Cleanup(func() { supervisorLaunchdActive = oldActive })

	var stdout, stderr bytes.Buffer
	if code := uninstallSupervisorLaunchd(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("uninstallSupervisorLaunchd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale sibling plist %q should be swept during uninstall; err=%v", stalePath, err)
	}
	if _, err := os.Stat(ownPath); !os.IsNotExist(err) {
		t.Fatalf("own plist %q should be removed by uninstall; err=%v", ownPath, err)
	}
}
