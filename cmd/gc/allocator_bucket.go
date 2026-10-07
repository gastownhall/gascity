package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/resilience"
)

// The planner's start budget (CONTRACT v5 P4): one token bucket for starts
// (a create or an adopt costs none), the city in-flight count, and the
// endpoint capacity breaker. The bucket refills by time, not by pass, so
// frequent passes cannot multiply the start rate. The planner is its only
// reader and writer (P1), so it needs no lock; the planner's admission
// (C1b-2) reads all three.

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

// capped clamps to capacity; a full bucket accrues no credit.
func (b bucketState) capped(capacity int) bucketState {
	if b.Tokens >= capacity {
		b.Tokens, b.Credit = capacity, 0
	}
	return b
}

// nextToken is when the bucket next holds a token, read as refill left it.
// It is zero when the bucket holds one now, or never refills (a capacity or
// interval that is not positive). Admission reports it so the planner can
// schedule a pass when starts are starved for tokens.
func (b bucketState) nextToken(capacity int, interval time.Duration) time.Time {
	if b.Tokens >= 1 || capacity <= 0 || interval <= 0 {
		return time.Time{}
	}
	period := interval / time.Duration(capacity)
	return b.LastRefill.Add(time.Duration(1-b.Tokens)*period - b.Credit)
}

// endpointGate is one endpoint's capacity breaker as admission reads it. The
// gather phase captures it from the guard; the pure decide never calls the
// guard. The zero value is shut, so a gate the gather phase never captured
// admits nothing.
type endpointGate uint8

const (
	gateShut   endpointGate = iota // open, or half-open with its probe in flight: admits nothing
	gateClosed                     // admits under tokens and the city cap
	gateProbe                      // half-open, or open with a probe due: admits one outstanding start, the probe
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

// admitStart reports whether one more start may be admitted (v5 P4):
// the bucket holds its one token, the city has fewer than limit starts in
// flight, and k's gate admits: closed admits, a probe gate admits only while
// k has nothing outstanding, a shut gate admits nothing. A create is admitted
// on the same test for its row's first start.
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

// inflightStart is a start's in-flight kind, which the city count reads.
const inflightStart = "start"

// cityInFlight is the city in-flight count (v5 P4): distinct bring-up rows,
// once each. They are the rows with a running start; the census rows holding
// a pending-create claim on an endpoint that is not shut, where every such
// row behind one probe gate shares one slot (a running start on one of them
// is that slot), so an endpoint going half-open after an outage does not fill
// the city cap with its whole backlog; and the
// running or ambiguous creates whose token no census row carries, since a
// row that carries it counts as itself. A row whose endpoint has no gate
// reading counts, erring toward fewer starts. No lease is an input (SC A5).
//
// counted holds the rows that hold a slot: a start for one continues the
// bring-up already counted and takes no further slot.
func cityInFlight(v inflightView, rows []bringUpRow, gate func(endpointKey) (endpointGate, bool)) (n int, counted map[rowKey]bool) {
	counted = make(map[rowKey]bool)
	for _, e := range v.Entries {
		if e.Kind == inflightStart && !counted[e.Key] {
			counted[e.Key] = true
			n++
		}
	}
	tokens := make(map[string]bool)
	probed := make(map[endpointKey]bool)
	for _, r := range rows {
		if g, ok := gate(r.Endpoint); r.PendingCreate && counted[r.Key] && ok && g == gateProbe {
			probed[r.Endpoint] = true
		}
	}
	for _, r := range rows {
		if r.Token != "" {
			tokens[r.Token] = true
		}
		if !r.PendingCreate || counted[r.Key] {
			continue
		}
		switch g, ok := gate(r.Endpoint); {
		case ok && g == gateShut:
			continue
		case ok && g == gateProbe:
			counted[r.Key] = true
			if probed[r.Endpoint] {
				continue
			}
			probed[r.Endpoint] = true
		default:
			counted[r.Key] = true
		}
		n++
	}
	return n + v.uncensusedCreates(inflightCensus{Tokens: tokens}), counted
}

// endpointOutstanding counts, per endpoint, the running starts and the
// running or ambiguous creates: a probe gate admits only an endpoint with
// none. An adopt takes no endpoint ticket (v5 S1), so it is not outstanding.
func endpointOutstanding(v inflightView) map[endpointKey]int {
	out := make(map[endpointKey]int)
	for _, e := range v.Entries {
		if e.Kind == inflightStart || e.Kind == inflightCreate {
			out[e.Endpoint]++
		}
	}
	return out
}
