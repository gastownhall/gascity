package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// initBareRepo creates a bare git repo with a pack.toml file.
// Returns the bare repo path.
func initBareRepo(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()

	// Create a working repo to populate, then bare-clone it.
	workDir := filepath.Join(dir, "work")
	bareDir := filepath.Join(dir, name+".git")

	mustGit(t, "", "init", workDir)

	topoContent := `[pack]
name = "` + name + `"
version = "1.0.0"
schema = 1

[[agent]]
name = "worker"
`
	if err := os.MkdirAll(filepath.Join(workDir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "pack.toml"), []byte(topoContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "prompts", "worker.md"), []byte("you are a worker"), 0o644); err != nil {
		t.Fatal(err)
	}

	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "initial")
	mustGit(t, workDir, "clone", "--bare", workDir, bareDir)

	return bareDir
}

// initBareRepoWithTag creates a bare repo with an initial commit tagged.
func initBareRepoWithTag(t *testing.T, name, tag string) string {
	t.Helper()
	dir := t.TempDir()
	workDir := filepath.Join(dir, "work")
	bareDir := filepath.Join(dir, name+".git")

	mustGit(t, "", "init", workDir)

	topoContent := `[pack]
name = "` + name + `"
version = "` + tag + `"
schema = 1

[[agent]]
name = "worker"
`
	if err := os.WriteFile(filepath.Join(workDir, "pack.toml"), []byte(topoContent), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "release "+tag)
	mustGit(t, workDir, "tag", "-a", tag, "-m", "Release "+tag)

	mustGit(t, workDir, "clone", "--bare", workDir, bareDir)
	return bareDir
}

// initBareRepoWithBranch creates a bare repo with a named branch.
func initBareRepoWithBranch(t *testing.T, name, branch string) string {
	t.Helper()
	dir := t.TempDir()
	workDir := filepath.Join(dir, "work")
	bareDir := filepath.Join(dir, name+".git")

	mustGit(t, "", "init", workDir)

	topoContent := `[pack]
name = "` + name + `"
version = "0.1.0"
schema = 1
`
	if err := os.WriteFile(filepath.Join(workDir, "pack.toml"), []byte(topoContent), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "initial on main")

	// Create branch with different content.
	mustGit(t, workDir, "checkout", "-b", branch)
	topoContent = `[pack]
name = "` + name + `"
version = "0.2.0-` + branch + `"
schema = 1
`
	if err := os.WriteFile(filepath.Join(workDir, "pack.toml"), []byte(topoContent), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "update on "+branch)

	mustGit(t, workDir, "clone", "--bare", workDir, bareDir)
	return bareDir
}

// gitEnvBlacklist lists git environment variables stripped from test
// commands to prevent the parent project's hooks and repo state from
// leaking into temp test repos (e.g., GIT_DIR set by pre-commit hooks).
var testGitEnvBlacklist = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	// Prepend -c core.hooksPath= to disable hooks inherited from the
	// parent project config (core.hooksPath leaks via local git config).
	fullArgs := append([]string{"-c", "core.hooksPath="}, args...)
	cmd := exec.Command("git", fullArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Build clean env: strip git-specific vars that leak from the parent
	// project's pre-commit hook context.
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && testGitEnvBlacklist[k] {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), string(out), err)
	}
}

func TestClonePack(t *testing.T) {
	bare := initBareRepo(t, "test-topo")
	cacheDir := filepath.Join(t.TempDir(), "cached")

	if err := clonePack(bare, cacheDir, ""); err != nil {
		t.Fatalf("clonePack: %v", err)
	}

	// Verify pack.toml exists in cache.
	if _, err := os.Stat(filepath.Join(cacheDir, "pack.toml")); err != nil {
		t.Errorf("pack.toml not found in cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "prompts", "worker.md")); err != nil {
		t.Errorf("prompts/worker.md not found in cache: %v", err)
	}
}

func TestClonePack_WithTag(t *testing.T) {
	bare := initBareRepoWithTag(t, "tagged", "v1.0.0")
	cacheDir := filepath.Join(t.TempDir(), "cached")

	if err := clonePack(bare, cacheDir, "v1.0.0"); err != nil {
		t.Fatalf("clonePack with tag: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cacheDir, "pack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `version = "v1.0.0"`) {
		t.Errorf("expected tagged version, got: %s", data)
	}
}

func TestClonePack_WithBranch(t *testing.T) {
	bare := initBareRepoWithBranch(t, "branched", "develop")
	cacheDir := filepath.Join(t.TempDir(), "cached")

	if err := clonePack(bare, cacheDir, "develop"); err != nil {
		t.Fatalf("clonePack with branch: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cacheDir, "pack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "develop") {
		t.Errorf("expected branch version, got: %s", data)
	}
}

func TestUpdatePack(t *testing.T) {
	// Create bare repo, clone it, then add a commit to the bare repo
	// (via a temporary worktree), and verify update fetches it.
	dir := t.TempDir()
	workDir := filepath.Join(dir, "work")
	bareDir := filepath.Join(dir, "test.git")

	mustGit(t, "", "init", workDir)
	if err := os.WriteFile(filepath.Join(workDir, "pack.toml"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "v1")
	mustGit(t, workDir, "clone", "--bare", workDir, bareDir)

	// Clone into cache.
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := clonePack(bareDir, cacheDir, ""); err != nil {
		t.Fatal(err)
	}

	// Push a new commit to bare repo.
	pushDir := filepath.Join(dir, "push")
	mustGit(t, "", "clone", bareDir, pushDir)
	if err := os.WriteFile(filepath.Join(pushDir, "pack.toml"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, pushDir, "add", "-A")
	mustGit(t, pushDir, "commit", "-m", "v2")
	mustGit(t, pushDir, "push")

	// Update cache.
	if err := updatePack(cacheDir, ""); err != nil {
		t.Fatalf("updatePack: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cacheDir, "pack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "v2" {
		t.Errorf("expected v2 after update, got: %s", data)
	}
}

func TestUpdatePackWithBranchRef(t *testing.T) {
	// Clone with ref="main", push a new commit to bare, call
	// updatePack(dir, "main"), and verify we get the new content.
	// This catches the bug where checkout "main" uses the stale local
	// branch instead of origin/main.
	dir := t.TempDir()
	workDir := filepath.Join(dir, "work")
	bareDir := filepath.Join(dir, "test.git")

	mustGit(t, "", "init", "--initial-branch=main", workDir)
	if err := os.WriteFile(filepath.Join(workDir, "pack.toml"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "v1")
	mustGit(t, workDir, "clone", "--bare", workDir, bareDir)

	// Clone into cache WITH ref="main" (creates local main branch).
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := clonePack(bareDir, cacheDir, "main"); err != nil {
		t.Fatal(err)
	}

	// Push a new commit to the bare repo.
	pushDir := filepath.Join(dir, "push")
	mustGit(t, "", "clone", bareDir, pushDir)
	if err := os.WriteFile(filepath.Join(pushDir, "pack.toml"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, pushDir, "add", "-A")
	mustGit(t, pushDir, "commit", "-m", "v2")
	mustGit(t, pushDir, "push")

	// Update cache with explicit branch ref.
	if err := updatePack(cacheDir, "main"); err != nil {
		t.Fatalf("updatePack with branch ref: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cacheDir, "pack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "v2" {
		t.Errorf("expected v2 after update with branch ref, got: %s", data)
	}
}

func TestUpdatePackWithDirtyCache(t *testing.T) {
	// Simulate local modifications in the cache (e.g. a pack script
	// writing into its own directory). updatePack should discard them
	// and check out the remote ref cleanly.
	dir := t.TempDir()
	workDir := filepath.Join(dir, "work")
	bareDir := filepath.Join(dir, "test.git")

	mustGit(t, "", "init", "--initial-branch=main", workDir)
	if err := os.WriteFile(filepath.Join(workDir, "pack.toml"), []byte("clean"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "initial")
	mustGit(t, workDir, "clone", "--bare", workDir, bareDir)

	// Clone into cache.
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := clonePack(bareDir, cacheDir, "main"); err != nil {
		t.Fatal(err)
	}

	// Dirty the cache: modify a tracked file and add an untracked file.
	if err := os.WriteFile(filepath.Join(cacheDir, "pack.toml"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "untracked.txt"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	// updatePack should succeed despite dirty working tree.
	if err := updatePack(cacheDir, "main"); err != nil {
		t.Fatalf("updatePack with dirty cache: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cacheDir, "pack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "clean" {
		t.Errorf("expected tracked file restored to %q, got %q", "clean", string(data))
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "untracked.txt")); err == nil {
		t.Error("expected untracked file to be cleaned, but it still exists")
	}
}

func TestFetchPacks_ClonesMissing(t *testing.T) {
	bare := initBareRepo(t, "remote-topo")
	cityRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityRoot, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	topos := map[string]PackSource{
		"myremote": {Source: bare},
	}

	if err := FetchPacks(topos, cityRoot); err != nil {
		t.Fatalf("FetchPacks: %v", err)
	}

	// Verify cache exists.
	topoFile := filepath.Join(cityRoot, ".gc", "cache", "packs", "myremote", "pack.toml")
	if _, err := os.Stat(topoFile); err != nil {
		t.Errorf("expected cache to exist: %v", err)
	}
}

func TestFetchPacks_SkipsExisting(t *testing.T) {
	bare := initBareRepo(t, "skip-test")
	cityRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityRoot, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	topos := map[string]PackSource{
		"cached": {Source: bare},
	}

	// First fetch.
	if err := FetchPacks(topos, cityRoot); err != nil {
		t.Fatal(err)
	}

	// Second fetch should not error (skips clone, does update).
	if err := FetchPacks(topos, cityRoot); err != nil {
		t.Fatalf("second FetchPacks should succeed: %v", err)
	}
}

func TestFetchPacks_InvalidSource(t *testing.T) {
	cityRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityRoot, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	topos := map[string]PackSource{
		"bad": {Source: "/nonexistent/repo.git"},
	}

	err := FetchPacks(topos, cityRoot)
	if err == nil {
		t.Fatal("expected error for invalid source")
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("error should mention pack name, got: %v", err)
	}
}

func TestLockfile_RoundTrip(t *testing.T) {
	cityRoot := t.TempDir()
	lock := &PackLock{
		Packs: map[string]LockedPack{
			"gastown": {
				Source: "https://github.com/example/gastown",
				Ref:    "v1.0.0",
				Commit: "abc123def456",
				Hash:   "sha256:e3b0c44",
			},
		},
	}

	if err := WriteLock(cityRoot, lock); err != nil {
		t.Fatalf("WriteLock: %v", err)
	}

	got, err := ReadLock(cityRoot)
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}

	if len(got.Packs) != 1 {
		t.Fatalf("expected 1 pack, got %d", len(got.Packs))
	}
	lt := got.Packs["gastown"]
	if lt.Source != "https://github.com/example/gastown" {
		t.Errorf("Source = %q, want gastown URL", lt.Source)
	}
	if lt.Ref != "v1.0.0" {
		t.Errorf("Ref = %q, want v1.0.0", lt.Ref)
	}
	if lt.Commit != "abc123def456" {
		t.Errorf("Commit = %q, want abc123def456", lt.Commit)
	}
	if lt.Hash != "sha256:e3b0c44" {
		t.Errorf("Hash = %q, want sha256:e3b0c44", lt.Hash)
	}
}

func TestReadLock_MissingFile(t *testing.T) {
	cityRoot := t.TempDir()
	lock, err := ReadLock(cityRoot)
	if err != nil {
		t.Fatalf("ReadLock on missing file: %v", err)
	}
	if len(lock.Packs) != 0 {
		t.Errorf("expected empty packs, got %d", len(lock.Packs))
	}
}

func TestLockFromCache(t *testing.T) {
	bare := initBareRepo(t, "locktest")
	cityRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityRoot, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	topos := map[string]PackSource{
		"locktest": {Source: bare},
	}

	if err := FetchPacks(topos, cityRoot); err != nil {
		t.Fatal(err)
	}

	lock, err := LockFromCache(topos, cityRoot)
	if err != nil {
		t.Fatalf("LockFromCache: %v", err)
	}

	lt, ok := lock.Packs["locktest"]
	if !ok {
		t.Fatal("expected locktest in lock")
	}
	if lt.Source != bare {
		t.Errorf("Source = %q, want %q", lt.Source, bare)
	}
	if lt.Commit == "" {
		t.Error("Commit should not be empty")
	}
	if !strings.HasPrefix(lt.Hash, "sha256:") {
		t.Errorf("Hash should start with sha256:, got %q", lt.Hash)
	}
}

func TestPackCachePath(t *testing.T) {
	got := PackCachePath("/city", "gastown", PackSource{Source: "url"})
	want := "/city/.gc/cache/packs/gastown"
	if got != want {
		t.Errorf("PackCachePath = %q, want %q", got, want)
	}

	got = PackCachePath("/city", "mono", PackSource{Source: "url", Path: "packages/topo"})
	want = "/city/.gc/cache/packs/mono/packages/topo"
	if got != want {
		t.Errorf("PackCachePath with Path = %q, want %q", got, want)
	}
}

func TestFetchPacks_WithRefTag(t *testing.T) {
	bare := initBareRepoWithTag(t, "tagref", "v2.0.0")
	cityRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityRoot, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	topos := map[string]PackSource{
		"tagref": {Source: bare, Ref: "v2.0.0"},
	}

	if err := FetchPacks(topos, cityRoot); err != nil {
		t.Fatalf("FetchPacks with ref: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cityRoot, ".gc", "cache", "packs", "tagref", "pack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `version = "v2.0.0"`) {
		t.Errorf("expected tagged content, got: %s", data)
	}
}

func TestFetchRemoteInclude(t *testing.T) {
	bare := initBareRepo(t, "inc-test")
	cityRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityRoot, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cacheName := includeCacheName(bare)
	cacheDir := filepath.Join(cityRoot, ".gc", "cache", "includes", cacheName)
	if err := clonePack(bare, cacheDir, ""); err != nil {
		t.Fatalf("pre-clone cached include: %v", err)
	}

	resolvedDir, err := fetchRemoteInclude(bare, "", cityRoot)
	if err != nil {
		t.Fatalf("fetchRemoteInclude: %v", err)
	}

	// Verify pack.toml exists in the cache.
	if _, err := os.Stat(filepath.Join(resolvedDir, "pack.toml")); err != nil {
		t.Errorf("pack.toml not in cache: %v", err)
	}

	// Cache path should be under cache/includes/.
	if !strings.Contains(resolvedDir, filepath.Join(".gc", "cache", "includes")) {
		t.Errorf("cacheDir = %q, want under cache/includes/", resolvedDir)
	}

	// Idempotent: second lookup returns the same cache path.
	cacheDir2, err := fetchRemoteInclude(bare, "", cityRoot)
	if err != nil {
		t.Fatalf("second fetchRemoteInclude: %v", err)
	}
	if cacheDir2 != resolvedDir {
		t.Errorf("cache path changed: %q → %q", resolvedDir, cacheDir2)
	}
}

func TestFetchRemoteInclude_WithRef(t *testing.T) {
	bare := initBareRepoWithTag(t, "inc-tagged", "v1.0.0")
	cityRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityRoot, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cacheName := includeCacheName(bare)
	cacheDir := filepath.Join(cityRoot, ".gc", "cache", "includes", cacheName)
	if err := clonePack(bare, cacheDir, "v1.0.0"); err != nil {
		t.Fatalf("pre-clone cached include: %v", err)
	}

	resolvedDir, err := fetchRemoteInclude(bare, "v1.0.0", cityRoot)
	if err != nil {
		t.Fatalf("fetchRemoteInclude: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(resolvedDir, "pack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `version = "v1.0.0"`) {
		t.Errorf("expected tagged content, got: %s", data)
	}
}

func TestFetchRemoteInclude_MissingCache(t *testing.T) {
	bare := initBareRepo(t, "inc-missing")
	cityRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityRoot, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := fetchRemoteInclude(bare, "", cityRoot)
	if err == nil {
		t.Fatal("expected missing cache error")
	}
	if !strings.Contains(err.Error(), "not cached") {
		t.Fatalf("error = %v, want not cached", err)
	}
}

func TestLoadPack_RemoteInclude(t *testing.T) {
	// Create a bare repo as the "remote" pack.
	bare := initBareRepo(t, "remote-maint")

	// Set up a city root and parent pack that includes the bare repo
	// via a remote URL. We pre-clone the bare repo into the cache/includes cache
	// to simulate what fetchRemoteInclude does, then verify loadPack
	// picks up the included agents.
	cityRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityRoot, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-populate the include cache (simulates fetchRemoteInclude).
	cacheName := includeCacheName(bare)
	cacheDir := filepath.Join(cityRoot, ".gc", packCacheDir, includeCacheDir, cacheName)
	if err := clonePack(bare, cacheDir, ""); err != nil {
		t.Fatalf("pre-clone: %v", err)
	}

	// Use the cache dir as a local include (since it's now a local clone).
	// This tests the full flow: loadPack reads the included pack.
	parentToml := `[pack]
name = "parent"
schema = 1
includes = ["` + cacheDir + `"]

[[agent]]
name = "boss"
`
	topoDir := filepath.Join(cityRoot, "packs", "parent")
	if err := os.MkdirAll(topoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(topoDir, "pack.toml"), []byte(parentToml), 0o644); err != nil {
		t.Fatal(err)
	}

	agents, _, _, _, _, _, _, err := loadPack(
		fsys.OSFS{},
		filepath.Join(topoDir, "pack.toml"),
		topoDir,
		cityRoot, "", nil)
	if err != nil {
		t.Fatalf("loadPack with remote include: %v", err)
	}

	// Should have 2 agents: worker (from remote include) + boss (parent).
	if len(agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(agents))
	}
	if agents[0].Name != "worker" {
		t.Errorf("agents[0].Name = %q, want worker (from remote)", agents[0].Name)
	}
	if agents[1].Name != "boss" {
		t.Errorf("agents[1].Name = %q, want boss (from parent)", agents[1].Name)
	}
}

func TestExpandCityPacks_SkipsMissingRemoteSubpath(t *testing.T) {
	// Simulate a remote pack include whose subpath no longer exists
	// in the upstream repo (e.g., a pack directory was deleted).
	// ExpandCityPacks should log a warning and skip it, not error.
	bare := initBareRepo(t, "skip-missing")
	cityRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityRoot, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cacheName := includeCacheName("file://" + bare)
	cacheDir := filepath.Join(cityRoot, ".gc", "cache", "includes", cacheName)
	if err := clonePack(bare, cacheDir, ""); err != nil {
		t.Fatalf("pre-clone cached include: %v", err)
	}

	// Use file:// URL with //subpath syntax pointing to a non-existent subpath.
	// This is a remote ref (isRemoteRef returns true) that resolves to a
	// directory that doesn't contain the expected subpath.
	ref := "file://" + bare + "//no-such-subpath"

	cfg := &City{
		Agents: []Agent{{Name: "existing"}},
		Workspace: Workspace{
			Includes: []string{ref},
		},
	}

	dirs, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, cityRoot)
	if err != nil {
		t.Fatalf("ExpandCityPacks should skip missing remote subpath, got error: %v", err)
	}
	if len(dirs) != 0 {
		t.Errorf("formula dirs = %v, want empty", dirs)
	}
	// Original agents should be preserved.
	if len(cfg.Agents) != 1 || cfg.Agents[0].Name != "existing" {
		t.Errorf("agents = %v, want only [existing]", cfg.Agents)
	}
}
