package rig

import (
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/pathdurability"
)

// TestRigPathDurabilityFindingAcceptsDurablePaths is the regression direction:
// the classifications a healthy city produces must register without complaint.
func TestRigPathDurabilityFindingAcceptsDurablePaths(t *testing.T) {
	for _, rigPath := range []string{
		"/city/rigs/backend",
		"/city/rigs/frontend",
		"/city/project",
		"/city/project2",
	} {
		t.Run(rigPath, func(t *testing.T) {
			res := pathdurability.Result{Class: pathdurability.CityDevice, Probed: rigPath}
			warning, err := rigPathDurabilityFinding(res, "/city", rigPath, false)
			if err != nil {
				t.Fatalf("rigPathDurabilityFinding(%q) returned error %v, want nil", rigPath, err)
			}
			if warning != "" {
				t.Fatalf("rigPathDurabilityFinding(%q) warned %q, want silence", rigPath, warning)
			}
		})
	}
}

// TestRigPathDurabilityFindingRefusesEphemeralPaths is the guard direction.
func TestRigPathDurabilityFindingRefusesEphemeralPaths(t *testing.T) {
	res := pathdurability.Result{Class: pathdurability.Ephemeral, Filesystem: "tmpfs", Probed: "/tmp"}
	warning, err := rigPathDurabilityFinding(res, "/city", "/tmp/adopt", false)
	if err == nil {
		t.Fatal("rigPathDurabilityFinding(/tmp/adopt) returned nil error, want a refusal")
	}
	if warning != "" {
		t.Fatalf("a refusal must not also warn; got %q", warning)
	}
	for _, want := range []string{"/tmp/adopt", "tmpfs", "--allow-ephemeral"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not mention %q", err.Error(), want)
		}
	}
}

// TestRigPathDurabilityFindingHonoursAllowEphemeral proves the escape hatch
// downgrades the refusal rather than silencing it.
func TestRigPathDurabilityFindingHonoursAllowEphemeral(t *testing.T) {
	res := pathdurability.Result{Class: pathdurability.Ephemeral, Filesystem: "tmpfs", Probed: "/tmp"}
	warning, err := rigPathDurabilityFinding(res, "/city", "/tmp/adopt", true)
	if err != nil {
		t.Fatalf("--allow-ephemeral still refused: %v", err)
	}
	if !strings.Contains(warning, "/tmp/adopt") || !strings.Contains(warning, "tmpfs") {
		t.Fatalf("warning %q does not name the path and filesystem", warning)
	}
}

// TestRigPathDurabilityFindingWarnsButAcceptsOtherDevices covers a second
// durable mount: gc cannot prove it survives, so it names the blind spot rather
// than refusing a legitimate second PVC, NFS share, or second disk.
func TestRigPathDurabilityFindingWarnsButAcceptsOtherDevices(t *testing.T) {
	res := pathdurability.Result{Class: pathdurability.OtherDevice, Probed: "/data/projects/rig"}
	warning, err := rigPathDurabilityFinding(res, "/city", "/data/projects/rig", false)
	if err != nil {
		t.Fatalf("a different durable device must not be refused: %v", err)
	}
	if !strings.Contains(warning, "/data/projects/rig") {
		t.Fatalf("warning %q does not name the path", warning)
	}
}

// TestRigPathDurabilityFindingIsSilentWhenUnprobeable proves the check fails
// open: a platform or mount the probe cannot read must never block a rig add.
func TestRigPathDurabilityFindingIsSilentWhenUnprobeable(t *testing.T) {
	res := pathdurability.Result{Class: pathdurability.Unknown, Probed: "/tmp/adopt"}
	warning, err := rigPathDurabilityFinding(res, "/city", "/tmp/adopt", false)
	if err != nil {
		t.Fatalf("unprobeable path was refused: %v", err)
	}
	if warning != "" {
		t.Fatalf("unprobeable path warned %q, want silence", warning)
	}
}

// TestCheckRigPathDurabilityJudgesRelativePathsAgainstTheCity exercises the real
// probe rather than a synthetic Result. Every other test in this file hands
// rigPathDurabilityFinding a verdict directly, which is exactly why a bug in how
// the path reaches the probe could survive them all: a rig path relative to the
// city was classified against the process working directory.
//
// This is defense in depth, not a reproduction of a reachable failure. Provision
// cannot deliver a relative path here — validateRequest rejects a non-absolute
// ProvisionRequest.Path before the durability check runs, and the CLI resolves a
// bare relative argument against the city before that (resolveRigAddPath). The
// caller that did reach Classify with a relative path is the boot audit in
// internal/config, which reads rig paths verbatim from city.toml. This test
// calls checkRigPathDurability directly, below validateRequest, so the guard
// stays honest if a future caller loses that resolution.
//
// The working directory is moved onto a filesystem the probe calls ephemeral, so
// the pre-fix behavior is a hard refusal and the post-fix behavior is silence.
func TestCheckRigPathDurabilityJudgesRelativePathsAgainstTheCity(t *testing.T) {
	cityPath := t.TempDir()

	// /dev/shm is tmpfs on Linux, which is what makes this discriminating. If it
	// is absent or shares a device with the city, the test cannot tell the two
	// behaviors apart, so skip loudly rather than pass for the wrong reason.
	ephemeralDir, err := os.MkdirTemp("/dev/shm", "gc-rig-cwd-*")
	if err != nil {
		t.Skipf("no usable tmpfs to run from: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(ephemeralDir); err != nil {
			t.Logf("removing %s: %v", ephemeralDir, err)
		}
	})
	if got := pathdurability.Classify(cityPath, ephemeralDir).Class; got != pathdurability.Ephemeral {
		t.Skipf("working directory classifies as %q, not %q; the relative-path bug is invisible here", got, pathdurability.Ephemeral)
	}
	t.Chdir(ephemeralDir)

	warning, err := checkRigPathDurability(cityPath, "rigs/backend", false)
	if err != nil {
		t.Fatalf("a rig path relative to the city was refused: %v", err)
	}
	if warning != "" {
		t.Fatalf("a rig path relative to the city warned %q, want silence", warning)
	}
}
