package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

const (
	bdBeadsVersionTestLinked = "v1.3.4"
	bdBeadsVersionTestSkewed = "v1.2.0"
)

// bdBuildInfoWithBeads is the build info of a bd built with `go install
// github.com/steveyegge/beads/cmd/bd@version`.
func bdBuildInfoWithBeads(version string) *debug.BuildInfo {
	return &debug.BuildInfo{
		Path: "github.com/steveyegge/beads/cmd/bd",
		Main: debug.Module{Path: "github.com/steveyegge/beads", Version: version},
	}
}

// gcBuildInfoLinkingBeads is the build info of a gc linking beads at the
// test's linked version.
func gcBuildInfoLinkingBeads() *debug.BuildInfo {
	return &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/gastownhall/gascity", Version: "(devel)"},
		Deps: []*debug.Module{{Path: "github.com/steveyegge/beads", Version: bdBeadsVersionTestLinked}},
	}
}

// bdBeadsVersionFixture is a bd-backed city whose only bd on PATH is a fake
// that records every execution, with the build-info readers injected.
type bdBeadsVersionFixture struct {
	city    string
	binDir  string
	bd      string
	execLog string
	// bdInfo answers the bd build-info reader by resolved path; a missing
	// entry reads as a file without Go build info.
	bdInfo map[string]*debug.BuildInfo
	// reads counts bd build-info reads by resolved path.
	reads map[string]int
	// gcInfo and gcInfoOK answer gc's own build-info reader.
	gcInfo   *debug.BuildInfo
	gcInfoOK bool
}

// newBdBeadsVersionFixture builds the fixture with gc linking beads at the
// test's linked version.
func newBdBeadsVersionFixture(t *testing.T) *bdBeadsVersionFixture {
	t.Helper()
	f := &bdBeadsVersionFixture{
		city:     t.TempDir(),
		binDir:   t.TempDir(),
		bdInfo:   map[string]*debug.BuildInfo{},
		reads:    map[string]int{},
		gcInfo:   gcBuildInfoLinkingBeads(),
		gcInfoOK: true,
	}
	f.execLog = filepath.Join(t.TempDir(), "bd-exec.log")
	if err := os.WriteFile(filepath.Join(f.city, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.bd = f.writeBd(t, filepath.Join(f.binDir, "bd"))
	for _, key := range []string{"GC_BEADS", "GC_BEADS_SCOPE_ROOT", "GC_BEADS_BACKEND", "BD_BIN"} {
		t.Setenv(key, "")
	}
	t.Setenv("PATH", f.binDir)

	prevBd, prevGc := bdBeadsVersionReadBdBuildInfo, bdBeadsVersionReadGcBuildInfo
	t.Cleanup(func() {
		bdBeadsVersionReadBdBuildInfo, bdBeadsVersionReadGcBuildInfo = prevBd, prevGc
	})
	bdBeadsVersionReadGcBuildInfo = func() (*debug.BuildInfo, bool) { return f.gcInfo, f.gcInfoOK }
	bdBeadsVersionReadBdBuildInfo = func(path string) (*debug.BuildInfo, error) {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		f.reads[resolved]++
		if info, ok := f.bdInfo[resolved]; ok {
			return info, nil
		}
		return nil, errors.New("no Go build info")
	}
	return f
}

// writeBd writes a fake bd that logs any execution and returns its path.
func (f *bdBeadsVersionFixture) writeBd(t *testing.T, path string) string {
	t.Helper()
	script := "#!/bin/sh\necho \"$0 $*\" >> '" + f.execLog + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func (f *bdBeadsVersionFixture) assertBdNeverExecuted(t *testing.T) {
	t.Helper()
	if data, err := os.ReadFile(f.execLog); err == nil {
		t.Fatalf("the check executed bd:\n%s", data)
	}
}

func (f *bdBeadsVersionFixture) run(t *testing.T, cfg *config.City) *doctor.CheckResult {
	t.Helper()
	r := newBdBeadsVersionCheck(f.city, cfg).Run(&doctor.CheckContext{CityPath: f.city})
	f.assertBdNeverExecuted(t)
	return r
}

// TestBdBeadsVersionCheckWarnsOnSkew: the bd the city scope resolves from
// PATH embeds another beads version than gc links. The warning names the
// scope, the bd, both versions and make install-bd.
func TestBdBeadsVersionCheckWarnsOnSkew(t *testing.T) {
	f := newBdBeadsVersionFixture(t)
	f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestSkewed)
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	r := f.run(t, cfg)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %+v", r.Status, r)
	}
	details := strings.Join(r.Details, "\n")
	for _, want := range []string{"city", f.bd, bdBeadsVersionTestSkewed, bdBeadsVersionTestLinked} {
		if !strings.Contains(details, want) {
			t.Errorf("details lack %q:\n%s", want, details)
		}
		if !strings.Contains(r.Message, want) {
			t.Errorf("message lacks %q: %s", want, r.Message)
		}
	}
	if !strings.Contains(r.FixHint, "make install-bd") {
		t.Errorf("fix hint does not name make install-bd: %q", r.FixHint)
	}
	if r.Name != "bd-beads-version" {
		t.Errorf("name = %q", r.Name)
	}
}

// TestBdBeadsVersionCheckOKWhenVersionsMatch: a bd built from the beads gc
// links is quiet.
func TestBdBeadsVersionCheckOKWhenVersionsMatch(t *testing.T) {
	f := newBdBeadsVersionFixture(t)
	f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestLinked)

	r := f.run(t, &config.City{Workspace: config.Workspace{Name: "demo"}})
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK: %+v", r.Status, r)
	}
	if !strings.Contains(r.Message, bdBeadsVersionTestLinked) {
		t.Fatalf("message does not name the matching version: %q", r.Message)
	}
}

// TestBdBeadsVersionCheckReadsASharedBdOnce: scopes whose bd paths resolve
// to one file (here through a symlink) read it once, and every scope is
// still named.
func TestBdBeadsVersionCheckReadsASharedBdOnce(t *testing.T) {
	f := newBdBeadsVersionFixture(t)
	f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestSkewed)
	link := filepath.Join(t.TempDir(), "bd")
	if err := os.Symlink(f.bd, link); err != nil {
		t.Fatal(err)
	}
	rigA, rigB := t.TempDir(), t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "a", Path: rigA}, {Name: "b", Path: rigB}},
	}
	check := newBdBeadsVersionCheck(f.city, cfg)
	check.resolveBd = func(_, scopeRoot string) (string, error) {
		if samePath(scopeRoot, rigB) {
			return link, nil
		}
		return f.bd, nil
	}

	r := check.Run(&doctor.CheckContext{CityPath: f.city})
	f.assertBdNeverExecuted(t)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %+v", r.Status, r)
	}
	if f.reads[f.bd] != 1 {
		t.Fatalf("the shared bd was read %d times, want 1", f.reads[f.bd])
	}
	details := strings.Join(r.Details, "\n")
	for _, want := range []string{"city", `rig "a"`, `rig "b"`, link} {
		if !strings.Contains(details, want) {
			t.Errorf("details lack %q:\n%s", want, details)
		}
	}
}

// TestBdBeadsVersionCheckNamesTheSkewedRig: only the rig whose own bd is
// skewed is named; the city's matching bd is not.
func TestBdBeadsVersionCheckNamesTheSkewedRig(t *testing.T) {
	f := newBdBeadsVersionFixture(t)
	f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestLinked)
	rigBd := f.writeBd(t, filepath.Join(t.TempDir(), "bd"))
	f.bdInfo[rigBd] = bdBuildInfoWithBeads(bdBeadsVersionTestSkewed)
	rigDir := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "fe", Path: rigDir}},
	}
	check := newBdBeadsVersionCheck(f.city, cfg)
	check.resolveBd = func(_, scopeRoot string) (string, error) {
		if samePath(scopeRoot, rigDir) {
			return rigBd, nil
		}
		return f.bd, nil
	}

	r := check.Run(&doctor.CheckContext{CityPath: f.city})
	f.assertBdNeverExecuted(t)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %+v", r.Status, r)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], `rig "fe"`) || !strings.Contains(r.Details[0], rigBd) {
		t.Fatalf("details = %q, want one line naming rig \"fe\" and %s", r.Details, rigBd)
	}
}

// TestBdBeadsVersionCheckIsWarmupEligibleAndRegistered: gc start's warm-up
// runs the check, and gc doctor and the warm-up both register it.
func TestBdBeadsVersionCheckIsWarmupEligibleAndRegistered(t *testing.T) {
	if !newBdBeadsVersionCheck("", nil).WarmupEligible() {
		t.Fatal("bd-beads-version must run in gc start's warm-up")
	}
	if newBdBeadsVersionCheck("", nil).CanFix() {
		t.Fatal("bd-beads-version has no fix: installing bd is the operator's act")
	}
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}
	for _, warm := range []bool{false, true} {
		names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
			SkipCityDoltCheck:    true,
			SkipManagedDoltCheck: true,
			SkipStorePreflight:   true,
			Warmup:               warm,
		}))
		found := false
		for _, name := range names {
			if name == "bd-beads-version" {
				found = true
			}
		}
		if !found {
			t.Fatalf("buildDoctorChecks(Warmup=%v) does not register bd-beads-version: %q", warm, names)
		}
	}
}

// TestBdBeadsVersionCheckOKWhenAVersionIsUnknown: when either version cannot
// be known, or no scope is bd-backed, nothing is compared. The check is OK and
// says why; it never warns, never errors and never executes bd.
func TestBdBeadsVersionCheckOKWhenAVersionIsUnknown(t *testing.T) {
	replacedBd := bdBuildInfoWithBeads(bdBeadsVersionTestSkewed)
	replacedBd.Main.Replace = &debug.Module{Path: "../beads"}
	gcWithoutBeads := &debug.BuildInfo{Main: debug.Module{Path: "github.com/gastownhall/gascity", Version: "(devel)"}}
	gcReplacedBeads := gcBuildInfoLinkingBeads()
	gcReplacedBeads.Deps[0].Replace = &debug.Module{Path: "../beads"}

	cases := []struct {
		name  string
		setup func(t *testing.T, f *bdBeadsVersionFixture)
		// reason is a fragment of the message saying why nothing was compared.
		reason string
	}{
		{
			name:   "bd not resolved",
			setup:  func(t *testing.T, _ *bdBeadsVersionFixture) { t.Setenv("PATH", t.TempDir()) },
			reason: "bd not resolved",
		},
		{
			name:   "shim or non-Go bd",
			setup:  func(*testing.T, *bdBeadsVersionFixture) {},
			reason: "no Go build info",
		},
		{
			name: "(devel) bd",
			setup: func(_ *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads("(devel)")
			},
			reason: "names no beads version",
		},
		{
			name: "bd with an empty version",
			setup: func(_ *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads("")
			},
			reason: "names no beads version",
		},
		{
			name:   "bd built from a replaced beads module",
			setup:  func(_ *testing.T, f *bdBeadsVersionFixture) { f.bdInfo[f.bd] = replacedBd },
			reason: "names no beads version",
		},
		{
			name: "gc without build info",
			setup: func(_ *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestSkewed)
				f.gcInfo, f.gcInfoOK = nil, false
			},
			reason: "gc was built without module build info",
		},
		{
			name: "gc linking no beads module",
			setup: func(_ *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestSkewed)
				f.gcInfo = gcWithoutBeads
			},
			reason: "gc's build info names no beads version",
		},
		{
			name: "gc linking a replaced beads module",
			setup: func(_ *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestSkewed)
				f.gcInfo = gcReplacedBeads
			},
			reason: "gc's build info names no beads version",
		},
		{
			name: "no bd-backed scope",
			setup: func(t *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestSkewed)
				t.Setenv("GC_BEADS", "file")
			},
			reason: "no scope uses a bd-backed store",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBdBeadsVersionFixture(t)
			tc.setup(t, f)

			r := f.run(t, &config.City{Workspace: config.Workspace{Name: "demo"}})
			if r.Status != doctor.StatusOK {
				t.Fatalf("status = %v, want OK: %+v", r.Status, r)
			}
			if !strings.Contains(r.Message, tc.reason) {
				t.Fatalf("message %q does not give the reason %q", r.Message, tc.reason)
			}
		})
	}
}

// TestBdBeadsVersionCheckOKWithoutCityConfig: with no city config there is
// no scope to compare.
func TestBdBeadsVersionCheckOKWithoutCityConfig(t *testing.T) {
	f := newBdBeadsVersionFixture(t)
	f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestSkewed)

	r := f.run(t, nil)
	if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "no city config") {
		t.Fatalf("result = %+v, want OK naming the missing city config", r)
	}
}
