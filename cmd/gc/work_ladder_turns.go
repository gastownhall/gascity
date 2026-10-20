package main

// The work ladder's rate-limit rule reads (audit 4, lever 2): what a seat's
// own transcript says its provider did with its latest turns. A seat whose
// turns the provider refuses with a rate limit is not ignoring its nudges, it
// cannot answer them, and the refusal may stick to its conversation;
// workladder.Decide recycles it to a fresh one. The reads go through the
// worker boundary (worker.TurnHandle), never the transcript directly, and the
// classification is the agent CLI's own (sessionlog): this file only counts.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/gastownhall/gascity/internal/workladder"
)

const (
	// ladderTurnReadBound bounds the parts of one seat's turn read that take
	// a context: the runtime probes of a turn still open. The session load
	// and the transcript's file reads take none.
	ladderTurnReadBound = 5 * time.Second
	// ladderReadMaxAge is how long a quiet seat's reading stands. A seat is
	// read again sooner when its activity, generation, conversation or the
	// ladder's last nudge to it moves; the bound catches the rest, such as a
	// refusal tmux's activity clock discounts (output right after a nudge).
	ladderReadMaxAge = 5 * time.Minute
	// ladderUnknownFor is how long after its last good reading a refused
	// seat whose turns cannot be read has its claims wait, rather than paced
	// as if its provider served it: twice ladderReadMaxAge, so it covers the
	// re-read when the good reading ages out. Past it, the ladder paces the
	// seat as ever: failing reads never silence it.
	ladderUnknownFor = 2 * ladderReadMaxAge
	// ladderCanaryFlight is how long a provider's fresh-recycle canary holds
	// the provider's other refused seats: long enough to see its fresh
	// conversation refused (a restart, the grace, one nudge: about 5
	// minutes), short enough that refused seats each get their turn.
	ladderCanaryFlight = 15 * time.Minute
	// ladderRefusingFor is how long a provider whose pool refused a canary's
	// fresh conversation first holds fresh recycles on it; each refusal in a
	// row doubles it, up to ladderRefusingMax, and a served turn resets it.
	// Then the next refused seat may try again: over a cooldown of length T,
	// about log2(T / ladderRefusingFor) conversations are spent, plus one per
	// ladderRefusingMax.
	ladderRefusingFor = workladder.DefaultCap
	ladderRefusingMax = 24 * time.Hour
)

// ladderTurnReader reads a seat's current and recent turns.
type ladderTurnReader func(ctx context.Context, row beads.Bead) (runtime.TurnHistory, error)

// rateLimitReading is one read of a seat's turns for the rate-limit rule, in
// the conversation and generation it was taken in.
type rateLimitReading struct {
	gen       int64
	key       string    // the session_key: the conversation read
	activity  time.Time // the seat's last activity when it was read
	lastNudge time.Time // the ladder's last nudge to the seat when it was read
	readAt    time.Time
	// refused is how many of the latest ended turns, newest first, the
	// provider refused with a rate limit; refusedAt when the newest ended,
	// and last its error.
	refused   int
	refusedAt time.Time
	last      runtime.TurnFailure
	// servedAt is when the newest ended turn ended, when the provider
	// answered it (it completed), whether or not a newer turn is open: the
	// only evidence of service the rule takes. Zero otherwise.
	servedAt time.Time
	// err is a read that failed: no refusal, until the reading is read again.
	err error
	// unknown: this read failed, and the last good reading of the same
	// conversation, younger than ladderUnknownFor, showed the provider
	// refusing the seat (as often as the rule needs): whether the ladder's
	// last nudge was refused too cannot be told, so the seat's claims wait
	// the pass out.
	unknown bool
}

// in reports whether the reading was taken in the conversation and
// generation meta's row is at now.
func (r rateLimitReading) in(meta map[string]string) bool {
	return r.gen == rowGeneration(meta) && r.key == strings.TrimSpace(meta["session_key"])
}

// rateLimitedTurns reads hist for the rule: how many of its latest ended
// turns, newest first, the provider refused with a rate limit, up to the
// first it did not, when the newest of them ended and its error; and when
// the newest turn the provider answered (completed) ended, anywhere in the
// recent turns and whether or not a newer turn is open: older service never
// misleads, since every consumer compares it with when its own question
// began (a canary, a hold, a fresh recycle). A turn still open has no answer
// yet: no refusal. How long a turn has been open proves nothing (a provider
// can take minutes to refuse one).
func rateLimitedTurns(hist runtime.TurnHistory) (refused int, refusedAt time.Time, last runtime.TurnFailure, servedAt time.Time) {
	if len(hist.Recent) == 0 {
		return 0, time.Time{}, runtime.TurnFailure{}, time.Time{}
	}
	for _, t := range hist.Recent {
		if t.Status == runtime.TurnStatusCompleted {
			servedAt = t.EndedAt
			break
		}
	}
	if hist.Current.Active() {
		return 0, time.Time{}, runtime.TurnFailure{}, servedAt
	}
	for _, t := range hist.Recent {
		if t.Status != runtime.TurnStatusFailed || !t.Failure.RateLimited {
			break
		}
		refused++
	}
	if refused == 0 {
		return 0, time.Time{}, runtime.TurnFailure{}, servedAt
	}
	return refused, hist.Recent[0].EndedAt, hist.Recent[0].Failure, servedAt
}

// ladderRateLimits caches each seat's last reading, and the last read that
// failed, so a seat whose turns cannot be read here is reported once per
// conversation rather than every pass.
type ladderRateLimits struct {
	mu     sync.Mutex
	last   map[string]rateLimitReading // by session ID
	good   map[string]rateLimitReading // by session ID: the last read that did not fail
	failed map[string]string           // by session ID: the conversation and error last reported
}

// read is row's reading: the cached one while the seat has not moved (its
// activity, generation, conversation and the ladder's last nudge to it all
// as read) and is younger than ladderReadMaxAge, else a fresh read through
// read. A read that fails is cached the same way, as no refusal, with the
// error; unknown when the last good reading of the same conversation,
// younger than ladderUnknownFor, showed refusals (a failed read must not
// take a queued claim out of its queue, nor, for longer than that, hold it).
func (c *ladderRateLimits) read(ctx context.Context, read ladderTurnReader, row beads.Bead, activity, lastNudge, now time.Time) (rateLimitReading, error) {
	r := rateLimitReading{gen: rowGeneration(row.Metadata), key: strings.TrimSpace(row.Metadata["session_key"]), activity: activity, lastNudge: lastNudge, readAt: now}
	c.mu.Lock()
	cached, ok := c.last[row.ID]
	c.mu.Unlock()
	if ok && cached.gen == r.gen && cached.key == r.key && cached.activity.Equal(activity) && cached.lastNudge.Equal(lastNudge) &&
		now.Sub(cached.readAt) < ladderReadMaxAge {
		return c.orGood(row.ID, cached, now), cached.err
	}
	ctx, cancel := context.WithTimeout(ctx, ladderTurnReadBound)
	defer cancel()
	hist, err := read(ctx, row)
	if err != nil {
		r.err = err
	} else {
		r.refused, r.refusedAt, r.last, r.servedAt = rateLimitedTurns(hist)
	}
	c.mu.Lock()
	if c.last == nil {
		c.last, c.good = make(map[string]rateLimitReading), make(map[string]rateLimitReading)
	}
	c.last[row.ID] = r
	if r.err == nil {
		c.good[row.ID] = r
	}
	c.mu.Unlock()
	return c.orGood(row.ID, r, now), r.err
}

// orGood is r, or, when r is a failed read, no refusal, unknown while the
// last good reading of the same conversation, younger than
// ladderUnknownFor, showed the provider refusing the seat.
func (c *ladderRateLimits) orGood(id string, r rateLimitReading, now time.Time) rateLimitReading {
	if r.err == nil {
		return r
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.good[id]
	return rateLimitReading{unknown: ok && g.gen == r.gen && g.key == r.key && g.refused >= workladder.DefaultRateLimitThreshold && now.Sub(g.readAt) < ladderUnknownFor}
}

// firstFailure records that row's turns could not be read (err), and
// reports whether that is news: the first failure in its conversation, or a
// different one.
func (c *ladderRateLimits) firstFailure(row beads.Bead, err error) bool {
	note := fmt.Sprintf("%d|%s|%v", rowGeneration(row.Metadata), strings.TrimSpace(row.Metadata["session_key"]), err)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed == nil {
		c.failed = make(map[string]string)
	}
	if c.failed[row.ID] == note {
		return false
	}
	c.failed[row.ID] = note
	return true
}

// keep forgets every seat a pass no longer governs.
func (c *ladderRateLimits) keep(ids map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.last {
		if !ids[id] {
			delete(c.last, id)
		}
	}
	for id := range c.failed {
		if !ids[id] {
			delete(c.failed, id)
		}
	}
	for id := range c.good {
		if !ids[id] {
			delete(c.good, id)
		}
	}
}

// ladderCanaries is the rule's per-provider evidence. A fresh conversation
// helps a seat whose refusal sticks to its conversation (2026-10-19: fresh
// seats on the cooling GLM pool were served) and nothing when the provider's
// whole pool refuses; then it only loses the seat's context. So one seat per
// provider at a time is recycled fresh, the provider's canary: the
// provider's other refused seats are held (Observation.FreshHeld) while it
// is in flight, ladderCanaryFlight. If the canary's fresh conversation is
// refused too, the provider is refusing, and its refused seats stay held for
// ladderRefusingFor. A held refused seat is paced as degraded (workladder:
// paused nudges, counting nothing), keeping its conversation; a seat on the
// provider that is not refused is paced as usual. A turn served on the
// provider after the canary ends the hold either way. The state lives in
// memory: a controller restart costs at most one more canary per provider.
type ladderCanaries struct {
	mu sync.Mutex
	by map[string]providerCanary
	// streak counts each provider's canaries refused in a row since it last
	// served a turn, and when the last was: the refusing hold doubles with it.
	streak map[string]refusalStreak
}

// refusalStreak is a provider's canaries refused in a row, the last at at.
type refusalStreak struct {
	n  int
	at time.Time
}

// providerCanary is a provider's canary seat, the generation it was
// recycled from and when, or, refusing, when its fresh conversation was
// refused too and for how long that holds.
type providerCanary struct {
	seat     string
	gen      int64
	since    time.Time
	refusing bool
	holds    time.Duration
}

// current is provider's canary at now, forgetting one past its period.
// The caller holds c.mu.
func (c *ladderCanaries) current(provider string, now time.Time) (providerCanary, bool) {
	pc, ok := c.by[provider]
	if !ok {
		return providerCanary{}, false
	}
	period := ladderCanaryFlight
	if pc.refusing {
		period = pc.holds
	}
	if now.Sub(pc.since) >= period {
		delete(c.by, provider)
		return providerCanary{}, false
	}
	return pc, true
}

// refusing reports whether provider's pool refused a canary's fresh
// conversation and that refusal still holds at now (it has served no turn
// since).
func (c *ladderCanaries) refusing(provider string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.current(provider, now)
	return ok && pc.refusing
}

// inFlight reports whether seat, now at generation gen, is provider's canary
// in flight: recycled fresh from an earlier generation, its fresh
// conversation not yet seen served or refused. Such a seat is read even
// while it works, so a turn served there releases the provider's queue.
func (c *ladderCanaries) inFlight(provider, seat string, gen int64, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.current(provider, now)
	return ok && !pc.refusing && pc.seat == seat && gen > pc.gen
}

// held reports whether seat's fresh recycle must wait on provider's canary
// at now: the provider is refusing, or another seat's canary is in flight.
func (c *ladderCanaries) held(provider, seat string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	pc, ok := c.current(provider, now)
	return ok && (pc.refusing || pc.seat != seat)
}

// recycled records seat's fresh recycle from generation gen at now as
// provider's canary.
func (c *ladderCanaries) recycled(provider, seat string, gen int64, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.by == nil {
		c.by = make(map[string]providerCanary)
	}
	if pc, ok := c.current(provider, now); !ok || !pc.refusing {
		c.by[provider] = providerCanary{seat: seat, gen: gen, since: now}
	}
}

// refused records, at now, that provider refused a canary's fresh
// conversation too, and returns until when that holds its refused seats'
// fresh recycles: ladderRefusingFor, doubled for each refusal in a row since
// it last served a turn, up to ladderRefusingMax.
func (c *ladderCanaries) refused(provider, seat string, now time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.by == nil {
		c.by = make(map[string]providerCanary)
	}
	if c.streak == nil {
		c.streak = make(map[string]refusalStreak)
	}
	n := c.streak[provider].n
	if now.Sub(c.streak[provider].at) > ladderRefusingMax {
		n = 0 // a day with no refusal: a new episode
	}
	holds := min(ladderRefusingFor<<min(n, 10), ladderRefusingMax)
	c.streak[provider] = refusalStreak{n: n + 1, at: now}
	c.by[provider] = providerCanary{seat: seat, since: now, refusing: true, holds: holds}
	return now.Add(holds)
}

// served records a turn provider served at at: a canary or a refusal
// recorded before it is over, and the refusals in a row start again.
func (c *ladderCanaries) served(provider string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pc, ok := c.by[provider]; ok && at.After(pc.since) {
		delete(c.by, provider)
	}
	if st, ok := c.streak[provider]; ok && at.After(st.at) {
		delete(c.streak, provider)
	}
}

// workerLadderTurns is the production turn source for one pass over env:
// each seat's turns read through its worker handle (the transcript of its
// current conversation, or its runtime's own turn record). The worker
// factory is built on the first read, so a pass with nothing to read builds
// none.
func workerLadderTurns(env externalReadsEnv) ladderTurnReader {
	var (
		factory *worker.Factory
		err     error
		built   bool
	)
	return func(ctx context.Context, row beads.Bead) (runtime.TurnHistory, error) {
		if !built {
			built = true
			factory, err = workerFactoryWithConfig(env.CityPath, env.CityStore, env.SP, env.Cfg)
		}
		if err != nil {
			return runtime.TurnHistory{}, fmt.Errorf("worker factory: %w", err)
		}
		h, err := factory.SessionByID(row.ID)
		if err != nil {
			return runtime.TurnHistory{}, err
		}
		return h.CurrentTurn(ctx)
	}
}
