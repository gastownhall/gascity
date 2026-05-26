package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookScriptsContainStamp(t *testing.T) {
	oldDate, oldCommit := date, commit
	t.Cleanup(func() { date, commit = oldDate, oldCommit })

	date = "2026-04-29T10:00:00Z"
	commit = "abc1234"

	for name, eventType := range beadHooks {
		t.Run(name, func(t *testing.T) {
			var content string
			if name == "on_close" {
				content = closeHookScript()
			} else {
				content = hookScript(eventType)
			}
			if !strings.Contains(content, "# gc-hook-stamp: 2026-04-29T10:00:00Z abc1234") {
				t.Errorf("hook %s missing stamp line:\n%s", name, content)
			}
		})
	}
}

func TestParseHookStampDate(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"with stamp", "#!/bin/sh\n# gc-hook-stamp: 2026-04-29T10:00:00Z abc1234\n", "2026-04-29T10:00:00Z"},
		{"no stamp", "#!/bin/sh\n# Installed by gc\n", ""},
		{"empty", "", ""},
		{"unknown date", "#!/bin/sh\n# gc-hook-stamp: unknown unknown\n", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHookStampDate([]byte(tt.content))
			if got != tt.want {
				t.Errorf("parseHookStampDate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInstallBeadHooksForwardOnly(t *testing.T) {
	oldDate, oldCommit := date, commit
	t.Cleanup(func() { date, commit = oldDate, oldCommit })

	dir := t.TempDir()

	// Install with a "newer" binary.
	date = "2026-06-01T00:00:00Z"
	commit = "new1111"
	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("newer install: %v", err)
	}

	path := filepath.Join(dir, ".beads", "hooks", "on_create")
	newerContent, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(newerContent), "2026-06-01") {
		t.Fatalf("newer hook missing expected stamp")
	}

	// Now run with an "older" binary — should NOT overwrite.
	date = "2025-01-01T00:00:00Z"
	commit = "old2222"
	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("older install: %v", err)
	}

	afterContent, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(newerContent, afterContent) {
		t.Errorf("stale binary overwrote newer hook.\nwant stamp from 2026-06-01, got:\n%s", afterContent)
	}
}

func TestInstallBeadHooksUpgradesLegacyHooks(t *testing.T) {
	oldDate, oldCommit := date, commit
	t.Cleanup(func() { date, commit = oldDate, oldCommit })

	dir := t.TempDir()
	hooksDir := filepath.Join(dir, ".beads", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a legacy hook (no stamp).
	legacy := "#!/bin/sh\n# Installed by gc — old version\necho old\n"
	if err := os.WriteFile(filepath.Join(hooksDir, "on_create"), []byte(legacy), 0o755); err != nil {
		t.Fatal(err)
	}

	date = "2026-01-01T00:00:00Z"
	commit = "aaa1111"
	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("installBeadHooks: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(hooksDir, "on_create"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "gc-hook-stamp") {
		t.Errorf("legacy hook was not upgraded to stamped version:\n%s", data)
	}
}

func TestInstallBeadHooksDevBuildAlwaysWrites(t *testing.T) {
	oldDate, oldCommit := date, commit
	t.Cleanup(func() { date, commit = oldDate, oldCommit })

	dir := t.TempDir()

	// Install with a stamped binary.
	date = "2099-01-01T00:00:00Z"
	commit = "future1"
	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("stamped install: %v", err)
	}

	// Dev build (unknown date) should still overwrite — dev builds always win.
	date = "unknown"
	commit = "unknown"
	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("dev install: %v", err)
	}

	path := filepath.Join(dir, ".beads", "hooks", "on_create")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "gc-hook-stamp: unknown unknown") {
		t.Errorf("dev build did not overwrite stamped hook:\n%s", data)
	}
}

func TestInstallBeadHooksCreatesScripts(t *testing.T) {
	dir := t.TempDir()
	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("installBeadHooks: %v", err)
	}

	hooksDir := filepath.Join(dir, ".beads", "hooks")

	for _, tc := range []struct {
		filename  string
		eventType string
	}{
		{"on_create", "bead.created"},
		{"on_close", "bead.closed"},
		{"on_update", "bead.updated"},
	} {
		t.Run(tc.filename, func(t *testing.T) {
			path := filepath.Join(hooksDir, tc.filename)
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatalf("hook %s not created: %v", tc.filename, err)
			}
			// Check executable permission.
			if fi.Mode()&0o111 == 0 {
				t.Errorf("hook %s not executable: %v", tc.filename, fi.Mode())
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading hook %s: %v", tc.filename, err)
			}
			content := string(data)

			// Starts with shebang.
			if !strings.HasPrefix(content, "#!/bin/sh") {
				t.Errorf("hook %s missing shebang: %q", tc.filename, content[:min(len(content), 20)])
			}
			// Contains the correct event type.
			if !strings.Contains(content, tc.eventType) {
				t.Errorf("hook %s missing event type %q:\n%s", tc.filename, tc.eventType, content)
			}
			// Contains gc event emit.
			if !strings.Contains(content, `GC_BIN="${GC_BIN:-gc}"`) {
				t.Errorf("hook %s missing GC_BIN fallback:\n%s", tc.filename, content)
			}
			if !strings.Contains(content, `"$GC_BIN" event emit`) {
				t.Errorf("hook %s missing '\"$GC_BIN\" event emit':\n%s", tc.filename, content)
			}
			if !strings.Contains(content, `PAYLOAD=$(printf '{"bead":%s}' "$DATA")`) {
				t.Errorf("hook %s does not wrap bd JSON as BeadEventPayload:\n%s", tc.filename, content)
			}
			if !strings.Contains(content, `--payload "$PAYLOAD"`) {
				t.Errorf("hook %s emits raw DATA instead of wrapped PAYLOAD:\n%s", tc.filename, content)
			}
			// Best-effort: stderr redirected, || true.
			if !strings.Contains(content, "|| true") {
				t.Errorf("hook %s missing '|| true' (best-effort):\n%s", tc.filename, content)
			}
			if !strings.Contains(content, `) </dev/null >/dev/null 2>&1 &`) {
				t.Errorf("hook %s missing detached background redirect:\n%s", tc.filename, content)
			}
			// Failure diagnostics: stderr captured to a log file, and
			// non-zero exit produces a dated diagnostic line.
			if !strings.Contains(content, `HOOK_LOG="${BEADS_DIR:-.beads}/hooks.log"`) {
				t.Errorf("hook %s missing HOOK_LOG definition:\n%s", tc.filename, content)
			}
			if !strings.Contains(content, `2>>"$HOOK_LOG"`) {
				t.Errorf("hook %s does not capture gc stderr to HOOK_LOG:\n%s", tc.filename, content)
			}
			if !strings.Contains(content, `>>"$HOOK_LOG" 2>/dev/null`) {
				t.Errorf("hook %s missing failure-diagnostic echo into HOOK_LOG:\n%s", tc.filename, content)
			}
			// on_close hook must also trigger convoy autoclose and wisp autoclose.
			if tc.filename == "on_close" {
				if !strings.Contains(content, `"$GC_BIN" convoy autoclose`) {
					t.Errorf("on_close hook missing '\"$GC_BIN\" convoy autoclose':\n%s", content)
				}
				if !strings.Contains(content, `"$GC_BIN" wisp autoclose`) {
					t.Errorf("on_close hook missing '\"$GC_BIN\" wisp autoclose':\n%s", content)
				}
			}
		})
	}
}

func TestInstallBeadHooksIdempotent(t *testing.T) {
	dir := t.TempDir()

	// Install twice — should not error.
	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("second install: %v", err)
	}

	// Verify hooks still correct after second install.
	path := filepath.Join(dir, ".beads", "hooks", "on_create")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading hook: %v", err)
	}
	if !strings.Contains(string(data), "bead.created") {
		t.Errorf("hook content wrong after idempotent install")
	}
}

func TestInstallBeadHooksDoesNotRewriteUnchangedHooks(t *testing.T) {
	dir := t.TempDir()

	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("first install: %v", err)
	}

	path := filepath.Join(dir, ".beads", "hooks", "on_create")
	past := time.Unix(123456789, 0)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("second install: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(past) {
		t.Fatalf("unchanged hook was rewritten: modtime = %s, want %s", info.ModTime(), past)
	}
}

func TestInstallBeadHooksReplacesMatchingSymlink(t *testing.T) {
	dir := t.TempDir()

	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("first install: %v", err)
	}

	path := filepath.Join(dir, ".beads", "hooks", "on_create")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	target := filepath.Join(dir, "outside-hook")
	if err := os.WriteFile(target, data, 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", target, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove(%s): %v", path, err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("Symlink: %v", err)
	}

	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("second install: %v", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%s): %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("matching symlink was preserved, want regular file")
	}
}

func TestInstallBeadHooksCreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	// No pre-existing .beads/ directory.
	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("installBeadHooks: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dir, ".beads", "hooks"))
	if err != nil {
		t.Fatalf(".beads/hooks not created: %v", err)
	}
	if !fi.IsDir() {
		t.Error(".beads/hooks is not a directory")
	}
}

func TestInstallBeadHooksInitIntegration(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_SESSION", "fake")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"init", cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc init = %d; stderr: %s", code, stderr.String())
	}

	// Verify hooks were installed at city root.
	hookPath := filepath.Join(cityPath, ".beads", "hooks", "on_create")
	if _, err := os.Stat(hookPath); err != nil {
		t.Errorf("gc init did not install bd hooks: %v", err)
	}
}

// TestCloseHookLogsFailureDiagnostic runs the installed on_close hook
// under sh with a deliberately broken GC_BIN and asserts that a dated
// diagnostic line lands in BEADS_DIR/hooks.log for each of the three
// gc invocations the hook is supposed to make. This is the visibility
// gap the script's `>/dev/null 2>&1 || true` pattern hid.
func TestCloseHookLogsFailureDiagnostic(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	dir := t.TempDir()
	if err := installBeadHooks(dir); err != nil {
		t.Fatalf("installBeadHooks: %v", err)
	}
	hookPath := filepath.Join(dir, ".beads", "hooks", "on_close")
	beadsDir := filepath.Join(dir, ".beads")

	cmd := exec.Command("sh", hookPath, "test-bead-id", "bead.closed")
	cmd.Stdin = strings.NewReader(`{"title":"hook diagnostic test"}`)
	cmd.Env = append(os.Environ(),
		"GC_BIN=/nonexistent/gc-bin-for-test",
		"BEADS_DIR="+beadsDir,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook exec failed: %v\nout: %s", err, out)
	}

	// Hook detaches into background; poll briefly for the log to be flushed.
	logPath := filepath.Join(beadsDir, "hooks.log")
	deadline := time.Now().Add(3 * time.Second)
	var content string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(logPath)
		if err == nil && len(data) > 0 {
			content = string(data)
			// Wait until all three failure markers have been emitted, since
			// the three gc calls run sequentially.
			if strings.Contains(content, "gc event emit") &&
				strings.Contains(content, "gc convoy autoclose") &&
				strings.Contains(content, "gc wisp autoclose") {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if content == "" {
		t.Fatalf("hooks.log not written or empty under %s", beadsDir)
	}
	for _, want := range []string{
		"test-bead-id",
		"/nonexistent/gc-bin-for-test",
		"gc event emit bead.closed failed",
		"gc convoy autoclose failed",
		"gc wisp autoclose failed",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("hooks.log missing %q:\n%s", want, content)
		}
	}
}

func TestInstallBeadHooksRigAddIntegration(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_SESSION", "fake")

	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "myapp")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "rig", "add", rigPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc rig add = %d; stderr: %s", code, stderr.String())
	}

	// Verify hooks were installed at rig path.
	hookPath := filepath.Join(rigPath, ".beads", "hooks", "on_create")
	if _, err := os.Stat(hookPath); err != nil {
		t.Errorf("gc rig add did not install bd hooks: %v", err)
	}
}
