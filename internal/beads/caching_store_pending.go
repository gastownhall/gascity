package beads

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cachePendingPublication identifies a definite successful durable mutation
// that still needs one or more complete observer publications. The encoded
// sequence keeps this value comparable, which lets recovery validate an exact
// snapshot without holding the registry lock across backing I/O. token changes
// only for a new durable mutation; generation may move forward across cache
// fences that do not add an event. gate reserves the mutation's original place
// in the per-ID observer queue. hadRow records whether this cache had ever
// observed the bead when the intent was first retained: it distinguishes a
// conditional-write evict/re-absorb of a previously published row (an update
// that must not resurrect a creation edge) from a genuine first observation
// (whose creation edge must survive). It stays a comparable value.
type cachePendingPublication struct {
	sequence   string
	generation uint64
	token      uint64
	gate       uint64
	hadRow     bool
}

type cachePendingEvent struct {
	eventType  string
	occurredAt time.Time
}

const (
	cachePendingEventFieldSeparator = "\x1f"
	cachePendingEventSeparator      = "\x1e"
)

func encodeCachePendingEvents(events []cachePendingEvent) string {
	if len(events) == 0 {
		return ""
	}
	var encoded strings.Builder
	for _, event := range events {
		if event.eventType == "" {
			continue
		}
		encoded.WriteString(event.eventType)
		encoded.WriteString(cachePendingEventFieldSeparator)
		if !event.occurredAt.IsZero() {
			encoded.WriteString(strconv.FormatInt(event.occurredAt.UnixNano(), 10))
		}
		encoded.WriteString(cachePendingEventSeparator)
	}
	return encoded.String()
}

func decodeCachePendingEvents(encoded string) []cachePendingEvent {
	if encoded == "" {
		return nil
	}
	parts := strings.Split(encoded, cachePendingEventSeparator)
	events := make([]cachePendingEvent, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		fields := strings.SplitN(part, cachePendingEventFieldSeparator, 2)
		if len(fields) == 0 || fields[0] == "" {
			continue
		}
		event := cachePendingEvent{eventType: fields[0]}
		if len(fields) == 2 && fields[1] != "" {
			if nanos, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				event.occurredAt = time.Unix(0, nanos).UTC()
			}
		}
		events = append(events, event)
	}
	return events
}

func mergeCachePendingEvent(sequence string, next cachePendingEvent) string {
	events := decodeCachePendingEvents(sequence)
	if len(events) == 0 {
		return encodeCachePendingEvents([]cachePendingEvent{next})
	}

	last := len(events) - 1
	switch {
	case next.eventType == "bead.updated" && len(events) == 1 && events[0].eventType == "bead.created":
		// A still-unpublished creation carries the latest recovered snapshot, so
		// an ordinary update before publication does not need a second event.
		return sequence
	case events[last].eventType == next.eventType:
		// Consecutive ordinary mutations of the same kind coalesce at the latest
		// occurrence. Distinct lifecycle edges remain ordered below.
		events[last] = next
	default:
		events = append(events, next)
	}
	return encodeCachePendingEvents(events)
}

func (p cachePendingPublication) events() []cachePendingEvent {
	return decodeCachePendingEvents(p.sequence)
}

func (p cachePendingPublication) containsEvent(eventType string) bool {
	for _, event := range p.events() {
		if event.eventType == eventType {
			return true
		}
	}
	return false
}

// cachePendingPublications is shared by every CachingStore over one durable
// scope. It is process-local recovery state, not a durable exactly-once outbox.
type cachePendingPublications struct {
	mu        sync.Mutex
	byID      map[string]cachePendingPublication
	nextToken uint64
	nextGate  uint64
}

func (p *cachePendingPublications) retain(id, eventType string, occurredAt time.Time, generation uint64, hadRow bool) (cachePendingPublication, bool) {
	if p == nil || id == "" || eventType == "" || generation == 0 {
		return cachePendingPublication{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byID == nil {
		p.byID = make(map[string]cachePendingPublication)
	}
	previous, existed := p.byID[id]
	if !existed {
		p.nextGate++
		previous.gate = p.nextGate
	}
	p.nextToken++
	previous.token = p.nextToken
	previous.sequence = mergeCachePendingEvent(previous.sequence, cachePendingEvent{
		eventType:  eventType,
		occurredAt: occurredAt,
	})
	if generation > previous.generation {
		previous.generation = generation
	}
	// Once any retention observed the row, the intent is not a first observation.
	previous.hadRow = previous.hadRow || hadRow
	p.byID[id] = previous
	return previous, !existed
}

// resequence preserves an older definite publication across a later cache
// fence. Some operations advance the shared generation even when they do not
// produce a replacement observer snapshot (for example a losing conditional
// write or an ambiguous write error). Without carrying the intent forward,
// reconciliation would reject it as stale forever.
func (p *cachePendingPublications) resequence(ids []string, generation uint64) {
	if p == nil || generation == 0 || len(ids) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range ids {
		if publication, ok := p.byID[id]; ok {
			if generation <= publication.generation {
				continue
			}
			publication.generation = generation
			p.byID[id] = publication
		}
	}
}

func (p *cachePendingPublications) resequenceIfToken(id string, token, generation uint64) bool {
	if p == nil || id == "" || token == 0 || generation == 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	publication, ok := p.byID[id]
	if !ok || publication.token != token {
		return false
	}
	if generation > publication.generation {
		publication.generation = generation
		p.byID[id] = publication
	}
	return true
}

// rewriteSequenceIfToken replaces the encoded event sequence of a retained
// intent while preserving its gate, token, and generation. Reconciliation uses
// it when a recovered first observation proves the retained sequence's final
// type is not the edge that must be published: an update retained for a bead
// this cache never created is really a creation, so the intent is rewritten so
// gate resolution emits the creation edge instead of erasing it. Returns false
// when the token no longer matches.
func (p *cachePendingPublications) rewriteSequenceIfToken(id string, token uint64, events []cachePendingEvent) bool {
	if p == nil || id == "" || token == 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	publication, ok := p.byID[id]
	if !ok || publication.token != token {
		return false
	}
	publication.sequence = encodeCachePendingEvents(events)
	p.byID[id] = publication
	return true
}

func (p *cachePendingPublications) hasAny() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byID) != 0
}

// oldestPendingOccurredAt returns the earliest first-event occurrence time
// across every retained publication. A reconcile deadline anchored on it fires
// recovery within one adaptive interval of the intent itself, rather than on
// cache freshness that busy writes keep advancing. The zero time reports that
// no retained intent carries a usable occurrence time.
func (p *cachePendingPublications) oldestPendingOccurredAt() time.Time {
	if p == nil {
		return time.Time{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var oldest time.Time
	for _, publication := range p.byID {
		events := publication.events()
		if len(events) == 0 {
			continue
		}
		first := events[0].occurredAt
		if first.IsZero() {
			continue
		}
		if oldest.IsZero() || first.Before(oldest) {
			oldest = first
		}
	}
	return oldest
}

func (p *cachePendingPublications) snapshot() map[string]cachePendingPublication {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.byID) == 0 {
		return nil
	}
	result := make(map[string]cachePendingPublication, len(p.byID))
	for id, publication := range p.byID {
		result[id] = publication
	}
	return result
}

func (p *cachePendingPublications) current(id string) (cachePendingPublication, bool) {
	if p == nil || id == "" {
		return cachePendingPublication{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	publication, ok := p.byID[id]
	return publication, ok
}

func (p *cachePendingPublications) matches(id string, want cachePendingPublication) bool {
	got, ok := p.current(id)
	return ok && got == want
}

func (p *cachePendingPublications) clearIfToken(id string, token uint64) bool {
	if p == nil || id == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if got, ok := p.byID[id]; !ok || got.token != token {
		return false
	}
	delete(p.byID, id)
	return true
}

func (g *cacheMutationGeneration) generationFor(id string) uint64 {
	if g == nil || id == "" {
		return 0
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.byID[id]
}

// retainPendingPublication retains an intent for a bead this cache had already
// observed. Callers on the genuine first-observation path (a mutation whose
// pre-read and post-write refresh both failed for a bead the cache never held)
// must use retainPendingPublicationHadRow with hadRow=false so a later recovered
// creation edge is not erased.
func (c *CachingStore) retainPendingPublication(id, eventType string) {
	c.retainPendingPublicationHadRow(id, eventType, true)
}

func (c *CachingStore) retainPendingPublicationHadRow(id, eventType string, hadRow bool) {
	c.retainPendingPublicationAt(id, eventType, time.Now().UTC(), hadRow)
}

func (c *CachingStore) retainPendingPublicationAt(id, eventType string, occurredAt time.Time, hadRow bool) {
	if c == nil || c.onChange == nil || c.pendingPublications == nil || c.mutationGeneration == nil {
		return
	}
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	generation := c.mutationGeneration.generationFor(id)
	publication, newGate := c.pendingPublications.retain(id, eventType, occurredAt, generation, hadRow)
	if publication.token == 0 {
		return
	}
	if newGate {
		c.reserveOrderedPendingGate(id, publication.gate)
	}

	// A generation can advance between the read above and retain. Catch the
	// exact intent up without appending a duplicate semantic event. A later
	// retain changes token and owns its own catch-up loop.
	for {
		latest := c.mutationGeneration.generationFor(id)
		if latest <= publication.generation {
			return
		}
		if !c.pendingPublications.resequenceIfToken(id, publication.token, latest) {
			return
		}
		publication.generation = latest
	}
}

type cachePendingRecovery struct {
	publication cachePendingPublication
	bead        Bead
	deps        []Dep
}

// readPendingPublications reads the complete targeted snapshot for each retained
// intent. The second result carries the intents whose bead is authoritatively
// absent (a final ErrNotFound from BOTH the ordinary and closed-projection
// reads): the caller reconciles these against the full scan to release an intent
// stranded by an out-of-band delete rather than leaking its gate placeholder
// forever. The third result reports whether any intent hit a non-NotFound read
// error, so the caller can back off its pending reconcile deadline instead of
// spinning at the poll floor on a persistently unreadable row.
func (c *CachingStore) readPendingPublications(snapshot map[string]cachePendingPublication) (map[string]cachePendingRecovery, map[string]cachePendingPublication, bool) {
	if len(snapshot) == 0 {
		return nil, nil, false
	}
	recovered := make(map[string]cachePendingRecovery, len(snapshot))
	var notFound map[string]cachePendingPublication
	recoveryFailed := false
	for id, publication := range snapshot {
		bead, deps, err := c.readBeadWithDeps(id)
		if errors.Is(err, ErrNotFound) {
			// A closed-hiding backing hides any committed close from the ordinary
			// targeted read whatever the intent's retained event type, so consult the
			// closed projection before treating the bead as absent. Only a closed
			// projection that ALSO reports ErrNotFound proves the row is truly gone;
			// a closed-read fault must defer, never masquerade as an out-of-band
			// delete.
			closedBead, closedDeps, closedErr := c.readClosedBeadWithDeps(id)
			switch {
			case closedErr == nil:
				bead, deps, err = closedBead, closedDeps, nil
			case errors.Is(closedErr, ErrNotFound):
				// Leave err as ErrNotFound: absence proven, classified below.
			default:
				err = closedErr
			}
		}
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				if notFound == nil {
					notFound = make(map[string]cachePendingPublication)
				}
				notFound[id] = publication
			} else {
				recoveryFailed = true
				c.recordProblem("recover pending observer publication", fmt.Errorf("%s: %w", id, err))
			}
			continue
		}
		if bead.ID != id {
			recoveryFailed = true
			c.recordProblem("recover pending observer publication", fmt.Errorf("%s: backing returned bead %q", id, bead.ID))
			continue
		}
		bead.Dependencies = cloneDeps(deps)
		bead.Needs = nil
		recovered[id] = cachePendingRecovery{
			publication: publication,
			bead:        bead,
			deps:        cloneDeps(deps),
		}
	}
	return recovered, notFound, recoveryFailed
}

// readClosedBeadWithDeps is the rare recovery path for stores whose ordinary
// Get intentionally hides closed rows. Prefer a targeted canonical read when
// available; otherwise ask the backing for its closed projection and filter by
// exact ID. A dependency failure defers publication rather than emitting a
// partial replacement snapshot.
func (c *CachingStore) readClosedBeadWithDeps(id string) (Bead, []Dep, error) {
	if getter, ok := c.backing.(CanonicalGetter); ok {
		bead, err := getter.GetCanonical(id)
		if err == nil {
			if bead.ID != id || bead.Status != "closed" {
				return Bead{}, nil, ErrNotFound
			}
			deps, depErr := c.backing.DepList(id, "")
			if depErr != nil {
				return Bead{}, nil, depErr
			}
			bead.Dependencies = cloneDeps(deps)
			bead.Needs = nil
			return bead, deps, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Bead{}, nil, err
		}
	}

	items, err := c.backing.List(ListQuery{
		Status:        "closed",
		IncludeClosed: true,
		AllowScan:     true,
		TierMode:      TierBoth,
	})
	if err != nil {
		return Bead{}, nil, err
	}
	for _, bead := range items {
		if bead.ID != id {
			continue
		}
		deps, depErr := c.backing.DepList(id, "")
		if depErr != nil {
			return Bead{}, nil, depErr
		}
		bead.Dependencies = cloneDeps(deps)
		bead.Needs = nil
		return bead, deps, nil
	}
	return Bead{}, nil, ErrNotFound
}

// validatePendingRecoveries drops reads superseded while backing I/O was in
// flight. The caller holds the shared mutation scope exclusively, so a match
// remains stable through cache merge and ordered-queue reservation.
func (c *CachingStore) validatePendingRecoveries(recovered map[string]cachePendingRecovery) map[string]cachePendingRecovery {
	if len(recovered) == 0 {
		return nil
	}
	valid := make(map[string]cachePendingRecovery, len(recovered))
	for id, recovery := range recovered {
		if c.mutationGeneration.generationFor(id) != recovery.publication.generation {
			continue
		}
		if !c.pendingPublications.matches(id, recovery.publication) {
			continue
		}
		valid[id] = recovery
	}
	return valid
}

// applyPendingRecoveriesLocked installs complete targeted reads after the
// ordinary full-scan merge and replaces any older same-ID diff notification
// with the coalesced pending event. Caller must hold c.mu for writing.
func (c *CachingStore) applyPendingRecoveriesLocked(res *mergeSectionResult, recovered map[string]cachePendingRecovery, listed map[string]Bead, now time.Time) {
	if len(recovered) == 0 {
		return
	}

	for id, recovery := range recovered {
		payload := cloneBead(recovery.bead)
		payload.Dependencies = cloneDeps(recovery.deps)
		payload.Needs = nil
		cacheBead := cloneBead(payload)
		if listedBead, ok := listed[id]; ok {
			// Keep the cache row's duplicated dependency fields in the same
			// shape List uses. The separately stored deps row remains the exact
			// targeted snapshot, while the observer payload is always complete.
			cacheBead.Dependencies = cloneDeps(listedBead.Dependencies)
			cacheBead.Needs = append([]string(nil), listedBead.Needs...)
		}
		if cacheBead.Status != "closed" {
			c.absorbFreshLocked(id, cacheBead, now, absorbOpts{
				depsMode:   depsExplicit,
				deps:       recovery.deps,
				seqMode:    seqClearGuarded,
				clearDirty: true,
			})
		} else {
			c.clearDependentReadyProjectionsLocked(id)
			// The cache is an active-row projection. A targeted authoritative
			// close must remove even a recent dirty fallback that the ordinary
			// active-only merge correctly fenced during this pass.
			c.evictLocked(id)
		}

		pendingEvents := recovery.publication.events()
		if len(pendingEvents) == 0 {
			continue
		}
		finalPendingEvent := pendingEvents[len(pendingEvents)-1]
		pendingNotification := cacheNotification{
			eventType:      finalPendingEvent.eventType,
			bead:           payload,
			occurredAt:     finalPendingEvent.occurredAt,
			resolvePending: true,
		}
		naturalIndex := -1
		for i := range res.notifications {
			if res.notifications[i].bead.ID != id {
				continue
			}
			res.notifications[i].bead = cloneBead(payload)
			if naturalIndex < 0 {
				naturalIndex = i
			}
		}
		if naturalIndex < 0 {
			res.notifications = append(res.notifications, pendingNotification)
			c.countPendingRecoveriesLocked(res, pendingEvents)
			continue
		}

		natural := res.notifications[naturalIndex]
		switch {
		case natural.eventType == "bead.created" && finalPendingEvent.eventType == "bead.updated" &&
			!recovery.publication.hadRow && !recovery.publication.containsEvent("bead.created"):
			// The full scan first observed this bead (a synthesized bead.created)
			// while the only retained intent is an update recorded for a bead this
			// cache never held (hadRow=false) — its pre-write snapshot and post-write
			// refresh both failed. Erasing the creation edge here would make the bead
			// invisible to created-gated consumers (eventexport drops bead.updated). A
			// hadRow=true intent instead falls through to the coalescing replace below:
			// that is a conditional-write evict/re-absorb of an already published row,
			// where the recovered update, not a second creation, is the correct edge.
			// Rewrite the intent to a creation so gate resolution publishes the
			// creation edge with the recovered complete payload; the natural
			// bead.created (already carrying that payload and counted by the merge)
			// resolves the gate.
			createdOccurredAt := finalPendingEvent.occurredAt
			if !payload.CreatedAt.IsZero() {
				createdOccurredAt = payload.CreatedAt.UTC()
			}
			c.pendingPublications.rewriteSequenceIfToken(id, recovery.publication.token,
				[]cachePendingEvent{{eventType: "bead.created", occurredAt: createdOccurredAt}})
			res.notifications[naturalIndex].bead = cloneBead(payload)
			res.notifications[naturalIndex].occurredAt = createdOccurredAt
			res.notifications[naturalIndex].resolvePending = true

		case natural.eventType == finalPendingEvent.eventType ||
			len(pendingEvents) == 1 && finalPendingEvent.eventType == "bead.created" && natural.eventType == "bead.updated" ||
			finalPendingEvent.eventType == "bead.updated" && natural.eventType == "bead.created" ||
			finalPendingEvent.eventType == "bead.closed" && natural.eventType == "bead.created":
			// Coalesce onto the recovered publication: same-type, a creation that
			// subsumes an update, an update that replaces an evict/re-absorb created,
			// or a recovered close that supersedes a lagging scan's stale
			// first-observation created (a bead first seen already closed emits no
			// creation edge, so the close must not be followed by a created).
			res.notifications[naturalIndex] = pendingNotification
			c.countRecoveryEventDeltaLocked(res, natural.eventType, -1)
			c.countPendingRecoveriesLocked(res, pendingEvents)

		default:
			// A distinct later lifecycle observation must remain after the older
			// recovered publication (for example updated -> closed). Insert rather
			// than replacing solely by bead ID.
			res.notifications = append(res.notifications, cacheNotification{})
			copy(res.notifications[naturalIndex+1:], res.notifications[naturalIndex:])
			res.notifications[naturalIndex] = pendingNotification
			c.countPendingRecoveriesLocked(res, pendingEvents)
		}
	}
}

func (c *CachingStore) countPendingRecoveriesLocked(res *mergeSectionResult, events []cachePendingEvent) {
	for _, event := range events {
		c.countRecoveryEventDeltaLocked(res, event.eventType, 1)
	}
}

func (c *CachingStore) countRecoveryEventDeltaLocked(res *mergeSectionResult, eventType string, delta int64) {
	switch eventType {
	case "bead.created":
		res.adds += delta
		c.stats.Adds += delta
	case "bead.closed", "bead.deleted":
		res.removes += delta
		c.stats.Removes += delta
	default:
		res.updates += delta
		c.stats.Updates += delta
	}
}
