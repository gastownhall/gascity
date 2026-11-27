package sourceworkflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// FailedMoleculeSweepCloseReason is the close_reason the failed-molecule sweep stamps.
const FailedMoleculeSweepCloseReason = "failed-molecule sweep: instantiation failed and no caller re-entered within the grace window"

// FailedMoleculeSweepBudget bounds one SweepFailedMolecules pass and says
// where it starts. The pass checks the budget, and ctx, as an owner decision
// first reads or closes; a decision it lets start runs to its end, and one
// that neither reads nor closes never asks.
type FailedMoleculeSweepBudget struct {
	// MaxChains is how many chains the pass attempts: a close, whether or not
	// it closes anything, and an owner or chain read that fails each count
	// one. A chain whose owner keeps it costs none of it. Zero or less places
	// no cap.
	MaxChains int
	// Deadline is when the pass stops reading: past it, once the pass has
	// read an owner or a chain, it starts no decision that reads again, so a
	// caller sweeping several stores under one deadline still reads and closes
	// in each. The zero time places no bound.
	Deadline time.Time
	// From is where the pass starts: at the failed bead an earlier pass
	// stopped at (FailedMoleculeSweep.Next), or else the first one after it,
	// going on oldest first and then round to the older ones, so a backlog of
	// chains whose owners keep them cannot hold every pass short of the
	// chains behind it. The zero cursor starts at the oldest.
	From FailedMoleculeSweepCursor
}

// FailedMoleculeSweepCursor names a failed bead by its place in the order a
// pass goes through them: CreatedAt, then ID.
type FailedMoleculeSweepCursor struct {
	CreatedAt time.Time
	ID        string
}

// after reports whether c sorts after b.
func (c FailedMoleculeSweepCursor) after(b beads.Bead) bool {
	return b.CreatedAt.Before(c.CreatedAt) || (b.CreatedAt.Equal(c.CreatedAt) && b.ID < c.ID)
}

// spent reports whether the pass may start no decision that reads or closes,
// having attempted attempted chains and issued reads reads past its discovery
// read: ctx is done, MaxChains chains were attempted, or Deadline has passed
// and the pass has read.
func (b FailedMoleculeSweepBudget) spent(ctx context.Context, attempted, reads int) bool {
	switch {
	case ctx.Err() != nil:
		return true
	case b.MaxChains > 0 && attempted >= b.MaxChains:
		return true
	default:
		return reads > 0 && !b.Deadline.IsZero() && !time.Now().Before(b.Deadline)
	}
}

// FailedMoleculeSweep reports one SweepFailedMolecules pass over one store.
type FailedMoleculeSweep struct {
	Chains      []string // owner IDs whose chains this pass closed, in the order it closed them
	ChainClosed []int    // beads newly closed per entry of Chains, parallel to it
	Closed      int      // beads newly closed
	More        bool     // the budget or ctx stopped the pass before it decided on every chain
	// Next is the failed bead the pass stopped at when More is set, where the
	// next pass goes on (FailedMoleculeSweepBudget.From); zero otherwise.
	Next FailedMoleculeSweepCursor
}

// SweepFailedMolecules closes the chains of open molecule_failed beads whose
// owner has let them go once their grace has passed: a failure stamp, a
// bead's UpdatedAt or else its CreatedAt, at or before cutoff. It reads
// through leg, a federated leg over store, and closes through store.
//
// A molecule whose instantiation failed is stamped molecule_failed and left
// open (molecule.markFailed). Its same-key caller closes it on re-entry; a
// keyless mint (an order dispatch) has none, so this sweep is its owner. Each
// open failed bead names the next bead up by a gc.root_bead_id naming another
// bead, else by its ParentID, and the walk up through open failed beads ends
// at the chain's owner:
//
//   - An open failed bead the walk cannot leave tops its chain: the bead, every
//     open bead naming it by gc.root_bead_id and their failed ParentID
//     descendants. The chain waits until that bead's own stamp is past grace,
//     and stays open while it is stamped gc.gc_exempt (beadmeta.GCExempt).
//   - A bead outside them owns the chain when a gc.root_bead_id names it or
//     when it is a ParentID parent stamped molecule_failed itself, such as a
//     v1 root that closed. The chain is the failed beads naming it by
//     gc.root_bead_id and their failed ParentID descendants, and when the
//     owner failed too, its own: a v1 root's steps name it by ParentID alone.
//     The owner keeps them while it is open, such as a live workflow that did
//     not fail, or stamped gc.gc_exempt. Once it is gone or closed, the chain
//     is swept when every member is past grace, since separate failed
//     attachments can share an owner without having failed together.
//   - A ParentID parent that never failed, such as a live bead a molecule was
//     attached to, owns nothing: the bead under it tops its own chain.
//
// Controller-owned rows are never members. Each chain closes in one ordered
// CloseAll batch with gc.outcome=skipped and FailedMoleculeSweepCloseReason;
// molecule_failed stays on the closed beads, so the same-key adoption lookups
// never take one back.
//
// The pass never scans the store. It reads the open failed beads once and
// walks the chains over those rows; past that it reads each outside owner or
// parent once, and, per chain whose top it closes, that bead and the beads
// naming it by gc.root_bead_id, each read fresh. A top whose fresh read no
// longer qualifies it (toppedChain) keeps its chain until the next pass. The
// pass goes through the failed beads oldest first from budget.From, round to
// the older ones, and stops at the first one whose decision ctx or budget does
// not let read or close: it reports More, and Next, that bead. A discovery
// read error returns at once; any other read or close error is wrapped with
// the ID it concerns and joined into the returned error, and the pass goes on.
func SweepFailedMolecules(ctx context.Context, leg beads.FederatedReader, store beads.MoleculeTarget, cutoff time.Time, budget FailedMoleculeSweepBudget) (FailedMoleculeSweep, error) {
	var out FailedMoleculeSweep
	if store.Absent() {
		return out, fmt.Errorf("sweeping failed molecules: bead store unavailable")
	}
	if !beads.SameStore(leg, store) {
		return out, fmt.Errorf("sweeping failed molecules: the read leg is not over the store the sweep closes in")
	}
	pass, err := readFailedMolecules(ctx, leg, store, budget)
	if err != nil {
		return out, fmt.Errorf("sweeping failed molecules: listing molecule_failed beads: %w", err)
	}
	for _, b := range pass.from(budget.From) {
		if pass.done[b.ID] {
			continue
		}
		owner, chain, err := pass.decide(b, cutoff)
		switch {
		case pass.stopped:
			out.More = true
			out.Next = FailedMoleculeSweepCursor{CreatedAt: b.CreatedAt, ID: b.ID}
			return out, errors.Join(pass.errs...)
		case err != nil:
			pass.errs = append(pass.errs, fmt.Errorf("closing failed molecule %s: %w", owner, err))
			continue
		case len(chain) == 0:
			continue
		}
		n, err := pass.close(chain)
		out.Closed += n
		if err != nil {
			pass.errs = append(pass.errs, fmt.Errorf("closing failed molecule %s: %w", owner, err))
			continue
		}
		out.Chains = append(out.Chains, owner)
		out.ChainClosed = append(out.ChainClosed, n)
	}
	return out, errors.Join(pass.errs...)
}

// failedMolecules is one sweep pass's view of a store: its open failed beads,
// read once and indexed by ID, by ParentID and by the bead their
// gc.root_bead_id names, and what the pass has read and decided since.
type failedMolecules struct {
	store  beads.MoleculeTarget
	reader beads.LiveReader
	failed []beads.Bead          // open, not controller-owned, oldest first
	byID   map[string]beads.Bead // failed by ID
	kids   map[string][]string   // IDs of failed beads by their ParentID
	naming map[string][]string   // IDs of failed beads by the other bead their gc.root_bead_id names
	// outside holds the live reads of beads outside failed, by ID.
	outside map[string]outsideBead
	// done marks the failed beads whose owner the pass resolved or that a
	// close batch held, so each is resolved and batched at most once.
	done map[string]bool
	// batched marks the beads a close batch of this pass held, closed or not.
	batched map[string]bool
	// decided marks the owners whose chains the pass decided on.
	decided map[string]bool
	errs    []error

	ctx    context.Context
	budget FailedMoleculeSweepBudget
	// reads counts the reads the pass issued past its discovery read, and
	// attempted the chains it attempted (FailedMoleculeSweepBudget): its
	// closes and its reads that failed.
	reads, attempted int
	// started is set once the decision at hand may read and close (startIO),
	// and stopped once ctx or the budget refused one: the pass ends before the
	// bead it was deciding.
	started, stopped bool
}

// errPassStopped is an outside read the pass did not make, having stopped.
var errPassStopped = errors.New("the sweep pass stopped before this read")

// outsideBead is one live read of a bead outside the open failed beads.
type outsideBead struct {
	bead  beads.Bead
	found bool  // false when the store holds no such bead
	err   error // the read failed; neither bead nor found is known
}

// readFailedMolecules lists the open molecule_failed beads leg reads, across
// both tiers, whatever their stamp's age: owner resolution and the grace gates
// need the ones inside grace too. store is where the pass closes, and ctx and
// budget bound it.
func readFailedMolecules(ctx context.Context, leg beads.FederatedReader, store beads.MoleculeTarget, budget FailedMoleculeSweepBudget) (*failedMolecules, error) {
	reader := beads.LiveFor(leg)
	listed, err := reader.List(beads.ListQuery{
		Metadata: map[string]string{beadmeta.MoleculeFailedMetadataKey: "true"},
		TierMode: beads.TierBoth,
		Live:     true,
		Sort:     beads.SortCreatedAsc,
	})
	if err != nil {
		return nil, err
	}
	p := &failedMolecules{
		store:   store,
		reader:  reader,
		byID:    make(map[string]beads.Bead, len(listed)),
		kids:    make(map[string][]string),
		naming:  make(map[string][]string),
		outside: make(map[string]outsideBead),
		done:    make(map[string]bool),
		batched: make(map[string]bool),
		decided: make(map[string]bool),
		ctx:     ctx,
		budget:  budget,
	}
	for _, b := range listed {
		if _, dup := p.byID[b.ID]; dup || b.Status == "closed" || coordclass.ControllerOwned(b) {
			continue
		}
		p.byID[b.ID] = b
		p.failed = append(p.failed, b)
	}
	for _, b := range p.failed {
		if parent := strings.TrimSpace(b.ParentID); parent != "" && parent != b.ID {
			p.kids[parent] = append(p.kids[parent], b.ID)
		}
		if root := namedRoot(b); root != "" {
			p.naming[root] = append(p.naming[root], b.ID)
		}
	}
	return p, nil
}

// from is the failed beads in the order a pass starting at c goes through
// them: oldest first from the first bead c does not sort after, then round to
// the older ones.
func (p *failedMolecules) from(c FailedMoleculeSweepCursor) []beads.Bead {
	start := 0
	for start < len(p.failed) && c.after(p.failed[start]) {
		start++
	}
	order := make([]beads.Bead, 0, len(p.failed))
	order = append(order, p.failed[start:]...)
	return append(order, p.failed[:start]...)
}

// decide resolves b's owner and, the first time the pass meets that owner,
// returns the chain the pass may close (chainToClose), with the owner for its
// error. It decides nothing when the pass stops at b.
func (p *failedMolecules) decide(b beads.Bead, cutoff time.Time) (string, []beads.Bead, error) {
	p.started = false
	owner, ok := p.owner(b)
	if p.stopped {
		return "", nil, nil
	}
	p.done[b.ID] = true
	if !ok || p.decided[owner] {
		return "", nil, nil
	}
	chain, err := p.chainToClose(owner, cutoff)
	if len(chain) > 0 {
		// The close is I/O too: a decision whose reads the pass already
		// made starts here.
		p.startIO()
	}
	if p.stopped {
		return "", nil, nil
	}
	p.decided[owner] = true
	return owner, chain, err
}

// startIO reports whether the decision at hand may read or close. The first
// time a decision asks, ctx and the budget must allow it, and a refusal stops
// the pass; once allowed, the decision runs to its end.
func (p *failedMolecules) startIO() bool {
	switch {
	case p.started:
		return true
	case p.budget.spent(p.ctx, p.attempted, p.reads):
		p.stopped = true
		return false
	default:
		p.started = true
		return true
	}
}

// namedRoot is the other bead b's gc.root_bead_id names, or "" when it names
// none or b itself.
func namedRoot(b beads.Bead) string {
	if root := strings.TrimSpace(b.Metadata[beadmeta.RootBeadIDMetadataKey]); root != b.ID {
		return root
	}
	return ""
}

// owner is the ID of the bead that owns b's failed chain (SweepFailedMolecules
// says which). The walk up from b follows a gc.root_bead_id naming another
// bead, else b's ParentID, through the open failed beads, and stops at a bead
// with no link, at a cycle, or at the first link that leaves them. The bool is
// false when a ParentID parent could not be read: the pass recorded that
// error, or stopped.
func (p *failedMolecules) owner(b beads.Bead) (string, bool) {
	seen := map[string]bool{b.ID: true}
	for {
		next, named := namedRoot(b), true
		if next == "" {
			next, named = strings.TrimSpace(b.ParentID), false
		}
		if next == "" || next == b.ID {
			return b.ID, true
		}
		if up, failed := p.byID[next]; failed {
			if seen[next] {
				return b.ID, true
			}
			seen[next] = true
			b = up
			continue
		}
		if named {
			return next, true
		}
		parent := p.readOutside(next)
		if parent.err != nil {
			return "", false
		}
		if parent.found && parent.bead.Metadata[beadmeta.MoleculeFailedMetadataKey] == "true" {
			return next, true
		}
		return b.ID, true
	}
}

// readOutside reads the bead id, outside the open failed beads, live, once a
// pass, recording a failed read in the pass's errors, and as an attempted
// chain, the first time. When the pass may not read (startIO), it reads
// nothing and answers errPassStopped.
func (p *failedMolecules) readOutside(id string) outsideBead {
	if got, ok := p.outside[id]; ok {
		return got
	}
	if !p.startIO() {
		return outsideBead{err: errPassStopped}
	}
	var got outsideBead
	p.reads++
	b, err := p.reader.Get(id)
	switch {
	case err == nil:
		got = outsideBead{bead: b, found: true}
	case errors.Is(err, beads.ErrNotFound):
	default:
		got.err = err
		p.attempted++
		p.errs = append(p.errs, fmt.Errorf("resolving failed molecule owner %s: %w", id, err))
	}
	p.outside[id] = got
	return got
}

// chainToClose returns the beads of owner's chain this pass may close, none
// when its owner keeps it or a batch of this pass already held them all.
func (p *failedMolecules) chainToClose(owner string, cutoff time.Time) ([]beads.Bead, error) {
	if top, ok := p.byID[owner]; ok {
		if beadmeta.GCExempt(top.Metadata) || failureStampAfter(top, cutoff) {
			return nil, nil
		}
		chain, err := p.toppedChain(top, cutoff)
		if err != nil {
			return nil, err
		}
		return p.notBatched(chain), nil
	}
	o := p.readOutside(owner)
	if o.err != nil || (o.found && (o.bead.Status != "closed" || beadmeta.GCExempt(o.bead.Metadata))) {
		return nil, nil
	}
	failedOwner := o.found && o.bead.Metadata[beadmeta.MoleculeFailedMetadataKey] == "true"
	chain := p.notBatched(p.orphanedChain(owner, failedOwner))
	for _, b := range chain {
		if failureStampAfter(b, cutoff) {
			return nil, nil
		}
	}
	return chain, nil
}

// toppedChain reads, fresh across both tiers, top, the open failed bead that
// tops its chain, and every open bead naming it by gc.root_bead_id, and adds
// their failed ParentID descendants. A read that fails counts as an attempted
// chain. It returns no chain when the pass may not read (startIO), or when the
// fresh read of top no longer qualifies it (stillTops): the snapshot the pass
// decided on is stale, and the next pass decides again.
func (p *failedMolecules) toppedChain(top beads.Bead, cutoff time.Time) ([]beads.Bead, error) {
	if !p.startIO() {
		return nil, nil
	}
	p.reads++
	root, err := p.reader.Get(top.ID)
	if err != nil {
		p.attempted++
		return nil, fmt.Errorf("reading root: %w", err)
	}
	if !stillTops(root, top, cutoff) {
		return nil, nil
	}
	p.reads++
	members, err := p.reader.List(beads.ListQuery{
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: top.ID},
		TierMode: beads.TierBoth,
		Live:     true,
	})
	if err != nil {
		p.attempted++
		return nil, fmt.Errorf("listing members: %w", err)
	}
	chain := append([]beads.Bead{root}, members...)
	seeds := make([]string, 0, len(chain))
	for _, b := range chain {
		seeds = append(seeds, b.ID)
	}
	return append(chain, p.descendants(seeds)...), nil
}

// stillTops reports whether fresh, a fresh read of the top the pass's
// snapshot holds as snapshot, still tops a chain the sweep may close: it is
// open, stamped molecule_failed, not stamped gc.gc_exempt, stamped at or
// before cutoff, and linked by gc.root_bead_id and ParentID as the snapshot
// was. A write since the discovery read can have changed any of them.
func stillTops(fresh, snapshot beads.Bead, cutoff time.Time) bool {
	switch {
	case fresh.Status == "closed", fresh.Metadata[beadmeta.MoleculeFailedMetadataKey] != "true":
		return false
	case beadmeta.GCExempt(fresh.Metadata), failureStampAfter(fresh, cutoff):
		return false
	default:
		return namedRoot(fresh) == namedRoot(snapshot) && strings.TrimSpace(fresh.ParentID) == strings.TrimSpace(snapshot.ParentID)
	}
}

// orphanedChain is the chain of owner, a bead outside the open failed beads:
// the failed beads naming it by gc.root_bead_id and their failed ParentID
// descendants, and when the owner failed too (failedOwner), its own failed
// ParentID descendants, as a v1 root's steps name it by ParentID alone. A
// ParentID child of an owner that never failed tops its own chain (owner).
func (p *failedMolecules) orphanedChain(owner string, failedOwner bool) []beads.Bead {
	var seeds []string
	if failedOwner {
		seeds = append(seeds, owner)
	}
	var chain []beads.Bead
	for _, id := range p.naming[owner] {
		seeds = append(seeds, id)
		chain = append(chain, p.byID[id])
	}
	return append(chain, p.descendants(seeds)...)
}

// descendants returns the open failed beads below seeds by ParentID, reached
// through open failed beads only, that seeds does not already name.
func (p *failedMolecules) descendants(seeds []string) []beads.Bead {
	seen := make(map[string]bool, len(seeds))
	for _, id := range seeds {
		seen[id] = true
	}
	var out []beads.Bead
	for level := seeds; len(level) > 0; {
		var next []string
		for _, id := range level {
			for _, kid := range p.kids[id] {
				if seen[kid] {
					continue
				}
				seen[kid] = true
				out = append(out, p.byID[kid])
				next = append(next, kid)
			}
		}
		level = next
	}
	return out
}

// notBatched drops the beads a close batch of this pass already held.
func (p *failedMolecules) notBatched(chain []beads.Bead) []beads.Bead {
	kept := chain[:0:0]
	for _, b := range chain {
		if !p.batched[b.ID] {
			kept = append(kept, b)
		}
	}
	return kept
}

// close closes chain's open beads in one ordered batch, counts an attempted
// chain, and marks the beads held by a batch of this pass, whether or not it
// closes them.
func (p *failedMolecules) close(chain []beads.Bead) (int, error) {
	p.attempted++
	for _, b := range chain {
		p.batched[b.ID] = true
		p.done[b.ID] = true
	}
	ids, err := orderedOpenBeads(p.store, chain, nil)
	if err != nil {
		return 0, fmt.Errorf("ordering close batch: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return p.store.CloseAll(ids, map[string]string{
		beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped,
		"close_reason":              FailedMoleculeSweepCloseReason,
	})
}

// failureStampAfter reports whether b's failure stamp, its UpdatedAt or its
// CreatedAt when UpdatedAt is zero, is after cutoff: inside the grace window.
func failureStampAfter(b beads.Bead, cutoff time.Time) bool {
	stamp := b.UpdatedAt
	if stamp.IsZero() {
		stamp = b.CreatedAt
	}
	return stamp.After(cutoff)
}
