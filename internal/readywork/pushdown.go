package readywork

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fanout"
	"github.com/gastownhall/gascity/internal/storeref"
)

// Pushdown: what each leg is asked, so that it reads only the rows the answer
// can contain.
//
// Without it, every leg returns its WHOLE ready set and the predicates are
// applied here: on a city with eleven thousand ready beads, a claim tier asking
// for the one bead assigned to it read all eleven thousand from every store,
// then kept one. The pushed predicates make each leg's read selective.
//
// # Each pushed predicate is no stricter than the Go filter
//
// A store may narrow by a pushed predicate or ignore it (beads.ReadyQuery's
// advisory contract); either way the predicate is re-applied to the merged set
// by compiled.matches, which stays the authority. What pushing must never do is
// drop a row the Go filter keeps, so a predicate is pushed only where every
// backend's comparison is provably the Go comparison:
//
//   - --assignee and --unassigned compare the stored assignee exactly in Go, as
//     `bd ready` and every store's own Assignee filter do.
//   - --metadata-field key=value is pushed only when the value is not itself a
//     JSON text and the key is in bd's key grammar. The SQL backends compare
//     JSON_UNQUOTE(JSON_EXTRACT(metadata, '$."key"')) with the value, which is
//     the Go string comparison for a stored JSON string; for a value that is a
//     JSON literal (true, 42, null, an object) the two can disagree, and a key
//     outside the grammar is refused by the backend rather than matched.
//   - --exclude-type (bd expands type aliases), --exclude-label (bd normalizes
//     labels) and bare-key --metadata-field are never pushed.
//
// # The limit is pushed only where it is the same top N
//
// Under the canonical order and --sort oldest, a store that can cut its ready
// set in that order after every readiness veto (beads.OrderedReadyReader) is
// asked for its first Limit+boundedSlack rows instead of all of them. Every leg
// that returned fewer rows than it was asked for returned all of them, and a
// leg is asked again for more while fewer than the limit of its rows pass the
// Go-only predicates and survive the merge (boundedRead), so each leg's top N
// is in hand and the merged top N is the city's.
//
// --sort newest is never pushed: no store cuts in that order.
//
// # Co-resident ids
//
// Both kinds of narrowing rest on one more fact: no id is held by two legs.
// Work legs mint disjoint id namespaces and nothing copies a work bead from one
// to another, and an id in a class binding's reserved namespace belongs to the
// binding alone. The exception is the class binding's work-shaped ids: `gc
// storage migrate` copies beads into the binding under their original ids and
// then clears the work store's copies, but a copy survives wherever that clear
// has not run: a city migrated by a build that kept its source, or a clear
// that did not finish. An unbounded read keeps the work store's copy (first
// leg wins) and the predicates judge THAT copy. A narrowed read can go wrong
// over such a relic in two directions:
//
//   - It can leave the work store's copy behind (a predicate it fails, or a
//     bound it falls past) and serve the binding's. So when the binding serves
//     a row of the answer under a work-shaped id, the earlier legs are asked
//     whether they hold it, and if one does, or cannot say, the answer is read
//     again without any narrowing (readNarrowed).
//   - It can count the binding's relic toward the binding's bound and then
//     drop the relic for the work store's copy in the merge, ending the
//     binding's read before rows of its own the answer needs. So a relic never
//     counts toward a bound (mergeKeeps).
//
// On a migrated city both concern the relics alone; the binding's own rows,
// every routed graph step included, need no check.
//
// The reserved-namespace half of that fact holds only while no work leg's
// prefix shadows a reserved one, and config only warns about a work prefix
// that does (config.ReservedPrefixWarnings). A city that ignored the warning
// and split anyway holds its relics under reserved-shaped ids. mergeKeeps
// trusts those, so they count toward the binding's bound and are never
// probed, and a narrowed read on such a city can differ from the plain one.
// Leaving every shadowed prefix out of the binding leg's ReservedPrefixes
// would close that gap (ga-xdtyo2).

// boundedSlack is how many rows past the limit a bounded leg read asks for.
// It absorbs the rows a Go-only predicate drops without a second read, and any
// difference between a store's order and the Go order at the cut, which can only
// be a tie broken differently.
const boundedSlack = 20

// metadataKeyGrammar is bd's metadata key grammar (storage.ValidateMetadataKey).
// A key outside it is refused by the backend instead of matched, so it is not
// pushed.
var metadataKeyGrammar = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_./]*$`)

// pushableMetadata reports whether f can be pushed into a store's ready read
// with the Go comparison's semantics (see the package comment above).
func pushableMetadata(f metadataFieldFilter) bool {
	return !f.present && metadataKeyGrammar.MatchString(f.key) && !json.Valid([]byte(f.value))
}

// readyReadQuery is the ready query every leg is asked: the federated tier and
// the pushable predicates.
func readyReadQuery(c compiled) beads.ReadyQuery {
	q := beads.ReadyQuery{
		Assignee:   c.assignee,
		TierMode:   beads.FederatedReadTier,
		Unassigned: c.query.Unassigned,
	}
	for _, f := range c.metadata {
		if !pushableMetadata(f) {
			continue
		}
		if q.MetadataEquals == nil {
			q.MetadataEquals = make(map[string]string, len(c.metadata))
		}
		// Two values for one key match nothing in Go; pushing either one still
		// returns a superset of nothing.
		q.MetadataEquals[f.key] = f.value
	}
	return q
}

// boundedOrder reports the order a bounded leg read cuts in, or false when the
// limit is not pushed at all.
func boundedOrder(c compiled) (beads.ReadyOrder, bool) {
	if c.query.Limit <= 0 {
		return 0, false
	}
	switch c.sort {
	case "":
		return beads.ReadyOrderCanonical, true
	case SortOldest:
		return beads.ReadyOrderOldest, true
	default:
		return 0, false
	}
}

// readyRead reads one leg's whole ready set for q, live.
func readyRead(q beads.ReadyQuery) legRead {
	return func(_ context.Context, leg Leg) legResult {
		rows, err := beads.HandlesFor(leg.Store).Live.Ready(q)
		return legResult{rows: rows, err: err}
	}
}

// readReadyCandidates reads the ready arm: each leg asked with the pushed
// predicates and, where it is exact, a pushed limit.
func readReadyCandidates(ctx context.Context, legs []Leg, c compiled, cs *crossStore) ([]beads.Bead, map[string]int, error) {
	plain := readyRead(beads.ReadyQuery{TierMode: beads.FederatedReadTier})
	q := readyReadQuery(c)
	order, bounded := boundedOrder(c)
	switch {
	case bounded:
		return readNarrowed(ctx, legs, c, boundedRead(c, q, order, cs), plain)
	case predicatesPushed(q):
		return readNarrowed(ctx, legs, c, readyRead(q), plain)
	default:
		return federate(ctx, legs, plain)
	}
}

// predicatesPushed reports whether q asks a leg for less than its whole ready
// set.
func predicatesPushed(q beads.ReadyQuery) bool {
	return q.Assignee != "" || q.Unassigned || len(q.MetadataEquals) > 0
}

// readNarrowed reads every leg with narrowed, a read that asks each leg for
// less than plain does, and answers from it when that answer is provably the
// one plain gives; otherwise it reads every leg with plain.
//
// The only way a narrowed read can answer differently is through first-leg-wins
// dedupe over an id two legs hold: a migrated bead's copy in the work store it
// was minted in, and its copy in the class binding it migrated to. An unbounded
// plain read keeps the work store's copy, and the predicates then judge THAT
// copy. A narrowed read can leave the work store's copy behind (a predicate it
// fails, or a bound it falls past) and serve the binding's instead. So when the
// binding serves a row of the answer under a work-shaped id, every earlier leg
// that was narrowed is asked whether it holds that id; if one does, or cannot
// say, the answer is read again with plain. The other direction, a bounded
// binding read ended by relics the merge then drops, cannot occur while no work
// prefix shadows a reserved one: boundedRead counts only the rows mergeKeeps
// trusts (Co-resident ids above).
func readNarrowed(ctx context.Context, legs []Leg, c compiled, narrowed, plain legRead) ([]beads.Bead, map[string]int, error) {
	results := readLegs(ctx, legs, narrowed)
	if err := firstLegError(legs, results); err != nil {
		return nil, nil, err
	}
	items, owners := merge(results)
	if narrowedAnswerIsExact(ctx, legs, c, items, owners) {
		return items, owners, nil
	}
	return federate(ctx, legs, plain)
}

// boundedRead asks one leg for its first Limit+boundedSlack ready rows in order,
// and asks again for more, unless ctx has ended, while the rows the answer can
// use fall short of the limit: rows the Go-only predicates keep and the merge
// keeps too (mergeKeeps). A relic the merge drops for an earlier leg's copy is
// not a row in hand, and counting it could end a binding's read before rows of
// its own the answer needs. A store that cannot cut in order returns its whole
// set.
//
// On a split city a row a cross-store blocker holds is not a row in hand either
// (crossstore.go), so it does not count toward the limit: a leg whose top rows
// are held widens its own cut, step by step, until enough of its rows can be
// served, and no leg's whole ready set is re-read for it.
func boundedRead(c compiled, q beads.ReadyQuery, order beads.ReadyOrder, cs *crossStore) legRead {
	limit := c.query.Limit
	return func(ctx context.Context, leg Leg) legResult {
		want := limit + boundedSlack
		for {
			bq := q
			bq.Limit = want
			rows, ordered, err := beads.ReadyOrdered(leg.Store, bq, order)
			if err != nil {
				return legResult{err: err}
			}
			if !ordered || len(rows) < want {
				return legResult{rows: rows}
			}
			var usable []beads.Bead
			for _, b := range rows {
				if mergeKeeps(leg, b.ID) && c.matches(b) {
					usable = append(usable, b)
				}
			}
			kept := len(usable)
			if cs != nil && kept >= limit {
				blocked, err := cs.blockedRows(ctx, cs.legIndex(leg.Store), usable)
				if err != nil {
					return legResult{err: err}
				}
				for _, b := range usable {
					if blocked[b.ID] {
						kept--
					}
				}
			}
			if kept >= limit {
				return legResult{rows: rows}
			}
			if err := ctx.Err(); err != nil {
				return legResult{err: err}
			}
			want *= 4
		}
	}
}

// narrowedAnswerIsExact reports whether a narrowed read's answer is the plain
// read's: true unless a row the answer takes from a class binding, under an id
// outside the binding's reserved namespaces, is also held by an earlier leg. A
// leg that cannot say whether it holds the id counts as holding it, and so does
// a leg whose probe comes after ctx has ended.
func narrowedAnswerIsExact(ctx context.Context, legs []Leg, c compiled, items []beads.Bead, owners map[string]int) bool {
	answer := c.filter(items)
	c.order(answer)
	if c.query.Limit > 0 && len(answer) > c.query.Limit {
		answer = answer[:c.query.Limit]
	}
	type probe struct {
		leg int
		id  string
	}
	var probes []probe
	for _, row := range answer {
		src := owners[row.ID]
		if mergeKeeps(legs[src], row.ID) {
			continue
		}
		for i := range src {
			probes = append(probes, probe{leg: i, id: row.ID})
		}
	}
	held := make([]bool, len(probes))
	fanout.Bounded(len(probes), LegFanOut, func(i int) {
		if ctx.Err() != nil {
			held[i] = true
			return
		}
		_, err := beads.HandlesFor(legs[probes[i].leg].Store).Live.Get(probes[i].id)
		held[i] = !errors.Is(err, beads.ErrNotFound)
	})
	return !slices.Contains(held, true)
}

// mergeKeeps reports whether no earlier leg can hold id, so the merge is sure
// to keep leg's copy of it: every id a work leg holds, and every id in one of a
// class binding's reserved namespaces. Only a binding's work-shaped ids, the
// relics an uncleared migration leaves, can lose to an earlier leg's copy;
// reserved-shaped ids can too where a work prefix shadows a reserved one (see
// Co-resident ids above).
func mergeKeeps(leg Leg, id string) bool {
	return !leg.ClassBinding || inAnyNamespace(id, leg.ReservedPrefixes)
}

// inAnyNamespace reports whether id is in one of the prefixes' namespaces: the
// prefix itself, or the prefix followed by "-" (storeref.IDInNamespace).
func inAnyNamespace(id string, prefixes []string) bool {
	for _, p := range prefixes {
		if storeref.IDInNamespace(id, p) {
			return true
		}
	}
	return false
}
