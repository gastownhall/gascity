package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// This file exercises the bd-beads-version doctor check (ga-upttvz): gc
// migrates a city store forward to the schema of the beads module it links,
// so a scope whose bd was built from another beads version fails every read
// with "schema version mismatch". The check compares the beads module
// embedded in each bd-backed scope's resolved bd with gc's own — reading the
// binaries' build info, never executing them — and warns on skew with the
// make install-bd remedy.

// bdBeadsVersionFixture is a temp city with a bd-backed store and a PATH that
// holds exactly the bd the test places there.
type bdBeadsVersionFixture struct {
	city   string
	cfg    *config.City
	binDir string
}

func newBdBeadsVersionFixture(t *testing.T) *bdBeadsVersionFixture {
	t.Helper()
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", binDir, err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("GC_BEADS", "")
	return &bdBeadsVersionFixture{
		city:   city,
		cfg:    &config.City{Workspace: config.Workspace{Name: "demo"}},
		binDir: binDir,
	}
}

// bdBeadsModInfo is the module-info text of a bd binary built from the given
// beads version.
func bdBeadsModInfo(version string) string {
	return "path\tgithub.com/steveyegge/beads/cmd/bd\nmod\tgithub.com/steveyegge/beads\t" + version + "\t\n"
}

// writeBuildinfoBd places a fake Go binary at path whose build info names
// main module beads at version; readGoBuildInfo parses it like a real bd.
func writeBuildinfoBd(t *testing.T, path, version string) string {
	t.Helper()
	src := fakeGoBinary(t, "go1.26.10", bdBeadsModInfo(version))
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fake bd: %v", err)
	}
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// gcBuildInfoWithBeads is gc's own build info when it links the given beads
// version.
func gcBuildInfoWithBeads(version string) *debug.BuildInfo {
	return &debug.BuildInfo{
		Path: "github.com/gastownhall/gascity/cmd/gc",
		Main: debug.Module{Path: "github.com/gastownhall/gascity", Version: "v0.9.0"},
		Deps: []*debug.Module{{Path: "github.com/steveyegge/beads", Version: version}},
	}
}

// ownBeadsVersion is the beads module every test's gc links; a skew test's bd
// embeds something else, so the two sides genuinely differ.
const ownBeadsVersion = "v1.3.4"

// ownBeads pins the check's view of gc to ownBeadsVersion.
func ownBeads(check *bdBeadsVersionCheck) {
	check.readOwnBuildInfo = func() (*debug.BuildInfo, bool) { return gcBuildInfoWithBeads(ownBeadsVersion), true }
}

func bdBeadsVersionCheckResult(t *testing.T, check *bdBeadsVersionCheck, city string) *doctor.CheckResult {
	t.Helper()
	r := check.Run(&doctor.CheckContext{CityPath: city})
	if r == nil {
		t.Fatal("the check returned no result")
	}
	return r
}

// TestBdBeadsVersionCheckWarnsOnSkew is AC-5: a bd-backed scope resolving a
// bd built from another beads module than gc links is a warning naming the
// scope, the bd path, both versions and the make install-bd remedy.
func TestBdBeadsVersionCheckWarnsOnSkew(t *testing.T) {
	f := newBdBeadsVersionFixture(t)
	bd := writeBuildinfoBd(t, filepath.Join(f.binDir, "bd"), "v1.2.0")

	check := newBdBeadsVersionCheck(f.city, f.cfg)
	ownBeads(check)
	r := bdBeadsVersionCheckResult(t, check, f.city)

	if r.Status != doctor.StatusWarning {
		t.Fatalf("a skewed bd reported %v, want a warning: %+v", r.Status, r)
	}
	if r.Severity != doctor.SeverityAdvisory {
		t.Fatalf("the warning gates consumers (%v); version skew is advisory", r.Severity)
	}
	details := strings.Join(r.Details, "\n")
	for _, want := range []string{f.city, bd, "v1.2.0", ownBeadsVersion} {
		if !strings.Contains(details, want) {
			t.Errorf("warning details lack %q:\n%s", want, details)
		}
	}
	if !strings.Contains(r.FixHint, "make install-bd") {
		t.Errorf("fix hint %q does not name make install-bd", r.FixHint)
	}
}

// TestBdBeadsVersionCheckOKWhenVersionsMatch is AC-5's other half: a bd
// embedding gc's own beads version is OK, not a warning.
func TestBdBeadsVersionCheckOKWhenVersionsMatch(t *testing.T) {
	f := newBdBeadsVersionFixture(t)
	writeBuildinfoBd(t, filepath.Join(f.binDir, "bd"), "v1.3.4")

	check := newBdBeadsVersionCheck(f.city, f.cfg)
	ownBeads(check)
	r := bdBeadsVersionCheckResult(t, check, f.city)

	if r.Status != doctor.StatusOK {
		t.Fatalf("a matching bd reported %v, want OK: %+v", r.Status, r)
	}
	if !strings.Contains(r.Message, ownBeadsVersion) {
		t.Errorf("the OK message should name the agreeing version: %q", r.Message)
	}
}

func TestBdBeadsVersionCheckRegistration(t *testing.T) {
	check := newBdBeadsVersionCheck(t.TempDir(), &config.City{})
	if got := check.Name(); got != "bd-beads-version" {
		t.Fatalf("check name = %q, want bd-beads-version", got)
	}
	if !check.WarmupEligible() {
		t.Fatal("bd-beads-version must be warm-up eligible so gc start warns too")
	}
	if check.CanFix() {
		t.Fatal("installing a matching bd is the operator's make install-bd, not a doctor fix")
	}
}

// TestBdBeadsVersionCheckReadsEachBinaryOnce: a city whose rigs all resolve
// one bd (here through a PATH symlink) reads that binary's build info once,
// not once per scope.
func TestBdBeadsVersionCheckReadsEachBinaryOnce(t *testing.T) {
	f := newBdBeadsVersionFixture(t)
	realBD := writeBuildinfoBd(t, filepath.Join(f.binDir, "real-bd"), "v1.3.4")
	linked := filepath.Join(f.binDir, "bd")
	if err := os.Symlink(realBD, linked); err != nil {
		t.Fatalf("symlink %s: %v", linked, err)
	}
	f.cfg.Rigs = []config.Rig{
		{Name: "rig-a", Path: "rigs/a"},
		{Name: "rig-b", Path: "rigs/b"},
	}

	check := newBdBeadsVersionCheck(f.city, f.cfg)
	ownBeads(check)
	reads := 0
	check.readBinaryBuildInfo = func(path string) (*debug.BuildInfo, error) {
		reads++
		return readGoBuildInfo(path)
	}
	r := bdBeadsVersionCheckResult(t, check, f.city)

	if r.Status != doctor.StatusOK {
		t.Fatalf("a city of matching bds reported %v: %+v", r.Status, r)
	}
	if reads != 1 {
		t.Fatalf("the check read the shared bd %d time(s) across 3 scopes, want 1", reads)
	}
}

// TestBdBeadsVersionCheckNamesTheSkewedRig: a rig that overrides the city
// backend keeps the ambient bd lookup while the city resolves its pinned bd,
// so the two can genuinely differ; the scope that differs is named.
func TestBdBeadsVersionCheckNamesTheSkewedRig(t *testing.T) {
	f := newBdBeadsVersionFixture(t)
	pinned := writeBuildinfoBd(t, filepath.Join(f.binDir, "pinned-bd"), "v1.3.4")
	ambient := writeBuildinfoBd(t, filepath.Join(f.binDir, "bd"), "v1.2.0")
	cityTOML := fmt.Sprintf("[workspace]\nname = \"demo\"\n\n[workspace.env]\nBD_BIN = %q\n", pinned)
	if err := os.WriteFile(filepath.Join(f.city, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	rigRoot := filepath.Join(f.city, "rigs", "skew-rig")
	if err := os.MkdirAll(filepath.Join(rigRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir rig .beads: %v", err)
	}
	// doltlite is a registered backend the rig can genuinely override with
	// (an unregistered name like sqlite fails LoadMetadataState and the rig
	// silently inherits the city pin); naming any backend keeps .beads
	// metadata.json present, so the scope still uses the bd store contract.
	if err := os.WriteFile(filepath.Join(rigRoot, ".beads", "metadata.json"), []byte(`{"backend":"doltlite"}`), 0o644); err != nil {
		t.Fatalf("write rig metadata: %v", err)
	}
	f.cfg.Rigs = []config.Rig{{Name: "skew-rig", Path: "rigs/skew-rig"}}

	check := newBdBeadsVersionCheck(f.city, f.cfg)
	ownBeads(check)
	r := bdBeadsVersionCheckResult(t, check, f.city)

	if r.Status != doctor.StatusWarning {
		t.Fatalf("a rig on another beads version reported %v, want a warning: %+v", r.Status, r)
	}
	details := strings.Join(r.Details, "\n")
	for _, want := range []string{"skew-rig", ambient, "v1.2.0", ownBeadsVersion} {
		if !strings.Contains(details, want) {
			t.Errorf("warning details lack %q:\n%s", want, details)
		}
	}
	if strings.Contains(details, pinned) {
		t.Errorf("the city's pinned bd is named as skewed although it matches:\n%s", details)
	}
}

// writeRecordingShimBD places a bd on PATH that records any execution at
// marker and answers nothing: the check must read build info without ever
// running bd, so the marker staying absent is the proof.
func writeRecordingShimBD(t *testing.T, path, marker string) {
	t.Helper()
	script := "#!/bin/sh\n: > " + marker + "\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write shim bd %s: %v", path, err)
	}
}

// TestBdBeadsVersionCheckUnknownSidesStayOK is AC-6: when either side's beads
// version cannot be named — no bd resolved, a shim or non-Go bd, a (devel) or
// replaced build, gc without module info — or the city has no bd-backed scope,
// the check reports OK with the reason, never a warning or error, and never
// executes bd.
func TestBdBeadsVersionCheckUnknownSidesStayOK(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T, f *bdBeadsVersionFixture) (marker string)
		wantTrace string
	}{
		{
			name: "no bd on PATH",
			setup: func(_ *testing.T, _ *bdBeadsVersionFixture) string {
				return ""
			},
			wantTrace: "no bd resolved",
		},
		{
			name: "shim bd that would record execution",
			setup: func(t *testing.T, f *bdBeadsVersionFixture) string {
				marker := filepath.Join(t.TempDir(), "executed")
				writeRecordingShimBD(t, filepath.Join(f.binDir, "bd"), marker)
				return marker
			},
			wantTrace: "no readable Go build info",
		},
		{
			name: "bd built from source at (devel)",
			setup: func(t *testing.T, f *bdBeadsVersionFixture) string {
				writeBuildinfoBd(t, filepath.Join(f.binDir, "bd"), "(devel)")
				return ""
			},
			wantTrace: "embeds no comparable beads version",
		},
		{
			name: "gc without module build info",
			setup: func(t *testing.T, f *bdBeadsVersionFixture) string {
				writeBuildinfoBd(t, filepath.Join(f.binDir, "bd"), "v1.3.4")
				return ""
			},
			wantTrace: "no module build info",
		},
		{
			name: "gc at (devel)",
			setup: func(t *testing.T, f *bdBeadsVersionFixture) string {
				writeBuildinfoBd(t, filepath.Join(f.binDir, "bd"), "v1.3.4")
				return ""
			},
			wantTrace: "unknown",
		},
		{
			name: "gc links a replaced beads module",
			setup: func(t *testing.T, f *bdBeadsVersionFixture) string {
				writeBuildinfoBd(t, filepath.Join(f.binDir, "bd"), "v1.3.4")
				return ""
			},
			wantTrace: "replaced",
		},
		{
			name: "file-backed city with no bd-backed scope",
			setup: func(t *testing.T, f *bdBeadsVersionFixture) string {
				cityTOML := "[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"file\"\n"
				if err := os.WriteFile(filepath.Join(f.city, "city.toml"), []byte(cityTOML), 0o644); err != nil {
					t.Fatalf("write city.toml: %v", err)
				}
				marker := filepath.Join(t.TempDir(), "executed")
				writeRecordingShimBD(t, filepath.Join(f.binDir, "bd"), marker)
				return marker
			},
			wantTrace: "no scope uses the bd store contract",
		},
		{
			name: "two scopes sharing one unreadable shim keep the shim's reason",
			setup: func(t *testing.T, f *bdBeadsVersionFixture) string {
				f.cfg.Rigs = []config.Rig{
					{Name: "rig-a", Path: "rigs/a"},
					{Name: "rig-b", Path: "rigs/b"},
				}
				marker := filepath.Join(t.TempDir(), "executed")
				writeRecordingShimBD(t, filepath.Join(f.binDir, "bd"), marker)
				return marker
			},
			wantTrace: "no readable Go build info",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newBdBeadsVersionFixture(t)
			marker := tc.setup(t, f)

			check := newBdBeadsVersionCheck(f.city, f.cfg)
			switch tc.name {
			case "gc without module build info":
				check.readOwnBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
			case "gc at (devel)":
				check.readOwnBuildInfo = func() (*debug.BuildInfo, bool) {
					return gcBuildInfoWithBeads("(devel)"), true
				}
			case "gc links a replaced beads module":
				check.readOwnBuildInfo = func() (*debug.BuildInfo, bool) {
					info := gcBuildInfoWithBeads("v1.3.4")
					info.Deps[0].Replace = &debug.Module{Path: "../beads", Version: "v0.0.0"}
					return info, true
				}
			default:
				ownBeads(check)
			}
			r := bdBeadsVersionCheckResult(t, check, f.city)

			if r.Status != doctor.StatusOK {
				t.Fatalf("an unknown side reported %v, want OK: %+v", r.Status, r)
			}
			if r.Severity != doctor.SeverityAdvisory {
				t.Fatalf("an unknown side carried severity %v; the check never gates anything", r.Severity)
			}
			if !strings.Contains(r.Message, tc.wantTrace) {
				t.Errorf("OK message %q does not carry the reason (%q)", r.Message, tc.wantTrace)
			}
			if marker != "" {
				if _, err := os.Stat(marker); err == nil {
					t.Error("the check executed bd to learn its version; build info must be read from the file")
				}
			}
			if tc.name == "two scopes sharing one unreadable shim keep the shim's reason" {
				if got := strings.Count(r.Message, "no readable Go build info"); got != 3 {
					t.Errorf("OK message names the shim reason %d time(s) across 3 scopes, want 3:\n%s", got, r.Message)
				}
			}
		})
	}
}
