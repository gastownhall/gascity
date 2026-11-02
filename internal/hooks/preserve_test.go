package hooks

import (
	"bytes"
	"fmt"
	iofs "io/fs"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/overlay"
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
// revert it on the next reconcile tick (#5554). Every versioned plugin is
// checked as installed: one that its own predicate reads as stale would be
// rewritten by every install and reverted by every tick. A file the predicate
// does not recognize as managed is preserved too, so the same file with its
// version zeroed must be replaced.
func TestPreserveManagedFileKeepsCurrentPlugin(t *testing.T) {
	version := regexp.MustCompile(`(GC_[A-Z]+_HOOK_VERSION\s*=\s*)[0-9]+`)
	for rel, provider := range managedOverlayHookPaths {
		t.Run(provider, func(t *testing.T) {
			fs := fsys.NewFake()
			if err := Install(fs, "/city", "/work", []string{provider}); err != nil {
				t.Fatalf("Install: %v", err)
			}
			installed, ok := fs.Files[filepath.Join("/work", rel)]
			if !ok {
				t.Fatalf("Install wrote no %s", rel)
			}
			if !PreserveManagedFile(rel, installed) {
				t.Errorf("freshly installed %s plugin was not preserved", provider)
			}
			stale := version.ReplaceAll(installed, []byte("${1}0"))
			if bytes.Equal(stale, installed) {
				t.Fatalf("%s has no hook version to zero", rel)
			}
			if PreserveManagedFile(rel, stale) {
				t.Errorf("%s plugin with its version zeroed was preserved; the bundled file is not recognized as managed", provider)
			}
		})
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

// managedOverlayHookPaths re-encodes by hand a pairing overlayManagedNeedsUpgrade
// already owns, and the drift fails open: a provider that gains a version marker
// without a map entry silently loses overlay-staging preservation. Walk the
// bundled overlay and assert the two agree.
//
// Mergeable paths (.cursor/hooks.json and friends) are exempt: the staging
// caller that consults PreserveManagedFile is
// StageProviderOverlayDirSkippingMergeable, which drops them before the
// predicate runs, so a map entry would be dead weight. They are excluded here
// deliberately, not by oversight.
func TestManagedOverlayHookPathsCoverEveryVersionedFile(t *testing.T) {
	providers := []string{
		"codex", "gemini", "antigravity", "kiro", "opencode",
		"mimocode", "copilot", "cursor", "pi", "omp", "kimi",
	}
	checked := 0
	for _, provider := range providers {
		base := path.Join("overlay", "per-provider", provider)
		if _, err := iofs.Stat(core.PackFS, base); err != nil {
			t.Fatalf("provider overlay %q missing: %v", provider, err)
		}
		err := iofs.WalkDir(core.PackFS, base, func(name string, d iofs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if name == base || d.IsDir() {
				return nil
			}
			rel := strings.TrimPrefix(name, base+"/")
			data, readErr := iofs.ReadFile(core.PackFS, name)
			if readErr != nil {
				return readErr
			}
			if overlayManagedNeedsUpgrade(provider, rel, data) == nil {
				return nil
			}
			if overlay.IsMergeablePath(filepath.FromSlash(rel)) {
				if got, ok := managedOverlayHookPaths[filepath.FromSlash(rel)]; ok {
					t.Errorf("mergeable path %q is listed in managedOverlayHookPaths (provider %q); "+
						"staging skips it before the predicate runs", rel, got)
				}
				return nil
			}
			checked++
			got, ok := managedOverlayHookPaths[filepath.FromSlash(rel)]
			if !ok {
				t.Errorf("provider %q file %q has a version marker but no managedOverlayHookPaths "+
					"entry, so overlay staging will silently revert it", provider, rel)
				return nil
			}
			if got != provider {
				t.Errorf("managedOverlayHookPaths[%q] = %q, want %q", rel, got, provider)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %q: %v", base, err)
		}
	}
	if checked != len(managedOverlayHookPaths) {
		t.Errorf("walked %d versioned non-mergeable overlay files but managedOverlayHookPaths has %d entries; "+
			"a stale entry no longer matches any bundled file", checked, len(managedOverlayHookPaths))
	}
}
