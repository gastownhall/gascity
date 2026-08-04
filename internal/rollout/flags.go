package rollout

import "strconv"

// resolved pairs a gate's effective value with the layer that produced it.
type resolved[T any] struct {
	value  T
	origin Origin
}

// Flags is the immutable per-process snapshot of every registered rollout gate.
// It is a value type: copy it and thread it by dependency injection; never point
// at it from package-level state.
//
// The zero value is DEGRADED-SAFE, not the builtin defaults: a never-Resolved
// Flags reads each gate's Go zero — BeadsConditionalWrites() returns ModeUnset
// (which ResolveCapability maps to the legacy path with a visible diagnostic),
// and FormulaV2() returns false (the legacy v1 path, NOT the builtin default
// true). So an unwired Flags runs legacy paths; OriginOf returns "" for a gate a
// zero Flags never resolved. Build defaults with ForTest or Resolve, never Flags{}.
type Flags struct {
	beadsConditionalWrites resolved[Mode]
	beadsGuardedRelease    resolved[Mode]
	formulaV2              resolved[bool]
	notices                []Notice
}

// OriginOf returns the Origin recorded for a registered gate Key (empty for an
// unknown key). For doctor/status rendering only — production reads use the
// typed accessors.
func (f Flags) OriginOf(key string) Origin {
	switch key {
	case keyBeadsConditionalWrites:
		return f.beadsConditionalWrites.origin
	case keyBeadsGuardedRelease:
		return f.beadsGuardedRelease.origin
	case keyDaemonFormulaV2:
		return f.formulaV2.origin
	default:
		return ""
	}
}

// ValueOf returns the resolved value of a registered gate Key in its canonical
// string spelling ("" for an unknown key). For doctor/status rendering only —
// production reads use the typed accessors (BeadsConditionalWrites/FormulaV2).
func (f Flags) ValueOf(key string) string {
	switch key {
	case keyBeadsConditionalWrites:
		return string(f.beadsConditionalWrites.value)
	case keyBeadsGuardedRelease:
		return string(f.beadsGuardedRelease.value)
	case keyDaemonFormulaV2:
		return strconv.FormatBool(f.formulaV2.value)
	default:
		return ""
	}
}

// Notices returns the resolution notices retained for the process lifetime.
func (f Flags) Notices() []Notice {
	if len(f.notices) == 0 {
		return nil
	}
	out := make([]Notice, len(f.notices))
	copy(out, f.notices)
	return out
}
