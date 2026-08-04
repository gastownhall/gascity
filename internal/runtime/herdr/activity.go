package herdr

import (
	"context"
	"strings"
	"sync"
	"time"
)

// This file implements GetLastActivity for herdr. herdr exposes no activity
// timestamp anywhere in its API, so the provider maintains one: a lazily
// started tracker polls agent.list and stamps a per-session last-activity
// time from what the polls OBSERVE. The design constraints, in order:
//
//   - Level-triggered only. herdr replays a backlog of recent events to every
//     new subscription (see events.go), so event content is never a stamp
//     source — a replayed "went idle" frame minutes after the fact would
//     refresh activity that didn't happen. Polls are the sole source of
//     truth; the session-event stream merely accelerates the next poll.
//   - working is continuously active. A status that SITS at working emits no
//     transitions. Stamping only transitions would make long legitimate work
//     look stale, and the progress-stall / idle-timeout nets would recycle a
//     live agent — the churn class that got the #312 idle nudger reverted
//     (#468). While the last polled status is working, GetLastActivity
//     returns now.
//   - Status outranks the revision counter. agent.list carries a per-pane
//     output revision, but a DETECTED agent's idle TUI may still redraw
//     (spinners, status lines), and stamping those ticks would make idle
//     sessions read as permanently active. The revision stamps activity only
//     for sessions herdr cannot classify (agent_status "unknown", e.g. bare
//     shells), where it is the only signal there is. Caveat, verified live:
//     herdr 0.7.3 moves the revision only while a client renders the pane —
//     on a headless server (gc's normal mode) it holds at 0, so this leg
//     contributes exactly when someone is attached and degrades to
//     first-observation + status-change stamping otherwise.
//   - Outages keep the last state. A failed poll must not fabricate idleness
//     or absence: wiping a working entry during a socket blip would hand a
//     config-drift reset or stall recycle a live agent to kill. Entries
//     change only when a poll SUCCEEDS.
//
// The tracker starts on the first GetLastActivity call — synchronously, so
// even that first answer reflects real state rather than a cold zero — and
// then lives as long as the provider. There is no teardown hook in the
// runtime.Provider surface; a provider swapped out by a config reload leaks
// one tracker until process exit, which is accepted and bounded (reload with
// a provider change is rare, and the tracker is one goroutine plus one
// subscription). Stamps are process-local: after a controller restart the
// idle clocks restart from first observation, so idle-driven actions fire at
// most one timeout period late — never early.

// Poll/debounce knobs. Vars so tests can shrink them; production values:
//   - activityPollInterval: reconcile cadence when no events arrive. 30s
//     matches the event stream's op bound and keeps steady-state load at two
//     cheap socket calls a minute.
//   - activityEventDebounce: coalesces an event burst into one poll, same
//     value as the stream's relist debounce.
//   - activityPollTimeout: bound on one agent.list call so a wedged server
//     cannot hang the seed call or the loop.
var (
	activityPollInterval  = 30 * time.Second
	activityEventDebounce = 300 * time.Millisecond
	activityPollTimeout   = 2 * time.Second
)

// Agent-status values the tracker interprets (herdr 0.7.3 enum: idle,
// working, blocked, done, unknown). Everything that is not working stamps at
// its observed transition and ages from there.
const (
	agentStatusWorking = "working"
	agentStatusUnknown = "unknown"
)

// activityEntry is the tracked state of one session.
type activityEntry struct {
	status   string
	revision uint64
	stamp    time.Time // last observed activity
}

// activityTracker owns the per-session activity map. All stamping decisions
// happen in poll(); reads never mutate observed state.
type activityTracker struct {
	startOnce sync.Once
	mu        sync.Mutex
	entries   map[string]activityEntry
	cancel    context.CancelFunc
	done      chan struct{} // closed when the run loop has fully exited
	now       func() time.Time
}

// start seeds the map synchronously and launches the tracking loop, once.
// Every caller of the first wave blocks on the seed, so no consumer ever
// reads a cold zero for a session that is observably live.
func (a *activityTracker) start(p *Provider) {
	a.startOnce.Do(func() {
		a.mu.Lock()
		if a.now == nil {
			a.now = time.Now
		}
		a.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		a.mu.Lock()
		a.cancel = cancel
		a.done = done
		a.mu.Unlock()
		a.poll(ctx, p.c)
		go func() {
			defer close(done)
			a.run(ctx, p)
		}()
	})
}

// stop cancels the tracking loop and waits for it to exit. Unused in
// production (the tracker lives as long as the provider); tests use it to
// stay hermetic.
func (a *activityTracker) stop() {
	a.mu.Lock()
	cancel, done := a.cancel, a.done
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// lastActivity answers GetLastActivity from tracked state: unknown session →
// zero; working → now (continuously active); anything else → the frozen
// stamp of its last observed change.
func (a *activityTracker) lastActivity(name string) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.entries[name]
	if !ok {
		return time.Time{}
	}
	if e.status == agentStatusWorking {
		return a.now()
	}
	return e.stamp
}

// run drives the poll loop: a slow reconcile ticker, accelerated by the
// provider's own session-event stream. Events are treated purely as "poll
// soon" hints — never as stamp sources — so the replayed backlog herdr sends
// each (re)subscribe is harmless here by construction.
func (a *activityTracker) run(ctx context.Context, p *Provider) {
	events, err := p.SubscribeSessionEvents(ctx)
	if err != nil {
		events = nil // nil channel blocks forever: degrade to ticker-only
	}
	ticker := time.NewTicker(activityPollInterval)
	defer ticker.Stop()
	debounce := time.NewTimer(activityEventDebounce)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()
	armed := false
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				events = nil // stream ended (ctx canceled); ticker continues
				continue
			}
			if !armed {
				debounce.Reset(activityEventDebounce)
				armed = true
			}
		case <-debounce.C:
			armed = false
			a.poll(ctx, p.c)
		case <-ticker.C:
			a.poll(ctx, p.c)
		}
	}
}

// poll reconciles the map against one agent.list snapshot. Stamps: first
// observation, any status change, and — only for status "unknown" — a moved
// output revision. A failed poll keeps the previous state untouched.
func (a *activityTracker) poll(ctx context.Context, c *client) {
	cctx, cancel := context.WithTimeout(ctx, activityPollTimeout)
	agents, err := c.sockAgentList(cctx)
	cancel()
	if err != nil {
		return
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	next := make(map[string]activityEntry, len(agents))
	for _, ag := range agents {
		if ag.Name == "" {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(ag.AgentStatus))
		prev, seen := a.entries[ag.Name]
		e := activityEntry{status: status, revision: ag.Revision, stamp: prev.stamp}
		switch {
		case !seen:
			e.stamp = now
		case status != prev.status:
			e.stamp = now
		case status == agentStatusUnknown && ag.Revision != prev.revision:
			e.stamp = now
		}
		next[ag.Name] = e
	}
	a.entries = next
}
