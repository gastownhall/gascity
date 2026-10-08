package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/gitcred"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/remotesource"
)

// rigIncludeSeamCalls records what the stubbed import seams saw.
type rigIncludeSeamCalls struct {
	mu       sync.Mutex
	resolved []string                 // sources passed to the version/head resolvers
	synced   map[string]config.Import // the last import set handed to syncImports
}

func (c *rigIncludeSeamCalls) resolvedSources() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.resolved...)
}

func (c *rigIncludeSeamCalls) syncedImports() map[string]config.Import {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.synced
}

// rigIncludeStubCommit is the commit every stubbed import seam reports.
const rigIncludeStubCommit = "c0ffee"

// stubRigIncludeImportSeams replaces the network-touching import seams for a
// rig-add test: no registry release, every tag lookup answers version, a HEAD
// probe answers rigIncludeStubCommit, syncImports locks every remote import
// at that commit, and installLockedImports is a no-op. The originals are
// restored on cleanup.
func stubRigIncludeImportSeams(t *testing.T, version string) *rigIncludeSeamCalls {
	t.Helper()
	commit := rigIncludeStubCommit
	calls := &rigIncludeSeamCalls{}
	prevRegistry := resolveImportRegistryRelease
	prevVersion := resolveImportVersion
	prevConstraint := defaultImportConstraint
	prevHead := resolveImportHeadCommit
	prevSync := syncImports
	prevInstall := installLockedImports
	t.Cleanup(func() {
		resolveImportRegistryRelease = prevRegistry
		resolveImportVersion = prevVersion
		defaultImportConstraint = prevConstraint
		resolveImportHeadCommit = prevHead
		syncImports = prevSync
		installLockedImports = prevInstall
	})
	resolveImportRegistryRelease = func(string, string) (packman.RegistryRelease, bool, error, error) {
		return packman.RegistryRelease{}, false, nil, nil
	}
	resolveImportVersion = func(_, source, _ string) (packman.ResolvedVersion, error) {
		calls.mu.Lock()
		calls.resolved = append(calls.resolved, source)
		calls.mu.Unlock()
		return packman.ResolvedVersion{Version: version, Commit: commit}, nil
	}
	defaultImportConstraint = packman.DefaultConstraint
	resolveImportHeadCommit = func(_, source string) (string, error) {
		calls.mu.Lock()
		calls.resolved = append(calls.resolved, source)
		calls.mu.Unlock()
		return commit, nil
	}
	syncImports = func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		calls.mu.Lock()
		calls.synced = imports
		calls.mu.Unlock()
		lock := &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}
		for _, imp := range imports {
			if remotesource.IsRemote(imp.Source) {
				lock.Packs[imp.Source] = packman.LockedPack{Version: version, Commit: commit}
			}
		}
		return lock, nil
	}
	installLockedImports = func(string) (*packman.Lockfile, error) {
		return &packman.Lockfile{}, nil
	}
	return calls
}

func syncedSources(imports map[string]config.Import) map[string]string {
	out := make(map[string]string, len(imports))
	for _, imp := range imports {
		out[imp.Source] = imp.Version
	}
	return out
}

func TestResolveRigIncludeImportsWritesDefaultConstraintAndDefersLock(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
	calls := stubRigIncludeImportSeams(t, "1.4.0")

	const source = "https://github.com/example/tools.git"
	input := []config.BoundImport{{Binding: "tools", Import: config.Import{Source: source}}}
	resolved, commit, err := resolveRigIncludeImports(cityPath, input)
	if err != nil {
		t.Fatalf("resolveRigIncludeImports: %v", err)
	}
	if input[0].Import.Version != "" {
		t.Fatalf("input slice was mutated: %+v", input)
	}
	want := []config.BoundImport{{Binding: "tools", Import: config.Import{Source: source, Version: "^1.4"}}}
	if !reflect.DeepEqual(resolved, want) {
		t.Fatalf("resolved = %+v, want %+v", resolved, want)
	}
	if got := syncedSources(calls.syncedImports())[source]; got != "^1.4" {
		t.Fatalf("syncImports saw %s at %q, want ^1.4", source, got)
	}
	if commit == nil {
		t.Fatal("commit is nil for a remote include; packs.lock would never be written")
	}
	lockPath := filepath.Join(cityPath, packman.LockfileName)
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("packs.lock exists before commit (err=%v); the lock write must be deferred", err)
	}
	if err := commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	lock, err := packman.ReadLockfile(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := lock.Packs[source]; got.Commit != rigIncludeStubCommit {
		t.Fatalf("packs.lock entry = %+v, want commit c0ffee", got)
	}
}

func TestResolveRigIncludeImportsLeavesBundledLocalAndRefSources(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
	calls := stubRigIncludeImportSeams(t, "1.4.0")

	bundled, ok := builtinpacks.CanonicalImportSource("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}
	const refSource = "https://example.com/r.git#v1"
	input := []config.BoundImport{
		{Binding: "gastown", Import: config.Import{Source: bundled}},
		{Binding: "x", Import: config.Import{Source: "./packs/x"}},
		{Binding: "r", Import: config.Import{Source: refSource}},
	}
	resolved, commit, err := resolveRigIncludeImports(cityPath, input)
	if err != nil {
		t.Fatalf("resolveRigIncludeImports: %v", err)
	}
	if commit == nil {
		t.Fatal("commit is nil; bundled and remote includes must be locked")
	}
	want := []config.BoundImport{
		{Binding: "gastown", Import: config.Import{Source: bundled, Version: config.PublicGastownPackVersion}},
		{Binding: "x", Import: config.Import{Source: "./packs/x"}},
		{Binding: "r", Import: config.Import{Source: refSource}},
	}
	if !reflect.DeepEqual(resolved, want) {
		t.Fatalf("resolved = %+v, want %+v", resolved, want)
	}
	if got := calls.resolvedSources(); len(got) != 0 {
		t.Fatalf("version resolvers consulted for %v; bundled, local, and #ref sources must not resolve", got)
	}
	synced := syncedSources(calls.syncedImports())
	if _, ok := synced["./packs/x"]; ok {
		t.Fatalf("local include joined the lock sync: %v", synced)
	}
	if v, ok := synced[refSource]; !ok || v != "" {
		t.Fatalf("#ref include sync entry = %q (present=%v), want version-less and present", v, ok)
	}
	if v := synced[bundled]; v != config.PublicGastownPackVersion {
		t.Fatalf("bundled include synced at %q, want the canonical pin", v)
	}
}

func TestRigAddIncludeResolutionFailureLeavesCityUntouched(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
	stubRigIncludeImportSeams(t, "1.4.0")
	resolveImportVersion = func(string, string, string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{}, errors.New("ls-remote boom")
	}
	syncImports = func(string, map[string]config.Import, packman.InstallMode) (*packman.Lockfile, error) {
		t.Error("syncImports ran after version resolution failed")
		return nil, errors.New("unreachable")
	}
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	tomlPath := filepath.Join(cityPath, "city.toml")
	before, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"https://github.com/example/tools.git"}, "", "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigAdd = %d, want 1; stdout:\n%s", code, stdout.String())
	}
	for _, want := range []string{"resolving rig imports", "boom"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
	assertRigAddLeftCityUntouched(t, cityPath, before, rigPath)
}

func TestRigAddIncludeBindingOverrideBundled(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")
	rigPath := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"tools=gastown"}, "", "", "", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("doRigAdd = %d, stderr:\n%s", code, stderr.String())
	}
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	bundled, ok := builtinpacks.CanonicalImportSource("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}
	imports := cfg.Rigs[0].Imports
	want := config.Import{Source: bundled, Version: config.PublicGastownPackVersion}
	if got := imports["tools"]; got != want {
		t.Fatalf("rig imports[tools] = %+v, want %+v", got, want)
	}
	if _, ok := imports["gastown"]; ok {
		t.Fatalf("rig also got a derived gastown binding: %+v", imports)
	}
}

func TestRigAddIncludeBindingCollisionFails(t *testing.T) {
	cases := map[string]struct {
		includes []string
		binding  string
	}{
		"two explicit bindings": {includes: []string{"tools=gastown", "tools=packs/x"}, binding: `"tools"`},
		"explicit vs derived":   {includes: []string{"gastown=packs/x", "gastown"}, binding: `"gastown"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
			writeLocalCityPacks(t, cityPath, "packs/x")
			t.Setenv("GC_DOLT", "skip")
			t.Setenv("GC_BEADS", "bd")
			before, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
			if err != nil {
				t.Fatal(err)
			}
			rigPath := filepath.Join(t.TempDir(), "myproj")
			if err := os.MkdirAll(rigPath, 0o755); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, tc.includes, "", "", "", false, false, &stdout, &stderr); code != 1 {
				t.Fatalf("doRigAdd = %d, want 1; stdout:\n%s", code, stdout.String())
			}
			if !strings.Contains(stderr.String(), "--include binding "+tc.binding) {
				t.Fatalf("stderr does not name binding %s:\n%s", tc.binding, stderr.String())
			}
			assertRigAddLeftCityUntouched(t, cityPath, before, rigPath)
		})
	}
}

// assertRigAddLeftCityUntouched checks that a failed rig add wrote nothing:
// city.toml is byte-identical, packs.lock is absent, and the rig has no store.
func assertRigAddLeftCityUntouched(t *testing.T, cityPath string, before []byte, rigPath string) {
	t.Helper()
	after, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("city.toml mutated by a failed rig add:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(cityPath, packman.LockfileName)); !os.IsNotExist(err) {
		t.Fatalf("packs.lock written by a failed rig add (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(rigPath, ".beads")); !os.IsNotExist(err) {
		t.Fatalf("rig .beads created by a failed rig add (err=%v)", err)
	}
}

// TestRigAddIncludeRemoteMatchesImportAdd is the parity guard: a non-bundled
// remote --include must land as the same rig import gc import add --rig
// writes (version constraint and packs.lock entry), so gc import check is
// clean right after the add. It uses a local file:// git fixture, so it needs
// no network.
func TestRigAddIncludeRemoteMatchesImportAdd(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "packs-repo")
	if err := os.MkdirAll(filepath.Join(repo, "packs", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "packs", "demo", "pack.toml"), []byte("[pack]\nname = \"demo\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registryVersionFixtureGit(t, repo, "init", "-q", "-b", "main")
	registryVersionFixtureGit(t, repo, "add", ".")
	registryVersionFixtureGit(t, repo, "commit", "-q", "-m", "demo 1.2.0")
	registryVersionFixtureGit(t, repo, "tag", "v1.2.0")
	tagCommit := registryVersionFixtureGit(t, repo, "rev-parse", "HEAD")
	source := "file://" + repo + "//packs/demo"

	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	writeEmptyRegistryConfig(t, home)
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	newCity := func() (string, string) {
		cityPath := t.TempDir()
		writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
		rigPath := filepath.Join(t.TempDir(), "myproj")
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatal(err)
		}
		return cityPath, rigPath
	}
	loadRigImport := func(cityPath, binding string) config.Import {
		t.Helper()
		cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
		if err != nil {
			t.Fatal(err)
		}
		imp, ok := cfg.Rigs[0].Imports[binding]
		if !ok {
			t.Fatalf("rig has no import %q: %+v", binding, cfg.Rigs[0].Imports)
		}
		return imp
	}

	city1, rig1 := newCity()
	var stdout, stderr bytes.Buffer
	if code := doRigAdd(fsys.OSFS{}, city1, rig1, []string{source}, "", "", "", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("rig add --include = %d, stderr:\n%s", code, stderr.String())
	}
	viaInclude := loadRigImport(city1, "demo")
	if viaInclude.Version != "^1.2" {
		t.Fatalf("rig add --include wrote version %q, want ^1.2", viaInclude.Version)
	}
	lock, err := packman.ReadLockfile(fsys.OSFS{}, city1)
	if err != nil {
		t.Fatal(err)
	}
	if got := lock.Packs[source]; got.Commit != tagCommit {
		t.Fatalf("packs.lock entry = %+v, want the v1.2.0 commit %s", got, tagCommit)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doImportCheck(city1, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "Import state OK") {
		t.Fatalf("gc import check after rig add = %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}

	city2, rig2 := newCity()
	stdout.Reset()
	stderr.Reset()
	if code := doRigAdd(fsys.OSFS{}, city2, rig2, nil, "", "", "", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("rig add = %d, stderr:\n%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--city", city2, "--rig", filepath.Base(rig2), "import", "add", source}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc import add --rig = %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	if viaImportAdd := loadRigImport(city2, "demo"); viaImportAdd != viaInclude {
		t.Fatalf("rig add --include wrote %+v, gc import add --rig wrote %+v; they must match", viaInclude, viaImportAdd)
	}
}

// TestRigAddReAddSameRemoteIncludeDoesNotWarn guards idempotent re-runs: a
// fresh add writes the resolved constraint for a remote include, so an
// identical re-add must compare equal to the stored import instead of
// warning that the --include was ignored. A different source still warns.
func TestRigAddReAddSameRemoteIncludeDoesNotWarn(t *testing.T) {
	cases := map[string]struct {
		reAddInclude string
		wantWarn     bool
	}{
		"same source":      {reAddInclude: "https://github.com/example/tools.git", wantWarn: false},
		"different source": {reAddInclude: "tools=https://github.com/example/other.git", wantWarn: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
			stubRigIncludeImportSeams(t, "1.4.0")
			t.Setenv("GC_DOLT", "skip")
			t.Setenv("GC_BEADS", "bd")
			rigPath := filepath.Join(t.TempDir(), "myproj")
			if err := os.MkdirAll(rigPath, 0o755); err != nil {
				t.Fatal(err)
			}
			const source = "https://github.com/example/tools.git"
			var stdout, stderr bytes.Buffer
			if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{source}, "", "", "", false, false, &stdout, &stderr); code != 0 {
				t.Fatalf("fresh doRigAdd = %d, stderr:\n%s", code, stderr.String())
			}
			stdout.Reset()
			stderr.Reset()
			if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{tc.reAddInclude}, "", "", "", false, false, &stdout, &stderr); code != 0 {
				t.Fatalf("re-add doRigAdd = %d, stderr:\n%s", code, stderr.String())
			}
			warned := strings.Contains(stderr.String(), "ignored")
			if warned != tc.wantWarn {
				t.Fatalf("re-add warned=%v, want %v; stderr:\n%s", warned, tc.wantWarn, stderr.String())
			}
		})
	}
}

// TestRigAddIncludeAuthFailurePrintsCredentialHint keeps the gc import add
// guidance: a private remote that rejects the clone gets the same
// "gc import credential add" hint from rig add.
func TestRigAddIncludeAuthFailurePrintsCredentialHint(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
	stubRigIncludeImportSeams(t, "1.4.0")
	resolveImportVersion = func(string, string, string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{}, &gitcred.AuthError{Host: "github.com", OrgPrefix: "github.com/example", Repo: "https://github.com/example/tools.git"}
	}
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")
	rigPath := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"https://github.com/example/tools.git"}, "", "", "", false, false, &stdout, &stderr); code != 1 {
		t.Fatalf("doRigAdd = %d, want 1; stdout:\n%s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "gc import credential add github.com/example") {
		t.Fatalf("stderr missing credential hint:\n%s", stderr.String())
	}
}
