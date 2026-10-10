package main

import (
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
)

// rewriteBdReadyInternalArgs appends --exclude-type=session and
// --exclude-label=order-tracking to a `gc bd ready` (or `gc bd list --ready`)
// invocation, and returns every other argv unchanged.
//
// gc keeps two kinds of bookkeeping bead in the work ledger that are not work:
// a session bead per live agent (session_beads.go, type sessionBeadType, label
// sessionBeadLabel) and an order-run tracking bead per in-flight order
// (order_dispatch.go, label labelOrderTracking). Both are created open,
// unassigned and dependency-free, and neither carries the ephemeral marker, so
// bd's ready frontier — which filters on status, claims, blockers and the
// ephemeral tier only — lists them as claimable P2 rows beside real work
// ("de-d0iq P2 mayor"). A ready-driven claimer that takes one edits the
// reconciler's own state record; closing a session bead tears the live session
// down (devcity de-9g6).
//
// The rewrite is a stopgap on the read side: the structural fix is to mark
// gc-internal beads at creation so every reader hides them. Until then the
// passthrough adds the two exclusions bd already understands. Each half is
// gated independently and skipped when the argv explicitly asks for what it
// would hide (--type session, --label/--label-any naming order-tracking), when
// the caller already excludes it, or — for the label half — when the argv
// carries --skip-labels, which bd refuses to combine with --exclude-label.
//
// Fails open like rewriteBdWispTierArgs: an argv this scanner cannot parse (an
// unrecognized flag, whose value consumption is undecidable) is forwarded
// untouched. The cost of guessing wrong here is a hidden row the operator did
// not ask to hide; the cost of declining is the status quo.
func rewriteBdReadyInternalArgs(bdArgs []string) []string {
	verb, verbArgs, ok := bdRelocatedClassVerb(bdArgs)
	if !ok {
		return bdArgs
	}
	switch verb {
	case "ready":
	case "list":
		if !slices.Contains(verbArgs, "--ready") {
			return bdArgs
		}
	default:
		return bdArgs
	}
	addType, addLabel, parsed := bdReadyInternalExclusions(verb, verbArgs)
	if !parsed || (!addType && !addLabel) {
		return bdArgs
	}
	out := make([]string, 0, len(bdArgs)+2)
	out = append(out, bdArgs...)
	if addType {
		out = append(out, "--exclude-type="+sessionBeadType)
	}
	if addLabel {
		out = append(out, "--exclude-label="+labelOrderTracking)
	}
	return out
}

// bdReadyInternalExclusions scans a ready-frontier argv and reports which of
// the two exclusions still need adding. parsed is false for any argv the
// scanner cannot read confidently, so the caller leaves such an argv alone.
func bdReadyInternalExclusions(verb string, args []string) (addType, addLabel, parsed bool) {
	valueFlags := bdflags.ValueFlags(verb)
	boolFlags := bdflags.BoolFlags(verb)
	addType, addLabel = true, true

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, value, joined := strings.Cut(arg, "=")
		if boolFlags[name] && !joined {
			if name == "--skip-labels" {
				// bd rejects --skip-labels together with --exclude-label.
				addLabel = false
			}
			continue
		}
		if !valueFlags[name] {
			if joined {
				// A joined unknown flag consumes nothing; keep scanning.
				continue
			}
			// Unrecognized flag: whether it consumes the next token is
			// unknowable, so the rest of the argv cannot be read reliably.
			return false, false, false
		}
		if !joined {
			if i+1 >= len(args) {
				// A dangling value flag is bd's error to report, not ours to
				// paper over by appending flags to a doomed command.
				return false, false, false
			}
			value = args[i+1]
			i++
		}
		switch name {
		case "--type", "-t":
			if csvContains(value, sessionBeadType) {
				addType = false
			}
		case "--exclude-type":
			if csvContains(value, sessionBeadType) {
				addType = false
			}
		case "--label", "-l", "--label-any":
			if csvContains(value, labelOrderTracking) {
				addLabel = false
			}
		case "--exclude-label":
			if csvContains(value, labelOrderTracking) {
				addLabel = false
			}
		}
	}
	return addType, addLabel, true
}

// csvContains reports whether want appears in a comma-separated bd flag
// value, matching the way bd splits its repeatable string-slice flags.
func csvContains(csv, want string) bool {
	for _, v := range strings.Split(csv, ",") {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}
