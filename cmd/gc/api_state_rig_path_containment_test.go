package main

// Negative coverage for the city-root containment guard on the API rig-create
// paths (ga-qe7qm). The pre-existing tests were all POSITIVE: they pinned that
// an in-city rig under a symlinked city is ACCEPTED, and the only negative
// (TestControllerStateCreateRigRejectsOutOfCityPath) used "../escape" and a
// foreign absolute tempdir. Nothing anywhere exercised the symlink-escape
// rejection, and nothing referenced the second pass at all — neutering it to
// `return nil` left every selection green.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// escapeLayout builds <root>/city with an escaping symlink <city>/link ->
// <root>/outside, and an existing <root>/outside/evil. It returns the city dir
// and the outside dir.
func escapeLayout(t *testing.T) (cityDir, outsideDir string) {
	t.Helper()
	root := t.TempDir()
	cityDir = filepath.Join(root, "city")
	outsideDir = filepath.Join(root, "outside")
	for _, d := range []string{cityDir, filepath.Join(outsideDir, "evil")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outsideDir, filepath.Join(cityDir, "link")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	return cityDir, outsideDir
}

// TestAssertRigPathWithinCityRejectsSymlinkEscape is the negative the guard
// never had: a "../"-free path that escapes the city through a symlinked
// ancestor must be refused, both when the leaf exists (the sync CreateRig
// shape) and when it does not (the git_url clone shape realPathForContainment
// was written for).
func TestAssertRigPathWithinCityRejectsSymlinkEscape(t *testing.T) {
	cityDir, _ := escapeLayout(t)

	for _, tc := range []struct {
		name string
		path string
	}{
		{"existing leaf", "link/evil"},
		{"absent leaf (git_url clone destination)", "link/absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := filepath.Join(cityDir, tc.path)
			if err := assertRigPathWithinCity(cityDir, raw); !errors.Is(err, configedit.ErrValidation) {
				t.Errorf("assertRigPathWithinCity(raw %q) = %v, want ErrValidation", raw, err)
			}
			// The real call sites resolve through resolveStoreScopeRoot first, so
			// pin that shape too — it is the one an API caller actually reaches.
			resolved := resolveStoreScopeRoot(cityDir, tc.path)
			if err := assertRigPathWithinCity(cityDir, resolved); !errors.Is(err, configedit.ErrValidation) {
				t.Errorf("assertRigPathWithinCity(resolved %q) = %v, want ErrValidation", resolved, err)
			}
		})
	}
}

// TestBothContainmentPassesRejectSymlinkEscapeIndependently is the control the
// escape test above needs. assertRigPathWithinCity short-circuits on the first
// pass, so an end-to-end negative passes even if the second pass is a no-op —
// that is precisely how the second pass went unpinned. This asserts EACH pass
// rejects the escape on its own, so neither can rot to `return nil` silently.
func TestBothContainmentPassesRejectSymlinkEscapeIndependently(t *testing.T) {
	cityDir, _ := escapeLayout(t)

	for _, leaf := range []string{"link/evil", "link/absent"} {
		target := resolveStoreScopeRoot(cityDir, leaf)
		if err := lexicalContainment(cityDir, target); !errors.Is(err, configedit.ErrValidation) {
			t.Errorf("lexicalContainment(%q) = %v, want ErrValidation "+
				"(the FIRST pass stopped catching the symlink escape)", leaf, err)
		}
		if err := symlinkAwareContainment(cityDir, target); !errors.Is(err, configedit.ErrValidation) {
			t.Errorf("symlinkAwareContainment(%q) = %v, want ErrValidation "+
				"(the SECOND pass stopped catching the symlink escape)", leaf, err)
		}
	}
}

// TestSymlinkAwareContainmentIsLoadBearing pins the inputs on which the second
// pass is the ONLY rejector, disproving the claim that it is unreachable
// defense-in-depth subsumed by the first pass.
//
// pathutil.NormalizePathForCompare swallows every EvalSymlinks error and walks
// up to the nearest resolvable ancestor, so for a path it cannot canonicalize
// at all it synthesizes an in-city-looking result and the first pass ACCEPTS.
// realPathForContainment returns the error instead, so the second pass fails
// closed. Delete the second pass and these inputs become accepted.
func TestSymlinkAwareContainmentIsLoadBearing(t *testing.T) {
	cityDir := t.TempDir()

	type loadBearingCase struct {
		name string
		path string
	}

	// A regular file used as a directory component: EvalSymlinks returns ENOTDIR,
	// which is not os.IsNotExist. This arm needs no symlink, so keep it out from
	// under the symlink-support skip below — it exercises a distinct EvalSymlinks
	// error class and must run on every platform.
	if err := os.WriteFile(filepath.Join(cityDir, "plainfile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []loadBearingCase{
		{"path under a regular file", filepath.Join(cityDir, "plainfile", "rig")},
	}

	// A symlink loop: EvalSymlinks returns ELOOP, also not os.IsNotExist. These
	// arms need symlink support, so add them only when it is available instead of
	// skipping the whole test (which would also drop the ENOTDIR arm above).
	loop := filepath.Join(cityDir, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Logf("symlinks unsupported on this platform, exercising only the ENOTDIR arm: %v", err)
	} else {
		cases = append(cases,
			loadBearingCase{"symlink loop", loop},
			loadBearingCase{"path under a symlink loop", filepath.Join(cityDir, "loop", "rig")},
		)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := lexicalContainment(cityDir, tc.path); err != nil {
				t.Fatalf("precondition: lexicalContainment(%q) = %v, want nil. "+
					"This test is only meaningful where the FIRST pass accepts; if "+
					"normalization changed so it now rejects, this arm no longer "+
					"proves the second pass is load-bearing and must be rebuilt.", tc.path, err)
			}
			if err := symlinkAwareContainment(cityDir, tc.path); !errors.Is(err, configedit.ErrValidation) {
				t.Errorf("symlinkAwareContainment(%q) = %v, want ErrValidation: the "+
					"second pass is the only thing refusing an uncanonicalizable path", tc.path, err)
			}
		})
	}
}

// TestContainmentPassesAcceptLegitimateInCityRigs is the other half of the
// control: a guard that rejects everything is as useless as one that rejects
// nothing. Every arm here must be ACCEPTED by both passes.
func TestContainmentPassesAcceptLegitimateInCityRigs(t *testing.T) {
	root := t.TempDir()
	realCity := filepath.Join(root, "real", "city")
	if err := os.MkdirAll(filepath.Join(realCity, "present"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The city reached through a symlinked ancestor — the shape resolveStoreScopeRoot
	// exists to support, and the one a too-eager guard would break.
	linkedCity := filepath.Join(root, "linkcity")
	if err := os.Symlink(realCity, linkedCity); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	for _, tc := range []struct {
		name string
		city string
		leaf string
	}{
		{"existing rig, real city", realCity, "present"},
		{"absent rig, real city", realCity, "notyet"},
		{"existing rig, symlinked city", linkedCity, "present"},
		{"absent rig, symlinked city", linkedCity, "notyet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := resolveStoreScopeRoot(tc.city, tc.leaf)
			if err := lexicalContainment(tc.city, target); err != nil {
				t.Errorf("lexicalContainment(%q, %q) = %v, want nil", tc.city, target, err)
			}
			if err := symlinkAwareContainment(tc.city, target); err != nil {
				t.Errorf("symlinkAwareContainment(%q, %q) = %v, want nil", tc.city, target, err)
			}
			if err := assertRigPathWithinCity(tc.city, target); err != nil {
				t.Errorf("assertRigPathWithinCity(%q, %q) = %v, want nil", tc.city, target, err)
			}
		})
	}
}

// TestControllerStateCreateRigRejectsSymlinkEscape drives the rejection through
// the real sync entry point, so the guard is pinned where it is actually wired
// and not only as a free function. A rejected rig must also leave no trace in
// config and must not have created anything outside the city.
func TestControllerStateCreateRigRejectsSymlinkEscape(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	cityDir, outsideDir := escapeLayout(t)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"city1\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	cs := newControllerState(context.Background(), &config.City{Workspace: config.Workspace{Name: "city1"}},
		runtime.NewFake(), events.NewFake(), "city1", cityDir)

	for _, p := range []string{"link/evil", "link/absent"} {
		if err := cs.CreateRig(config.Rig{Name: "evil", Path: p}); !errors.Is(err, configedit.ErrValidation) {
			t.Errorf("CreateRig(path=%q) err = %v, want ErrValidation", p, err)
		}
	}
	if got := cs.Config(); got != nil && len(got.Rigs) != 0 {
		t.Fatalf("a rejected rig leaked into config: %+v", got.Rigs)
	}
	// The guard runs before any filesystem side effect, so the escape target
	// must be untouched: no MkdirAll, no store write outside the city.
	if _, err := os.Stat(filepath.Join(outsideDir, "absent")); !os.IsNotExist(err) {
		t.Fatalf("CreateRig created %s outside the city: stat err = %v", filepath.Join(outsideDir, "absent"), err)
	}
	if entries, err := os.ReadDir(filepath.Join(outsideDir, "evil")); err != nil {
		t.Fatalf("read escape target: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("CreateRig planted %d entries in %s outside the city", len(entries), filepath.Join(outsideDir, "evil"))
	}
}

// TestProvisionRigFromGitRejectsSymlinkEscape pins the async git_url path. The
// clone destination is server-derived and absent, so this is the missing-leaf
// case realPathForContainment exists for. The rejection must happen before the
// clone: no manifest callback may fire, or a RemoveAll teardown would later be
// pointed outside the city.
func TestProvisionRigFromGitRejectsSymlinkEscape(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	cityDir, outsideDir := escapeLayout(t)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"city1\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	cs := newControllerState(context.Background(), &config.City{Workspace: config.Workspace{Name: "city1"}},
		runtime.NewFake(), events.NewFake(), "city1", cityDir)

	manifested := 0
	onManifest := func(api.RigProvisionManifest) error { manifested++; return nil }

	_, err := cs.ProvisionRigFromGit(context.Background(),
		config.Rig{Name: "evil", Path: "link/absent"},
		"https://github.com/example/repo.git", nil, onManifest)
	if !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("ProvisionRigFromGit(path=link/absent) err = %v, want ErrValidation", err)
	}
	if manifested != 0 {
		t.Errorf("manifest callback fired %d times for a rejected path; the guard must "+
			"run before the record-then-create handshake", manifested)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "absent")); !os.IsNotExist(err) {
		t.Fatalf("ProvisionRigFromGit created %s outside the city: stat err = %v",
			filepath.Join(outsideDir, "absent"), err)
	}
}

// TestTeardownPartialRigRefusesUncanonicalizablePathThroughGlue pins the second
// containment pass through the production glue on a LOAD-BEARING input — one the
// first (lexical) pass accepts and only symlinkAwareContainment refuses. The
// isolation tests above call the two passes directly, and the escape tests above
// use symlink-escape inputs the FIRST pass already rejects, so none of them would
// redden if symlinkAwareContainment were dropped from assertRigPathWithinCity.
// This drives the highest-value destructive caller (TeardownPartialRig fences
// os.RemoveAll on a CreatedDir read back from a durable, attacker-influenceable-
// over-time record) with a path-under-a-regular-file target: EvalSymlinks returns
// ENOTDIR, so NormalizePathForCompare synthesizes an in-city path (first pass
// accepts) while realPathForContainment fails closed (second pass rejects).
// Deleting or reordering the symlinkAwareContainment call in assertRigPathWithinCity
// lets the RemoveAll proceed and reddens this test. No symlink is needed, so it
// runs on every platform.
func TestTeardownPartialRigRefusesUncanonicalizablePathThroughGlue(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"city1\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	// A regular file used as a directory component yields ENOTDIR, which is not
	// os.IsNotExist, so the first pass accepts the synthesized in-city path and
	// only the fail-closed second pass rejects it.
	plainfile := filepath.Join(cityDir, "plainfile")
	if err := os.WriteFile(plainfile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	createdDir := filepath.Join(cityDir, "plainfile", "rig")

	// Precondition: this pins the second pass only where the FIRST pass accepts.
	// If normalization ever changed so the lexical pass rejects this input, the
	// test no longer proves the second pass is load-bearing through the glue.
	if err := lexicalContainment(cityDir, createdDir); err != nil {
		t.Fatalf("precondition: lexicalContainment(%q) = %v, want nil", createdDir, err)
	}

	cs := newControllerState(context.Background(), &config.City{Workspace: config.Workspace{Name: "city1"}},
		runtime.NewFake(), events.NewFake(), "city1", cityDir)

	err := cs.TeardownPartialRig(context.Background(),
		api.RigProvisionManifest{RigName: "evil", CreatedDir: createdDir})
	if !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("TeardownPartialRig(CreatedDir=%q) err = %v, want ErrValidation: the second "+
			"containment pass must refuse the RemoveAll on an uncanonicalizable path through the glue", createdDir, err)
	}
	// The RemoveAll must have been refused before any side effect: the regular
	// file the rejected path tunnels through is still present and unmodified.
	if b, err := os.ReadFile(plainfile); err != nil || string(b) != "x" {
		t.Fatalf("TeardownPartialRig disturbed the escape target: ReadFile(%q) = %q, %v; want %q, nil",
			plainfile, b, err, "x")
	}
}
