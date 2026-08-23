// Package gate is the dependency-leaf half of internal/rollout: the Mode
// value kind and the generic enable-AND-capable resolver, with no imports
// beyond the standard library.
//
// It exists because consumers of the capability product cannot import
// internal/rollout itself: rollout depends on internal/config for Resolve,
// and config transitively reaches internal/beads (config → orders → beads),
// so beads importing rollout would cycle. Store-layer consumers import THIS
// package; everything else (the resolver, the registry, Flags) stays in
// internal/rollout, which re-exports these definitions as type aliases so
// the two spellings are one type. TestRolloutImportBoundary enforces that
// this package never grows a non-stdlib import.
package gate

import (
	"context"
	"fmt"
	"strings"
)

// Mode is the tri-state value kind for a correctness/migration rollout gate.
type Mode string

const (
	// ModeUnset is the zero value: "nobody threaded a mode." It resolves AS Off
	// but carries a diagnostic reason so an unwired call site is visible rather
	// than silently defaulting.
	ModeUnset Mode = ""
	// Off runs the legacy path, byte-identical to pre-flag behavior. Off is
	// zero-cost: a capability predicate is never consulted.
	Off Mode = "off"
	// Auto runs the new path where the runtime is capable and loud-degrades to
	// the legacy path otherwise — never a silent unconditional fallback.
	Auto Mode = "auto"
	// Require runs the new path or refuses closed; a silent fallback is
	// inexpressible.
	Require Mode = "require"
)

// ParseMode parses a user-supplied spelling into a Mode. It is case- and
// space-tolerant ("Require", " AUTO " are accepted) and recognizes ONLY the
// three mode names — bool/truthy spellings and the empty string are errors that
// name the off|auto|require grammar. (A tri-state gate has no meaningful bool
// spelling; ModeUnset is produced by absence, never by parsing a value.)
func ParseMode(s string) (Mode, error) {
	switch normalizeToken(s) {
	case "off":
		return Off, nil
	case "auto":
		return Auto, nil
	case "require":
		return Require, nil
	default:
		return ModeUnset, fmt.Errorf("invalid mode %q: want one of off, auto, require", s)
	}
}

// normalizeToken lowercases and trims surrounding whitespace for the mode
// grammar (case/space tolerant break-glass values).
func normalizeToken(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// Capability reports whether the runtime can execute a gate's new path. It is
// supplied per-call by a consumer-owned adapter (beads CAS supplies a bd/store
// probe; a future non-beads gate supplies its own) and is NEVER stored on a Spec
// or in the registry — that is what keeps this package free of consumer imports
// and the capability model general. A nil Capability means "this gate has no
// runtime capability question" and is vacuously capable.
type Capability func(ctx context.Context) (capable bool, reason string)

// Decision is the four-way verdict of the enable-AND-capable product.
type Decision string

const (
	// UseLegacy runs the old path (Off, or ModeUnset defaulted to Off).
	UseLegacy Decision = "use_legacy"
	// UseNew runs the new path (Auto or Require, and capable).
	UseNew Decision = "use_new"
	// DegradeLoud runs the old path but obliges the caller to surface a
	// diagnostic (Auto and not capable) — never a silent fallback.
	DegradeLoud Decision = "degrade_loud"
	// RefuseClosed is a typed refusal that must not fall back to the old path
	// (Require and not capable).
	RefuseClosed Decision = "refuse_closed"
)

// ResolveCapability computes the enable-AND-capable product — here and nowhere
// else, for every rollout gate, generically. The cell contract:
//
//	ModeUnset          -> UseLegacy    ("mode unset; defaulted to off"); cap not consulted
//	Off                -> UseLegacy    ("mode off"); cap NOT consulted (Off is zero-cost)
//	Auto,    capable   -> UseNew
//	Auto,    !capable  -> DegradeLoud  (reason carries the predicate's reason)
//	Require, capable   -> UseNew
//	Require, !capable  -> RefuseClosed (reason carries the predicate's reason)
//
// A nil cap is vacuously capable, so Auto/Require with a nil predicate resolve to
// UseNew. The capability predicate's reason string propagates verbatim into the
// returned reason.
func ResolveCapability(ctx context.Context, mode Mode, pred Capability) (Decision, string) {
	switch mode {
	case ModeUnset:
		return UseLegacy, "mode unset; defaulted to off"
	case Off:
		return UseLegacy, "mode off"
	case Auto, Require:
		// fall through to the capability check below.
	default:
		// An unrecognized mode is treated as the safe legacy path; Resolve
		// rejects out-of-enum config before a value ever reaches here.
		return UseLegacy, "unrecognized mode " + string(mode) + "; defaulted to off"
	}

	capable, reason := true, "no capability predicate"
	if pred != nil {
		capable, reason = pred(ctx)
	}
	if capable {
		return UseNew, reason
	}
	if mode == Require {
		return RefuseClosed, reason
	}
	return DegradeLoud, reason
}
