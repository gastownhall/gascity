//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathdurability"
)

// deviceOf returns the filesystem device path lives on, read straight from the
// kernel. It is deliberately not routed through internal/pathdurability: these
// tests use it to establish their own precondition, and a precondition proved
// with the code under test cannot detect that code breaking.
func deviceOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %q: no syscall.Stat_t available", path)
	}
	return st.Dev
}

// ephemeralRigPath returns a path on a filesystem that cannot survive a restart,
// on a different device from cityPath. /dev/shm is tmpfs on every Linux host and
// needs no privilege.
//
// The precondition is established from the kernel (does /dev/shm exist, is it a
// different device from the city) rather than from pathdurability.Classify. An
// earlier version asked Classify itself and skipped when it disagreed, which
// meant gutting Classify retired this whole file silently and turned the package
// green. Once the precondition holds, a Classify that does not say Ephemeral is
// the bug these tests exist to catch, so it fails loudly instead.
func ephemeralRigPath(t *testing.T, cityPath string) string {
	t.Helper()
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skipf("no /dev/shm on this host: %v", err)
	}
	shmDev, cityDev := deviceOf(t, "/dev/shm"), deviceOf(t, cityPath)
	if shmDev == cityDev {
		t.Skipf("/dev/shm and the city dir are the same device (%d); this host cannot express a rig on a separate ephemeral device", shmDev)
	}

	// A unique parent per test run: the rig path itself must not exist (the
	// refusal test asserts nothing was created), and a fixed name collides
	// across concurrent `go test` runs on one host.
	base, err := os.MkdirTemp("/dev/shm", "gc-rig-durability-")
	if err != nil {
		t.Fatalf("creating scratch dir under /dev/shm: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	rigPath := filepath.Join(base, "rig")

	if got := pathdurability.Classify(cityPath, rigPath); got.Class != pathdurability.Ephemeral {
		t.Fatalf("Classify(%q, %q).Class = %q, want %q: /dev/shm is a separate device from the city, so the durability probe must see it as ephemeral",
			cityPath, rigPath, got.Class, pathdurability.Ephemeral)
	}
	return rigPath
}

func newRigAddCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "durability-city", "[workspace]\n", "")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")
	return cityPath
}

// TestRigAddRefusesNonPersistentPath is the guard direction, end to end through
// the CLI entry point: registering a rig somewhere that dies with the container
// must fail before anything is written.
func TestRigAddRefusesNonPersistentPath(t *testing.T) {
	cityPath := newRigAddCity(t)
	rigPath := ephemeralRigPath(t, cityPath)

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doRigAdd accepted a non-persistent rig path; stdout: %s", stdout.String())
	}
	msg := stderr.String()
	for _, want := range []string{rigPath, "tmpfs", "--allow-ephemeral"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q does not mention %q", msg, want)
		}
	}
	// The refusal must land before any mutation: nothing may be created.
	if _, err := os.Stat(rigPath); err == nil {
		t.Fatalf("rig directory %s was created despite the refusal", rigPath)
	}
	cityToml, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	if strings.Contains(string(cityToml), filepath.Base(filepath.Dir(rigPath))) {
		t.Fatalf("city.toml records the refused rig:\n%s", cityToml)
	}
}

// TestRigAddAcceptsPathOnTheCityDevice is control 1: the same operation on a
// path that shares the city's device must still succeed. Without this, a guard
// that refuses everything would look identical to a guard that works.
func TestRigAddAcceptsPathOnTheCityDevice(t *testing.T) {
	cityPath := newRigAddCity(t)
	rigPath := filepath.Join(cityPath, "rigs", "durable-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd refused a city-rooted rig path: %s", stderr.String())
	}
	combined := stdout.String() + stderr.String()
	for _, unwanted := range []string{"does not survive", "will not survive", "different filesystem"} {
		if strings.Contains(combined, unwanted) {
			t.Fatalf("city-rooted rig path drew durability output %q:\n%s", unwanted, combined)
		}
	}
}

// TestRigAddAllowEphemeralOptsIn is control 2: the escape hatch downgrades the
// refusal to a warning rather than silencing the finding.
func TestRigAddAllowEphemeralOptsIn(t *testing.T) {
	cityPath := newRigAddCity(t)
	rigPath := ephemeralRigPath(t, cityPath)

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr,
		withAllowEphemeralPath(true))
	if code != 0 {
		t.Fatalf("--allow-ephemeral still refused: %s", stderr.String())
	}
	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, "will not survive") {
		t.Fatalf("--allow-ephemeral suppressed the warning entirely:\n%s", combined)
	}
}

// TestStartBootsCityWithAllowedEphemeralRig is the end-to-end control for the
// audit's warn-only contract, over real config text rather than a literal.
//
// `gc rig add --allow-ephemeral` and `gc start` are the two halves of one
// workflow, and nothing in either half's own tests notices when they disagree.
// They did: the boot audit's warning rides Provenance.Warnings into
// splitStrictConfigWarnings, which treats any warning it does not recognize as
// fatal under strict mode (on by default), so `gc rig add --allow-ephemeral`
// exited 0 and the very next `gc start` exited 1 on the city it had just
// created — with --no-strict hidden and refused outside the legacy standalone
// path.
func TestStartBootsCityWithAllowedEphemeralRig(t *testing.T) {
	cityPath := newRigAddCity(t)
	rigPath := ephemeralRigPath(t, cityPath)

	var stdout, stderr bytes.Buffer
	if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr,
		withAllowEphemeralPath(true)); code != 0 {
		t.Fatalf("gc rig add --allow-ephemeral failed: %s", stderr.String())
	}

	_, prov, err := loadStartCityConfig(cityPath)
	if err != nil {
		t.Fatalf("loadStartCityConfig: %v", err)
	}

	// Non-vacuous: if the audit produced no warning at all, the split below
	// would pass while proving nothing.
	var durability []string
	for _, w := range prov.Warnings {
		if strings.Contains(w, rigPath) {
			durability = append(durability, w)
		}
	}
	if len(durability) == 0 {
		t.Fatalf("the boot audit said nothing about %s; warnings = %q", rigPath, prov.Warnings)
	}

	fatal, nonFatal := splitStrictConfigWarnings(prov.Warnings)
	for _, w := range fatal {
		if strings.Contains(w, rigPath) {
			t.Fatalf("gc start refuses to boot the city gc rig add --allow-ephemeral just created: %q", w)
		}
	}
	for _, w := range durability {
		found := false
		for _, n := range nonFatal {
			if n == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("durability warning went missing from the strict split instead of staying a warning: %q", w)
		}
	}
}
