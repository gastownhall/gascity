package main

import (
	"sync"
	"time"
)

// defaultDemandMismatchThreshold is the consecutive-cycle count at which a
// demand-mismatch episode is first published, absent a caller override.
const defaultDemandMismatchThreshold = 3

// demandMismatchTracker detects the stranded-demand treadmill ga-oedyvj
// documents: a template pool wakes on demand, the session claims nothing,
// idle-timeout kills it, and demand is unchanged — repeatedly. It is
// detection-only, in-memory, and best-effort: state does not survive a
// reconciler restart and callers decide what (if anything) to do with a
// published episode.
//
// recordIdleKillCycle is called once per idle-timeout kill. The caller
// supplies whether the killed session still held open assigned work and the
// template's current pool demand. Pool demand is already computed by the
// reconciler for other purposes; the assigned-work answer costs one store
// query at the kill site.
//
// That query is deliberately the Open variant, not the Awake variant the
// idle-timeout ladder itself gathers (ga-nllza6). Reaching the kill means no
// *awake* assigned work held the session up, so asking the broader Open
// question here is what separates a session killed holding nothing at all —
// the stranded-demand cycle this tracker counts — from one killed by the
// ladder's same-bead defer backstop while it still held open work.
type demandMismatchTracker struct {
	mu         sync.Mutex
	threshold  int
	byTemplate map[string]*demandMismatchEpisode
}

// demandMismatchEpisode is the in-flight state for one template's
// consecutive no-claim idle-kill streak.
type demandMismatchEpisode struct {
	count       int
	lastDemand  int
	firstSeen   time.Time
	lastSeen    time.Time
	publishedAt int // count value at last publish; 0 means never published this episode.
}

// newDemandMismatchTracker creates a tracker. threshold <= 0 defaults to
// defaultDemandMismatchThreshold.
func newDemandMismatchTracker(threshold int) *demandMismatchTracker {
	if threshold <= 0 {
		threshold = defaultDemandMismatchThreshold
	}
	return &demandMismatchTracker{
		threshold:  threshold,
		byTemplate: make(map[string]*demandMismatchEpisode),
	}
}

// recordIdleKillCycle records one idle-timeout-kill observation for
// template and reports whether the caller should publish a demand-mismatch
// event now.
//
// A killed session that still held open assigned work, or a template with
// no current pool demand, is not a stranded-demand cycle and resets the
// episode. A drop in demand since the last observation is treated as
// evidence that some session made progress on the template's backlog
// elsewhere: the old episode resets, but this observation — itself still a
// no-claim idle-kill — starts a new one at count 1 rather than being
// discarded. Otherwise the episode continues, publishing once at threshold
// and then backing off at doubling multiples (threshold, 2x, 4x, ...) so a
// long-lived episode does not flood the event bus.
func (t *demandMismatchTracker) recordIdleKillCycle(template string, hasOpenWork bool, demand int, now time.Time) (shouldPublish bool, count int, firstSeen time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if hasOpenWork || demand <= 0 {
		delete(t.byTemplate, template)
		return false, 0, time.Time{}
	}

	ep, ok := t.byTemplate[template]
	if !ok || demand < ep.lastDemand {
		ep = &demandMismatchEpisode{count: 1, lastDemand: demand, firstSeen: now, lastSeen: now}
		t.byTemplate[template] = ep
		return false, ep.count, ep.firstSeen
	}

	ep.count++
	ep.lastDemand = demand
	ep.lastSeen = now

	if ep.count < t.threshold {
		return false, ep.count, ep.firstSeen
	}

	next := t.threshold
	for next <= ep.publishedAt {
		next *= 2
	}
	if ep.count < next {
		return false, ep.count, ep.firstSeen
	}
	ep.publishedAt = ep.count
	return true, ep.count, ep.firstSeen
}
