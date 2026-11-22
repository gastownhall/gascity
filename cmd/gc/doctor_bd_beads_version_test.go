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
	bdBeadsVersionTestOlder  = "v1.2.0"
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
	city   string
	binDir string
	// bd is the fake bd's resolved path, the key of bdInfo and reads;
	// bdOnPath is the unresolved path a PATH lookup returns and the check
	// reports. They differ where the temp dir sits under a symlink, as
	// macOS's /var -> /private/var does.
	bd       string
	bdOnPath string
	execLog  string
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
		binDir:   filepath.Join(t.TempDir(), "bin"),
		bdInfo:   map[string]*debug.BuildInfo{},
		reads:    map[string]int{},
		gcInfo:   gcBuildInfoLinkingBeads(),
		gcInfoOK: true,
	}
	// PATH reaches the fake bd through a symlinked directory, as it does
	// under macOS's /var -> /private/var, so the path the check reports and
	// the resolved path differ on every platform.
	if err := os.Symlink(t.TempDir(), f.binDir); err != nil {
		t.Fatal(err)
	}
	f.execLog = filepath.Join(t.TempDir(), "bd-exec.log")
	if err := os.WriteFile(filepath.Join(f.city, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.bdOnPath = filepath.Join(f.binDir, "bd")
	f.bd = f.writeBd(t, f.bdOnPath)
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

// TestBdBeadsVersionCheckWarnsOnAnOlderBd: the bd the city scope resolves
// from PATH is older than the beads gc links. The warning names the scope,
// the bd as PATH resolves it, both versions and the risk, and the fix hint
// names the bd version required rather than a make target in a checkout
// release users do not have.
func TestBdBeadsVersionCheckWarnsOnAnOlderBd(t *testing.T) {
	f := newBdBeadsVersionFixture(t)
	f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestOlder)
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	r := f.run(t, cfg)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %+v", r.Status, r)
	}
	details := strings.Join(r.Details, "\n")
	for _, want := range []string{"city", f.bdOnPath, bdBeadsVersionTestOlder, bdBeadsVersionTestLinked, "older than", "refuses to read"} {
		if !strings.Contains(details, want) {
			t.Errorf("details lack %q:\n%s", want, details)
		}
		if !strings.Contains(r.Message, want) {
			t.Errorf("message lacks %q: %s", want, r.Message)
		}
	}
	for _, want := range []string{"install bd " + bdBeadsVersionTestLinked, "newer release of the same major version", "BD_BIN"} {
		if !strings.Contains(r.FixHint, want) {
			t.Errorf("fix hint lacks %q: %q", want, r.FixHint)
		}
	}
	if strings.Contains(r.FixHint, "make install-bd") {
		t.Errorf("fix hint names make install-bd, a target in a checkout release users lack: %q", r.FixHint)
	}
	if r.Name != "bd-beads-version" {
		t.Errorf("name = %q", r.Name)
	}
}

// TestBdBeadsVersionCheckAcceptsCompatibleBds: by the policy the native-store
// preflight applies (gastownhall/gascity#5164), a newer release of the linked
// major version and a prerelease of the same release are compatible, so the
// check is quiet about them.
func TestBdBeadsVersionCheckAcceptsCompatibleBds(t *testing.T) {
	for _, tc := range []struct {
		name, bd, linked string
	}{
		{"newer patch release", "v1.3.5", "v1.3.4"},
		{"newer minor prerelease", "v1.4.0-rc.2", "v1.3.4"},
		{"prerelease of the same release", "v1.4.0-rc.1", "v1.4.0-rc.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBdBeadsVersionFixture(t)
			f.bdInfo[f.bd] = bdBuildInfoWithBeads(tc.bd)
			f.gcInfo.Deps[0].Version = tc.linked

			r := f.run(t, &config.City{Workspace: config.Workspace{Name: "demo"}})
			if r.Status != doctor.StatusOK {
				t.Fatalf("status = %v, want OK for bd %s against gc's %s: %+v", r.Status, tc.bd, tc.linked, r)
			}
			if !strings.Contains(r.Message, "compatible with the "+tc.linked) {
				t.Fatalf("message does not say the bd is compatible with %s: %q", tc.linked, r.Message)
			}
		})
	}
}

// TestBdBeadsVersionCheckWarnsOnANewerMajorBd: semver does not promise a
// newer major version is compatible, and the preflight's policy does not
// accept one either.
func TestBdBeadsVersionCheckWarnsOnANewerMajorBd(t *testing.T) {
	f := newBdBeadsVersionFixture(t)
	f.bdInfo[f.bd] = bdBuildInfoWithBeads("v2.0.0")

	r := f.run(t, &config.City{Workspace: config.Workspace{Name: "demo"}})
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %+v", r.Status, r)
	}
	for _, want := range []string{"v2.0.0", "newer major version", bdBeadsVersionTestLinked} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("message lacks %q: %s", want, r.Message)
		}
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
	f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestOlder)
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

// TestBdBeadsVersionCheckNamesTheRigWithAnOlderBd: only the rig whose own bd
// is older is named; the city's matching bd is not.
func TestBdBeadsVersionCheckNamesTheRigWithAnOlderBd(t *testing.T) {
	f := newBdBeadsVersionFixture(t)
	f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestLinked)
	rigBd := f.writeBd(t, filepath.Join(t.TempDir(), "bd"))
	f.bdInfo[rigBd] = bdBuildInfoWithBeads(bdBeadsVersionTestOlder)
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
	gcWithoutBeads := &debug.BuildInfo{Main: debug.Module{Path: "github.com/gastownhall/gascity", Version: "(devel)"}}
	gcReplacedBeads := gcBuildInfoLinkingBeads()
	gcReplacedBeads.Deps[0].Replace = &debug.Module{Path: "github.com/fork/beads", Version: "v1.3.9"}
	gcPseudoBeads := gcBuildInfoLinkingBeads()
	gcPseudoBeads.Deps[0].Version = "v1.3.5-0.20261101120000-abcdef123456"

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
			reason: "beads version unknown: source build",
		},
		{
			name: "bd with an empty version",
			setup: func(_ *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads("")
			},
			reason: "beads version unknown: source build",
		},
		{
			// Since Go 1.24 a build from a modified checkout says +dirty.
			name: "bd built from a modified checkout",
			setup: func(_ *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads("v0.0.0-20261107052026-05ebdd78fd3c+dirty")
			},
			reason: "beads version unknown: modified source build",
		},
		{
			name: "bd at a pseudo-version",
			setup: func(_ *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads("v1.3.5-0.20261101120000-abcdef123456")
			},
			reason: "beads version unknown: pseudo-version",
		},
		{
			name: "bd built from another main module",
			setup: func(_ *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = &debug.BuildInfo{Main: debug.Module{Path: "example.com/bdwrapper", Version: "v1.0.0"}}
			},
			reason: "beads version unknown: no beads module",
		},
		{
			name: "gc without build info",
			setup: func(_ *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestOlder)
				f.gcInfo, f.gcInfoOK = nil, false
			},
			reason: "gc was built without module build info",
		},
		{
			name: "gc linking no beads module",
			setup: func(_ *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestOlder)
				f.gcInfo = gcWithoutBeads
			},
			reason: "the beads gc links names no release (no beads module)",
		},
		{
			name: "gc linking a replaced beads module",
			setup: func(_ *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestOlder)
				f.gcInfo = gcReplacedBeads
			},
			reason: "the beads gc links names no release (replaced module)",
		},
		{
			name: "gc linking beads at a pseudo-version",
			setup: func(_ *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestOlder)
				f.gcInfo = gcPseudoBeads
			},
			reason: "the beads gc links names no release (pseudo-version)",
		},
		{
			name: "no bd-backed scope",
			setup: func(t *testing.T, f *bdBeadsVersionFixture) {
				f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestOlder)
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
	f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestOlder)

	r := f.run(t, nil)
	if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "no city config") {
		t.Fatalf("result = %+v, want OK naming the missing city config", r)
	}
}

// TestStartWarmupStderrNamesBdBeadsVersionSkew: gc start's warm-up runs the
// check, and its stderr names bd-beads-version when bd is skewed. The
// warm-up stays fail-open: it returns normally and start carries on.
func TestStartWarmupStderrNamesBdBeadsVersionSkew(t *testing.T) {
	f := newBdBeadsVersionFixture(t)
	f.bdInfo[f.bd] = bdBuildInfoWithBeads(bdBeadsVersionTestOlder)
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_MAIL", "fake")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	var stderr strings.Builder
	runStartWarmup(f.city, cfg, nil, &stderr)
	if !strings.Contains(stderr.String(), "bd-beads-version") {
		t.Fatalf("warm-up stderr does not name bd-beads-version:\n%s", stderr.String())
	}
}
