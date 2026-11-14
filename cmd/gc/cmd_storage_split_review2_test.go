package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

// pseudoBuilt is a `go install …@<ref>` build's module version.
const pseudoBuilt = "v1.5.2-0.20261113052233-a07d754c75ab"

// fakeSessionPathGC puts an executable gc that prints printed for
// `gc version` first on the PATH, under a HOME of the test's own, and has
// the build-info read report built for it (an error when built is "").
func fakeSessionPathGC(t *testing.T, printed, built string) string {
	t.Helper()
	dir := t.TempDir()
	gc := filepath.Join(dir, "gc")
	if err := os.WriteFile(gc, []byte("#!/bin/sh\necho "+printed+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := sessionPathGCBuildInfo
	t.Cleanup(func() { sessionPathGCBuildInfo = prev })
	sessionPathGCBuildInfo = func(path string) (*debug.BuildInfo, error) {
		if path != gc {
			return nil, errors.New("not the fake gc")
		}
		if built == "" {
			return nil, errors.New("no build info")
		}
		return &debug.BuildInfo{Main: debug.Module{Version: built}}, nil
	}
	return gc
}

// TestProbeSessionPathGCVersionReadsAPseudoVersionBuild (MUST): `gc version`
// prints the normalized version, so the session-PATH probe reads the
// binary's own module version and reports a pseudo-version build as one,
// which the floor does not compare; every other build reports its line.
func TestProbeSessionPathGCVersionReadsAPseudoVersionBuild(t *testing.T) {
	printed := normalizeVersion(pseudoBuilt)
	for _, tc := range []struct {
		name, printed, built, want string
	}{
		{"pseudo-version build", printed, pseudoBuilt, pseudoBuilt},
		{"release build", "1.5.2", "v1.5.2", "1.5.2"},
		{"pseudo-version of another base", printed, "v1.4.0-0.20261113052233-a07d754c75ab", printed},
		{"no build info", printed, "", printed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gc := fakeSessionPathGC(t, tc.printed, tc.built)
			path, got, err := probeSessionPathGCVersion(nil)
			if err != nil || path != gc || got != tc.want {
				t.Fatalf("probe = %q, %q, %v; want %q, %q", path, got, err, gc, tc.want)
			}
		})
	}
	if !gcVersionBelowFloor("1.5.2", splitMinGCVersion) || gcVersionBelowFloor(pseudoBuilt, splitMinGCVersion) {
		t.Fatal("precondition: 1.5.2 compares below the floor and its pseudo-version build does not")
	}
}

// TestStorageSplitCompletesWithAPseudoVersionGcOnTheSessionPATH (MUST, the
// round-2 reviewer's scenario): this gc is a pseudo-version build and is the
// gc on the session PATH. The split finishes, and doctor warns that the
// PATH gc cannot be compared rather than erring that it is below the floor.
func TestStorageSplitCompletesWithAPseudoVersionGcOnTheSessionPATH(t *testing.T) {
	c := newGuidedSplitCity(t)
	printed := normalizeVersion(pseudoBuilt)
	setGCVersion(t, printed, pseudoBuilt)
	fakeSessionPathGC(t, printed, pseudoBuilt)
	prev := minGCVersionPathProbe
	t.Cleanup(func() { minGCVersionPathProbe = prev })
	minGCVersionPathProbe = probeSessionPathGCVersion
	code, stdout, stderr := c.split(splitOptions{})
	if code != 0 {
		t.Fatalf("split by a pseudo-version build that is the session PATH gc = %d:\n%s\n%s", code, stdout, stderr)
	}
	// P0 (round 2's session-PATH check) notes it rather than blocking.
	if !strings.Contains(stdout, "which cannot be compared with the floor") || strings.Contains(stdout, "[BLOCK]") {
		t.Fatalf("the rehearsal did not note the pseudo-version PATH gc:\n%s", stdout)
	}
	c.assertSplit()
	res := newStorageSplitCheck(c.reload().Cfg, c.request.CityPath).Run(nil)
	if res.Status == doctor.StatusError || !strings.Contains(res.Message, "cannot be compared") {
		t.Fatalf("doctor after the split = %v: %s", res.Status, res.Message)
	}
}

// TestBuiltVersion (V1, V2): the version a binary was built as is the
// injected one, or, when none was injected, the module version the
// toolchain recorded, unnormalized.
func TestBuiltVersion(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: pseudoBuilt}}
	for _, tc := range []struct {
		name, injected string
		ok             bool
		info           *debug.BuildInfo
		want           string
	}{
		{"injected release", "1.6.0", true, info, "1.6.0"},
		{"injected v-release", "v1.6.0", true, info, "v1.6.0"},
		{"go install build", "dev", true, info, pseudoBuilt},
		{"empty injection", "", true, info, pseudoBuilt},
		{"devel module", "dev", true, &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "(devel)"},
		{"no build info", "dev", false, info, "dev"},
		{"nil build info", "dev", true, nil, "dev"},
	} {
		if got := builtVersion(tc.injected, tc.ok, tc.info); got != tc.want {
			t.Errorf("%s: builtVersion = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCoTenantLivenessAsksTheControllerFirst (L1): a co-tenant whose
// controller answers its ping is live, whoever holds its lock.
func TestCoTenantLivenessAsksTheControllerFirst(t *testing.T) {
	other := t.TempDir()
	prev := coTenantControllerPing
	t.Cleanup(func() { coTenantControllerPing = prev })
	coTenantControllerPing = func(path string) int {
		if path == other {
			return 4242
		}
		return 0
	}
	if why := coTenantLiveness(other); !strings.Contains(why, "PID 4242, answers") {
		t.Fatalf("liveness of a co-tenant whose controller answers = %q", why)
	}
	if why := coTenantLiveness(t.TempDir()); why != "" {
		t.Fatalf("liveness of a city with no controller and no lock = %q", why)
	}
}
