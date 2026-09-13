package rig

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/pathdurability"
)

// checkRigPathDurability decides whether a rig may be registered at rigPath.
//
// A rig directory that cannot outlive the process registering it is a trap: the
// registration succeeds, the city looks healthy, and the rig content vanishes
// the next time the container is replaced — with no remote and no recovery.
// Refusing at registration is the only point where that is still cheap.
//
// It returns a warning to surface, or a fatal error whose text is exactly what
// the CLI prints after the "gc rig add: " prefix.
func checkRigPathDurability(cityPath, rigPath string, allowEphemeral bool) (warning string, err error) {
	return rigPathDurabilityFinding(pathdurability.Classify(cityPath, rigPath), cityPath, rigPath, allowEphemeral)
}

// rigPathDurabilityFinding turns a durability verdict into the operator-facing
// outcome. It is split from the probe so every branch is testable without
// syscalls or a particular host mount layout.
func rigPathDurabilityFinding(res pathdurability.Result, cityPath, rigPath string, allowEphemeral bool) (warning string, err error) {
	switch res.Class {
	case pathdurability.Ephemeral:
		if allowEphemeral {
			return fmt.Sprintf(
				"%s is on %s and will not survive a restart of this machine or container; --allow-ephemeral was given, so the rig is registered anyway",
				rigPath, res.Filesystem,
			), nil
		}
		return "", fmt.Errorf(
			"%s is on %s, which does not survive a restart of this machine or container: a rig registered there is lost with no way to recover it. "+
				"Register the rig under the city directory (%s) instead, or pass --allow-ephemeral to accept the loss",
			rigPath, res.Filesystem, cityPath,
		)
	case pathdurability.OtherDevice:
		return fmt.Sprintf(
			"%s is on a different filesystem from the city (%s); gc cannot verify it survives a restart of this machine or container",
			rigPath, cityPath,
		), nil
	default:
		// CityDevice is durable by construction; Unknown means the probe could
		// not run, and a check that could not run must never block a rig add.
		return "", nil
	}
}
