package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/gitcred"
	"github.com/gastownhall/gascity/internal/importsvc"
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
		t.Fatal("commit is nil; the bundled include must be locked")
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
	if v, ok := synced[refSource]; ok {
		t.Fatalf("#ref include joined the lock sync at %q; packman would lock the newest tag, not the ref", v)
	}
	if v := synced[bundled]; v != config.PublicGastownPackVersion {
		t.Fatalf("bundled include synced at %q, want the canonical pin", v)
	}
}

// TestResolveRigIncludeImportsRefusesCredentialInURL keeps gc import add's
// credential-in-URL refusal for every remote include, including a "#ref" one
// that takes no default version: nothing is resolved or locked, and the error
// never echoes the secret.
func TestResolveRigIncludeImportsRefusesCredentialInURL(t *testing.T) {
	cases := map[string]string{
		"plain": "https://user:hunter2@example.com/r.git",
		"#ref":  "https://user:hunter2@example.com/r.git#v1",
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
			calls := stubRigIncludeImportSeams(t, "1.4.0")
			input := []config.BoundImport{{Binding: "r", Import: config.Import{Source: source}}}
			_, _, err := resolveRigIncludeImports(cityPath, input)
			if !errors.Is(err, importsvc.ErrInvalidSource) {
				t.Fatalf("err = %v, want ErrInvalidSource", err)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("err %q leaks the embedded credential", err)
			}
			if got := calls.resolvedSources(); len(got) != 0 {
				t.Fatalf("version resolvers consulted for %v; a refused source must not be probed", got)
			}
			if got := calls.syncedImports(); got != nil {
				t.Fatalf("syncImports saw %v; a refused source must not be locked", got)
			}
		})
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

// rigIncludeRemoteRepo is a real file:// pack repository (packs/demo on
// branch main) for the unstubbed rig add --include tests.
type rigIncludeRemoteRepo struct {
	dir    string
	source string // file:// source of packs/demo
	revs   int
}

// newRigIncludeRemoteRepo creates the repository with no commits and points
// GC_HOME at an empty registry config, so resolution reads only its git tags.
func newRigIncludeRemoteRepo(t *testing.T) *rigIncludeRemoteRepo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "packs-repo")
	if err := os.MkdirAll(filepath.Join(dir, "packs", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "packs", "demo", "pack.toml"), []byte("[pack]\nname = \"demo\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registryVersionFixtureGit(t, dir, "init", "-q", "-b", "main")
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	writeEmptyRegistryConfig(t, home)
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")
	return &rigIncludeRemoteRepo{dir: dir, source: "file://" + dir + "//packs/demo"}
}

// commit records a new revision of the repository, tags it when tag is not
// empty, and returns its commit.
func (r *rigIncludeRemoteRepo) commit(t *testing.T, tag string) string {
	t.Helper()
	r.revs++
	rev := strconv.Itoa(r.revs)
	if err := os.WriteFile(filepath.Join(r.dir, "REVISION"), []byte(rev+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registryVersionFixtureGit(t, r.dir, "add", ".")
	registryVersionFixtureGit(t, r.dir, "commit", "-q", "-m", "revision "+rev)
	if tag != "" {
		registryVersionFixtureGit(t, r.dir, "tag", tag)
	}
	return registryVersionFixtureGit(t, r.dir, "rev-parse", "HEAD")
}

// newRigIncludeCity returns an empty schema-2 city.
func newRigIncludeCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
	return cityPath
}

// rigAddIncludes runs gc rig add for a fresh rig directory named rigName with
// the given --include tokens and fails the test unless it succeeds.
func rigAddIncludes(t *testing.T, cityPath, rigName string, includes ...string) {
	t.Helper()
	rigPath := filepath.Join(t.TempDir(), rigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, includes, "", "", "", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc rig add %s --include %v = %d, stderr:\n%s", rigName, includes, code, stderr.String())
	}
}

// rigDemoImport returns the import the named rig declares under "demo", the
// binding the fixture's packs/demo source derives.
func rigDemoImport(t *testing.T, cityPath, rigName string) config.Import {
	t.Helper()
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rig := range cfg.Rigs {
		if rig.Name != rigName {
			continue
		}
		imp, ok := rig.Imports["demo"]
		if !ok {
			t.Fatalf("rig %q has no import \"demo\": %+v", rigName, rig.Imports)
		}
		return imp
	}
	t.Fatalf("city has no rig %q", rigName)
	return config.Import{}
}

// readPacksLock returns the raw packs.lock bytes of cityPath.
func readPacksLock(t *testing.T, cityPath string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cityPath, packman.LockfileName))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertImportCheckOK fails the test unless gc import check reports a clean
// import state for cityPath.
func assertImportCheckOK(t *testing.T, cityPath string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := doImportCheck(cityPath, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "Import state OK") {
		t.Fatalf("gc import check = %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
}

// TestRigAddIncludeRemoteMatchesImportAdd is the parity guard: a non-bundled
// remote --include must write the same import gc import add --rig writes for
// the same source (the newest tag's default caret), lock it, and leave
// gc import check clean, so the two commands cannot drift.
func TestRigAddIncludeRemoteMatchesImportAdd(t *testing.T) {
	repo := newRigIncludeRemoteRepo(t)
	tagCommit := repo.commit(t, "v1.2.0")

	city1 := newRigIncludeCity(t)
	rigAddIncludes(t, city1, "myproj", repo.source)
	viaInclude := rigDemoImport(t, city1, "myproj")
	if viaInclude.Version != "^1.2" {
		t.Fatalf("rig add --include wrote version %q, want ^1.2", viaInclude.Version)
	}
	lock, err := packman.ReadLockfile(fsys.OSFS{}, city1)
	if err != nil {
		t.Fatal(err)
	}
	if got := lock.Packs[repo.source]; got.Commit != tagCommit {
		t.Fatalf("packs.lock entry = %+v, want the v1.2.0 commit %s", got, tagCommit)
	}
	assertImportCheckOK(t, city1)

	city2 := newRigIncludeCity(t)
	rigAddIncludes(t, city2, "myproj")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--city", city2, "--rig", "myproj", "import", "add", repo.source}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc import add --rig = %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	if viaImportAdd := rigDemoImport(t, city2, "myproj"); viaImportAdd != viaInclude {
		t.Fatalf("rig add --include wrote %+v, gc import add --rig wrote %+v; they must match", viaInclude, viaImportAdd)
	}
}

// TestRigAddIncludeJoinsCityPinForImportedSource is the shared-lock guard:
// packs.lock holds one entry per source, so a second rig including a remote
// source the city already imports must join the existing pin, even after the
// pack published a caret-incompatible release (semver) or a new commit
// (tagless), instead of failing the merged constraint or moving the pin.
func TestRigAddIncludeJoinsCityPinForImportedSource(t *testing.T) {
	cases := map[string]struct {
		tag, nextTag string
		want         func(pinned string) string
	}{
		"semver":  {tag: "v0.4.0", nextTag: "v0.5.0", want: func(string) string { return "^0.4" }},
		"tagless": {want: func(pinned string) string { return "sha:" + pinned }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := newRigIncludeRemoteRepo(t)
			pinned := repo.commit(t, tc.tag)
			cityPath := newRigIncludeCity(t)
			rigAddIncludes(t, cityPath, "alpha", repo.source)
			if got := rigDemoImport(t, cityPath, "alpha").Version; got != tc.want(pinned) {
				t.Fatalf("alpha import version = %q, want %q", got, tc.want(pinned))
			}
			repo.commit(t, tc.nextTag)
			before := readPacksLock(t, cityPath)

			rigAddIncludes(t, cityPath, "bravo", repo.source)
			if got := rigDemoImport(t, cityPath, "bravo").Version; got != tc.want(pinned) {
				t.Fatalf("bravo import version = %q, want the city's %q", got, tc.want(pinned))
			}
			if after := readPacksLock(t, cityPath); !bytes.Equal(after, before) {
				t.Fatalf("packs.lock moved for the existing importer:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			assertImportCheckOK(t, cityPath)
		})
	}
}

// TestRigAddIncludeKeepsLockedPinOfVersionlessImport guards the shared pin when
// the city's existing import of the source declares no version (as earlier
// rig add --include wrote, or a hand-written import): the new rig takes the
// locked version's caret, so packs.lock stays byte-identical instead of moving
// every importer to the newest release. Moving a pin stays gc import upgrade.
func TestRigAddIncludeKeepsLockedPinOfVersionlessImport(t *testing.T) {
	repo := newRigIncludeRemoteRepo(t)
	pinned := repo.commit(t, "v0.4.0")
	cityPath := newRigIncludeCity(t)
	packPath := filepath.Join(cityPath, "pack.toml")
	pack, err := os.ReadFile(packPath)
	if err != nil {
		t.Fatal(err)
	}
	pack = append(pack, fmt.Sprintf("\n[imports.demo]\nsource = %q\n", repo.source)...)
	if err := os.WriteFile(packPath, pack, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doImportInstall(cityPath, &stdout, &stderr); code != 0 {
		t.Fatalf("gc import install = %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	lock, err := packman.ReadLockfile(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := lock.Packs[repo.source]; got.Version != "0.4.0" || got.Commit != pinned {
		t.Fatalf("installed packs.lock entry = %+v, want 0.4.0 at %s", got, pinned)
	}
	repo.commit(t, "v0.5.0")
	before := readPacksLock(t, cityPath)

	rigAddIncludes(t, cityPath, "bravo", repo.source)
	if got := rigDemoImport(t, cityPath, "bravo").Version; got != "^0.4" {
		t.Fatalf("bravo import version = %q, want the locked 0.4.0's ^0.4", got)
	}
	if after := readPacksLock(t, cityPath); !bytes.Equal(after, before) {
		t.Fatalf("packs.lock moved for the version-less importer:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	assertImportCheckOK(t, cityPath)
}

// TestRigAddIncludeRefSourceStaysOutOfLock guards the "#ref" exception:
// packman resolves a source's newest tag and ignores its ref, while packs.lock
// wins over the ref when the city loads, so locking "<src>#v0.4.0" would load
// v0.5.0. The include is written as given and kept out of packs.lock.
func TestRigAddIncludeRefSourceStaysOutOfLock(t *testing.T) {
	repo := newRigIncludeRemoteRepo(t)
	repo.commit(t, "v0.4.0")
	repo.commit(t, "v0.5.0")
	cityPath := newRigIncludeCity(t)
	refSource := repo.source + "#v0.4.0"

	rigAddIncludes(t, cityPath, "alpha", "demo="+refSource)
	if got := rigDemoImport(t, cityPath, "alpha"); got != (config.Import{Source: refSource}) {
		t.Fatalf("alpha import = %+v, want %q as given with no version", got, refSource)
	}
	lock, err := packman.ReadLockfile(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Packs) != 0 {
		t.Fatalf("packs.lock = %+v, want no entry for the #v0.4.0 include; the ref would be ignored at load", lock.Packs)
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
