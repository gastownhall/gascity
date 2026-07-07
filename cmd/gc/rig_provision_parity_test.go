package main

import (
	"context"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// packsLockFetchedTS matches the wall-clock fetch timestamp packs.lock records,
// the one non-deterministic field either provisioning path stamps at commit
// time. Normalizing it isolates the provisioning logic under test.
var packsLockFetchedTS = regexp.MustCompile(`fetched = "[^"]*"`)

// TestRigAddLocalAndAPIProduceIdenticalArtifacts is the Decision 7 proof: a rig
// add through the CLI wrapper (doRigAddWithResult) and through the controller's
// StateMutator (controllerState.CreateRig) must write byte-identical on-disk
// artifacts, because both now delegate to internal/rig.Provision. Absolute city
// paths embedded in the artifacts are normalized to <CITY> before comparison,
// since the two cities live in different temp dirs.
//
// The rig lives INSIDE the city (<city>/repo) with no .git, so the site-binding
// path resolution and the default-branch probe behave identically on both paths.
//
// The proof walks the ENTIRE city tree on both sides rather than a curated file
// list: a hand-picked list silently omits any artifact the two paths implement
// independently (e.g. the ~14 repo/.beads/formulas/*.toml files the API layer
// materializes by hand) and cannot see an extra file appearing on one side. The
// manifest equality catches a missing or extra file; the per-file byte compare
// catches divergent content.
func TestRigAddLocalAndAPIProduceIdenticalArtifacts(t *testing.T) {
	t.Run("plain city", func(t *testing.T) {
		assertRigAddArtifactsIdentical(t, "[workspace]\nname = \"parity-city\"\n", false, "")
	})

	t.Run("root-pack default rig import", func(t *testing.T) {
		bundledSource, ok := builtinpacks.CanonicalImportSource("gastown")
		if !ok {
			t.Fatal("bundled gastown pack not registered")
		}
		// A version-less bundled default-rig import forces the ComposePacks leg
		// to resolve and commit packs.lock plus the full repo/.beads/formulas/*
		// set, so the manifest pins the non-trivial artifacts both paths must
		// agree on rather than mutual absence.
		cityToml := fmt.Sprintf("[workspace]\nname = \"parity-city\"\n\n[defaults.rig.imports.gastown]\nsource = %q\n", bundledSource)
		assertRigAddArtifactsIdentical(t, cityToml, true, "")
	})

	t.Run("git rig probes default branch", func(t *testing.T) {
		// A git-inited rig drives the ProbeBranch leg on both paths, so the
		// persisted default_branch in city.toml is included in the byte-identical
		// comparison instead of being a mutual empty.
		assertRigAddArtifactsIdentical(t, "[workspace]\nname = \"parity-city\"\n", false, "trunk")
	})
}

// assertRigAddArtifactsIdentical provisions the same rig through the CLI and API
// paths in two sibling cities built from cityToml, then asserts every on-disk
// artifact is byte-identical after path normalization. wantPacksLock asserts the
// packs.lock artifact is actually present (guarding the ComposePacks leg from
// silently regressing to mutual absence). When gitBranch is non-empty the rig is
// git-inited with that branch as origin/HEAD so the ProbeBranch leg runs and the
// persisted default_branch is compared.
func assertRigAddArtifactsIdentical(t *testing.T, cityToml string, wantPacksLock bool, gitBranch string) {
	t.Helper()
	// The exact env the existing controller CreateRig tests run under: file
	// provider (no managed-Dolt lifecycle) + GC_DOLT=skip guarding the
	// contract-city branch, so no bd/Dolt process spawns.
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	// City A — CLI path.
	cityA := t.TempDir()
	writeSchema2RigCity(t, cityA, "parity-city", cityToml, "")
	rigA := filepath.Join(cityA, "repo")
	if err := os.MkdirAll(rigA, 0o755); err != nil {
		t.Fatalf("mkdir rig A: %v", err)
	}
	if gitBranch != "" {
		gitInitWithOriginHead(t, rigA, gitBranch)
	}
	if _, code := doRigAddWithResult(fsys.OSFS{}, cityA, rigA, nil, "", "", "", false, false, io.Discard, io.Discard); code != 0 {
		t.Fatalf("CLI doRigAddWithResult returned non-zero code %d", code)
	}

	// City B — API path.
	cityB := t.TempDir()
	writeSchema2RigCity(t, cityB, "parity-city", cityToml, "")
	rigB := filepath.Join(cityB, "repo")
	if err := os.MkdirAll(rigB, 0o755); err != nil {
		t.Fatalf("mkdir rig B: %v", err)
	}
	if gitBranch != "" {
		gitInitWithOriginHead(t, rigB, gitBranch)
	}
	cfgB, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityB, "city.toml"))
	if err != nil {
		t.Fatalf("load city B config: %v", err)
	}
	cs := newControllerState(context.Background(), cfgB, runtime.NewFake(), events.NewFake(), "parity-city", cityB)
	if err := cs.CreateRig(config.Rig{Name: "repo", Path: rigB}); err != nil {
		t.Fatalf("API CreateRig: %v", err)
	}

	manifestA := cityArtifactManifest(t, cityA)
	manifestB := cityArtifactManifest(t, cityB)
	assertManifestsEqual(t, manifestA, manifestB)

	if wantPacksLock && !manifestContains(manifestA, "packs.lock") {
		t.Fatalf("packs.lock absent from provisioned tree; ComposePacks leg did not run\nmanifest: %v", manifestA)
	}
	if gitBranch != "" {
		cityTomlA, readErr := os.ReadFile(filepath.Join(cityA, "city.toml"))
		if readErr != nil {
			t.Fatalf("read city A city.toml: %v", readErr)
		}
		wantBranch := fmt.Sprintf("default_branch = %q", gitBranch)
		if !strings.Contains(string(cityTomlA), wantBranch) {
			t.Fatalf("city.toml missing %s; ProbeBranch leg was not exercised:\n%s", wantBranch, cityTomlA)
		}
	}

	for _, rel := range manifestA {
		assertCityFileParity(t, cityA, cityB, rel)
	}
}

// gitInitWithOriginHead makes dir a git repo whose origin/HEAD points at branch,
// matching newRepoWithOriginHead but operating on a caller-chosen directory (the
// rig must live inside the city so its path normalizes to <CITY>).
func gitInitWithOriginHead(t *testing.T, dir, branch string) {
	t.Helper()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)
}

// cityArtifactManifest returns the sorted relative paths of every provisioning
// artifact under root. The rig's own .git directory is test scaffolding created
// identically on both sides — not something Provision writes — so it is skipped
// to keep git internals out of the byte-identity proof.
func cityArtifactManifest(t *testing.T, root string) []string {
	t.Helper()
	var rels []string
	err := filepath.WalkDir(root, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(rels)
	return rels
}

// assertManifestsEqual fails with the symmetric difference when the two trees do
// not contain exactly the same set of relative paths.
func assertManifestsEqual(t *testing.T, a, b []string) {
	t.Helper()
	setB := make(map[string]bool, len(b))
	for _, p := range b {
		setB[p] = true
	}
	setA := make(map[string]bool, len(a))
	for _, p := range a {
		setA[p] = true
	}
	var cliOnly, apiOnly []string
	for _, p := range a {
		if !setB[p] {
			cliOnly = append(cliOnly, p)
		}
	}
	for _, p := range b {
		if !setA[p] {
			apiOnly = append(apiOnly, p)
		}
	}
	if len(cliOnly) > 0 || len(apiOnly) > 0 {
		t.Fatalf("CLI and API produced different artifact sets\n  CLI-only: %v\n  API-only: %v", cliOnly, apiOnly)
	}
}

func manifestContains(manifest []string, rel string) bool {
	for _, p := range manifest {
		if p == rel {
			return true
		}
	}
	return false
}

// assertCityFileParity byte-compares one relative artifact across the two cities
// after the two legitimate normalizations: each city's own absolute root to
// <CITY> (so path-embedding files compare structurally) and the packs.lock fetch
// timestamp to <TS>. No other normalization is applied — anything else differing
// is a real divergence the proof must surface.
func assertCityFileParity(t *testing.T, cityA, cityB, rel string) {
	t.Helper()
	aBytes, aErr := os.ReadFile(filepath.Join(cityA, rel))
	if aErr != nil {
		t.Fatalf("%s: reading CLI artifact: %v", rel, aErr)
	}
	bBytes, bErr := os.ReadFile(filepath.Join(cityB, rel))
	if bErr != nil {
		t.Fatalf("%s: reading API artifact: %v", rel, bErr)
	}

	aNorm := normalizeArtifact(string(aBytes), cityA)
	bNorm := normalizeArtifact(string(bBytes), cityB)
	if aNorm != bNorm {
		t.Fatalf("%s: CLI and API artifacts differ after normalization:\n--- CLI ---\n%s\n--- API ---\n%s", rel, aNorm, bNorm)
	}
}

// normalizeArtifact replaces the city's own absolute root with <CITY> and the
// packs.lock fetch timestamp with <TS>, leaving only provisioning-logic content.
func normalizeArtifact(content, cityPath string) string {
	content = strings.ReplaceAll(content, cityPath, "<CITY>")
	return packsLockFetchedTS.ReplaceAllString(content, `fetched = "<TS>"`)
}
