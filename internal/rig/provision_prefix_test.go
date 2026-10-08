package rig

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func writeBeadsConfig(t *testing.T, fs *fsys.Fake, rigPath, content string) {
	t.Helper()
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := fs.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ReadBeadsPrefix must return the store's prefix verbatim: Beads matches
// prefixes case-sensitively, so a lowercased read would fork the id series of
// a mixed-case store on adopt.
func TestReadBeadsPrefixPreservesCase(t *testing.T) {
	fs := fsys.NewFake()
	writeBeadsConfig(t, fs, "/rig", "backend: dolt\nissue_prefix: KitFlowApp\nissue-prefix: KitFlowApp\n")
	got, ok := ReadBeadsPrefix(fs, "/rig")
	if !ok || got != "KitFlowApp" {
		t.Fatalf("ReadBeadsPrefix() = (%q, %v), want (%q, true)", got, ok, "KitFlowApp")
	}
}

func TestResolveRigPrefix(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "hq", Prefix: "hq"}}
	tests := []struct {
		name          string
		req           ProvisionRequest
		adoptedPrefix string
		want          string
	}{
		{"adopt with --prefix naming the store keeps its mixed case", ProvisionRequest{Name: "contentbuild", Adopt: true, Prefix: "KitFlowApp"}, "KitFlowApp", "KitFlowApp"},
		{"adopt with --prefix differing only by case keeps the store's", ProvisionRequest{Name: "contentbuild", Adopt: true, Prefix: "kitflowapp"}, "KitFlowApp", "KitFlowApp"},
		{"adopt without --prefix still derives from the name (validate reports the mismatch)", ProvisionRequest{Name: "content-build", Adopt: true}, "KitFlowApp", "cb"},
		{"adopt with a different --prefix resolves the lowercased override", ProvisionRequest{Name: "contentbuild", Adopt: true, Prefix: "Other"}, "KitFlowApp", "other"},
		{"adopt with no store prefix falls back to the lowercased override", ProvisionRequest{Name: "contentbuild", Adopt: true, Prefix: "Mixed"}, "", "mixed"},
		{"fresh add lowercases --prefix as before", ProvisionRequest{Name: "contentbuild", Prefix: "Mixed"}, "", "mixed"},
		{"fresh add derives from the name as before", ProvisionRequest{Name: "content-build"}, "", "cb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveRigPrefix(cfg, tt.req, tt.req.Name, false, nil, tt.adoptedPrefix)
			if err != nil {
				t.Fatalf("resolveRigPrefix() error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveRigPrefix() = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("adopt still rejects a collision with HQ", func(t *testing.T) {
		req := ProvisionRequest{Name: "contentbuild", Adopt: true, Prefix: "hq"}
		_, err := resolveRigPrefix(cfg, req, req.Name, false, nil, "HQ")
		if err == nil || !strings.Contains(err.Error(), "collides with HQ") {
			t.Fatalf("resolveRigPrefix() error = %v, want HQ collision", err)
		}
	})
}

// validateAdoptAndBeadsStore compares byte-for-byte on --adopt (a --prefix
// that differs from the store's is rejected with the existing text) and keeps
// the historical lowercased comparison and message text on re-add.
func TestValidateAdoptAndBeadsStorePrefixCase(t *testing.T) {
	fs := fsys.NewFake()
	writeBeadsConfig(t, fs, "/rig", "issue_prefix: KitFlowApp\n")
	if err := fs.WriteFile("/rig/.beads/metadata.json", []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := Deps{FS: fs}

	adopt := ProvisionRequest{Name: "contentbuild", Path: "/rig", Adopt: true}
	if err := validateAdoptAndBeadsStore(deps, adopt, "/rig", rigMutationPlan{prefix: "KitFlowApp"}); err != nil {
		t.Fatalf("adopt with the store's own prefix: %v", err)
	}
	err := validateAdoptAndBeadsStore(deps, adopt, "/rig", rigMutationPlan{prefix: "other"})
	if err == nil || !strings.Contains(err.Error(), `already has bead prefix "KitFlowApp" (requested "other")`) {
		t.Fatalf("adopt with a different prefix: error = %v", err)
	}

	// A re-add of a rig whose city.toml prefix was stored lowercased by an
	// older gc keeps the historical case-insensitive comparison.
	existing := &config.Rig{Name: "contentbuild", Path: "/rig", Prefix: "kitflowapp"}
	reAdd := ProvisionRequest{Name: "contentbuild", Path: "/rig"}
	if err := validateAdoptAndBeadsStore(deps, reAdd, "/rig", rigMutationPlan{prefix: "kitflowapp", reAdd: true, existingRig: existing}); err != nil {
		t.Fatalf("re-add keeps case-insensitive prefix comparison: %v", err)
	}
	err = validateAdoptAndBeadsStore(deps, reAdd, "/rig", rigMutationPlan{prefix: "other", reAdd: true, existingRig: existing})
	if err == nil || !strings.Contains(err.Error(), `has bead prefix "kitflowapp" but city.toml has "other"`) {
		t.Fatalf("re-add mismatch keeps the lowercased presentation: error = %v", err)
	}
}

// buildNextRigConfig stores the adopted prefix verbatim so EffectivePrefix (and
// EnsureCanonicalConfig on every later start) reproduce it instead of a
// name-derived or lowercased one.
func TestBuildNextRigConfigStoresAdoptedPrefix(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "hq", Prefix: "hq"}}
	deps := Deps{
		FS:       fsys.NewFake(),
		Cfg:      cfg,
		CityPath: "/city",
		ComposePacks: func(_ string, imports []config.BoundImport) ([]config.BoundImport, func() error, error) {
			return imports, func() error { return nil }, nil
		},
	}
	req := ProvisionRequest{Name: "contentbuild", Path: "/rig", Adopt: true, Prefix: "KitFlowApp"}
	next, _, _, err := buildNextRigConfig(deps, req, "/rig", "main", "KitFlowApp", false, false, -1, nil, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Rigs) != 1 || next.Rigs[0].Prefix != "KitFlowApp" || next.Rigs[0].EffectivePrefix() != "KitFlowApp" {
		t.Fatalf("stored rig = %+v, want prefix KitFlowApp", next.Rigs)
	}
}
