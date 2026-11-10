package configedit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
)

func cityWithGuardedRig(betaPath string) string {
	return `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"

[[rigs]]
name = "beta"
path = "` + betaPath + `"
federation = "rig"
namespace = "exclusive"
`
}

func TestEdit_KeepsRigFederation(t *testing.T) {
	dir := t.TempDir()
	betaPath := filepath.Join(t.TempDir(), "beta")
	if err := os.MkdirAll(betaPath, 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeTOML(t, dir, cityWithGuardedRig(betaPath))
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.Edit(func(cfg *config.City) error {
		return configedit.SetAgentSuspended(cfg, "mayor", true)
	}); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Rigs) != 1 {
		t.Fatalf("rigs = %+v, want one rig", cfg.Rigs)
	}
	if got := cfg.Rigs[0]; got.Federation != config.RigFederationRig || got.Namespace != config.RigNamespaceExclusive {
		t.Fatalf("rig after edit = %+v, want federation rig, namespace exclusive", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`federation = "rig"`, `namespace = "exclusive"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("city.toml after edit lacks %s:\n%s", want, data)
		}
	}
}

func TestEdit_RefusesGuardedRigSharingADirectory(t *testing.T) {
	dir := t.TempDir()
	betaPath := filepath.Join(t.TempDir(), "beta")
	if err := os.MkdirAll(betaPath, 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeTOML(t, dir, cityWithGuardedRig(betaPath))
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.Edit(func(cfg *config.City) error {
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: "alpha", Path: betaPath, Prefix: "al"})
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), `rig "beta": federation = "rig" needs its own store directory`) {
		t.Fatalf("Edit error = %v, want the shared-directory refusal", err)
	}
}

func TestEdit_RefusesInvalidRigFederationValue(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.Edit(func(cfg *config.City) error {
		cfg.Rigs[0].Federation = "bogus"
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), `rig "my-rig": federation "bogus" is not one of "city", "rig"`) {
		t.Fatalf("Edit error = %v, want the federation enum refusal", err)
	}
}
