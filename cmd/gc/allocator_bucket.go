package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/resilience"
)

// The allocator's start budget (CONTRACT C5.7, C5.9 as amended by AM5 and
// C6): one token bucket for grants (a create costs none), the city in-flight
// cap, and the endpoint capacity breaker. The bucket refills by time, not by
// pass, so per-event passes cannot multiply the start rate (design §4a). The
// allocator is its only reader and writer, so it needs no lock.
//
// Unwired in this slice: P3-5b's admission walk calls admitStart, and P3-7
// carries the bucket between passes.

// bucketState is the token bucket: capacity max_wakes_per_tick, refilled by
// the same number per patrol interval. Refill is continuous: Credit holds the
// time accrued toward the next token, so the arithmetic stays exact.
type bucketState struct {
	Tokens     int
	Credit     time.Duration
	LastRefill time.Time
}

// refill accrues tokens for the time since the last refill, up to capacity.
// The first refill fills the bucket. A clock that steps back accrues nothing
// and re-anchors the mark at now, so refill resumes from the new clock
// rather than freezing until it regains the old mark. A non-positive
// interval or capacity accrues nothing: a bad config must not refill the
// bucket to full on every pass.
func (b bucketState) refill(now time.Time, capacity int, interval time.Duration) bucketState {
	capacity = max(capacity, 0)
	if b.LastRefill.IsZero() {
		return bucketState{Tokens: capacity, LastRefill: now}
	}
	if capacity > 0 && interval > 0 {
		if now.After(b.LastRefill) {
			b.Credit += now.Sub(b.LastRefill)
		}
		if period := interval / time.Duration(capacity); period > 0 {
			n := b.Credit / period
			b.Tokens += int(min(n, time.Duration(capacity)))
			b.Credit -= n * period
		}
	}
	b.LastRefill = now
	return b.capped(capacity)
}

// debit takes cost tokens, or reports that the bucket holds too few.
func (b bucketState) debit(cost int) (bucketState, bool) {
	if b.Tokens < cost {
		return b, false
	}
	b.Tokens -= cost
	return b, true
}

// refund returns n tokens: a grant released before any key issued it, the
// only refund (C5.7).
func (b bucketState) refund(n, capacity int) bucketState {
	b.Tokens += n
	return b.capped(capacity)
}

// capped clamps to capacity; a full bucket accrues no credit.
func (b bucketState) capped(capacity int) bucketState {
	if b.Tokens >= capacity {
		b.Tokens, b.Credit = capacity, 0
	}
	return b
}

// untilTokens is how long until the bucket holds cost tokens; 0 if it does.
// The allocator arms its pass timer with it when starved.
func (b bucketState) untilTokens(cost, capacity int, interval time.Duration) time.Duration {
	if b.Tokens >= cost {
		return 0
	}
	if cost > capacity || capacity <= 0 || interval <= 0 {
		return interval
	}
	period := interval / time.Duration(capacity)
	return time.Duration(cost-b.Tokens)*period - b.Credit
}

// endpointGate is one endpoint's capacity breaker as admission reads it. The
// gather phase captures it from the guard; the pure decide never calls the
// guard. The zero value is shut, so a gate the gather phase never captured
// admits nothing.
type endpointGate uint8

const (
	gateShut   endpointGate = iota // open, or half-open with its probe in flight: admits nothing
	gateClosed                     // admits under tokens and the city cap
	gateProbe                      // half-open, or open with a probe due: admits one outstanding grant, the probe
)

// endpointGateOf reads k's gate from the guard. Eligible is read-only for
// breaker state; it refreshes k's lastSeen, which keeps a key the allocator
// still uses from being forgotten.
func endpointGateOf(g *endpointCapacityGuard, k endpointKey) endpointGate {
	ok, st := g.Eligible(k)
	switch {
	case !ok:
		return gateShut
	case st.State == resilience.StateClosed:
		return gateClosed
	}
	return gateProbe
}

// admitStart reports whether one more start may be reserved (AM5, C5.9):
// the bucket holds its one token, the city has fewer than limit starts in
// flight, and k's gate admits: closed admits, a probe gate admits only while
// k has nothing outstanding, a shut gate admits nothing. A create is admitted
// on the same test for its row's first grant.
func admitStart(b bucketState, inFlight, limit int, gate endpointGate, outstanding int) bool {
	if b.Tokens < 1 || inFlight >= limit {
		return false
	}
	switch gate {
	case gateClosed:
		return true
	case gateProbe:
		return outstanding == 0
	}
	return false
}

// ledgerCounts reports whether e stands for an effect in flight: every create
// or grant entry still in the ledger, except a failure that wrote nothing
// (it clears at once and represents nothing). A landed grant counts until its
// marker clears it; from then the census row's start lease counts it.
func ledgerCounts(e ledgerEntry) bool {
	return e.State != ledgerFailed || e.WroteRow
}

// cityInFlight counts starts in flight city-wide (START-003), once per effect
// (C5.13): ledger creates and grants, plus census rows with a live start
// lease, plus never-started pending creates whose endpoint is not shut. A
// row an entry already stands for, by key or by create token, is not counted
// again. Pending rows parked behind a shut endpoint hold no start slot (F5):
// they wait for their endpoint, and starving every other endpoint behind
// them would turn one broker outage into a city outage. Behind a probe gate
// they count as one, the probe, so an endpoint going half-open after an
// outage does not fill the city cap with its whole backlog (F5 probe window,
// P3-5b). A pending row whose endpoint has no gate in gates counts, erring
// toward fewer starts. A start lease counts whatever its endpoint's gate:
// that start is already running.
//
// parked holds the pending rows it counted. A grant for one continues the
// effect already counted, so it takes no further slot (C5.13). Behind a
// probe gate every parked row shares the one slot, so each is in parked.
func cityInFlight(view []ledgerEntry, c ledgerCensus, gates map[endpointKey]endpointGate) (n int, parked map[rowKey]bool) {
	keys := make(map[rowKey]bool)
	tokens := make(map[string]bool)
	parked = make(map[rowKey]bool)
	for _, e := range view {
		if !ledgerCounts(e) {
			continue
		}
		if e.Key.ID != "" {
			if keys[e.Key] {
				continue
			}
			keys[e.Key] = true
		}
		if e.Kind == kindCreate && e.Marker.InstanceToken != "" {
			tokens[e.Marker.InstanceToken] = true
		}
		n++
	}
	probed := make(map[endpointKey]bool)
	for k, r := range c.Rows {
		if keys[k] || (r.InstanceToken != "" && tokens[r.InstanceToken]) {
			continue
		}
		switch g, ok := gates[r.Endpoint]; {
		case r.StartLease:
			n++
		case !r.PendingCreate || (ok && g == gateShut):
		case ok && g == gateProbe && probed[r.Endpoint]:
			parked[k] = true
		default:
			probed[r.Endpoint] = true
			parked[k] = true
			n++
		}
	}
	return n, parked
}

// endpointOutstanding counts, per endpoint, the reserved or issued grants and
// the creates still in the ledger, once per row: a probe gate admits only an
// endpoint with none. A committed grant is not outstanding: its start has
// already resolved the endpoint's ticket.
func endpointOutstanding(view []ledgerEntry) map[endpointKey]int {
	out := make(map[endpointKey]int)
	seen := make(map[rowKey]bool)
	for _, e := range view {
		switch {
		case e.Kind == kindGrant && (e.State == ledgerReserved || e.State == ledgerIssued):
		case e.Kind == kindCreate && ledgerCounts(e):
		default:
			continue
		}
		if e.Key.ID != "" {
			if seen[e.Key] {
				continue
			}
			seen[e.Key] = true
		}
		out[e.Endpoint]++
	}
	return out
}
