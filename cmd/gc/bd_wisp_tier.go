package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
)

// wispTierTypes are the bead types that live in the wisps (ephemeral) storage
// tier. bd's `list` sets SkipWisps on every query that does not pass
// --include-infra, so a filter naming one of these types can only ever match
// rows the query has already excluded.
var wispTierTypes = map[string]bool{
	"molecule": true,
	"wisp":     true,
}

// rewriteBdWispTierArgs appends --include-infra to a `gc bd list` invocation
// whose filters can only match the wisps tier, and returns every other argv
// unchanged.
//
// bd hides the wisps tier from `list` by default: absent --include-infra it
// sets SkipWisps on the query (beads cmd/bd/list_filter.go). Molecules and
// wisps live in that tier, so `gc bd list --type=molecule` returns [] on a
// ledger holding live molecules — an empty result and exit 0, which reads as
// "no molecules exist" rather than "this query cannot see them". That silent
// blindness broke witness patrol-wisp reconciliation across rigs: the startup
// query found nothing every cycle, so leaked wisps were never burned and
// accumulated across restarts (ga-3k3, daytripper dt-xrt5).
//
// The rewrite closes the disagreement between gc's two paths to the same
// query rather than inventing a new dialect: BdStore.listViaBDList
// (internal/beads/bdstore.go) already passes --include-infra unconditionally,
// so a molecule visible through the store seam was invisible through the
// passthrough seam. Only a filter that NAMES the wisps tier is widened —
// --type=molecule/wisp, --mol-type, --wisp-type — so an unfiltered `gc bd
// list` returns exactly what it returned before.
//
// Fails open: an argv this scanner cannot parse (an unrecognized flag, whose
// value consumption is undecidable) is forwarded untouched. The cost of
// guessing wrong here is a widened result set the operator did not ask for;
// the cost of declining is the status quo.
func rewriteBdWispTierArgs(bdArgs []string) []string {
	verb, verbArgs, ok := bdRelocatedClassVerb(bdArgs)
	if !ok || verb != "list" || !bdListFiltersWispTier(verbArgs) {
		return bdArgs
	}
	out := make([]string, 0, len(bdArgs)+1)
	out = append(out, bdArgs...)
	return append(out, "--include-infra")
}

// bdListFiltersWispTier reports whether a `bd list` argument list filters on
// the wisps tier without already asking for it. It returns false for any argv
// it cannot parse confidently, so the caller leaves such an argv alone.
func bdListFiltersWispTier(args []string) bool {
	valueFlags := bdflags.ValueFlags("list")
	boolFlags := bdflags.BoolFlags("list")
	filtersWispTier := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, value, joined := strings.Cut(arg, "=")
		switch name {
		case "--include-infra":
			// The caller already asked for the wisps tier; nothing to add.
			return false
		case "--type", "-t":
			if !joined {
				if i+1 >= len(args) {
					// A dangling --type is bd's error to report, not ours to
					// paper over by appending a flag to a doomed command.
					return false
				}
				value = args[i+1]
				i++
			}
			if wispTierTypes[strings.ToLower(strings.TrimSpace(value))] {
				filtersWispTier = true
			}
			continue
		case "--mol-type", "--wisp-type":
			// Both filters are meaningful only against wisps-tier rows.
			if !joined {
				if i+1 >= len(args) {
					return false
				}
				i++
			}
			filtersWispTier = true
			continue
		}
		if joined || boolFlags[name] {
			continue
		}
		if valueFlags[name] {
			i++
			continue
		}
		// Unrecognized flag: whether it consumes the next token is unknowable,
		// so the rest of the argv cannot be read reliably. Decline.
		return false
	}
	return filtersWispTier
}
