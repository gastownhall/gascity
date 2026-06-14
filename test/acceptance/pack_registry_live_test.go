//go:build acceptance_a

package acceptance_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/deps"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/packregistry"
	"github.com/gastownhall/gascity/internal/remotesource"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestPackRegistryMainIsPreRegisteredOnVanillaInstall(t *testing.T) {
	env := newIsolatedAcceptanceEnv(t)

	out, err := helpers.RunGC(env, "", "pack", "registry", "list")
	if err != nil {
		t.Fatalf("gc pack registry list failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, packregistry.DefaultRegistryName) || !strings.Contains(out, packregistry.DefaultRegistrySource) {
		t.Fatalf("default registry not listed as main:\n%s", out)
	}
}

func TestPackRegistryLiveImportsEveryCatalogPack(t *testing.T) {
	source := strings.TrimSpace(os.Getenv("GC_TEST_GASCITY_PACKS_REGISTRY"))
	if source == "" {
		t.Skip("set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live import smoke test")
	}
	env := newIsolatedAcceptanceEnv(t)
	var out string

	if source == packregistry.DefaultRegistryName {
		var err error
		out, err = helpers.RunGC(env, "", "pack", "registry", "refresh", packregistry.DefaultRegistryName)
		if err != nil {
			t.Fatalf("gc pack registry refresh main failed: %v\n%s", err, out)
		}
	} else {
		var err error
		out, err = helpers.RunGC(env, "", "pack", "registry", "remove", packregistry.DefaultRegistryName)
		if err != nil {
			t.Fatalf("gc pack registry remove main failed: %v\n%s", err, out)
		}
		out, err = helpers.RunGC(env, "", "pack", "registry", "add", packregistry.DefaultRegistryName, source)
		if err != nil {
			t.Fatalf("gc pack registry add main failed: %v\n%s", err, out)
		}
	}

	cfg, err := packregistry.LoadConfig(env.Get("GC_HOME"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	regs, err := selectAcceptanceRegistries(cfg.Registries, "main")
	if err != nil {
		t.Fatal(err)
	}
	catalog, _, err := packregistry.ReadCachedRegistryCatalog(env.Get("GC_HOME"), regs[0])
	if err != nil {
		t.Fatalf("ReadCachedRegistryCatalog(main): %v", err)
	}
	if len(catalog.Packs) == 0 {
		t.Fatal("main registry catalog has no packs")
	}

	c := helpers.NewCity(t, env)
	// Builtin packs compose only through explicit pinned imports; the
	// gastown catalog pack's formulas extend core recipes (mol-polecat-base).
	c.WriteConfig("[workspace]\nname = \"pack-registry-smoke\"\n")
	coreSource, _ := builtinpacks.Source("core")
	bdSource, _ := builtinpacks.Source("bd")
	c.AppendToPack("[pack]\nname = \"pack-registry-smoke\"\nschema = 1\n" +
		"\n[imports.core]\nsource = \"" + coreSource + "\"\nversion = \"" + config.BundledPackImportVersion + "\"\n" +
		"\n[imports.bd]\nsource = \"" + bdSource + "\"\nversion = \"" + config.BundledPackImportVersion + "\"\n")
	type expectedPack struct {
		Name    string
		Source  string
		Version string
		Commit  string
	}
	var expected []expectedPack
	for _, pack := range catalog.Packs {
		release, ok := latestAcceptanceRelease(pack)
		if !ok {
			t.Fatalf("registry pack %q has no active release", pack.Name)
		}
		binding := strings.ReplaceAll(pack.Name, "/", "-")
		version := "sha:" + release.Commit
		out, err := c.GC("import", "add", pack.Source, "--name", binding, "--version", version)
		if err != nil {
			t.Fatalf("gc import add %s failed: %v\n%s", pack.Name, err, out)
		}
		expected = append(expected, expectedPack{
			Name:    pack.Name,
			Source:  pack.Source,
			Version: version,
			Commit:  release.Commit,
		})
	}

	packToml := c.ReadFile("pack.toml")
	for _, pack := range expected {
		binding := strings.ReplaceAll(pack.Name, "/", "-")
		if !strings.Contains(packToml, fmt.Sprintf("[imports.%s]", binding)) &&
			!strings.Contains(packToml, fmt.Sprintf("[imports.%s]", strconv.Quote(binding))) {
			t.Fatalf("pack.toml missing import binding for %s:\n%s", pack.Name, packToml)
		}
	}

	out, err = c.GC("import", "install")
	if err != nil {
		t.Fatalf("gc import install failed: %v\n%s", err, out)
	}
	out, err = c.GC("import", "check")
	if err != nil {
		t.Fatalf("gc import check failed: %v\n%s", err, out)
	}
	out, err = c.GC("config", "show", "--validate")
	if err != nil {
		t.Fatalf("gc config show --validate failed: %v\n%s", err, out)
	}

	lock, err := packman.ReadLockfile(fsys.OSFS{}, c.Dir)
	if err != nil {
		t.Fatalf("ReadLockfile: %v", err)
	}
	for _, pack := range expected {
		locked, ok := lock.Packs[pack.Source]
		if !ok {
			t.Fatalf("packs.lock missing source for registry pack %s (%s)", pack.Name, pack.Source)
		}
		if locked.Version != pack.Version || locked.Commit != pack.Commit {
			t.Fatalf("packs.lock entry for %s = version %q commit %q, want %q %q", pack.Name, locked.Version, locked.Commit, pack.Version, pack.Commit)
		}
		cachePath, err := packman.RepoCachePath(pack.Source, pack.Commit)
		if err != nil {
			t.Fatalf("RepoCachePath(%s): %v", pack.Name, err)
		}
		packPath := filepath.Join(cachePath, remotesource.Parse(pack.Source).Subpath, "pack.toml")
		if _, err := os.Stat(packPath); err != nil {
			t.Fatalf("cached pack %s missing pack.toml at %s: %v", pack.Name, packPath, err)
		}
	}
}

func newIsolatedAcceptanceEnv(t *testing.T) *helpers.Env {
	t.Helper()
	root := helpers.TempDir(t)
	gcHome := filepath.Join(root, "gc-home")
	runtimeDir := filepath.Join(root, "runtime")
	for _, dir := range []string{gcHome, runtimeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	if err := helpers.WriteSupervisorConfig(gcHome); err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	return helpers.NewEnv(testEnv.Get("GC_ACCEPTANCE_GC_BIN"), gcHome, runtimeDir)
}

func latestAcceptanceRelease(pack packregistry.CatalogPack) (packregistry.CatalogRelease, bool) {
	var latest packregistry.CatalogRelease
	ok := false
	for _, release := range pack.Releases {
		if release.Withdrawn {
			continue
		}
		if !ok || deps.CompareVersions(latest.Version, release.Version) < 0 {
			latest = release
			ok = true
		}
	}
	return latest, ok
}

func selectAcceptanceRegistries(regs []packregistry.Registry, name string) ([]packregistry.Registry, error) {
	for _, reg := range regs {
		if reg.Name == name {
			return []packregistry.Registry{reg}, nil
		}
	}
	return nil, fmt.Errorf("registry %q is not configured", name)
}
