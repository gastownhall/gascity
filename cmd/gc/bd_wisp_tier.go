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

// bdListOutputOnlyValueFlags are `bd list` value flags that shape the answer —
// format, ordering, paging — rather than select rows. Every other recognized
// `bd list` value flag is a row predicate. Pinned against bdflags' `list`
// manifest (bd 1.3.2) by
// TestBdWispTierOutputOnlyTablesAreGroundedInBdFlagManifest: a flag bd
// gains lands as an unclassified predicate — the widened default — until
// this table or the scanner names it.
var bdListOutputOnlyValueFlags = map[string]bool{
	"--format": true, "-n": true, "--limit": true, "--max-rows": true,
	"--offset": true, "--sort": true,
}

// bdListOutputOnlyBoolFlags are `bd list` boolean flags that shape the answer
// rather than select rows: display modes and pager control, --skip-labels
// emptying a hydration column, --include-comments enriching every row, and
// --deps annotating the tree and ordering siblings. Every other recognized
// `bd list` boolean flag narrows or widens the row set.
var bdListOutputOnlyBoolFlags = map[string]bool{
	"--brief": true, "--deps": true, "--flat": true, "--include-comments": true,
	"--long": true, "--no-pager": true, "--pretty": true, "--reverse": true,
	"-r": true, "--skip-labels": true, "--tree": true, "-w": true, "--watch": true,
}

// rewriteBdWispTierArgs appends --include-infra to a `gc bd list` invocation
// whose filters can match rows bd hides, and returns every other argv
// unchanged.
//
// bd hides the wisps tier from `list` by default: absent --include-infra it
// sets SkipWisps on the query (beads cmd/bd/list_filter.go), and molecules
// and wisps live in that tier. Two shapes silently lose rows:
//
//   - A filter that NAMES the tier — --type=molecule/wisp, --mol-type,
//     --wisp-type — can only ever match hidden rows, so it returns [] and
//     exit 0 on a ledger holding live molecules, which reads as "no
//     molecules exist" rather than "this query cannot see them". That
//     blindness broke witness patrol-wisp reconciliation across rigs (ga-3k3,
//     daytripper dt-xrt5).
//
//   - A filter that selects rows WITHOUT naming the tier — --assignee,
//     --status, --label, --parent, a time window, the rest of the predicate
//     family — can match hidden wisps-tier rows even though it also matches
//     visible ones. The deacon startup check "gc bd list --assignee=X
//     --status=open" answered [] with a live ephemeral patrol wisp in the
//     ledger, so the deacon concluded it had no successor wisp and poured a
//     duplicate on every startup (city-jf7: three surplus wisps on
//     2026-10-30). The witness liveness map and the refinery's own work
//     query carry the same shape.
//
// The rewrite closes the disagreement between gc's two paths to the same
// query rather than inventing a new dialect: BdStore.listViaBDList
// (internal/beads/bdstore.go) already passes --include-infra unconditionally,
// so anything visible through the store seam is visible through the
// passthrough seam too.
//
// An unfiltered `gc bd list` — or one narrowed only by output-shaping flags,
// store selection, or a non-wisp --type that provably cannot match the tier
// — returns exactly what it returned before: browse keeps bd's documented
// default, and only lookups that can reach hidden rows are widened.
//
// Fails open: an argv this scanner cannot parse confidently (an unrecognized
// separate flag, whose value consumption is undecidable; a value flag left
// dangling at the end, which bd itself will reject) is forwarded untouched.
// An unrecognized JOINED flag (--new-flag=value) self-delimits, so the scan
// stays decidable and the flag counts as a predicate: the cost of guessing
// wrong is a widened result set the operator did not ask for, and the cost
// of declining is the leak class this rewrite closes.
func rewriteBdWispTierArgs(bdArgs []string) []string {
	verb, verbArgs, ok := bdRelocatedClassVerb(bdArgs)
	if !ok || verb != "list" || !bdListNeedsWispTierAccess(verbArgs) {
		return bdArgs
	}
	out := make([]string, 0, len(bdArgs)+1)
	out = append(out, bdArgs...)
	return append(out, "--include-infra")
}

// bdListNeedsWispTierAccess reports whether a `bd list` argument list can
// match rows bd hides unless --include-infra (or --include-ephemeral, which
// also opens the wisp plane) is passed. It returns false for any argv it
// cannot parse confidently, so the caller leaves such an argv alone.
func bdListNeedsWispTierAccess(args []string) bool {
	valueFlags := bdflags.ValueFlags("list")
	boolFlags := bdflags.BoolFlags("list")
	globalValue := bdflags.GlobalValueFlags()
	globalBool := bdflags.GlobalBoolFlags()
	namesWispTier := false
	predicates := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, value, joined := strings.Cut(arg, "=")
		switch name {
		case "--include-infra", "--include-ephemeral":
			// The caller already asked for the wisp plane; nothing to add.
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
				namesWispTier = true
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
			namesWispTier = true
			continue
		}
		recognized := boolFlags[name] || valueFlags[name]
		outputOnly := bdListOutputOnlyBoolFlags[name] || bdListOutputOnlyValueFlags[name] ||
			globalBool[name] || globalValue[name]
		if joined {
			// A joined flag self-delimits, so the scan stays decidable. An
			// unknown name counts as a predicate — the pinned tables may lag
			// bd, and missing a new predicate re-opens the leak class while
			// over-widening on a new display flag costs a wider result.
			if !recognized || !outputOnly {
				predicates = true
			}
			continue
		}
		if boolFlags[name] {
			// Globals select a store or profile and display flags shape the
			// answer; every other recognized boolean narrows or widens the
			// row set.
			if !outputOnly {
				predicates = true
			}
			continue
		}
		if valueFlags[name] {
			if i+1 >= len(args) {
				// Dangling value: bd's error to report, not ours to paper
				// over.
				return false
			}
			i++
			if !outputOnly {
				predicates = true
			}
			continue
		}
		// Unrecognized separate flag: whether it consumes the next token is
		// unknowable, so the rest of the argv cannot be read reliably.
		// Decline.
		return false
	}
	return namesWispTier || predicates
}
