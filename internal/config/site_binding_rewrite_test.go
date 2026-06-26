package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// Regression test for ga-lurp5d: rewriting a city.toml that is a symlink
// (e.g., into a checked-out repo) must write through the link — temp file in
// the target's directory, rename over the target — instead of replacing the
// link with a regular file.
func TestWriteCityAndRigSiteBindingsForEditWritesThroughSymlink(t *testing.T) {
	cases := []struct {
		name       string
		linkTarget func(target, linkDir string) string
	}{
		{name: "absolute link", linkTarget: func(target, _ string) string { return target }},
		{name: "relative link", linkTarget: func(target, linkDir string) string {
			rel, err := filepath.Rel(linkDir, target)
			if err != nil {
				t.Fatal(err)
			}
			return rel
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			checkoutDir := filepath.Join(dir, "checkout")
			if err := os.MkdirAll(checkoutDir, 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(checkoutDir, "city.toml")
			if err := os.WriteFile(target, []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cityDir := filepath.Join(dir, "city")
			if err := os.MkdirAll(cityDir, 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(cityDir, "city.toml")
			linkTarget := tc.linkTarget(target, cityDir)
			if err := os.Symlink(linkTarget, link); err != nil {
				t.Fatal(err)
			}

			cfg, err := Load(fsys.OSFS{}, link)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			cfg.Workspace.Name = "renamed-city"

			if err := WriteCityAndRigSiteBindingsForEdit(fsys.OSFS{}, link, cfg); err != nil {
				t.Fatalf("WriteCityAndRigSiteBindingsForEdit: %v", err)
			}

			info, err := os.Lstat(link)
			if err != nil {
				t.Fatalf("Lstat link: %v", err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("city.toml symlink was replaced by a %v entry; rewrite must write through the link", info.Mode())
			}
			gotTarget, err := os.Readlink(link)
			if err != nil {
				t.Fatalf("Readlink: %v", err)
			}
			if gotTarget != linkTarget {
				t.Fatalf("link target = %q, want %q", gotTarget, linkTarget)
			}
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("ReadFile target: %v", err)
			}
			if !strings.Contains(string(data), `name = "renamed-city"`) {
				t.Fatalf("target content = %q, want updated workspace name written through the link", data)
			}
		})
	}
}

// Regression test for ga-lurp5d: a gc binary that does not recognize keys in
// the on-disk city.toml must refuse the rewrite instead of silently dropping
// them (observed in prod: agent_defaults.provider and beads.bd_compatibility
// eaten by an older binary's round-trip).
func TestWriteCityAndRigSiteBindingsForEditRefusesToDropUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city.toml")
	original := []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nfuture_unknown_knob = \"keep-me\"\n")
	if err := os.WriteFile(cityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Workspace.Name = "renamed-city"

	err = WriteCityAndRigSiteBindingsForEdit(fsys.OSFS{}, cityPath, cfg)
	if err == nil {
		t.Fatal("WriteCityAndRigSiteBindingsForEdit succeeded, want refusal for unknown key")
	}
	if !strings.Contains(err.Error(), "beads.future_unknown_knob") {
		t.Fatalf("error = %v, want mention of beads.future_unknown_knob", err)
	}
	current, readErr := os.ReadFile(cityPath)
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if !bytes.Equal(current, original) {
		t.Fatalf("city.toml was rewritten despite refusal:\n%s", current)
	}
}

// Regression for PR #3428 review: the byte-preserving append path must NOT
// inherit the full-rewrite unknown-key refusal. Appending a [[rigs]] block
// leaves the existing bytes untouched, so a city.toml carrying keys this gc
// binary does not recognize (e.g. an older gc editing a newer gc's city) is
// preserved verbatim instead of being rejected.
func TestAppendRigAndWriteSiteBindingsForEditPreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city.toml")
	original := []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nfuture_unknown_knob = \"keep-me\"\n")
	if err := os.WriteFile(cityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	newRig := Rig{Name: "frontend", Path: "/srv/frontend"}
	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Rigs:      []Rig{newRig},
	}
	if err := AppendRigAndWriteSiteBindingsForEdit(fsys.OSFS{}, cityPath, cfg, newRig); err != nil {
		t.Fatalf("AppendRigAndWriteSiteBindingsForEdit: %v", err)
	}

	rewritten, err := os.ReadFile(cityPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(rewritten), `future_unknown_knob = "keep-me"`) {
		t.Fatalf("append dropped the unknown key:\n%s", rewritten)
	}
	if !strings.Contains(string(rewritten), `name = "frontend"`) {
		t.Fatalf("append did not add the rig block:\n%s", rewritten)
	}
}

// The append path keeps the protective half of the rewrite guard: if the
// on-disk city.toml no longer parses as TOML, appending would compound the
// corruption, so it is refused and the file is left untouched. (The caller
// loaded config from this same path before mutating it.)
func TestAppendRigAndWriteSiteBindingsForEditRefusesUnparsableCity(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city.toml")
	original := []byte("this is = = not valid toml [[[\n")
	if err := os.WriteFile(cityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	newRig := Rig{Name: "frontend", Path: "/srv/frontend"}
	cfg := &City{Rigs: []Rig{newRig}}
	err := AppendRigAndWriteSiteBindingsForEdit(fsys.OSFS{}, cityPath, cfg, newRig)
	if err == nil {
		t.Fatal("AppendRigAndWriteSiteBindingsForEdit succeeded, want refusal for unparsable city.toml")
	}
	if !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("error = %v, want parse-refusal guidance", err)
	}
	current, readErr := os.ReadFile(cityPath)
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if !bytes.Equal(current, original) {
		t.Fatalf("city.toml was modified despite refusal:\n%s", current)
	}
}

// The funnel's rollback must also preserve a symlinked city.toml: when the
// site-binding write fails after city.toml was rewritten, the snapshot
// restore writes back through the resolved target instead of replacing the
// link with a regular file.
func TestWriteCityAndRigSiteBindingsForEditRestoresSymlinkOnBindingFailure(t *testing.T) {
	dir := t.TempDir()
	checkoutDir := filepath.Join(dir, "checkout")
	if err := os.MkdirAll(checkoutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(checkoutDir, "city.toml")
	original := []byte("[workspace]\nname = \"test-city\"\n")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatal(err)
	}
	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cityDir, "city.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// Force persistRigSiteBindings to fail after the city.toml write: .gc
	// exists but is read-only, so the site-binding temp file cannot be
	// created there.
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gcDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gcDir, 0o755) })

	cfg := &City{
		Workspace: Workspace{Name: "renamed-city"},
		Rigs:      []Rig{{Name: "frontend", Path: filepath.Join(dir, "frontend")}},
	}
	err := WriteCityAndRigSiteBindingsForEdit(fsys.OSFS{}, link, cfg)
	if err == nil {
		t.Fatal("WriteCityAndRigSiteBindingsForEdit succeeded, want site-binding write failure")
	}
	if !strings.Contains(err.Error(), "restored city.toml") {
		t.Fatalf("error = %v, want restore confirmation", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("city.toml symlink was replaced by a %v entry; rollback must restore through the link", info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if !bytes.Equal(data, original) {
		t.Fatalf("target content = %q, want restored original %q", data, original)
	}
}

// The funnel's rollback must also preserve a symlinked .gc/site.toml: the
// snapshot records the resolved target persistSiteBinding writes to, so a
// site-binding failure after the city.toml rewrite restores the target's
// contents instead of renaming over (or removing) the link itself.
func TestWriteCityAndRigSiteBindingsForEditPreservesSiteTomlSymlinkOnBindingFailure(t *testing.T) {
	originalCity := []byte("[workspace]\nname = \"test-city\"\n")

	// setupCity creates a writable city dir with a regular city.toml and a
	// writable .gc dir, and links .gc/site.toml to linkTarget.
	setupCity := func(t *testing.T, dir, linkTarget string) (cityPath, link string) {
		t.Helper()
		cityDir := filepath.Join(dir, "city")
		gcDir := filepath.Join(cityDir, ".gc")
		if err := os.MkdirAll(gcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		cityPath = filepath.Join(cityDir, "city.toml")
		if err := os.WriteFile(cityPath, originalCity, 0o644); err != nil {
			t.Fatal(err)
		}
		link = filepath.Join(gcDir, "site.toml")
		if err := os.Symlink(linkTarget, link); err != nil {
			t.Fatal(err)
		}
		return cityPath, link
	}

	editCfg := func(dir string) *City {
		return &City{
			Workspace: Workspace{Name: "renamed-city"},
			Rigs:      []Rig{{Name: "frontend", Path: filepath.Join(dir, "frontend")}},
		}
	}

	assertLinkAndCityRestored := func(t *testing.T, link, linkTarget, cityPath string) {
		t.Helper()
		info, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("Lstat link: %v", err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf(".gc/site.toml symlink was replaced by a %v entry; rollback must restore through the link", info.Mode())
		}
		gotTarget, err := os.Readlink(link)
		if err != nil {
			t.Fatalf("Readlink: %v", err)
		}
		if gotTarget != linkTarget {
			t.Fatalf("link target = %q, want %q", gotTarget, linkTarget)
		}
		cityData, err := os.ReadFile(cityPath)
		if err != nil {
			t.Fatalf("ReadFile city.toml: %v", err)
		}
		if !bytes.Equal(cityData, originalCity) {
			t.Fatalf("city.toml = %q, want restored original %q", cityData, originalCity)
		}
	}

	t.Run("write fails at read-only link target", func(t *testing.T) {
		dir := t.TempDir()
		checkoutDir := filepath.Join(dir, "checkout")
		if err := os.MkdirAll(checkoutDir, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(checkoutDir, "site.toml")
		originalSite := []byte("[[rig]]\nname = \"backend\"\npath = \"/srv/backend\"\n")
		if err := os.WriteFile(target, originalSite, 0o644); err != nil {
			t.Fatal(err)
		}
		// The link target's directory is read-only, so the forward
		// site-binding write fails there — while the link-side .gc stays
		// writable, which is exactly where a rollback at the unresolved
		// path would "succeed" by renaming over the link.
		if err := os.Chmod(checkoutDir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(checkoutDir, 0o755) })
		cityPath, link := setupCity(t, dir, target)

		err := WriteCityAndRigSiteBindingsForEdit(fsys.OSFS{}, cityPath, editCfg(dir))
		if err == nil {
			t.Fatal("WriteCityAndRigSiteBindingsForEdit succeeded, want site-binding write failure")
		}

		assertLinkAndCityRestored(t, link, target, cityPath)
		data, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("ReadFile target: %v", err)
		}
		if !bytes.Equal(data, originalSite) {
			t.Fatalf("target content = %q, want untouched original %q", data, originalSite)
		}
	})

	t.Run("load failure restores target through link", func(t *testing.T) {
		dir := t.TempDir()
		checkoutDir := filepath.Join(dir, "checkout")
		if err := os.MkdirAll(checkoutDir, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(checkoutDir, "site.toml")
		// Unparsable content makes persistRigSiteBindings fail after the
		// city.toml rewrite, so rollback must rewrite these exact bytes at
		// the resolved target, not rename over the link.
		originalSite := []byte("not toml [\n")
		if err := os.WriteFile(target, originalSite, 0o644); err != nil {
			t.Fatal(err)
		}
		cityPath, link := setupCity(t, dir, target)

		err := WriteCityAndRigSiteBindingsForEdit(fsys.OSFS{}, cityPath, editCfg(dir))
		if err == nil {
			t.Fatal("WriteCityAndRigSiteBindingsForEdit succeeded, want site-binding load failure")
		}
		if !strings.Contains(err.Error(), "restored city.toml") {
			t.Fatalf("error = %v, want restore confirmation", err)
		}

		assertLinkAndCityRestored(t, link, target, cityPath)
		data, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("ReadFile target: %v", err)
		}
		if !bytes.Equal(data, originalSite) {
			t.Fatalf("target content = %q, want restored original %q", data, originalSite)
		}
	})

	t.Run("dangling link is not removed by rollback", func(t *testing.T) {
		dir := t.TempDir()
		checkoutDir := filepath.Join(dir, "checkout")
		if err := os.MkdirAll(checkoutDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// The dangling target's parent cannot be created, so the forward
		// write fails; the !existed rollback branch must remove the
		// (missing) target, never the link itself.
		if err := os.Chmod(checkoutDir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(checkoutDir, 0o755) })
		target := filepath.Join(checkoutDir, "sub", "site.toml")
		cityPath, link := setupCity(t, dir, target)

		err := WriteCityAndRigSiteBindingsForEdit(fsys.OSFS{}, cityPath, editCfg(dir))
		if err == nil {
			t.Fatal("WriteCityAndRigSiteBindingsForEdit succeeded, want site-binding write failure")
		}
		if !strings.Contains(err.Error(), "restored city.toml") {
			t.Fatalf("error = %v, want restore confirmation", err)
		}

		assertLinkAndCityRestored(t, link, target, cityPath)
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatalf("Lstat target = %v, want not-exist after rollback", err)
		}
	})
}

// A brand-new city.toml (no existing file) must still be writable: the
// unknown-key guard only applies when there is existing content to lose.
func TestWriteCityAndRigSiteBindingsForEditAllowsFreshWrite(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city.toml")
	cfg := &City{Workspace: Workspace{Name: "test-city"}}

	if err := WriteCityAndRigSiteBindingsForEdit(fsys.OSFS{}, cityPath, cfg); err != nil {
		t.Fatalf("WriteCityAndRigSiteBindingsForEdit: %v", err)
	}
	data, err := os.ReadFile(cityPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), `name = "test-city"`) {
		t.Fatalf("city.toml = %q, want workspace name", data)
	}
}
