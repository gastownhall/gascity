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
