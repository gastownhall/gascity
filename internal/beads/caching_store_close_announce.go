package beads

import (
	"sort"
	"time"
)

// This file holds the CachingStore's bead.closed announcement bookkeeping: the
// queue of closes a non-announcing absorb installed (gastownhall/gascity#6860),
// the drain that announces each queued close once, and the record of the
// closes the cache announced (announcedCloses). That is once per cache,
// not once per close: a close another process made and announced itself
// reaches this cache either as its bead.closed, absorbed as already announced,
// or through a read, and when the read lands first this cache announces the
// close as well, so the bus carries two bead.closed for it. A read cannot tell
// that close from a bd close, which announces nothing, so it never waits for a
// peer's event: bead.closed delivery is at least once, and a consumer must
// treat a repeat for a bead it already saw close as a no-op.

// trackCloseTransitionLocked keeps unannouncedCloses in step with the row just
// installed for id. A row that is not closed cancels any queued close (a
// reopen before the drain must not announce a stale close), drops the record
// of the close the cache announced (announcedCloses), and starts a new close
// generation for the closing writes in flight (noteNotClosedLocked). A
// not-closed to closed transition is queued unless the caller announces it
// itself, and so is a closed row installed with no cached row before it while
// a closing write of ours is in flight (closeIntents) and its generation's
// close is unannounced. A queued close carries the close generation of the
// writes in flight when it was queued, 0 for none (queuedClose). Any other
// closed row installed with no cached row before it, and not announced, is
// marked silently installed (silentCloses), so a refresh with evidence of a
// recent close still announces it, unless claimed: a closing write of ours
// installs it and has claimed its close or found it announced elsewhere. A
// closed row laid over a closed one keeps the mark it had. Caller must hold
// c.mu in write mode.
func (c *CachingStore) trackCloseTransitionLocked(id string, previous Bead, hadPrevious bool, installed Bead, announced, claimed bool) {
	if installed.Status != "closed" {
		c.noteNotClosedLocked(id)
		c.dropQueuedCloseLocked(id)
		delete(c.announcedCloses, id)
		delete(c.silentCloses, id)
		return
	}
	if announced {
		c.dropQueuedCloseLocked(id)
		delete(c.silentCloses, id)
		return
	}
	switch intent := c.closeIntents[id]; {
	case hadPrevious && previous.Status != "closed":
		// A close observed over a cached open row.
	case !hadPrevious && intent != nil && intent.announced < intent.generation:
		// A close a write of ours is making, observed before that write
		// claims it: queue it so the claim (or the drain) announces it.
	case !hadPrevious && claimed:
		delete(c.silentCloses, id)
		return
	case !hadPrevious:
		// A close nobody announced, installed over no cached row.
		if c.silentCloses == nil {
			c.silentCloses = make(map[string]struct{})
		}
		c.silentCloses[id] = struct{}{}
		return
	default:
		return
	}
	delete(c.silentCloses, id)
	if c.unannouncedCloses == nil {
		c.unannouncedCloses = make(map[string]queuedClose)
	}
	queued := queuedClose{bead: cloneBead(installed)}
	if intent := c.closeIntents[id]; intent != nil {
		queued.generation = intent.generation
	}
	c.unannouncedCloses[id] = queued
	c.hasUnannouncedCloses.Store(true)
}

// queuedClose is a close waiting for the drain, with the close generation of
// id's in-flight closing writes when it was queued (0 for none). Draining it
// announces that generation's close and no later one: a close a write begins
// after a reopen is new, even when the reopen installed no row and so left this
// close queued.
type queuedClose struct {
	bead       Bead
	generation uint64
}

// announcedClose is a close the cache announced: when, and which version of
// the row it announced, by Revision where the backing stamps one and by
// UpdatedAt where it does not.
type announcedClose struct {
	at        time.Time
	updatedAt time.Time
	revision  int64
}

// announcedClosesPruneAt is the smallest record size from which recording a
// close prunes the expired ones first. Below it, the reconcile merge's prune
// keeps the record small, and a close costs no scan of it.
const announcedClosesPruneAt = 4096

// recordAnnouncedClose records that the cache is announcing b's close now.
// emitChange calls it for every bead.closed, before the announcement reaches
// onChange. Once the record holds announcedClosesPruneAt entries, and
// announcedClosesPruneNext, it prunes the expired ones first, so a cache whose
// reconcile loop does not run keeps a bounded record, and a burst of closes
// inside one recentWriteVerifyWindow, whose prunes free nothing, scans the
// record a logarithmic number of times rather than at every close. A record
// of at least announcedClosesPruneAt entries is also pruned once the last
// prune is a window old, so the size a burst raised announcedClosesPruneNext
// to does not hold its expired entries until the record grows back to it.
// Callers must not hold c.mu.
func (c *CachingStore) recordAnnouncedClose(b Bead) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clockNow()
	if n := len(c.announcedCloses); n >= max(announcedClosesPruneAt, c.announcedClosesPruneNext) ||
		(n >= announcedClosesPruneAt && now.Sub(c.announcedClosesPrunedAt) > recentWriteVerifyWindow) {
		c.pruneAnnouncedClosesLocked(now)
	}
	c.recordAnnouncedCloseLocked(b.ID, b, now)
}

// recordAnnouncedCloseLocked records that the cache announced id's close at
// now, closed being the row the announcement carries. Caller must hold c.mu
// in write mode.
func (c *CachingStore) recordAnnouncedCloseLocked(id string, closed Bead, now time.Time) {
	if c.announcedCloses == nil {
		c.announcedCloses = make(map[string]announcedClose)
	}
	c.announcedCloses[id] = announcedClose{at: now, updatedAt: closed.UpdatedAt, revision: closed.Revision}
}

// announcesCloseLocked reports whether RefreshRowReport announces fresh, the
// row it read for id, as a close rowRefreshChange does not: the close of a
// row the cache does not hold, or holds closed only because an install that
// announced nothing put it there (heldSilentlyLocked), with
// opts.AnnounceUnheldClose or opts.SeenOpen, or, with opts.SeenOpen, of a row
// the cache holds closed at another version. Neither is announced while a
// closing write of this cache is in flight for the row (that write announces
// it), or when the cache announced this close recently
// (announcedCloseCoversLocked). Caller must hold c.mu.
func (c *CachingStore) announcesCloseLocked(id string, opts RowRefreshOptions, cached Bead, held bool, fresh Bead) bool {
	switch {
	case fresh.Status != "closed" || c.closeIntents[id] != nil:
		return false
	case !held || c.heldSilentlyLocked(id, cached):
		if !opts.AnnounceUnheldClose && !opts.SeenOpen {
			return false
		}
	case !opts.SeenOpen || cached.Status != "closed" || sameRowVersion(cached, fresh):
		return false
	}
	return !c.announcedCloseCoversLocked(id, fresh, c.clockNow(), opts.SeenOpen)
}

// heldSilentlyLocked reports whether cached, id's cached row, is a closed row
// installed over no cached row with its close announced by nobody
// (silentCloses): for announcing its close, the cache holds it no more than
// it holds an uncached row. Caller must hold c.mu.
func (c *CachingStore) heldSilentlyLocked(id string, cached Bead) bool {
	if cached.Status != "closed" {
		return false
	}
	_, silent := c.silentCloses[id]
	return silent
}

// pruneSilentClosesLocked drops the silentCloses marks of rows the cache does
// not hold closed. An install or eviction of a row keeps its own mark in
// step; this sweeps the marks of rows dropped wholesale. Caller must hold c.mu
// in write mode.
func (c *CachingStore) pruneSilentClosesLocked() {
	for id := range c.silentCloses {
		if b, held := c.beads[id]; !held || b.Status != "closed" {
			delete(c.silentCloses, id)
		}
	}
}

// sameRowVersion reports whether a and b carry the same nonzero Revision, so
// are the same version of one row. Rows without one are never known to be.
func sameRowVersion(a, b Bead) bool {
	return a.Revision != 0 && a.Revision == b.Revision
}

// announcedCloseCoversLocked reports whether fresh, a closed row read for id,
// is a close the cache announced within recentWriteVerifyWindow of now. When
// both the announced row and fresh carry a Revision, it is that close if the
// revisions match: every write since, an edit or a reopen and close, mints a
// new one. Otherwise it is that close if UpdatedAt matches, unless seenOpen
// (RowRefreshOptions.SeenOpen): a backing that stamps whole seconds can tie
// a reopen and close inside the second of the announced close. A close that
// is not covered is announced again, as bead.closed delivery allows; so is a
// close announced with a row read before it committed (a lagged read after a
// closing write). Caller must hold c.mu.
func (c *CachingStore) announcedCloseCoversLocked(id string, fresh Bead, now time.Time, seenOpen bool) bool {
	ac, ok := c.announcedCloses[id]
	switch {
	case !ok || now.Sub(ac.at) > recentWriteVerifyWindow:
		return false
	case ac.revision != 0 && fresh.Revision != 0:
		return fresh.Revision == ac.revision
	}
	return !seenOpen && fresh.UpdatedAt.Equal(ac.updatedAt)
}

// pruneAnnouncedClosesLocked drops the announcedCloses entries older than
// recentWriteVerifyWindow, sets announcedClosesPruneNext to twice the entries
// it kept, and dates the prune (announcedClosesPrunedAt). Caller must hold
// c.mu in write mode.
func (c *CachingStore) pruneAnnouncedClosesLocked(now time.Time) {
	for id, ac := range c.announcedCloses {
		if now.Sub(ac.at) > recentWriteVerifyWindow {
			delete(c.announcedCloses, id)
		}
	}
	c.announcedClosesPruneNext = 2 * len(c.announcedCloses)
	c.announcedClosesPrunedAt = now
}

// dropQueuedCloseLocked removes id's queued close and reports whether one was
// queued. Caller must hold c.mu in write mode.
func (c *CachingStore) dropQueuedCloseLocked(id string) bool {
	if _, queued := c.unannouncedCloses[id]; !queued {
		return false
	}
	delete(c.unannouncedCloses, id)
	c.hasUnannouncedCloses.Store(len(c.unannouncedCloses) > 0)
	return true
}

// takeUnannouncedClosesLocked drains the queued closes in id order. Each
// drained close is noted for the generation it was queued under
// (noteCloseClaimedLocked), so the claims of the writes making that close
// decline and a write begun in a later generation still claims its own.
// Caller must hold c.mu in write mode.
func (c *CachingStore) takeUnannouncedClosesLocked() []Bead {
	if len(c.unannouncedCloses) == 0 {
		return nil
	}
	ids := make([]string, 0, len(c.unannouncedCloses))
	for id := range c.unannouncedCloses {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	closed := make([]Bead, 0, len(ids))
	for _, id := range ids {
		queued := c.unannouncedCloses[id]
		closed = append(closed, queued.bead)
		c.noteCloseClaimedLocked(id, queued.generation)
	}
	c.unannouncedCloses = nil
	c.hasUnannouncedCloses.Store(false)
	return closed
}

// announceUnannouncedCloses emits bead.closed for every close queued by
// trackCloseTransitionLocked. Draining under c.mu makes each queued close go
// out exactly once however many goroutines race here. The close was inferred
// from a read, so it carries ChangeRefresh, the inferred-from-a-read source;
// ChangeScan is only the reconcile scan's diff. Callers must not hold c.mu.
func (c *CachingStore) announceUnannouncedCloses() {
	if !c.hasUnannouncedCloses.Load() {
		return
	}
	c.mu.Lock()
	closed := c.takeUnannouncedClosesLocked()
	c.mu.Unlock()
	for _, b := range closed {
		c.emitChange(ChangeRefresh, "bead.closed", b)
	}
}

// claimCloseLocked decides whether a write that is about to announce id's close
// owns that bead.closed, and must be called before the write installs its row.
// It owns it when it cancels a close a read queued but has not drained yet, or
// when the cached row it replaces is not closed. A cached row that is already
// closed and not queued was announced by whoever installed it, so the write
// does not announce again. With no cached row, or one held only silently
// (heldSilentlyLocked), uncachedOwns decides: a write that itself closed the
// bead owns its close; one that merely refreshed an uncached row does not.
// Even a write that closed the bead declines when closed, the row it is
// about to announce, is a close this cache announced recently
// (announcedCloseCoversLocked): its backing close was a no-op on a row
// closed already, whose close a re-scan or refresh announced before the row
// left the cache. A nil closed (a write that has not read its row back yet)
// skips that check.
//
// fenced is racedWriteLocked's verdict: the write installs nothing. A held row
// that is not closed would then stay cached under the dirty mark, and the read
// that settles the mark (a dirty Get or list overlay, RefreshRow, a reconcile)
// would see it close and announce the close this write announces. So a fenced
// claim drops that row, as evictForConditionalClose does: the dirty mark keeps
// readers on the backing, and a read that finds no held row announces nothing.
// Caller must hold c.mu in write mode.
func (c *CachingStore) claimCloseLocked(id string, closed *Bead, uncachedOwns, fenced bool) bool {
	if c.dropQueuedCloseLocked(id) {
		return true
	}
	previous, held := c.beads[id]
	if !held || c.heldSilentlyLocked(id, previous) {
		if !uncachedOwns {
			return false
		}
		return closed == nil || !c.announcedCloseCoversLocked(id, *closed, c.clockNow(), false)
	}
	if previous.Status == "closed" {
		return false
	}
	if fenced {
		delete(c.beads, id)
		delete(c.deps, id)
	}
	return true
}

// updateEventTypeLocked names the event a write that installs installed for id
// announces: bead.closed when it leaves the bead closed and owns that close
// (claimCloseLocked), else bead.updated. An update that leaves the bead closed
// when it was not closed before is a close, whatever spelling asked for it, so
// it is announced as Close announces it; announcing it as bead.updated hid it
// from every bead.closed consumer (gastownhall/gascity#6860). With no cached
// row to compare, the update's own status=closed is the evidence. A close
// announced elsewhere while the update was in flight (announcedElsewhere, from
// its close intent) is not claimed; a claimed one is noted for the update's
// close generation. fenced is claimCloseLocked's. Caller must hold c.mu in
// write mode, before installing the row.
func (c *CachingStore) updateEventTypeLocked(id string, installed Bead, opts UpdateOpts, generation uint64, announcedElsewhere, fenced bool) string {
	if installed.Status != "closed" || announcedElsewhere {
		return "bead.updated"
	}
	if c.claimCloseLocked(id, &installed, updateCloses(opts), fenced) {
		c.noteCloseClaimedLocked(id, generation)
		return "bead.closed"
	}
	return "bead.updated"
}

// closeIntent is the in-flight state of the closing writes for one bead.
type closeIntent struct {
	// writers counts the closing writes between their backing write and
	// their claim.
	writers int
	// generation numbers the close the writes are making. It starts at 1 and
	// moves on each time the bead is seen not closed while they are in
	// flight (a reopen, or a read of a row still open): a close after that
	// is a new one, owed its own announcement. A write keeps the generation
	// it began in.
	generation uint64
	// announced is the latest generation whose close was announced while the
	// writes were in flight, 0 for none: by a closing write's claim, by a
	// drained queue entry, or by something else that saw the close (a
	// reconcile pass or RefreshRow that evicted the row as closed, an applied
	// bead.closed event). A drained entry credits only the generation it was
	// queued under, and none when it was queued with no intent in flight or
	// under an intent that has since ended. A write begun in generation g
	// declines its claim when announced >= g.
	announced uint64
}

// beginCloseIntent registers a closing write for id before its backing write
// (closeIntents) and returns the close generation it begins in. The write must
// end it with claimCloseIntentLocked, or with endCloseIntent when it fails
// before claiming.
func (c *CachingStore) beginCloseIntent(id string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeIntents == nil {
		c.closeIntents = make(map[string]*closeIntent)
	}
	intent := c.closeIntents[id]
	if intent == nil {
		intent = &closeIntent{generation: 1}
		c.closeIntents[id] = intent
	}
	intent.writers++
	return intent.generation
}

// endCloseIntent ends a closing write that will not claim (its backing write
// failed).
func (c *CachingStore) endCloseIntent(id string, generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endCloseIntentLocked(id, generation)
}

// endCloseIntentLocked drops one in-flight closing write for id, begun in
// generation, and reports whether something else announced that generation's
// close while the write was in flight. A close queued for the drain is then a
// repeat of the announced one and is dropped, unless the bead was seen not
// closed since the announcement: a close queued after that is a new one, and
// the drain still owes it. When the last write ends, a close still queued
// outlives the intent it was queued under and credits no generation when
// drained: a later intent numbers its generations from 1 again. Caller must
// hold c.mu in write mode.
func (c *CachingStore) endCloseIntentLocked(id string, generation uint64) (announcedElsewhere bool) {
	intent := c.closeIntents[id]
	if intent == nil {
		return false
	}
	intent.writers--
	if intent.writers <= 0 {
		delete(c.closeIntents, id)
		if queued, ok := c.unannouncedCloses[id]; ok {
			queued.generation = 0
			c.unannouncedCloses[id] = queued
		}
	}
	if intent.announced < generation {
		return false
	}
	if intent.announced >= intent.generation {
		c.dropQueuedCloseLocked(id)
	}
	return true
}

// noteCloseAnnouncedLocked records that id's close was announced by something
// other than its in-flight closing writes, so the claims of the writes begun
// in its current generation decline. It is a no-op when no closing write is in
// flight. Caller must hold c.mu in write mode.
func (c *CachingStore) noteCloseAnnouncedLocked(id string) {
	if intent := c.closeIntents[id]; intent != nil {
		intent.announced = intent.generation
	}
}

// noteCloseClaimedLocked records that id's close of generation was announced,
// by the claim of a closing write begun in it or by the drain of a close
// queued under it, so the claims of the writes begun in it or earlier decline.
// A write begun in a later generation is making a newer close and still claims
// it. Generation 0 credits nothing. Caller must hold c.mu in write mode.
func (c *CachingStore) noteCloseClaimedLocked(id string, generation uint64) {
	if intent := c.closeIntents[id]; intent != nil {
		intent.announced = max(intent.announced, generation)
	}
}

// noteNotClosedLocked starts a new close generation for id's in-flight closing
// writes: the bead was seen not closed, so a close after this is a new one,
// and an announcement of an earlier close does not cover it. It is a no-op
// when no closing write is in flight. Caller must hold c.mu in write mode.
func (c *CachingStore) noteNotClosedLocked(id string) {
	if intent := c.closeIntents[id]; intent != nil {
		intent.generation++
	}
}

// claimCloseIntentLocked ends a closing write's intent and claims its close
// (claimCloseLocked) under the same lock, so no read can queue the close
// between the two. A close something else announced while the write was in
// flight is not claimed (endCloseIntentLocked); a claimed close is noted for
// the write's generation (noteCloseClaimedLocked). closed and fenced are
// claimCloseLocked's. Caller must hold c.mu in write mode.
func (c *CachingStore) claimCloseIntentLocked(id string, generation uint64, closed *Bead, uncachedOwns, fenced bool) bool {
	if c.endCloseIntentLocked(id, generation) {
		return false
	}
	own := c.claimCloseLocked(id, closed, uncachedOwns, fenced)
	if own {
		c.noteCloseClaimedLocked(id, generation)
	}
	return own
}

// updateCloses reports whether opts writes status=closed.
func updateCloses(opts UpdateOpts) bool {
	return opts.Status != nil && *opts.Status == "closed"
}
