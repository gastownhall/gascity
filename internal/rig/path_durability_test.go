package rig

import (
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
