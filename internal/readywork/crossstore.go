package readywork

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fanout"
)

// Cross-store blockers: one rule on a split city, bd's answer everywhere else.
//
// # Where it applies (owner decision)
//
// Only on a city with a class binding — a split city, whose legs include one
// with ClassBinding set. A city that relocates nothing, with or without rigs,
// keeps every store's own ready answer byte for byte: bd never blocks a
// dependent on an "external:" target or on a target whose id prefix differs from
// its own (beads v1.3.2, issueops.IsExternalDepTarget), and that is the answer
// such a city has always had. None of this file runs for it.
//
// # The rule on a split city
//
// A blocking edge (blocks, waits-for, conditional-blocks) whose target the
// dependent's own store does not hold blocks until the target is closed — and
// not closed blocked (beads.DependencySatisfied) — in EVERY store that holds
// it. A target no store holds is unresolvable and blocks too; it never fails
// the read. An "external:<id>" target names the bead <id> and is looked up as
// such; an "external:<rig>:<capability>" target names no bead and never blocks,
// exactly as bd treats it (beads.ExternalBlockerTarget).
//
// # Why the reader enforces it
//
// The backends answer such an edge in opposite ways: SQLite and the in-memory
// stores read the absent target as never closed and strand the dependent
// forever; bd/Dolt reads a cross-prefix or external target as never blocking
// and serves the dependent at once. So the reader re-judges, through the legs it
// federates. Whether a leg strands is asked of the leg
// (beads.ReadyBlockedByAbsentFor), never read off the methods it has: every
// store cmd/gc opens sits behind a wrapper that forwards the listing, and over
// bd/Dolt that wrapper strands nothing.
//
//   - A row a STRANDING store served from its own Ready had every target
//     judged, but only that store's copy of it. A target in a class binding's
//     own reserved namespace is the binding's alone and needs no second look.
//     Any other target may have a second copy, a migrated relic and the row it
//     was copied from: a binding's row has it judged in every store, and a work
//     row has it judged wherever a class binding also holds it.
//   - A row a bd-semantics store served is checked for the targets bd did not
//     judge: cross-prefix and external ones. A same-prefix target bd judged,
//     unless a class binding holds it — a migrated relic, which bd reads as a
//     missing same-prefix target and so as no blocker at all.
//   - A row a stranding store withholds only because a target is absent from it
//     is listed (beads.AbsentBlockedReader) and served once every one of its
//     targets is satisfied wherever it is held. A listed row is judged in full
//     even when it reached the reader another way: a cache's own ready
//     projection reads an absent target as no blocker, and serves the row.
//
// Every target that needs a verdict is read by id from each leg (beads.GetMany:
// one exact batch read where the leg has one, otherwise one keyed Get per id,
// never a listing), and every verdict and row decision is memoized for the
// read, so the bounded read's per-leg refill and the final gate share them.
//
// The in-progress tier judges each blocker as the ready arm does, from every
// copy the legs hold, and reports it by the copy that holds it unreleased: an
// open copy's status, BlockerClosedBlocked when every such copy is closed
// blocked, BlockerUnresolved when no store holds it, each with a reason naming
// where. So the resumed holder sees its bead gated exactly when the ready arm
// would hold it. On a city with no binding it reads only the serving leg, as it
// always did, and still never fails the tier.

// BlockerUnresolved is the blocked_by status of a blocker no store holds. It is
// not "closed", so every consumer that reads blocked_by treats the row as gated
// (cmd/gc isDepBlockedHookCandidate), which is the owner rule.
const BlockerUnresolved = "unresolved"

// BlockerClosedBlocked is the blocked_by status, on a split city, of a blocker
// that is closed but releases nothing: every copy that holds it unreleased is
// closed with gc.work_outcome=blocked (beads.DependencySatisfied). It is not
// "closed", so every consumer that reads blocked_by treats the row as gated, as
// the ready arm and the drain hold it.
const BlockerClosedBlocked = "blocked"

// IsSplit reports whether legs are a split city's: one of them is a class
// binding. Only then does the cross-store rule apply.
func IsSplit(legs []Leg) bool {
	for _, leg := range legs {
		if leg.ClassBinding && leg.Store != nil {
			return true
		}
	}
	return false
}

// blockerVerdict is what the federation knows about one blocker.
type blockerVerdict struct {
	// status is the blocker's status as the store that holds it reports it,
	// or BlockerUnresolved when no store holds it.
	status string
	// satisfied reports whether the blocker releases its dependents: closed,
	// and not closed blocked, in every store that holds it.
	satisfied bool
	// reason says where the verdict came from when it did not come from the
	// row's own store.
	reason string
}

// verdictOfCopies judges a blocker from every copy the legs hold, labeled by
// the leg that holds it, in leg order. Every copy must release its dependents.
// An unreleased blocker reports the first copy that is not closed when there is
// one, and BlockerClosedBlocked when every unreleased copy is closed blocked,
// so a reader that judges by status (blocked_by) sees the blocker held whenever
// the verdict holds it.
func verdictOfCopies(id string, copies []heldCopy) blockerVerdict {
	if len(copies) == 0 {
		return blockerVerdict{
			status: BlockerUnresolved,
			reason: fmt.Sprintf("blocker %s is held by no store this city federates", id),
		}
	}
	holders := make([]string, 0, len(copies))
	var held *heldCopy
	for i, c := range copies {
		holders = append(holders, c.label)
		if beads.DependencySatisfied(c.bead.Status, beads.ReadinessWorkOutcome(c.bead.Metadata)) {
			continue
		}
		if held == nil || (held.bead.Status == "closed" && c.bead.Status != "closed") {
			held = &copies[i]
		}
	}
	if held != nil && held.bead.Status == "closed" {
		return blockerVerdict{
			status: BlockerClosedBlocked,
			reason: fmt.Sprintf("blocker %s is closed with gc.work_outcome=blocked in the %s store, which releases none of its dependents", id, held.label),
		}
	}
	if held != nil {
		return blockerVerdict{
			status: held.bead.Status,
			reason: fmt.Sprintf("blocker %s is held unreleased in the %s store", id, held.label),
		}
	}
	return blockerVerdict{
		status:    "closed",
		satisfied: true,
		reason:    fmt.Sprintf("blocker %s is closed in the %s store", id, strings.Join(holders, ", ")),
	}
}

// heldCopy is one leg's copy of a blocker.
type heldCopy struct {
	label string
	bead  beads.Bead
}

// crossStore is one read's cross-store context. A nil *crossStore is a city
// with no binding: every method is then the store's own answer.
type crossStore struct {
	legs []Leg

	mu sync.Mutex
	// blocked memoizes each judged row of a leg: true when a blocker holds it.
	blocked map[legRow]bool
	// stranded marks the rows a leg listed through beads.AbsentBlockedReader:
	// that store judged none of their absent targets, so every target is
	// verified.
	stranded map[legRow]bool
	// verdicts memoizes each verified blocker id across the legs.
	verdicts map[string]blockerVerdict
}

// legRow names one leg's copy of a row. A migrated bead can be held by two legs
// under one id, each copy with its own edges, so a row is judged per leg.
type legRow struct {
	leg int
	id  string
}

// newCrossStore returns the cross-store context for legs, or nil when legs are
// not a split city's.
func newCrossStore(legs []Leg) *crossStore {
	if !IsSplit(legs) {
		return nil
	}
	return &crossStore{
		legs:     legs,
		blocked:  make(map[legRow]bool),
		stranded: make(map[legRow]bool),
		verdicts: make(map[string]blockerVerdict),
	}
}

// legIndex returns the index of the leg serving store, or -1.
func (cs *crossStore) legIndex(store beads.Store) int {
	for i, leg := range cs.legs {
		if leg.Store == store {
			return i
		}
	}
	return -1
}

// strands reports whether a leg's store reads an absent target as a blocker,
// and so judged its own copy of every target of every row its Ready served.
func strands(leg Leg) bool {
	_, ok := beads.ReadyBlockedByAbsentFor(leg.Store)
	return ok
}

// listStranded reads the rows leg's Ready withholds only because a target is
// absent from it, as Ready would select them for q; a leg that strands nothing
// lists none.
func listStranded(_ context.Context, leg Leg, q beads.ReadyQuery) legResult {
	reader, ok := beads.ReadyBlockedByAbsentFor(leg.Store)
	if !ok {
		return legResult{}
	}
	rows, err := reader.ReadyBlockedByAbsent(q)
	return legResult{rows: rows, err: err}
}

// addStranded lists, from every stranding leg, the rows its Ready withholds
// only because a target is absent from it, and merges them as rows that leg
// served. They are candidates; the gate serves one only once every target is
// satisfied.
func (cs *crossStore) addStranded(ctx context.Context, q beads.ReadyQuery, items []beads.Bead, owners map[string]int) ([]beads.Bead, error) {
	if cs == nil {
		return items, nil
	}
	results := readLegs(ctx, cs.legs, func(ctx context.Context, leg Leg) legResult {
		return listStranded(ctx, leg, q)
	})
	if err := firstLegError(cs.legs, results); err != nil {
		return nil, err
	}
	for i, res := range results {
		items = cs.mergeStranded(i, res.rows, items, owners)
	}
	return items, nil
}

// mergeStranded records rows as stranded by legs[own] and appends each one no
// earlier leg served to items, as a row own served. A listed row already in
// items stays where it is, but its copy in own is still judged in full: own's
// store judged none of its absent targets, whichever read found the row.
func (cs *crossStore) mergeStranded(own int, rows, items []beads.Bead, owners map[string]int) []beads.Bead {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, b := range rows {
		cs.stranded[legRow{own, b.ID}] = true
		if _, seen := owners[b.ID]; seen {
			continue
		}
		owners[b.ID] = own
		items = append(items, b)
	}
	return items
}

// blockedRows returns which of rows, every one served by legs[own], a blocker
// holds back, judging each row once per read.
func (cs *crossStore) blockedRows(ctx context.Context, own int, rows []beads.Bead) (map[string]bool, error) {
	out := make(map[string]bool, len(rows))
	if cs == nil || own < 0 || own >= len(cs.legs) || len(rows) == 0 {
		return out, nil
	}
	leg := cs.legs[own]
	stranding := strands(leg)
	var pending []string
	cs.mu.Lock()
	for _, row := range rows {
		if blocked, done := cs.blocked[legRow{own, row.ID}]; done {
			out[row.ID] = blocked
			continue
		}
		pending = append(pending, row.ID)
	}
	cs.mu.Unlock()
	if len(pending) == 0 {
		return out, nil
	}
	deps, err := legDeps(ctx, leg, pending)
	if err != nil {
		return nil, err
	}

	// Which targets need a verdict, per row.
	type edge struct{ row, target string }
	var verify []edge
	var relicCandidates []edge
	cs.mu.Lock()
	for _, id := range pending {
		for _, dep := range deps[id] {
			if !beads.IsReadyBlockingDependencyType(dep.Type) || dep.DependsOnID == id {
				continue
			}
			target, capability := beads.ExternalBlockerTarget(dep.DependsOnID)
			switch {
			case capability:
				// Names no bead; bd never blocks on it, nor does gc.
			case target != dep.DependsOnID, cs.stranded[legRow{own, id}]:
				verify = append(verify, edge{id, target})
			case stranding && leg.ClassBinding && inAnyNamespace(target, leg.ReservedPrefixes):
				// The binding judged a target in its own reserved namespace,
				// which no other store holds.
			case stranding && leg.ClassBinding:
				// The binding judged its copy of a work-shaped target; the
				// store the target was minted in may hold another.
				verify = append(verify, edge{id, target})
			case stranding, beads.BdIDPrefix(target) == beads.BdIDPrefix(id):
				// The store judged its own copy: a stranding store every
				// target it holds, bd a same-prefix one. A class binding may
				// hold another (a migrated relic), checked below.
				relicCandidates = append(relicCandidates, edge{id, target})
			default:
				verify = append(verify, edge{id, target})
			}
		}
	}
	cs.mu.Unlock()
	if len(relicCandidates) > 0 {
		held, err := cs.heldByBindings(ctx, own, edgeTargets(relicCandidates, func(e edge) string { return e.target }))
		if err != nil {
			return nil, err
		}
		for _, e := range relicCandidates {
			if held[e.target] {
				verify = append(verify, e)
			}
		}
	}
	verdicts, err := cs.verdictsFor(ctx, edgeTargets(verify, func(e edge) string { return e.target }))
	if err != nil {
		return nil, err
	}
	blockedNow := make(map[string]bool, len(pending))
	for _, e := range verify {
		if !verdicts[e.target].satisfied {
			blockedNow[e.row] = true
		}
	}
	cs.mu.Lock()
	for _, id := range pending {
		cs.blocked[legRow{own, id}] = blockedNow[id]
		out[id] = blockedNow[id]
	}
	cs.mu.Unlock()
	return out, nil
}

func edgeTargets[E any](edges []E, target func(E) string) []string {
	seen := make(map[string]bool, len(edges))
	var out []string
	for _, e := range edges {
		t := target(e)
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// heldByBindings reports which ids any class-binding leg other than own holds:
// one read by id of each binding (beads.GetMany).
func (cs *crossStore) heldByBindings(ctx context.Context, own int, ids []string) (map[string]bool, error) {
	held := make(map[string]bool)
	for i, leg := range cs.legs {
		if i == own || !leg.ClassBinding || leg.Store == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		found, err := beads.GetMany(leg.Store, ids)
		if err != nil {
			return nil, fmt.Errorf("%s store: %w", leg.Label, err)
		}
		for id := range found {
			held[id] = true
		}
	}
	return held, nil
}

// verdictsFor judges ids across every leg — one read by id of each leg, at
// most LegFanOut at once — reusing the verdicts this read already holds.
func (cs *crossStore) verdictsFor(ctx context.Context, ids []string) (map[string]blockerVerdict, error) {
	out := make(map[string]blockerVerdict, len(ids))
	var missing []string
	cs.mu.Lock()
	for _, id := range ids {
		if v, ok := cs.verdicts[id]; ok {
			out[id] = v
			continue
		}
		missing = append(missing, id)
	}
	cs.mu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	copies, err := readCopies(ctx, cs.legs, missing)
	if err != nil {
		return nil, err
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, id := range missing {
		v := verdictOfCopies(id, copies[id])
		cs.verdicts[id] = v
		out[id] = v
	}
	return out, nil
}

// readCopies reads ids from every leg, one read by id of each leg
// (beads.GetMany), at most LegFanOut at once, and returns each id's copies in
// leg order. A leg read that fails fails the answer: a blocker that could not
// be read is not one that is absent.
func readCopies(ctx context.Context, legs []Leg, ids []string) (map[string][]heldCopy, error) {
	found := make([]map[string]beads.Bead, len(legs))
	errs := make([]error, len(legs))
	fanout.Bounded(len(legs), LegFanOut, func(i int) {
		if legs[i].Store == nil {
			return
		}
		if errs[i] = ctx.Err(); errs[i] != nil {
			return
		}
		found[i], errs[i] = beads.GetMany(legs[i].Store, ids)
	})
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("%s store: reading blockers: %w", legs[i].Label, err)
		}
	}
	out := make(map[string][]heldCopy, len(ids))
	for i, byID := range found {
		for _, id := range ids {
			if b, ok := byID[id]; ok {
				out[id] = append(out[id], heldCopy{label: legs[i].Label, bead: b})
			}
		}
	}
	return out, nil
}

// legDeps reads the down edges of ids from leg: one batched read where the
// store has one, otherwise one live DepList per row, at most LegFanOut at once.
// Edges are written once, at creation or by an explicit dep add, so a batch a
// cache answers from memory is as good as a live one; the blocker STATUSES the
// verdict turns on are always read live (readCopies).
func legDeps(ctx context.Context, leg Leg, ids []string) (map[string][]beads.Dep, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if batch, ok := beads.DepListBatchFor(leg.Store); ok {
		deps, err := batch.DepListBatch(ids)
		if err == nil {
			return deps, nil
		}
		if !errors.Is(err, beads.ErrDepListBatchUnsupported) {
			return nil, fmt.Errorf("%s store: listing dependencies: %w", leg.Label, err)
		}
	}
	live := beads.HandlesFor(leg.Store).Live
	lists := make([][]beads.Dep, len(ids))
	errs := make([]error, len(ids))
	fanout.Bounded(len(ids), LegFanOut, func(i int) {
		if errs[i] = ctx.Err(); errs[i] != nil {
			return
		}
		lists[i], errs[i] = live.DepList(ids[i], "down")
	})
	out := make(map[string][]beads.Dep, len(ids))
	for i, id := range ids {
		if errs[i] != nil {
			return nil, fmt.Errorf("%s store: listing dependencies of %q: %w", leg.Label, id, errs[i])
		}
		out[id] = lists[i]
	}
	return out, nil
}

// gate returns the rows of items no blocker holds, in their order, judging
// only as many as the bound needs: rows are judged a chunk at a time, grouped
// by the leg that served them, until limit rows are kept or items run out.
func (cs *crossStore) gate(ctx context.Context, owners map[string]int, items []beads.Bead, limit int) ([]beads.Bead, error) {
	if cs == nil || len(items) == 0 {
		if limit > 0 && len(items) > limit {
			items = items[:limit]
		}
		return items, nil
	}
	chunk := len(items)
	if limit > 0 {
		chunk = limit + boundedSlack
	}
	var kept []beads.Bead
	for start := 0; start < len(items) && (limit <= 0 || len(kept) < limit); start += chunk {
		part := items[start:min(start+chunk, len(items))]
		byLeg := make(map[int][]beads.Bead)
		var order []int
		for _, row := range part {
			idx, ok := owners[row.ID]
			if !ok {
				continue
			}
			if _, seen := byLeg[idx]; !seen {
				order = append(order, idx)
			}
			byLeg[idx] = append(byLeg[idx], row)
		}
		blocked := make(map[string]bool, len(part))
		errs := make([]error, len(order))
		results := make([]map[string]bool, len(order))
		fanout.Bounded(len(order), LegFanOut, func(i int) {
			results[i], errs[i] = cs.blockedRows(ctx, order[i], byLeg[order[i]])
		})
		for i, err := range errs {
			if err != nil {
				return nil, err
			}
			for id, b := range results[i] {
				blocked[id] = b
			}
		}
		for _, row := range part {
			if !blocked[row.ID] {
				kept = append(kept, row)
			}
		}
	}
	if limit > 0 && len(kept) > limit {
		kept = kept[:limit]
	}
	return kept, nil
}

// blockerStatuses resolves, for the in-progress tier, each target of a row
// served by legs[own], keyed by the target as written. A target with no entry
// is not a blocker of the row and is omitted from blocked_by.
//
// On a split city every target is judged from every copy the legs hold
// (verdictsFor), as the ready arm judges it, so a closed copy on the serving
// leg does not hide an open copy in another store; "external:<id>" is the bead
// <id>; a capability ref is omitted; a target no store holds is unresolved.
//
// On a city with no binding the answer is bd's: bd never blocks on an
// "external:" target, nor on a target the serving store does not hold, so both
// are omitted and no other leg is read.
func blockerStatuses(ctx context.Context, cs *crossStore, legs []Leg, own int, targets []string) (map[string]blockerVerdict, error) {
	out := make(map[string]blockerVerdict, len(targets))
	ids := make([]string, 0, len(targets))
	rawFor := make(map[string][]string, len(targets))
	for _, raw := range targets {
		if cs == nil && strings.HasPrefix(strings.TrimSpace(raw), "external:") {
			continue
		}
		target, capability := beads.ExternalBlockerTarget(raw)
		if capability {
			continue
		}
		if _, seen := rawFor[target]; !seen {
			ids = append(ids, target)
		}
		rawFor[target] = append(rawFor[target], raw)
	}
	set := func(id string, v blockerVerdict) {
		for _, raw := range rawFor[id] {
			out[raw] = v
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	if cs != nil {
		verdicts, err := cs.verdictsFor(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			set(id, verdicts[id])
		}
		return out, nil
	}
	held, err := beads.GetMany(legs[own].Store, ids)
	if err != nil {
		return nil, fmt.Errorf("%s store: %w", legs[own].Label, err)
	}
	for _, id := range ids {
		if b, ok := held[id]; ok {
			set(id, blockerVerdict{status: b.Status, satisfied: beads.DependencySatisfied(b.Status, beads.ReadinessWorkOutcome(b.Metadata))})
		}
	}
	return out, nil
}

// LegGate applies the split-city cross-store rule to ready rows read one leg
// at a time (pool demand, the API's cached ready read): to the rows one leg
// served from its own Ready it adds the rows that leg strands behind an absent
// blocker (its query selects them as Ready would), and it drops every row a
// blocker holds. A nil *LegGate is a city with no binding, and returns every
// row as read, so such a city's answer is the store's own.
//
// It is the same rule `gc ready` applies, through the same code, so the
// controller's demand and the worker's serve cannot disagree about a row.
//
// A LegGate is one moment's view: every blocker verdict, every judged row and
// every leg's absent-blocker listing it reads is kept for its life. A reader
// that gates several snapshots of the same legs (demand gates a live and a
// cached snapshot of each store) so reads each of them once, not once per
// snapshot. Build one for each pass.
type LegGate struct {
	cs *crossStore
	q  beads.ReadyQuery

	mu       sync.Mutex
	listings map[int]*legListing
}

// legListing is one leg's absent-blocker listing, read once per gate.
type legListing struct {
	once sync.Once
	res  legResult
}

// NewLegGate returns the gate over legs, whose stranded rows are listed as
// Ready would select them for q, or nil when legs are not a split city's.
func NewLegGate(legs []Leg, q beads.ReadyQuery) *LegGate {
	cs := newCrossStore(legs)
	if cs == nil {
		return nil
	}
	return &LegGate{cs: cs, q: q, listings: make(map[int]*legListing)}
}

// Gate gates rows, which legs[own] served from its own Ready. An own outside
// the legs returns rows as they are.
func (g *LegGate) Gate(ctx context.Context, own int, rows []beads.Bead) ([]beads.Bead, error) {
	if g == nil || own < 0 || own >= len(g.cs.legs) {
		return rows, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	owners := make(map[string]int, len(rows))
	items := make([]beads.Bead, 0, len(rows))
	for _, b := range rows {
		if _, seen := owners[b.ID]; seen {
			continue
		}
		owners[b.ID] = own
		items = append(items, b)
	}
	listed := g.listing(ctx, own)
	if listed.err != nil {
		return nil, fmt.Errorf("%s store: %w", g.cs.legs[own].Label, listed.err)
	}
	items = g.cs.mergeStranded(own, listed.rows, items, owners)
	return g.cs.gate(ctx, owners, items, 0)
}

// listing reads legs[own]'s absent-blocker listing, once for the gate's life.
func (g *LegGate) listing(ctx context.Context, own int) legResult {
	g.mu.Lock()
	l := g.listings[own]
	if l == nil {
		l = &legListing{}
		g.listings[own] = l
	}
	g.mu.Unlock()
	l.once.Do(func() { l.res = listStranded(ctx, g.cs.legs[own], g.q) })
	return l.res
}
