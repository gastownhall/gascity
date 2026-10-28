package beads

import (
	"maps"
	"sort"
)

// This file holds the CachingStore's bead.updated announcement for a change
// another process made that a READ installed: a live or parent-children list, a
// dirty-row Get, or the dirty overlay of a cached list. It is the update-shaped
// twin of the close queue in caching_store_close_announce.go.
//
// A write made outside this process — `gc hook --claim` claiming a step, an
// agent's `bd update`, a human — announces nothing. The reconcile scan is what
// normally notices it, diffs the cached row against the backing and announces
// bead.updated. But when a read refetches the row first, it installs the
// backing's row into the cache, and the scan that follows finds cache and
// backing in agreement and announces nothing. The change then never reaches the
// event bus at all, and every consumer that folds bead.* events — the run
// projection behind GET /runs, /runs/{id} and /runs/{id}/steps, the run detail
// view — keeps the stale row until some later change (typically the close)
// replaces it, while GET /bead/{id}, which reads the cache, already shows the
// new state. The close queue closed that hole for closes (#6860); this queue
// closes it for the transitions short of a close, a claim above all.
//
// Only lifecycle-visible fields count as a change (readAbsorbChanged): status,
// assignee and metadata. A list row and a point read legitimately carry other
// fields differently (labels, edges, the ready verdict), and announcing those
// differences would turn every alternating list/Get into an update flood. The
// announcement carries the row as installed, through the same ChangeRefresh
// source as RefreshRow, so a consumer treats it as the read inference it is.
//
// Reads overlap, and the one that answers last may have read first. Every
// install that changed what a consumer reads is therefore stamped
// (fenceReadInstallLocked), so a read begun before it installs, and so
// announces, nothing: announcements for a row never move backwards.

// readAbsorbChanged reports whether installed differs from previous in a field
// a bead.* consumer reads for lifecycle: status, assignee or metadata.
func readAbsorbChanged(previous, installed Bead) bool {
	return previous.Status != installed.Status ||
		previous.Assignee != installed.Assignee ||
		!maps.Equal(previous.Metadata, installed.Metadata)
}

// trackReadUpdateLocked keeps unannouncedUpdates in step with the row just
// installed for id. A read install (announce) whose row changed a held, open
// row's lifecycle fields queues the row. A closed row drops any queued update:
// its close is announced by the close queue or by the closer itself and
// carries the final state. Any other install that puts a different row in
// place supersedes the queued one: that installer announces its own row (a
// write, a reconcile diff), or the row came from the event bus, and replaying
// the older read after it would move consumers backwards. An install of the
// same row leaves the queue alone, so a no-diff reconcile cannot swallow it.
// Caller must hold c.mu in write mode.
func (c *CachingStore) trackReadUpdateLocked(id string, previous Bead, hadPrevious bool, installed Bead, announce bool) {
	if installed.Status == "closed" {
		c.dropQueuedUpdateLocked(id)
		return
	}
	if !announce {
		if queued, ok := c.unannouncedUpdates[id]; ok && readAbsorbChanged(queued, installed) {
			c.dropQueuedUpdateLocked(id)
		}
		return
	}
	if !hadPrevious || !readAbsorbChanged(previous, installed) {
		return
	}
	if c.unannouncedUpdates == nil {
		c.unannouncedUpdates = make(map[string]Bead)
	}
	c.unannouncedUpdates[id] = cloneBead(installed)
	c.hasUnannouncedUpdates.Store(true)
}

// fenceReadInstallLocked stamps id (noteMutationLocked) after a read install
// (announce) that changed what the cache holds for it in the fields a
// bead.* consumer reads: a row the cache did not hold, or a held row whose
// status, assignee or metadata changed (readAbsorbChanged). Two backing reads
// of one row are unordered: without a stamp, a read that began before the
// install, and may have read an older row, looks no different from one that
// began after it, so it would install its row over the newer one and queue it
// as a change, announcing a claimed or closed row unclaimed or running
// again. The stamp refuses every refetch, list refresh and scan that
// began before the install (refetchFencedLocked, the reconcile's
// fencedAfterSnapshot), as RefreshRow's stamp of a changed row does. An
// install that changed nothing a consumer reads is not stamped, so a list row
// and a point read that carry other fields differently do not move the
// mutation sequence on every read. Caller must hold c.mu in write mode, after
// the install cleared the fences its seqMode clears.
func (c *CachingStore) fenceReadInstallLocked(id string, previous Bead, hadPrevious bool, installed Bead, announce bool) {
	if !announce || (hadPrevious && !readAbsorbChanged(previous, installed)) {
		return
	}
	c.noteMutationLocked(id)
}

// dropQueuedUpdateLocked removes id's queued update. Caller must hold c.mu in
// write mode.
func (c *CachingStore) dropQueuedUpdateLocked(id string) {
	if _, queued := c.unannouncedUpdates[id]; !queued {
		return
	}
	delete(c.unannouncedUpdates, id)
	c.hasUnannouncedUpdates.Store(len(c.unannouncedUpdates) > 0)
}

// announceUnannouncedUpdates emits bead.updated for every update queued by
// trackReadUpdateLocked, in id order. Draining under c.mu makes each queued
// update go out exactly once however many goroutines race here. The update was
// inferred from a read, so it carries ChangeRefresh. Callers must not hold c.mu.
func (c *CachingStore) announceUnannouncedUpdates() {
	if !c.hasUnannouncedUpdates.Load() {
		return
	}
	c.mu.Lock()
	ids := make([]string, 0, len(c.unannouncedUpdates))
	for id := range c.unannouncedUpdates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	updated := make([]Bead, 0, len(ids))
	for _, id := range ids {
		updated = append(updated, c.unannouncedUpdates[id])
	}
	c.unannouncedUpdates = nil
	c.hasUnannouncedUpdates.Store(false)
	c.mu.Unlock()
	for _, b := range updated {
		c.emitChange(ChangeRefresh, "bead.updated", b)
	}
}

// announceReadInstalls drains everything a read installed without announcing:
// the queued updates and the queued closes. Callers must not hold c.mu.
func (c *CachingStore) announceReadInstalls() {
	c.announceUnannouncedUpdates()
	c.announceUnannouncedCloses()
}
