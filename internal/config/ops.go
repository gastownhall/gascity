package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// OpsConfig holds the operation-journal rollout gates, which record the
// requests a session receives as ops beside their unchanged legacy execution,
// and Authoritative, the conversation lane's switch, which is not a gate.
// Each gate takes "off" (no op row; today's behavior), "auto" (shadow: the
// request is recorded where the store can record it and runs unrecorded with a
// loud degrade where it cannot) or "require" (durable: a request whose op
// cannot be recorded is refused before any effect). Any other value fails
// config load. A GC_OPS_CONTENT, GC_OPS_CONTROL or GC_OPS_LIFECYCLE env
// override takes precedence; read the effective values through
// internal/rollout.Flags, never these fields directly. The Go field names end
// in Mode because cmd/gc's prompt-boundary lint forbids a gate's config field
// name in prompt-rendering code, and a bare Content or Lifecycle would collide
// with unrelated identifiers there.
type OpsConfig struct {
	// Records message deliveries (session submit and messages, nudge, mail
	// notify) as message ops. "off" is the shipped default for all three
	// gates. "auto" records where the store can and degrades loudly where it
	// cannot. "require" refuses a request it cannot record before any effect.
	// Experimental: each mode takes effect as each request surface gains op
	// recording. A change takes effect at the next process start.
	ContentMode string `toml:"content,omitempty" jsonschema:"default=off,enum=off,enum=auto,enum=require"`
	// Records interrupt (stop) and respond requests as ops. "off" is the
	// shipped default. "auto" records where the store can and degrades loudly
	// where it cannot. "require" refuses a request it cannot record before any
	// effect. Experimental: each mode takes effect as each request surface
	// gains op recording. A change takes effect at the next process start.
	ControlMode string `toml:"control,omitempty" jsonschema:"default=off,enum=off,enum=auto,enum=require"`
	// Records wake, suspend, reset (restart), kill and close requests as ops.
	// "off" is the shipped default. "auto" records where the store can and
	// degrades loudly where it cannot. "require" refuses a request it cannot
	// record before any effect. Experimental: each mode takes effect as each
	// request surface gains op recording. A change takes effect at the next
	// process start.
	LifecycleMode string `toml:"lifecycle,omitempty" jsonschema:"default=off,enum=off,enum=auto,enum=require"`
	// Authoritative lists the op kinds the conversation lane executes instead
	// of their legacy path. The default is empty, and message is not on by
	// default anywhere yet. This build accepts message only. Experimental: no
	// request surface writes lane ops yet, so listing a kind changes no
	// request's delivery in this build. The city's controller, standalone
	// or supervisor-hosted, runs the lane host that delivers lane ops, and
	// gc doctor reports a listed kind as an error when no lane host serves
	// the city. A keystroke-mode runtime (tmux) takes delivery at an idle
	// boundary, a protocol-mode runtime (t3bridge) at its turn boundary.
	// A listed kind needs a sessions store with guarded batches. A change
	// takes effect at the next controller start.
	Authoritative []string `toml:"authoritative,omitempty" jsonschema:"enum=message"`
}

// laneAuthoritativeKinds are the op kinds this build's conversation lane can
// execute, so the only kinds [ops] authoritative accepts.
var laneAuthoritativeKinds = []opjournal.Kind{opjournal.KindMessage}

// LaneExecutes reports whether [ops] authoritative lists kind.
func (o OpsConfig) LaneExecutes(kind string) bool {
	return slices.Contains(o.Authoritative, kind)
}

// validateOpsModes rejects an out-of-enum value in any [ops] gate, and an
// [ops] authoritative entry that is empty, repeated, not an op kind, or a kind
// this build's conversation lane cannot execute. Parse runs
// it on its single layer and LoadWithIncludesOptions runs it again on the
// composed root, beside validateBeadsModes, because a fragment may supply any
// of these fields. A typo must never read as "off": an operator who wrote
// "requre" would believe requests are refused unless recorded while none is.
// The empty string (unset) is valid and defaults to off. A blank value such as
// " " is not unset, because internal/rollout reads any non-empty value as set,
// so it is rejected like a typo.
func validateOpsModes(o OpsConfig) error {
	for _, f := range []struct{ key, raw string }{
		{"ops.content", o.ContentMode},
		{"ops.control", o.ControlMode},
		{"ops.lifecycle", o.LifecycleMode},
	} {
		if f.raw == "" {
			continue
		}
		if _, err := gate.ParseMode(f.raw); err != nil {
			return fmt.Errorf("%s: %w", f.key, err)
		}
	}
	return validateOpsAuthoritative(o.Authoritative)
}

// validateOpsAuthoritative checks [ops] authoritative entry by entry. Entries
// are exact: a kind the lane executes is matched verbatim, so a padded or
// cased spelling is refused rather than silently read as unlisted.
func validateOpsAuthoritative(kinds []string) error {
	accepted := make([]string, len(laneAuthoritativeKinds))
	for i, k := range laneAuthoritativeKinds {
		accepted[i] = string(k)
	}
	seen := make(map[string]bool, len(kinds))
	for i, raw := range kinds {
		switch {
		case strings.TrimSpace(raw) == "":
			return fmt.Errorf("ops.authoritative[%d]: empty entry; accepted kinds: %s", i, strings.Join(accepted, ", "))
		case seen[raw]:
			return fmt.Errorf("ops.authoritative[%d]: %q is listed twice", i, raw)
		}
		seen[raw] = true
		if slices.Contains(accepted, raw) {
			continue
		}
		if _, err := opjournal.Lookup(opjournal.Kind(raw)); err != nil {
			return fmt.Errorf("ops.authoritative[%d]: %q is not an op kind; accepted kinds: %s", i, raw, strings.Join(accepted, ", "))
		}
		return fmt.Errorf("ops.authoritative[%d]: the conversation lane cannot execute %q in this build; accepted kinds: %s", i, raw, strings.Join(accepted, ", "))
	}
	return nil
}
