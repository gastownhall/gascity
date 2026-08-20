package config

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathdurability"
)

// classifyRigPathDurability is the injectable durability probe. Tests replace it
// to drive each verdict without depending on the host's mount layout.
var classifyRigPathDurability = pathdurability.Classify

// nonPersistentRigPathWarnings audits the rig paths a city is about to use and
// names any that cannot survive a restart of this machine or container.
//
// This is the boot-time counterpart to the registration guard: a city can carry
// a bad binding that predates the guard, or that was inherited from another
// machine, and the operator should learn about it from a warning rather than
// from the rig content being gone after the next restart. It only ever warns —
// refusing to load a city because one rig path looks doomed would brick a
// running city that a warning could have saved.
//
// Only the ephemeral verdict is reported. A rig on some other durable-looking
// device is flagged once at registration; repeating that on every config load
// would only train operators to ignore the warning that matters.
func nonPersistentRigPathWarnings(fs fsys.FS, cityRoot string, rigs []Rig) []string {
	// The probe reads the real filesystem, so it can only answer for a config
	// being loaded from the real filesystem. Against a synthetic FS the paths do
	// not exist as written and the host would be asked about the wrong device.
	if !isRealFilesystem(fs) {
		return nil
	}
	var warnings []string
	for _, rig := range rigs {
		if rig.Path == "" {
			continue
		}
		res := classifyRigPathDurability(cityRoot, rig.Path)
		if res.Class != pathdurability.Ephemeral {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"rig %q is bound to %s, which is on %s and does not survive a restart of this machine or container: "+
				"the rig content is lost on the next restart. Move it under the city directory (%s) and re-register it with `gc rig add`",
			rig.Name, rig.Path, res.Filesystem, cityRoot,
		))
	}
	return warnings
}

// isRealFilesystem reports whether fs is the host filesystem rather than a test
// double.
func isRealFilesystem(fs fsys.FS) bool {
	switch fs.(type) {
	case fsys.OSFS, *fsys.OSFS:
		return true
	default:
		return false
	}
}
