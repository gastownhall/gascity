package beads

import "sort"

// This file holds the CachingStore's bead.closed announcement bookkeeping: the
// queue of closes a non-announcing absorb installed (gastownhall/gascity#6860)
// and the drain that announces each queued close once. That is once per cache,
// not once per close: a close another process made and announced itself
// reaches this cache either as its bead.closed, absorbed as already announced,
// or through a read, and when the read lands first this cache announces the
// close as well, so the bus carries two bead.closed for it. A read cannot tell
// that close from a bd close, which announces nothing, so it never waits for a
// peer's event: bead.closed delivery is at least once, and a consumer must
// treat a repeat for a bead it already saw close as a no-op.

// trackCloseTransitionLocked keeps unannouncedCloses in step with the row just
// installed for id. A row that is not closed cancels any queued close (a
// reopen before the drain must not announce a stale close) and starts a new
// close generation for the closing writes in flight (noteNotClosedLocked). A
// not-closed to closed transition is queued unless the caller announces it
// itself, and so is a closed row installed with no cached row before it while
// a closing write of ours is in flight (closeIntents) and its generation's
// close is unannounced. A queued close carries the close generation of the
// writes in flight when it was queued, 0 for none (queuedClose). Caller must
// hold c.mu in write mode.
func (c *CachingStore) trackCloseTransitionLocked(id string, previous Bead, hadPrevious bool, installed Bead, announced bool) {
	if installed.Status != "closed" {
		c.noteNotClosedLocked(id)
		c.dropQueuedCloseLocked(id)
		return
	}
	if announced {
		c.dropQueuedCloseLocked(id)
		return
	}
	switch intent := c.closeIntents[id]; {
	case hadPrevious && previous.Status != "closed":
		// A close observed over a cached open row.
	case !hadPrevious && intent != nil && intent.announced < intent.generation:
		// A close a write of ours is making, observed before that write
		// claims it: queue it so the claim (or the drain) announces it.
	default:
		return
	}
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
// from a read, so it carries ChangeScan's inferred source. Callers must not
// hold c.mu.
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
// does not announce again. With no cached row, uncachedOwns decides: a write
// that itself closed the bead owns its close; one that merely refreshed an
// uncached row does not.
//
// fenced is racedWriteLocked's verdict: the write installs nothing. A held row
// that is not closed would then stay cached under the dirty mark, and the read
// that settles the mark (a dirty Get or list overlay, RefreshRow, a reconcile)
// would see it close and announce the close this write announces. So a fenced
// claim drops that row, as evictForConditionalClose does: the dirty mark keeps
// readers on the backing, and a read that finds no held row announces nothing.
// Caller must hold c.mu in write mode.
func (c *CachingStore) claimCloseLocked(id string, uncachedOwns, fenced bool) bool {
	if c.dropQueuedCloseLocked(id) {
		return true
	}
	previous, held := c.beads[id]
	if !held {
		return uncachedOwns
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
	if c.claimCloseLocked(id, updateCloses(opts), fenced) {
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
// the write's generation (noteCloseClaimedLocked). fenced is
// claimCloseLocked's. Caller must hold c.mu in write mode.
func (c *CachingStore) claimCloseIntentLocked(id string, generation uint64, uncachedOwns, fenced bool) bool {
	if c.endCloseIntentLocked(id, generation) {
		return false
	}
	own := c.claimCloseLocked(id, uncachedOwns, fenced)
	if own {
		c.noteCloseClaimedLocked(id, generation)
	}
	return own
}

// updateCloses reports whether opts writes status=closed.
func updateCloses(opts UpdateOpts) bool {
	return opts.Status != nil && *opts.Status == "closed"
}
