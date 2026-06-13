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
