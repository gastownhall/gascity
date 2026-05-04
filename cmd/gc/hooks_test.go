package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
