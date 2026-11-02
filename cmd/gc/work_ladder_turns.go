package main

// The work ladder's turn reads (#8132 split C1, C3): what a seat's own turns
// say about its progress and its provider. A turn its provider failed (a rate
// limit, a Cloudflare 524) is activity, but not progress: workladder.Decide
// judges progress on a completed turn when the seat's newest turn failed. The
// newest turns its provider refused with a rate limit drive the seat's
// rate-limit episode (work_ladder_ratelimit.go). The reads go through
// the worker boundary (worker.Handle.CurrentTurn), never the transcript
// directly, and the outcome is the agent CLI's own (sessionlog): this file
// only reduces it.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/gastownhall/gascity/internal/workladder"
)

const (
	// ladderTurnReadBound bounds the parts of one seat's turn read that take
	// a context: the runtime probes of a turn still open.
	ladderTurnReadBound = 5 * time.Second
	// ladderTurnsMaxAge is how long a seat's reading stands while the seat
	// has not moved (its generation, conversation, activity and the ladder's
	// last nudge to it all as read). The bound catches a turn end the
	// activity clock does not show.
	ladderTurnsMaxAge = 5 * time.Minute
)

// ladderTurnReader reads a seat's current and recent turns.
type ladderTurnReader func(ctx context.Context, row beads.Bead) (runtime.TurnHistory, error)

// ladderTurnReading reduces hist to what the ladder judges progress and rate
// limits by, in conversation conv: whether the newest ended turn failed
// (whatever the cause, with no turn open), and when the newest turn the
// provider completed ended, anywhere in the recent turns; how many of the
// newest ended turns in a row the provider refused with a rate limit (none
// while a turn is open: it has no answer yet), when the newest of them
// ended, and whether its source classed it a spent quota. A turn that ended
// with no status (a family that records no turn end, an end inferred from
// an idle boundary) is not a failure. Every rate-limit fact is the source's
// own classification (runtime.TurnFailure); no text is read.
func ladderTurnReading(hist runtime.TurnHistory, conv int64) workladder.TurnReading {
	r := workladder.TurnReading{Read: true, Conv: conv}
	for _, t := range hist.Recent {
		if t.Status == runtime.TurnStatusCompleted {
			r.ServedAt = t.EndedAt
			break
		}
	}
	open := hist.Current.Active()
	r.LastFailed = !open && len(hist.Recent) > 0 && hist.Recent[0].Status == runtime.TurnStatusFailed
	if open {
		return r
	}
	for _, t := range hist.Recent {
		if t.Status != runtime.TurnStatusFailed || !t.Failure.RateLimited {
			break
		}
		r.Refused++
	}
	if r.Refused > 0 {
		r.RefusedAt, r.Quota = hist.Recent[0].EndedAt, hist.Recent[0].Failure.Quota
	}
	return r
}

// ladderRefusal is the failure of hist's newest refused turn, when a reading
// counts it refused (ladderTurnReading): the source's class, status and text,
// for the events and alerts that report it to people and packs. Decide never
// sees it.
func ladderRefusal(hist runtime.TurnHistory) runtime.TurnFailure {
	if hist.Current.Active() || len(hist.Recent) == 0 || hist.Recent[0].Status != runtime.TurnStatusFailed || !hist.Recent[0].Failure.RateLimited {
		return runtime.TurnFailure{}
	}
	return hist.Recent[0].Failure
}

// rowConversation is a row's conversation: its continuation epoch, the
// default when unset or not positive.
func rowConversation(meta map[string]string) int64 {
	if e, err := strconv.ParseInt(strings.TrimSpace(meta["continuation_epoch"]), 10, 64); err == nil && e > 0 {
		return e
	}
	return session.DefaultContinuationEpoch
}

// ladderTurnsKey is what a cached reading was read at: a reading stands
// while none of it moves. delivered is the seat's last nudge it took
// (Record.Delivered): a refusal that answers it may be newer than the
// reading.
type ladderTurnsKey struct {
	gen, conv           int64
	activity, delivered time.Time
}

// ladderTurnsEntry is one seat's cached reading, with the failure of its
// newest refused turn (ladderRefusal).
type ladderTurnsEntry struct {
	key     ladderTurnsKey
	readAt  time.Time
	reading workladder.TurnReading
	refusal runtime.TurnFailure
}

// ladderTurns caches each seat's last reading, and the last read failure it
// reported, so a seat whose turns cannot be read is reported once per
// conversation and error rather than every pass. Neither feeds a decision:
// a controller restart costs one re-read per seat and at most one repeated
// log line.
type ladderTurns struct {
	mu     sync.Mutex
	last   map[string]ladderTurnsEntry // by session ID
	failed map[string]string           // by session ID: the conversation and error last reported
}

// read is row's reading at now, keyed by key: the cached one while key
// stands and it is younger than ladderTurnsMaxAge, else a fresh read through
// read. A read that fails is no reading, cached the same way, with its
// error.
func (c *ladderTurns) read(ctx context.Context, read ladderTurnReader, row beads.Bead, key ladderTurnsKey, now time.Time) (ladderTurnsEntry, error) {
	c.mu.Lock()
	cached, ok := c.last[row.ID]
	c.mu.Unlock()
	if ok && cached.key == key && now.Sub(cached.readAt) < ladderTurnsMaxAge {
		return cached, nil
	}
	ctx, cancel := context.WithTimeout(ctx, ladderTurnReadBound)
	defer cancel()
	hist, err := read(ctx, row)
	entry := ladderTurnsEntry{key: key, readAt: now}
	if err == nil {
		entry.reading, entry.refusal = ladderTurnReading(hist, key.conv), ladderRefusal(hist)
	}
	c.mu.Lock()
	if c.last == nil {
		c.last = make(map[string]ladderTurnsEntry)
	}
	c.last[row.ID] = entry
	c.mu.Unlock()
	return entry, err
}

// firstFailure records that row's turns could not be read in conversation
// conv (err), and reports whether that is news: the first failure in that
// conversation, or a different one.
func (c *ladderTurns) firstFailure(id string, conv int64, err error) bool {
	note := fmt.Sprintf("%d|%v", conv, err)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed == nil {
		c.failed = make(map[string]string)
	}
	if c.failed[id] == note {
		return false
	}
	c.failed[id] = note
	return true
}

// keep forgets every seat a pass no longer governs.
func (c *ladderTurns) keep(ids map[string]bool) {
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
}

// turns is the seat's reading on this pass, when the ladder may act on it:
// a turn source, a running, quiet seat whose gate is open, with work
// outstanding. Otherwise, and when its turns cannot be read, it is no
// reading: the ladder judges progress on the activity clock, and no
// rate-limit episode moves on it. A read that fails is logged once per
// seat, conversation and error, unless the family records no turns at all.
// delivered is the seat's last nudge it took (Record.Delivered): a reading
// taken before it is read again.
func (p *ladderPass) turns(row beads.Bead, rt ladderRuntime, gate workladder.Gate, targets []ladderTarget, delivered time.Time) ladderTurnsEntry {
	if p.turnReader == nil || rt.running != workladder.TriTrue || rt.quiet.State != workladder.Quiescent || gate.Held || len(targets) == 0 {
		return ladderTurnsEntry{}
	}
	key := ladderTurnsKey{gen: rowGeneration(row.Metadata), conv: rowConversation(row.Metadata), activity: rt.activity, delivered: delivered}
	r, err := p.l.ladderTurns.read(p.ctx, p.turnReader, row, key, p.now)
	if err != nil && !errors.Is(err, runtime.ErrTurnIdentityUnsupported) && p.l.ladderTurns.firstFailure(row.ID, key.conv, err) {
		fmt.Fprintf(p.l.stderr, "work-ladder: %s: reading its turns: %v; judging its progress on its activity\n", row.ID, err) //nolint:errcheck // best-effort stderr
	}
	return r
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
