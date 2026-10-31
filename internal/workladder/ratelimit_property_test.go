package workladder

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The rate-limit episode's invariants (RL1-RL10),
// checked over random worlds from a fixed seed. One world is a pool
// whose provider serves, refuses the conversations it is stuck on (sticky:
// a fresh conversation is served), or refuses everything, flipping at
// random; and one or more seats on one or two provider names drawing on it.
// Each seat's turns answer the nudges that reach it, 1s to 20m later, as the
// pool stands when the turn starts (plus 5% failures that are no rate
// limit), and the seat takes turns of its own. The harness plays the lane:
// it reads turns only on acting passes (90% readable), delivers nudges
// directly (10% fail) or as notices settled later with a random fate,
// restarts the seat fresh after 1 to 3 held passes (5% of restarts lose the
// new conversation, and 3% of requests are cleared with no restart at all),
// and keeps two copies of every cell: one committed through its text, one
// in memory (RL6). A seat may also stand for hours under an operator hold,
// or hold no work for hours before its work comes back; in half the worlds
// the lane then decides no seat that holds no work, no entry and no
// release, so a workless seat's episode stands as it is until then.

// rlPool is how the world's provider pool answers a turn.
type rlPool uint8

const (
	rlServing  rlPool = iota // every turn served
	rlSticky                 // the conversations it is stuck on refused; newer ones served
	rlRefusing               // every turn refused
)

type rlOutcome uint8

const (
	rlCompleted rlOutcome = iota
	rlRefused             // a rate limit
	rlFailed              // another failure (a 524)
)

type rlTurn struct {
	start, end time.Time
	conv       int64
	outcome    rlOutcome
	quota      bool
}

// rlSim is one seat as the harness sees it.
type rlSim struct {
	provider  string
	gen, conv int64
	rec, twin Record // committed through the text; in memory
	work      []Work
	turns     []rlTurn
	// restarting counts the passes left before a requested fresh restart
	// lands (the gate holds the seat restart_pending until then).
	restarting int
	hold       RestartHold
	direct     bool // nudges are delivered directly, else as notices
	ops        int
	delivered  time.Time // the newest delivery this generation, as the harness saw it
	answeredAt time.Time // the end of the newest answered refusal the harness saw
	// heldUntil ends an operator hold: the gate holds the seat for hours,
	// keeping its generation. idleUntil ends a stretch with no work (a pool
	// sibling took it), and parked is the work that comes back then.
	heldUntil, idleUntil time.Time
	parked               []Work
	// episode bookkeeping (RL3)
	epRestarts, epAlerts int
	lastHeard            map[ClaimKey]time.Time
	lastPaused           map[ClaimKey]time.Time
}

type rlWorld struct {
	rng       *rand.Rand
	now       time.Time
	pool      rlPool
	quota     bool          // this world's refusing pool is a spent quota
	laneSkips bool          // the lane decides no seat holding no work, no entry and no release
	stuck     map[int]int64 // seat -> the newest conversation a sticky pool refuses
	seats     []*rlSim
	degraded  map[string]bool // per provider name
	lastFresh map[string]time.Time
	// poolRestarts counts fresh restarts since the pool began refusing
	// everything, and poolCloses the episodes closed meanwhile (stale, a
	// conversation someone else changed, or a turn served that began before
	// the pool refused): each seat restarts at most once per episode.
	poolRestarts, poolCloses int
}

// rlCoverage counts what the sims exercised, so a world that never reaches
// a branch fails loudly instead of passing vacuously.
var rlCoverage = struct {
	moves    sync64Map
	restarts atomic.Int64
	// Acting passes reading a refusal of the last delivered nudge that is
	// no evidence: past the evidence bound (old), standing alone in an open
	// episode past FreshConfirm with no nudge delivered since it opened
	// (unprobed), or from before a restarted episode's restart (preRestart).
	old, unprobed, preRestart atomic.Int64
}{moves: sync64Map{}}

type sync64Map struct{ m [64]atomic.Int64 }

var rlMoveSlots = []RateLimitMove{
	{RateLimitTransitionOpened, ""},
	{RateLimitTransitionRestarted, ""},
	{RateLimitTransitionHeld, RateLimitCauseAttached},
	{RateLimitTransitionHeld, RateLimitCauseAwaitingHuman},
	{RateLimitTransitionHeld, RateLimitCauseBlocked},
	{RateLimitTransitionExhausted, RateLimitCauseFreshRefused},
	{RateLimitTransitionExhausted, RateLimitCauseRestartLost},
	{RateLimitTransitionExhausted, RateLimitCauseAgain},
	{RateLimitTransitionExhausted, RateLimitCauseQuota},
	{RateLimitTransitionClosed, RateLimitCauseServed},
	{RateLimitTransitionClosed, RateLimitCauseStale},
	{RateLimitTransitionClosed, RateLimitCauseConversationChanged},
}

func (m *sync64Map) add(mv RateLimitMove) {
	if i := slices.Index(rlMoveSlots, mv); i >= 0 {
		m.m[i].Add(1)
	}
}

func newRLWorld(seed uint64, seats int) *rlWorld {
	w := &rlWorld{
		rng: rand.New(rand.NewPCG(seed, 0x726174656c696d)), now: t0, stuck: map[int]int64{},
		degraded: map[string]bool{}, lastFresh: map[string]time.Time{},
	}
	w.quota, w.laneSkips = w.chance(20), w.chance(50)
	providers := []string{"p1"}
	if seats > 1 && w.chance(50) {
		providers = append(providers, "p2")
	}
	for i := range seats {
		s := &rlSim{
			provider: providers[w.rng.IntN(len(providers))], gen: 1, conv: 1, direct: w.chance(50),
			lastHeard: map[ClaimKey]time.Time{}, lastPaused: map[ClaimKey]time.Time{},
		}
		// Every seat holds a started claim; some hold more (RL3: the
		// episode is the seat's however many claims it holds).
		s.work = []Work{claim(fmt.Sprintf("s%d", i))}
		if w.chance(50) {
			s.work = append(s.work, claim(fmt.Sprintf("t%d", i)), trig(fmt.Sprintf("u%d", i)))
		}
		w.seats = append(w.seats, s)
	}
	return w
}

func (w *rlWorld) chance(n int) bool { return w.rng.IntN(100) < n }

func (w *rlWorld) between(lo, hi time.Duration) time.Duration {
	return lo + time.Duration(w.rng.Int64N(int64(hi-lo)+1))
}

// outcome is how the pool answers a turn of seat i's conversation conv.
func (w *rlWorld) outcome(i int, conv int64) (rlOutcome, bool) {
	if w.chance(5) {
		return rlFailed, false
	}
	switch {
	case w.pool == rlRefusing:
		return rlRefused, w.quota
	case w.pool == rlSticky && conv <= w.stuck[i]:
		return rlRefused, false
	}
	return rlCompleted, false
}

// startTurn starts a turn of seat i at at, unless one is running then.
func (w *rlWorld) startTurn(i int, at time.Time, lo, hi time.Duration) {
	s := w.seats[i]
	if s.running(at) {
		return
	}
	o, q := w.outcome(i, s.conv)
	s.turns = append(s.turns, rlTurn{start: at, end: at.Add(w.between(lo, hi)), conv: s.conv, outcome: o, quota: q})
}

func (s *rlSim) running(at time.Time) bool {
	return slices.ContainsFunc(s.turns, func(t rlTurn) bool { return t.conv == s.conv && !t.start.After(at) && t.end.After(at) })
}

// reading is the seat's turns in its conversation at now, reduced as the
// lane reduces them.
func (s *rlSim) reading(now time.Time) TurnReading {
	r := TurnReading{Read: true, Conv: s.conv}
	var ended []rlTurn
	for _, t := range s.turns {
		if t.conv == s.conv && !t.end.After(now) {
			ended = append(ended, t)
		}
	}
	slices.SortStableFunc(ended, func(a, b rlTurn) int { return a.end.Compare(b.end) })
	for _, t := range ended {
		if t.outcome == rlCompleted {
			r.ServedAt = t.end
		}
	}
	if s.running(now) || len(ended) == 0 {
		return r
	}
	last := ended[len(ended)-1]
	r.LastFailed = last.outcome != rlCompleted
	for i := len(ended) - 1; i >= 0 && ended[i].outcome == rlRefused; i-- {
		r.Refused++
	}
	if r.Refused > 0 {
		r.RefusedAt, r.Quota = last.end, last.quota
	}
	return r
}

// providerFreshAt is the newest fresh restart of another seat on seat i's
// provider, read off their committed cells, as the lane builds it.
func (w *rlWorld) providerFreshAt(i int) time.Time {
	var at time.Time
	for j, o := range w.seats {
		if j != i && o.provider == w.seats[i].provider && o.rec.RateLimit.LastFresh.After(at) {
			at = o.rec.RateLimit.LastFresh
		}
	}
	return at
}

// step advances the world: the clock, the pool, provider health, and each
// seat's restarts, conversations and own turns.
func (w *rlWorld) step() {
	steps := []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 3 * time.Minute, 7 * time.Minute, maxStep}
	w.now = w.now.Add(steps[w.rng.IntN(len(steps))])
	if w.chance(5) {
		prev := w.pool
		w.pool = rlPool(w.rng.IntN(3))
		if w.pool == rlSticky {
			for i, s := range w.seats {
				w.stuck[i] = s.conv
			}
		}
		if w.pool == rlRefusing && prev != rlRefusing {
			w.poolRestarts, w.poolCloses = 0, 0
		}
	}
	for name := range map[string]bool{"p1": true, "p2": true} {
		if w.chance(4) {
			w.degraded[name] = !w.degraded[name]
		}
	}
	for i, s := range w.seats {
		if s.restarting > 0 {
			// The restart lands in a new generation, unless its request
			// was cleared with no restart at all.
			if s.restarting--; s.restarting == 0 && !w.chance(3) {
				s.gen++
				if !w.chance(5) { // else the restart lost its new conversation
					s.conv++
				}
				s.delivered = time.Time{}
			}
		}
		if w.chance(1) { // someone else resets the conversation
			s.conv++
		}
		if w.chance(5) {
			s.hold = RestartHold(w.rng.IntN(3))
		}
		if !w.now.Before(s.heldUntil) && w.chance(1) { // an operator hold or a quarantine
			s.heldUntil = w.now.Add(w.between(time.Hour, 4*time.Hour))
		}
		switch {
		case !s.idleUntil.IsZero() && !w.now.Before(s.idleUntil): // the work comes back
			s.work, s.parked, s.idleUntil = s.parked, nil, time.Time{}
		case s.idleUntil.IsZero() && len(s.work) > 0 && w.chance(1): // a pool sibling takes the work
			s.work, s.parked, s.idleUntil = nil, s.work, w.now.Add(w.between(30*time.Minute, 4*time.Hour))
		}
		if w.chance(3) && !s.running(w.now) { // the agent's own turn
			w.startTurn(i, w.now.Add(-w.between(0, time.Minute)), 10*time.Second, 10*time.Minute)
		}
	}
}

// observe builds seat i's observation for this pass.
func (w *rlWorld) observe(i int) Observation {
	s := w.seats[i]
	o := Observation{
		Now: w.now, Gen: s.gen, Outstanding: slices.Clone(s.work), Started: map[ClaimKey]StartSource{}, StartedRead: true,
		Runtime: TriTrue, Quiet: Quiet{State: Quiescent}, Conv: s.conv, Provider: ProviderState{Degraded: w.degraded[s.provider]},
		RestartHold: s.hold, ProviderFreshAt: w.providerFreshAt(i),
	}
	for _, wk := range s.work {
		if wk.Kind == KindClaim {
			o.Started[wk.Key] = SrcAgent
		}
	}
	switch {
	case s.restarting > 0:
		o.Gate = Gate{Held: true, Reason: "restart_pending"}
	case w.now.Before(s.heldUntil):
		o.Gate = Gate{Held: true, Reason: "operator_hold"}
	case w.chance(3):
		o.Gate = Gate{Held: true, Reason: "draining"}
	}
	if w.chance(2) {
		o.Runtime = TriUnknown
	}
	if s.running(w.now) {
		o.Quiet = Quiet{State: NotQuiet, Reason: "turn running"}
	}
	for _, wk := range s.work {
		if w.chance(3) {
			if o.Hold == nil {
				o.Hold = map[ClaimKey]bool{}
			}
			o.Hold[wk.Key] = true
		}
	}
	var last time.Time
	for _, t := range s.turns {
		if t.conv == s.conv && !t.start.After(w.now) {
			last = minTime(t.end, w.now)
		}
	}
	o.LastActivity = last
	if o.seatActs() && len(o.Outstanding) > 0 && w.chance(90) {
		o.Turns = s.reading(w.now)
	}
	return o
}

// settleNotices settles seat i's pending notice marks, as the lane folds
// their fates before a pass: most of them, and every one older than the
// lane's bound. A delivered notice reached the seat at the start of its
// delivering attempt, within the bound of its reservation, and its turn
// answers it from then.
func (w *rlWorld) settleNotices(i int) {
	s := w.seats[i]
	for _, e := range s.rec.Entries {
		m := e.Notice
		if m.IsZero() || (w.chance(30) && w.now.Sub(m.At) < 5*time.Minute) {
			continue
		}
		fate := NoticeFate(1 + w.rng.IntN(4))
		if w.chance(40) {
			fate = NoticeDelivered
		}
		var at time.Time
		if fate == NoticeDelivered {
			at = m.At.Add(w.between(0, min(4*time.Minute+55*time.Second, w.now.Sub(m.At))))
		}
		r, _, ok := SettleNotice(s.rec, m.Op, fate, at)
		if !ok {
			continue
		}
		s.rec = r
		s.twin, _, _ = SettleNotice(s.twin, m.Op, fate, at)
		if fate == NoticeDelivered && s.rec.Gen == s.gen {
			s.delivered = maxTime(s.delivered, at)
			w.startTurn(i, at, time.Second, 20*time.Minute)
		}
	}
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// refusedSince reports whether this pass's reading shows the provider
// refused the seat after its last delivered nudge of this generation, while
// it holds work, however long ago.
func refusedSince(o Observation, delivered time.Time, entries int) bool {
	t := o.Turns
	return t.Read && t.Conv == o.Conv && entries > 0 && t.Refused >= DefaultRateLimitThreshold && !delivered.IsZero() && t.RefusedAt.After(delivered)
}

// answered is the RL evidence computed apart from Decide: refusedSince, the
// refusal no longer than RateLimitEvidenceMaxAge ago.
func answered(o Observation, delivered time.Time, entries int) bool {
	return refusedSince(o, delivered, entries) && o.Now.Sub(o.Turns.RefusedAt) <= DefaultCap
}

// alertMove is the move an episode's alert reason rides with.
var alertMove = map[string][]RateLimitMove{
	AlertRateLimitedAgain:        {{RateLimitTransitionExhausted, RateLimitCauseAgain}},
	AlertRateLimitedQuota:        {{RateLimitTransitionExhausted, RateLimitCauseQuota}},
	AlertRateLimitedFreshRefused: {{RateLimitTransitionExhausted, RateLimitCauseFreshRefused}, {RateLimitTransitionExhausted, RateLimitCauseRestartLost}},
	AlertRateLimitedHeld:         {{RateLimitTransitionHeld, RateLimitCauseAttached}, {RateLimitTransitionHeld, RateLimitCauseAwaitingHuman}, {RateLimitTransitionHeld, RateLimitCauseBlocked}},
}

// pass runs one ladder pass over seat i and checks RL1-RL9 on it.
func (w *rlWorld) pass(i, step int) error {
	s := w.seats[i]
	w.settleNotices(i)
	if s.rec.Gen != s.gen && s.rec.V != 0 {
		s.delivered = time.Time{}
	}
	if w.laneSkips && len(s.work) == 0 && len(s.rec.Entries) == 0 && s.rec.Release == nil {
		return nil // the lane neither decides nor writes the seat
	}
	o := w.observe(i)
	prev := s.rec
	d := Decide(prev, o, p)
	fail := func(format string, a ...any) error {
		return fmt.Errorf("seat %d step %d at %v: %s\nprev %s\nobs %+v\nnext %s\nacts %+v", i, step, o.Now.Sub(t0), fmt.Sprintf(format, a...), EncodeLadder(prev), o, EncodeLadder(d.Next), d.Actions)
	}

	// RL6: the in-memory twin decides the same, and Changed is exactly
	// "the stored text differs".
	dt := Decide(s.twin, o, p)
	if !reflect.DeepEqual(dt.Actions, d.Actions) || EncodeLadder(dt.Next) != EncodeLadder(d.Next) || dt.Changed != d.Changed {
		return fail("RL6: the in-memory twin decided %+v / %s", dt.Actions, EncodeLadder(dt.Next))
	}
	empty := func(r Record) bool { return len(r.Entries) == 0 && r.Release == nil && r.RateLimit.IsZero() }
	if emptyCell := empty(d.Next) && (prev.V == 0 || empty(prev)); !emptyCell && d.Changed != (EncodeLadder(prev) != EncodeLadder(d.Next)) {
		return fail("RL6: Changed=%v disagrees with the cell text", d.Changed)
	}

	acting := o.seatActs()
	degraded := o.Provider.Degraded
	ans := answered(o, s.delivered, len(d.Next.Entries))
	prl := clampRateLimit(prev.RateLimit, o.Now)
	nrl := d.Next.RateLimit
	probed := s.delivered.After(prl.Since) // a nudge reached the seat after its episode opened
	if acting && refusedSince(o, s.delivered, len(d.Next.Entries)) {
		switch {
		case !ans:
			rlCoverage.old.Add(1)
		case prl.Phase == RateLimitOpen && !probed && o.Now.Sub(prl.Since) >= DefaultPausedCap:
			rlCoverage.unprobed.Add(1)
		case prl.Phase == RateLimitRestarted && !o.Turns.RefusedAt.After(prl.FreshAt):
			rlCoverage.preRestart.Add(1)
		}
	}

	// RL1: an acting, healthy pass reading an answered refusal leaves an
	// episode standing; an episode opens only on such a pass.
	if acting && !degraded && ans && nrl.Phase == RateLimitClear {
		return fail("RL1: an answered refusal left no episode")
	}
	var moves []RateLimitMove
	restarts, nudges := 0, 0
	for _, a := range d.Actions {
		switch a := a.(type) {
		case RateLimitMove:
			moves = append(moves, a)
			rlCoverage.moves.add(a)
			if a.Transition == RateLimitTransitionOpened && (!acting || degraded || !ans) {
				return fail("RL1: opened without an answered refusal (acting %v, degraded %v, delivered %v)", acting, degraded, s.delivered.Sub(t0))
			}
			if a.Transition != RateLimitTransitionClosed && (degraded || !acting) {
				return fail("RL1: %v while degraded or not acting", a)
			}
		case FreshRestart:
			restarts++
		case Nudge:
			nudges++
		}
	}

	// RL2: the restart's preconditions.
	if restarts > 0 {
		rlCoverage.restarts.Add(1)
		switch {
		case prl.Phase != RateLimitOpen || o.Now.Sub(prl.Since) < DefaultPausedCap:
			return fail("RL2: restarted from %+v", prl)
		case !ans || degraded || o.RestartHold != RestartHoldNone:
			return fail("RL2: restarted with answered %v, degraded %v, hold %v", ans, degraded, o.RestartHold)
		case anyMarked(d.Next) || d.Next.Release != nil || len(o.Hold) > 0:
			return fail("RL2: restarted over a pending notice, release or unread claim")
		case !prl.LastFresh.IsZero() && o.Now.Sub(prl.LastFresh) < DefaultCap:
			return fail("RL2: restarted %v after the seat's last", o.Now.Sub(prl.LastFresh))
		case !o.ProviderFreshAt.IsZero() && o.Now.Sub(o.ProviderFreshAt) < DefaultProviderFreshGap:
			return fail("RL2: restarted %v after another seat on the provider", o.Now.Sub(o.ProviderFreshAt))
		case nudges > 0:
			return fail("RL2: a nudge in the restart's pass")
		}
	}
	// RL2: every move but opening and closing stands on evidence: an
	// exhausted restart on a refusal after it, and a confirmed episode's
	// move (its restart, hold or exhaustion) on an answered refusal of a
	// nudge delivered after the episode opened, FreshConfirm past its
	// opening. FreshConfirm is observed, never inferred from an old Since.
	for _, m := range moves {
		switch {
		case m.Transition == RateLimitTransitionOpened || m.Transition == RateLimitTransitionClosed:
		case prl.Phase == RateLimitRestarted:
			if !ans || !o.Turns.RefusedAt.After(prl.FreshAt) {
				return fail("RL2: %v on no refusal after the restart", m)
			}
		case !ans || !probed || o.Now.Sub(prl.Since) < DefaultPausedCap:
			return fail("RL2: %v with answered %v, a nudge delivered since the episode opened %v", m, ans, probed)
		}
	}

	// RL3: one restart and one alert per episode, each alert with its move.
	for _, a := range d.Actions {
		switch a := a.(type) {
		case RateLimitMove:
			if a.Transition == RateLimitTransitionClosed {
				s.epRestarts, s.epAlerts = 0, 0
			}
		case FreshRestart:
			if s.epRestarts++; s.epRestarts > 1 {
				return fail("RL3: a second fresh restart in one episode")
			}
		case Alert:
			want, ok := alertMove[a.Reason]
			if !ok {
				continue
			}
			if s.epAlerts++; s.epAlerts > 1 {
				return fail("RL3: a second rate-limit alert in one episode")
			}
			if !slices.ContainsFunc(moves, func(m RateLimitMove) bool { return slices.Contains(want, m) }) {
				return fail("RL3: alert %s without its move", a.Reason)
			}
			if a.Key != subject(d.Next) {
				return fail("RL3: alert keyed %v, not the seat's subject %v", a.Key, subject(d.Next))
			}
		}
	}

	// RL4: paced paused while an episode stands.
	if nrl.Phase != RateLimitClear {
		for _, a := range d.Actions {
			switch a := a.(type) {
			case Nudge:
				if !a.Paused {
					return fail("RL4: a counted nudge while rate limited")
				}
				if pe, ok := entryOf(prev, a.Key); ok && prev.Gen == o.Gen && pe.Kind == a.Kind && pe.Since.Equal(entryOrZero(d.Next, a.Key).Since) &&
					pe.Attempts+pe.Paused > 0 && o.Now.Sub(pe.Last) < DefaultPausedCap {
					return fail("RL4: %v nudged %v after its last", a.Key, o.Now.Sub(pe.Last))
				}
			case Recycle:
				return fail("RL4: a recycle while rate limited")
			case Alert:
				switch a.Reason {
				case AlertStartedUnanswered, AlertContinuationUnanswered, AlertReleaseUnfinished:
					return fail("RL4: %s while rate limited", a.Reason)
				}
			}
		}
		if d.Next.Release != nil && (prev.Release == nil || *d.Next.Release != *prev.Release) {
			return fail("RL4: a release decided while rate limited")
		}
	}

	// RL5: liveness. An episode whose probe was refused has restarted or
	// alerted by FreshConfirm + Cap, and Checked moves only on an answered
	// refusal.
	if acting && ans && probed && !degraded && nrl.Phase == RateLimitOpen && prl.Phase == RateLimitOpen && nrl.Since.Equal(prl.Since) &&
		o.Now.Sub(nrl.Since) >= DefaultPausedCap+DefaultCap && nrl.Alerted == "" {
		return fail("RL5: confirmed %v ago and neither restarted nor alerted", o.Now.Sub(nrl.Since))
	}
	// No new answered refusal for EvidenceMaxAge: closed, by the harness's
	// own clock of the newest answered refusal, not the cell's.
	if ans && o.Turns.RefusedAt.After(s.answeredAt) {
		s.answeredAt = minTime(o.Turns.RefusedAt, o.Now)
	}
	if acting && nrl.Phase != RateLimitClear && o.Now.Sub(s.answeredAt) > DefaultCap {
		return fail("RL5: no new answered refusal for %v and still open", o.Now.Sub(s.answeredAt))
	}
	if nrl.Phase != RateLimitClear && !nrl.Checked.Equal(prl.Checked) && !ans {
		return fail("RL5: Checked moved to %v on no answered refusal", nrl.Checked.Sub(t0))
	}
	if t := o.Turns; acting && t.Read && t.Conv == o.Conv && prl.Phase != RateLimitClear {
		ref := prl.Since
		if prl.Phase == RateLimitRestarted {
			ref = prl.FreshAt
		}
		if t.ServedAt.After(ref) && nrl.Phase != RateLimitClear && nrl.Since.Equal(prl.Since) {
			return fail("RL5: served at %v and still open", t.ServedAt.Sub(t0))
		}
	}
	// WL1 holds: every entry of a nudgeable seat hears from the ladder.
	eligible := acting
	for _, e := range d.Next.Entries {
		since, ok := s.lastHeard[e.Key]
		switch {
		case !eligible || !ok || o.Hold[e.Key] || prev.Gen != o.Gen || (d.Next.Release != nil && d.Next.Release.Key == e.Key):
			s.lastHeard[e.Key] = o.Now
		case o.Now.Sub(since) > DefaultCap+3*maxStep+2*DefaultBase:
			return fail("WL1: %v of a quiet seat has not heard from the ladder for %v", e.Key, o.Now.Sub(since))
		}
	}
	for _, a := range d.Actions {
		switch a := a.(type) {
		case Nudge:
			s.lastHeard[a.Key] = o.Now
		case Alert:
			s.lastHeard[a.Key] = o.Now
		}
	}
	for k := range s.lastHeard { // work that left the cell starts afresh if it comes back
		if _, ok := entryOf(d.Next, k); !ok {
			delete(s.lastHeard, k)
		}
	}

	// RL8: progress, as C1 judges it.
	if prev.V != 0 && prev.Gen == o.Gen {
		for _, e := range d.Next.Entries {
			pe, ok := entryOf(prev, e.Key)
			if !ok || pe.Kind != e.Kind || e.Rung != pe.Rung {
				continue
			}
			evidence := o.LastActivity
			if o.Turns.Read && o.Turns.Conv == o.Conv && o.Turns.LastFailed {
				evidence = o.Turns.ServedAt
			}
			counted := !pe.Last.IsZero() && (pe.Attempts > 0 || pe.Paused > 0 || pe.Unreleased)
			want := acting && !o.Hold[e.Key] && counted && !evidence.Before(pe.Last.Add(DefaultProgressReset))
			if got := !e.Since.Equal(pe.Since); got != want {
				return fail("RL8: %v reset %v, want %v", e.Key, got, want)
			}
		}
	}

	// Commit (the text, and the twin in memory), then the lane's effects.
	if d.Changed {
		r, err := DecodeLadder(EncodeLadder(d.Next))
		if err != nil {
			return fail("the cell does not decode: %v", err)
		}
		s.rec, s.twin = r, d.Next
	}
	for _, a := range d.Actions {
		switch a := a.(type) {
		case Nudge:
			if s.direct {
				if w.chance(10) {
					continue // the delivery failed: perhaps it reached the seat, but nothing says so
				}
				s.rec, _ = NudgeDelivered(s.rec, a.Key, s.gen, o.Now)
				s.twin, _ = NudgeDelivered(s.twin, a.Key, s.gen, o.Now)
				s.delivered = maxTime(s.delivered, o.Now)
				w.startTurn(i, o.Now, time.Second, 20*time.Minute)
				continue
			}
			s.ops++
			m := NoticeMark{Op: fmt.Sprintf("op-%d-%d", i, s.ops), At: o.Now, Paused: a.Paused}
			s.rec, _ = MarkNotice(s.rec, a.Key, m)
			s.twin, _ = MarkNotice(s.twin, a.Key, m)
		case FreshRestart:
			s.restarting = 1 + w.rng.IntN(3)
			if t := w.lastFresh[s.provider]; !t.IsZero() && o.Now.Sub(t) < DefaultProviderFreshGap {
				return fail("RL10: fresh restarts on %s %v apart", s.provider, o.Now.Sub(t))
			}
			w.lastFresh[s.provider] = o.Now
			if w.pool == rlRefusing {
				if w.poolRestarts++; w.poolRestarts > len(w.seats)+w.poolCloses {
					return fail("RL10: %d fresh restarts in one pool-wide refusal of %d seats (%d episodes closed)", w.poolRestarts, len(w.seats), w.poolCloses)
				}
			}
		case Recycle:
			if w.chance(70) {
				s.gen++
			}
		case Release:
			s.work = slices.DeleteFunc(s.work, func(wk Work) bool { return wk.Key == a.Key })
		case RateLimitMove:
			if a.Transition == RateLimitTransitionClosed && w.pool == rlRefusing {
				w.poolCloses++
			}
		}
	}
	// RL9: Delivered is the newest delivery of this generation.
	if s.rec.Gen == s.gen && !s.rec.Delivered.Equal(s.delivered) {
		return fail("RL9: Delivered %v, the newest delivery %v", s.rec.Delivered.Sub(t0), s.delivered.Sub(t0))
	}
	return nil
}

func entryOrZero(r Record, k ClaimKey) Entry {
	e, _ := entryOf(r, k)
	return e
}

func (w *rlWorld) run() error {
	for step := range 160 {
		w.step()
		for i := range w.seats {
			if err := w.pass(i, step); err != nil {
				return err
			}
		}
	}
	return nil
}

// rlWorlds is how many worlds each rate-limit sim runs: a fifth of the
// ladder's sequences, since each world runs 160 passes over hours of pool
// flips (20k worlds; 2k under -short).
func rlWorlds() int { return sequences() / 5 }

// RL1-RL9 on one seat.
func TestRateLimitInvariants(t *testing.T) {
	forEachN(t, rlWorlds(), func(seed uint64) error { return newRLWorld(seed, 1).run() })
	rlCovered(t)
}

// RL10: 2 to 5 seats on one or two provider names drawing on one pool,
// with RL1-RL9 on every seat (5k worlds, about as many seat runs as the
// one-seat sim).
func TestRateLimitInvariantsAcrossSeats(t *testing.T) {
	forEachN(t, rlWorlds()/4, func(seed uint64) error {
		return newRLWorld(seed, 2+int(seed%4)).run()
	})
	rlCovered(t)
}

func rlCovered(t *testing.T) {
	t.Helper()
	var missing []string
	for i, mv := range rlMoveSlots {
		if rlCoverage.moves.m[i].Load() == 0 {
			missing = append(missing, fmt.Sprintf("%s/%s", mv.Transition, mv.Cause))
		}
	}
	for _, c := range []struct {
		name string
		n    *atomic.Int64
	}{
		{"a fresh restart", &rlCoverage.restarts},
		{"a refusal past the evidence bound", &rlCoverage.old},
		{"an unprobed refusal past FreshConfirm", &rlCoverage.unprobed},
		{"a refusal from before the restart", &rlCoverage.preRestart},
	} {
		if c.n.Load() == 0 {
			missing = append(missing, c.name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the sims never reached: %s", strings.Join(missing, ", "))
	}
}

// Rule 1 is inert without readings: the ladder's own sim, with or without
// marks and readings that carry no refusals, never opens an episode (the
// sim's invariants run unchanged; this pins the inertness itself).
func TestRateLimitInertWithoutRefusals(t *testing.T) {
	forEachN(t, rlWorlds(), func(seed uint64) error {
		s := newSim(seed)
		s.marks, s.turns = seed%2 == 0, seed%3 == 0
		for range 60 {
			o := s.step()
			d := Decide(s.rec, o, p)
			for _, a := range d.Actions {
				switch a.(type) {
				case FreshRestart, RateLimitMove:
					return fmt.Errorf("%T with no refusal read", a)
				}
			}
			if d.Changed {
				s.rec = d.Next
			}
		}
		return nil
	})
}
