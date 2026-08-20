package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathdurability"
)

// ephemeralRigPath returns a path on a filesystem that cannot survive a restart,
// on a different device from cityPath. /dev/shm is tmpfs on every Linux host and
// needs no privilege. The test skips when the host cannot provide that shape.
func ephemeralRigPath(t *testing.T, cityPath string) string {
	t.Helper()
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("no /dev/shm on this host")
	}
	rigPath := filepath.Join("/dev/shm", "gc-rig-durability-"+t.Name())
	if got := pathdurability.Classify(cityPath, rigPath); got.Class != pathdurability.Ephemeral {
		t.Skipf("host does not present /dev/shm as a separate ephemeral device (got %q)", got.Class)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rigPath) })
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
	if strings.Contains(string(cityToml), "gc-rig-durability") {
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
