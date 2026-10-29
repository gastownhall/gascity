package readywork

import (
	"context"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fanout"
	"github.com/gastownhall/gascity/internal/storeref"
)

// Leg is one federated source: the store, the name a failure reports it by, and
// the plan's verdict on what that failure MEANS.
//
// The label is what turns "the federation failed" into "the graph store
// failed", which is the difference between a diagnosable outage and a silent
// short answer. OnError is carried rather than consulted: the plan marks a rig
// leg PartialDegrade, and this reader escalates it to fatal because a CLI array
// has nowhere to say "this is short". Carrying it keeps that escalation visible
// at the point it happens.
type Leg struct {
	Label   string
	Store   beads.Store
	OnError storeref.ErrPolicy
	// ClassBinding marks a relocated coordination-class binding. It is the one
	// leg whose rows can also live in an earlier leg: `gc storage migrate`
	// copies beads across with their original, work-shaped ids, so until its
	// clear of the work store has run a migrated bead can be held both by the
	// work store it was minted in and by the binding. The pushdown's exactness
	// guard reads it (pushdown.go).
	ClassBinding bool
	// ReservedPrefixes are the reserved id namespaces a class binding holds.
	// The pushdown treats an id in one of them as the binding's alone, one no
	// earlier leg can also hold; only the binding's other, work-shaped ids can.
	// That holds only while no work leg's prefix shadows a reserved one
	// (pushdown.go, Co-resident ids).
	ReservedPrefixes []string
}

// LegFanOut bounds how many legs are read at once.
//
// A leg read is latency-bound, one query against its own store, so reading them
// one after another makes every answer wait for the SUM of them; the bound keeps
// a many-rig city from opening a connection storm against one server. Legs past
// it wait for a slot.
const LegFanOut = 8

// Read answers q over legs: validate, federate, filter, order, bound, enrich,
// project.
//
// Every flag is validated before a single store is touched. The legs are read
// concurrently (at most LegFanOut at once) and merged in LEG ORDER, deduped by
// id with the first leg to return an id winning, so the answer and the leg an
// error names do not depend on which read finished first.
//
// The bound is applied LAST, after the merge and after the order: the answer is
// the top N of the city, not of whichever leg filled the quota first. A store
// may be ASKED for a bounded answer (pushdown.go), but only where that is
// provably the same top N.
func Read(legs []Leg, q Query) ([]ReadyBead, error) {
	return ReadContext(context.Background(), legs, q)
}

// ReadContext is Read under ctx: once ctx ends, no further store read starts,
// and the answer is ctx's error rather than rows.
//
// It is checked before every leg read, before every refill and probe, and
// between the candidate read and the blocker reads. A store read already in
// flight runs to its own end, because beads.Store reads take no context; a
// caller that must stop those too hands in stores bound to ctx (the supervisor
// binds every leg to its request, cmd/gc ready_work_api.go).
func ReadContext(ctx context.Context, legs []Leg, q Query) ([]ReadyBead, error) {
	c, err := compile(q)
	if err != nil {
		return nil, err
	}
	return readCompiled(ctx, legs, c, false)
}

// ReadUntilUnblocked is ReadContext for an in_progress answer ended at its first
// unblocked row, one with no blocker that is not closed: the gated rows ahead of
// it, then it. It is `gc ready --until-unblocked`, the crash-recovery tier's
// read, which resumes the first row of its window it can serve and needs
// nothing past it.
//
// Blockers are resolved in answer order and only as far as the cut, so a row
// past it whose blockers cannot be resolved does not fail the read. A row at or
// ahead of the cut still does: the cut cannot be placed past a row whose
// blockers are unknown, since that row may be the one to resume. A window with
// no unblocked row is answered whole.
//
// The cut is not a parameter of GET /work/ready. The controller answers the
// whole window with every row resolved, and the CLI ends that answer with
// CutUntilUnblocked, which is this read's answer whenever every row of the
// window resolves. When one past the cut does not, the controller's read fails
// and the CLI reads in-process, through this.
func ReadUntilUnblocked(ctx context.Context, legs []Leg, q Query) ([]ReadyBead, error) {
	c, err := compileUntilUnblocked(q)
	if err != nil {
		return nil, err
	}
	return readCompiled(ctx, legs, c, true)
}

// ValidateUntilUnblocked reports whether q is a query ReadUntilUnblocked can
// cut, without reading anything.
func ValidateUntilUnblocked(q Query) error {
	_, err := compileUntilUnblocked(q)
	return err
}

// compileUntilUnblocked compiles q and refuses every arm but in_progress, whose
// rows alone carry their blockers: anywhere else the cut would be a flag that
// looks honored and selects nothing.
func compileUntilUnblocked(q Query) (compiled, error) {
	c, err := compile(q)
	if err != nil {
		return compiled{}, err
	}
	if c.status != StatusInProgress {
		return compiled{}, fmt.Errorf("--until-unblocked needs --status %s, the one status whose rows carry their blockers", StatusInProgress)
	}
	return c, nil
}

// CutUntilUnblocked ends rows, an in_progress answer whose every row carries its
// blockers, at its first unblocked row: the cut ReadUntilUnblocked makes,
// applied to an answer that resolved every row.
func CutUntilUnblocked(rows []ReadyBead) []ReadyBead {
	for i, row := range rows {
		if !hasOpenBlocker(row.BlockedBy) {
			return rows[:i+1]
		}
	}
	return rows
}

// readCompiled answers the compiled query c over legs, ending an in_progress
// answer at its first unblocked row when untilUnblocked is set.
func readCompiled(ctx context.Context, legs []Leg, c compiled, untilUnblocked bool) ([]ReadyBead, error) {
	cs := newCrossStore(legs)
	items, owners, err := readCandidates(ctx, legs, c, cs)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, err
	}
	if c.status == "" {
		// On a split city the ready arm re-judges cross-store blockers
		// (crossstore.go): it adds the rows a store strands behind an absent
		// blocker, then withholds every row a blocker still holds. Elsewhere
		// cs is nil and this is the stores' own answer.
		items, err = readyAnswer(ctx, cs, c, items, owners)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil {
			return nil, err
		}
		return toReadyBeads(items, nil), nil
	}
	items = c.filter(items)
	c.order(items)
	if c.query.Limit > 0 && len(items) > c.query.Limit {
		items = items[:c.query.Limit]
	}
	// Enrich AFTER the bound, so the dependency reads are paid only for the rows
	// actually emitted. The crash-recovery tier asks for --limit=20
	// --until-unblocked and serves the first of those rows its gate passes, so
	// its rows are resolved only as far as that one (blockedByUntilUnblocked).
	var blockers map[string][]ReadyBeadBlocker
	if c.status == StatusInProgress {
		if untilUnblocked {
			items, blockers, err = blockedByUntilUnblocked(ctx, cs, items, legs, owners)
		} else {
			blockers, err = blockedByForRows(ctx, cs, items, legs, owners)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil {
			return nil, err
		}
	}
	return toReadyBeads(items, blockers), nil
}

// readyAnswer turns the ready arm's merged candidates into its answer: the
// stranded rows added, the predicates applied, the order imposed, the bound
// applied after the cross-store gate. A bounded read already sized each leg's
// cut past the rows the gate holds (boundedRead), and every verdict is
// memoized, so the gate here re-reads nothing it has judged.
func readyAnswer(ctx context.Context, cs *crossStore, c compiled, items []beads.Bead, owners map[string]int) ([]beads.Bead, error) {
	items, err := cs.addStranded(ctx, readyReadQuery(c), items, owners)
	if err != nil {
		return nil, err
	}
	items = c.filter(items)
	c.order(items)
	return cs.gate(ctx, owners, items, c.query.Limit)
}

// readCandidates reads the merged candidate set from the legs, and which leg
// served each row.
//
// An empty --status is the default: claimable, ready work. Any other status
// lists beads in exactly that status; the flag is a real selector, not a single
// special case with an "everything else means ready" fallthrough, which answers
// `--status closed` with open work.
//
// Every arm states beads.FederatedReadTier, and none has a branch that could
// state anything else. A tier left at the zero value is not a neutral default
// across these legs: the work legs' bead-policy layer rewrites it to TierBoth and
// the relocated class leg has no such layer, so the merged answer would be one
// question asked of the work stores and a narrower one asked of the store that
// holds the execution DAG (ga-8lyxc).
//
// owners maps each merged row to the index of the leg that served it. It is
// recorded at MERGE time rather than re-probed afterwards: the merge is
// first-leg-wins, and a co-resident bead that is open in one leg and in_progress
// in another would resolve to the wrong store under a fresh probe.
func readCandidates(ctx context.Context, legs []Leg, c compiled, cs *crossStore) ([]beads.Bead, map[string]int, error) {
	switch c.status {
	case "":
		return readReadyCandidates(ctx, legs, c, cs)
	case StatusOpen:
		return readOpenStatusCandidates(ctx, legs)
	case StatusInProgress:
		// The crash-recovery tier, and the one whose rows carry blocked_by. The
		// assignee is pushed into each leg's list; it is matched exactly on every
		// backend, and re-applied to the merged set like every predicate.
		plain := beads.ListQuery{Status: c.status, TierMode: beads.FederatedReadTier}
		if c.assignee == "" {
			return federate(ctx, legs, listRead(plain))
		}
		narrowed := plain
		narrowed.Assignee = c.assignee
		return readNarrowed(ctx, legs, c, listRead(narrowed), listRead(plain))
	default:
		return federate(ctx, legs, listRead(beads.ListQuery{
			Status:   c.status,
			TierMode: beads.FederatedReadTier,
		}))
	}
}

// readOpenStatusCandidates answers `--status open` with Gas City's "open": every
// non-closed bead that is not in_progress, whatever richer status its store
// records.
//
// The bd-backed store keeps bd's own statuses (blocked, deferred, hooked,
// pinned, custom ones like review) and folds them into "open" only when it maps
// a row (beads.mapBdStatus). A ListQuery{Status: "open"} is pushed down to bd as
// `--status=open`, which matches only the literal stored "open", so those rows
// were missing from the open leg, and the blocked leg's bd rows are relabeled
// "open" and filtered out there too. They were invisible to all four --status
// legs, which the workflows pack uses as its exhaustive root inventory, so an
// invisible root could be slung a duplicate child workflow. The native store
// already answers a non-Live open as "exclude closed and in_progress" (a Live
// open there is bd's raw one, nativeListPushesRawOpen); reading every
// non-closed row and keeping the mapped "open" ones gives the bd-backed store the
// same answer without widening ListQuery{Status: "open"} for the controller
// callers whose readiness gates rely on bd's narrower status (ga-3mv5d3).
func readOpenStatusCandidates(ctx context.Context, legs []Leg) ([]beads.Bead, map[string]int, error) {
	items, owners, err := federate(ctx, legs, listRead(beads.ListQuery{
		AllowScan: true,
		TierMode:  beads.FederatedReadTier,
	}))
	if err != nil {
		return nil, nil, err
	}
	out := items[:0]
	for _, item := range items {
		if item.Status == StatusOpen {
			out = append(out, item)
			continue
		}
		delete(owners, item.ID)
	}
	return out, owners, nil
}

// legRead reads one leg. Every read goes through the store's LIVE handle: a
// caching-wrapped leg answers from its backing store, never from its read cache
// (see the package documentation). A read that makes more than one store call
// checks ctx before each call after the first.
type legRead func(ctx context.Context, leg Leg) legResult

// legResult is one leg's read, kept at that leg's index so every merge is
// ordered by leg position whatever order the reads finished in.
type legResult struct {
	rows []beads.Bead
	err  error
}

func listRead(q beads.ListQuery) legRead {
	return func(_ context.Context, leg Leg) legResult {
		rows, err := beads.HandlesFor(leg.Store).Live.List(q)
		return legResult{rows: rows, err: err}
	}
}

// federate reads every leg, at most LegFanOut at once, and merges the rows in
// leg order, deduped by id with the FIRST leg to return an id winning: first by
// POSITION in legs, not by which read finished first.
//
// Any leg error, including a partial read (which beads reports as an error
// carrying rows), fails the whole federation, ESCALATING the plan's
// PartialDegrade verdict on the rig legs: a CLI array has nowhere to say "this is
// short", so a degraded leg would be served as a short array indistinguishable
// from "no work".
func federate(ctx context.Context, legs []Leg, read legRead) ([]beads.Bead, map[string]int, error) {
	results := readLegs(ctx, legs, read)
	if err := firstLegError(legs, results); err != nil {
		return nil, nil, err
	}
	items, owners := merge(results)
	return items, owners, nil
}

// readLegs runs read against every leg, at most LegFanOut at once, and returns
// each leg's outcome at that leg's index.
//
// Every leg is a DISTINCT store (the plan dedupes legs by store identity), so no
// two reads here share a store, and each writes only its own slot. Every leg is
// read even when an earlier one fails: the reads have no side effects, and
// firstLegError picks the error a sequential reader would have stopped at. A leg
// whose turn comes after ctx has ended is not read; its slot holds ctx's error.
func readLegs(ctx context.Context, legs []Leg, read legRead) []legResult {
	results := make([]legResult, len(legs))
	fanout.Bounded(len(legs), LegFanOut, func(i int) {
		if err := ctx.Err(); err != nil {
			results[i] = legResult{err: err}
			return
		}
		results[i] = read(ctx, legs[i])
	})
	return results
}

// firstLegError returns the error of the first leg, by POSITION, whose read
// failed, labeled with that leg. First by position rather than first to fail is
// deliberate: it is the leg a sequential reader would have hit and stopped at,
// so the error names the same leg however the reads raced.
func firstLegError(legs []Leg, results []legResult) error {
	for i, res := range results {
		if res.err != nil {
			return fmt.Errorf("%s store: %w", legs[i].Label, res.err)
		}
	}
	return nil
}

// merge concatenates the legs' rows in leg order, first leg to return an id
// winning, and records which leg served each kept row.
func merge(results []legResult) ([]beads.Bead, map[string]int) {
	var merged []beads.Bead
	owners := make(map[string]int)
	for i, res := range results {
		for _, b := range res.rows {
			if _, seen := owners[b.ID]; seen {
				continue
			}
			owners[b.ID] = i
			merged = append(merged, b)
		}
	}
	return merged, owners
}

// blockedByForRows resolves each in_progress row's blocking dependencies
// through the leg that SERVED that row.
//
// Reading through the serving leg is the whole point. The shell enrichment this
// replaces resolves blockers with `bd show` in the agent's work directory, which
// on a split city resolves nothing for a relocated id and, failing open, serves
// the candidate as unblocked: correct for a resume, but it re-serves a
// gate-blocked step on every tick. A relocated step's blockers live in the same
// store the step does, so the leg that answered is the leg that can answer this.
//
// The reads run concurrently: the legs, each leg's rows, and each leg's distinct
// blocking targets. Each row is answered exactly as the per-row walk answered
// it, with its blockers in the same order, and a failure names the same row and
// blocker it always did.
//
// A dependency read that FAILS is returned, not swallowed. Reporting a gated
// bead as unblocked is what re-serves work no worker can advance; the federation
// is fail-loud for the same reason. A read whose turn comes after ctx has ended
// is not made, and fails its row with ctx's error.
func blockedByForRows(ctx context.Context, cs *crossStore, items []beads.Bead, legs []Leg, owners map[string]int) (map[string][]ReadyBeadBlocker, error) {
	blockers, failed := resolveBlockers(ctx, cs, items, legs, owners)
	// Report the failure of the first row, in answer order, whose enrichment
	// failed: the row the sequential walk would have stopped at.
	for _, item := range items {
		if err, bad := failed[item.ID]; bad {
			return nil, err
		}
	}
	return blockers, nil
}

// blockedByUntilUnblocked is blockedByForRows for an answer cut at its first
// unblocked row (ReadUntilUnblocked): it returns the rows through that one, and
// their blockers.
//
// The rows are resolved in answer order, a batch of LegFanOut at a time, which
// keeps the concurrency of the full walk while a window whose first row is
// unblocked costs one batch. A row whose enrichment failed fails the read when
// the walk reaches it, because the cut cannot be placed past a row whose
// blockers are unknown. A failure past the cut, in the cut's own batch, is
// never consulted, and no later batch is read.
func blockedByUntilUnblocked(ctx context.Context, cs *crossStore, items []beads.Bead, legs []Leg, owners map[string]int) ([]beads.Bead, map[string][]ReadyBeadBlocker, error) {
	out := make(map[string][]ReadyBeadBlocker)
	for start := 0; start < len(items); start += LegFanOut {
		batch := items[start:min(start+LegFanOut, len(items))]
		blockers, failed := resolveBlockers(ctx, cs, batch, legs, owners)
		for i, item := range batch {
			if err, bad := failed[item.ID]; bad {
				return nil, nil, err
			}
			if rowBlockers := blockers[item.ID]; len(rowBlockers) > 0 {
				out[item.ID] = rowBlockers
			}
			if !hasOpenBlocker(blockers[item.ID]) {
				return items[:start+i+1], out, nil
			}
		}
	}
	return items, out, nil
}

// hasOpenBlocker reports whether any of blockers is not closed: the serve
// gate's reading of blocked_by, where every status but closed blocks
// (internal/config openBlockerCountJQ).
func hasOpenBlocker(blockers []ReadyBeadBlocker) bool {
	for _, blocker := range blockers {
		if !strings.EqualFold(blocker.Status, StatusClosed) {
			return true
		}
	}
	return false
}

// resolveBlockers reads the blockers of every row of items through the leg that
// served it, as blockedByForRows describes, and returns each row's blockers
// (only rows that have any) and each failed row's error.
func resolveBlockers(ctx context.Context, cs *crossStore, items []beads.Bead, legs []Leg, owners map[string]int) (map[string][]ReadyBeadBlocker, map[string]error) {
	if len(owners) == 0 || len(items) == 0 {
		return nil, nil
	}
	byLeg := make(map[int][]string)
	var legOrder []int
	for _, item := range items {
		idx, ok := owners[item.ID]
		if !ok || legs[idx].Store == nil {
			continue
		}
		if _, seen := byLeg[idx]; !seen {
			legOrder = append(legOrder, idx)
		}
		byLeg[idx] = append(byLeg[idx], item.ID)
	}
	perLeg := make([]legBlockers, len(legOrder))
	fanout.Bounded(len(legOrder), LegFanOut, func(i int) {
		idx := legOrder[i]
		perLeg[i] = readLegBlockers(ctx, cs, legs, idx, byLeg[idx])
	})
	failed := make(map[string]error)
	out := make(map[string][]ReadyBeadBlocker, len(items))
	for _, lb := range perLeg {
		for id, err := range lb.errs {
			failed[id] = err
		}
		for id, blockers := range lb.blockers {
			if len(blockers) > 0 {
				out[id] = blockers
			}
		}
	}
	return out, failed
}

// legBlockers is one leg's enrichment: each row's blockers, or the error that
// row's enrichment hit.
type legBlockers struct {
	blockers map[string][]ReadyBeadBlocker
	errs     map[string]error
}

// readLegBlockers resolves the blockers of ids, every one of them served by
// legs[own].
//
// Each row's edges are read with the same per-row DepList the sequential walk
// used, so every row's blockers come back in the order that read returns them.
// The distinct targets are then read by id from the serving leg in one call
// (blockerStatuses, crossstore.go); on a split city every target is read
// from every leg, one call each, judged from every copy as the ready arm
// judges it, and reported with the status of the copy that holds it and a
// reason. A target no store holds is reported as BlockerUnresolved, and one
// whose every unreleased copy is closed blocked as BlockerClosedBlocked; every
// blocked_by reader treats both as gated: such a blocker blocks its row, and
// never fails the tier. An "external:<rig>:<capability>" target names no bead
// and is omitted, as bd omits it. A blocker READ that fails still fails the
// answer.
func readLegBlockers(ctx context.Context, cs *crossStore, legs []Leg, own int, ids []string) legBlockers {
	leg := legs[own]
	out := legBlockers{blockers: make(map[string][]ReadyBeadBlocker, len(ids)), errs: make(map[string]error)}
	live := beads.HandlesFor(leg.Store).Live
	deps := make([][]beads.Dep, len(ids))
	depErrs := make([]error, len(ids))
	fanout.Bounded(len(ids), LegFanOut, func(i int) {
		if depErrs[i] = ctx.Err(); depErrs[i] != nil {
			return
		}
		deps[i], depErrs[i] = live.DepList(ids[i], "down")
	})
	var targets []string
	seen := make(map[string]bool)
	for i, id := range ids {
		if depErrs[i] != nil {
			out.errs[id] = fmt.Errorf("%s store: listing dependencies of %q: %w", leg.Label, id, depErrs[i])
			continue
		}
		for _, dep := range deps[i] {
			if !beads.IsReadyBlockingDependencyType(dep.Type) || seen[dep.DependsOnID] {
				continue
			}
			seen[dep.DependsOnID] = true
			targets = append(targets, dep.DependsOnID)
		}
	}
	verdicts, verr := blockerStatuses(ctx, cs, legs, own, targets)
	for i, id := range ids {
		if depErrs[i] != nil {
			continue
		}
		var blockers []ReadyBeadBlocker
		for _, dep := range deps[i] {
			if !beads.IsReadyBlockingDependencyType(dep.Type) {
				continue
			}
			if verr != nil {
				out.errs[id] = fmt.Errorf("%s store: resolving blocker %q of %q: %w", leg.Label, dep.DependsOnID, id, verr)
				blockers = nil
				break
			}
			v, ok := verdicts[dep.DependsOnID]
			if !ok {
				// Not a blocker of this row (blockerStatuses): a capability
				// ref, or on a city with no binding one bd never blocks on.
				continue
			}
			blockers = append(blockers, ReadyBeadBlocker{ID: dep.DependsOnID, Status: v.status, Reason: v.reason})
		}
		if _, failed := out.errs[id]; !failed {
			out.blockers[id] = blockers
		}
	}
	return out
}
