package rollout

import "github.com/gastownhall/gascity/internal/rollout/gate"

// Mode is the tri-state value kind for a correctness/migration rollout gate.
// The definition lives in the dependency-leaf gate package (see its package
// doc for the config→orders→beads cycle that forces the split); the alias
// makes rollout.Mode and gate.Mode one identical type, so existing callers
// and the Spec/Resolve machinery are unaffected.
type Mode = gate.Mode

const (
	// ModeUnset is the zero value: "nobody threaded a mode." See gate.ModeUnset.
	ModeUnset = gate.ModeUnset
	// Off runs the legacy path, byte-identical to pre-flag behavior. See gate.Off.
	Off = gate.Off
	// Auto runs the new path where capable, loud-degrading otherwise. See gate.Auto.
	Auto = gate.Auto
	// Require runs the new path or refuses closed. See gate.Require.
	Require = gate.Require
)

// ParseMode parses a user-supplied spelling into a Mode; see gate.ParseMode
// for the grammar contract (mode names only, never bool spellings).
func ParseMode(s string) (Mode, error) {
	return gate.ParseMode(s)
}
