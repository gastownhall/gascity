package hooks

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func installedOpenCodePlugin(t *testing.T) []byte {
	t.Helper()
	fs := fsys.NewFake()
	if err := Install(fs, "/city", "/work", []string{"opencode"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	return fs.Files["/work/.opencode/plugins/gascity.js"]
}

// A current managed hook file must be preserved so overlay staging cannot
// revert it on the next reconcile tick (#5554).
func TestPreserveManagedFileKeepsCurrentPlugin(t *testing.T) {
	rel := filepath.Join(".opencode", "plugins", "gascity.js")
	if !PreserveManagedFile(rel, installedOpenCodePlugin(t)) {
		t.Fatal("freshly installed OpenCode plugin was not preserved")
	}
}

// A newer local version must also survive: opencodeHookNeedsUpgrade compares
// with <, so a higher version is explicitly not stale.
func TestPreserveManagedFileKeepsNewerPlugin(t *testing.T) {
	rel := filepath.Join(".opencode", "plugins", "gascity.js")
	newer := bytes.Replace(installedOpenCodePlugin(t),
		[]byte(fmt.Sprintf("GC_OPENCODE_HOOK_VERSION = %d", managedOpenCodeHookVersion)),
		[]byte("GC_OPENCODE_HOOK_VERSION = 999"), 1)
	if !PreserveManagedFile(rel, newer) {
		t.Fatal("a newer managed plugin was not preserved")
	}
}

// A stale file must still be replaced, so the predicate cannot strand a hook.
func TestPreserveManagedFileReplacesStalePlugin(t *testing.T) {
	rel := filepath.Join(".opencode", "plugins", "gascity.js")
	stale := bytes.Replace(installedOpenCodePlugin(t),
		[]byte(fmt.Sprintf("GC_OPENCODE_HOOK_VERSION = %d", managedOpenCodeHookVersion)),
		[]byte("GC_OPENCODE_HOOK_VERSION = 0"), 1)
	if PreserveManagedFile(rel, stale) {
		t.Fatal("a stale managed plugin was preserved")
	}
}

// Anything not recognized as a managed hook file stages exactly as before.
func TestPreserveManagedFileIgnoresUnmanagedPaths(t *testing.T) {
	for _, rel := range []string{
		filepath.Join(".codex", "hooks.json"),
		filepath.Join(".claude", "settings.json"),
		"AGENTS.md",
		"",
	} {
		if PreserveManagedFile(rel, []byte("anything")) {
			t.Errorf("unmanaged path %q was preserved", rel)
		}
	}
}

// Install already refuses to replace a user-authored plugin at the managed path
// (TestInstallOpenCodeHookPreservesUserAuthoredPlugin). Overlay staging must
// reach the same conclusion, otherwise the two writers disagree and the tick
// destroys what Install deliberately kept.
func TestPreserveManagedFileKeepsUserAuthoredPlugin(t *testing.T) {
	rel := filepath.Join(".opencode", "plugins", "gascity.js")
	if !PreserveManagedFile(rel, []byte("export default async function customPlugin() {}\n")) {
		t.Fatal("a user-authored plugin at the managed path was not preserved")
	}
}
