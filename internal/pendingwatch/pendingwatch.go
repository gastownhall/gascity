// Package pendingwatch turns observations of session pending interactions
// (an approval prompt or question a session is blocked on) into
// session.pending / session.pending_cleared transitions, exactly once per
// transition.
//
// The controller drives it (cmd/gc pending_interaction_lane.go): detection
// needs only the controller, not an API client watching an event stream, so an
// order or agent gated on session.pending sees the same transitions whether or
// not a dashboard is open. The API only reads and streams the event log.
//
// State. The set of interactions already announced lives in memory and is
// rebuilt from the durable city event log when the controller starts
// (Rebuild): the last session.pending / session.pending_cleared event per
// session wins. There is no side state file to go stale.
//
// Cost. A pass reads each probed session's last-activity time (for tmux, from
// the fleet snapshot the runtime already keeps, not a fork per session) and
// captures the session's screen only when its output changed since the last
// capture, the session was never captured, or a poke (an answered
// interaction) forced it. A city whose sessions sit unchanged costs no
// captures at all; a session whose output keeps moving costs at most one
// capture per pass. Probes run at most ProbeConcurrency at a time.
package pendingwatch

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Clear reasons carried on a session.pending_cleared transition. The values
// are the wire enum of the session.pending_cleared payload.
const (
	// ReasonResolved: the session is still probed but no longer reports an
	// interaction: it was answered or withdrawn.
	ReasonResolved = "resolved"
	// ReasonReplaced: the session now reports a different request_id; a
	// session.pending for the new interaction follows.
	ReasonReplaced = "replaced"
	// ReasonSessionGone: the session left the probed set (closed, asleep,
	// suspended, or otherwise no longer active).
	ReasonSessionGone = "session_gone"
)

// ProbeConcurrency bounds how many sessions one pass probes at once.
const ProbeConcurrency = 4

// Session is one session a pass may probe.
type Session struct {
	// ID is the session bead ID: the transitions' session_id.
	ID string
	// Name is the runtime session name the probes address.
	Name     string
	Template string
	Alias    string
}

// Listing is the probe set of one pass. Partial means the listing itself was
// incomplete, so a published session missing from it may still exist and is
// not cleared as gone.
type Listing struct {
	Sessions []Session
	Partial  bool
}

// Observation is one session's probe outcome.
type Observation struct {
	Pending   *runtime.PendingInteraction
	Supported bool
	Err       error
}

// Published is what was last announced for one session.
type Published struct {
	RequestID string
	Kind      string
}

// Transition is one session.pending or session.pending_cleared to publish.
type Transition struct {
	// Type is events.SessionPending or events.SessionPendingCleared.
	Type    string
	Session Session
	// Pending is the interaction a session.pending announces.
	Pending runtime.PendingInteraction
	// Previous is the announced interaction a session.pending_cleared clears.
	Previous Published
	// Reason is the clear reason (ReasonResolved, ReasonReplaced,
	// ReasonSessionGone).
	Reason string
}

// Deps are the watcher's I/O seams. List, Probe and Publish are required.
type Deps struct {
	// List returns the sessions to probe this pass.
	List func() (Listing, error)
	// Activity returns a session's last output time; ok is false when the
	// runtime cannot tell, in which case the session is probed every pass.
	Activity func(name string) (at time.Time, ok bool)
	// Probe reads a session's pending interaction.
	Probe func(ctx context.Context, name string) Observation
	// Publish appends one transition to the durable event log and returns
	// nil only once the log acknowledged it. A failed publish is retried on
	// a later pass; nothing is marked announced until it lands.
	Publish func(Transition) error
	// Logf receives diagnostics; nil discards them.
	Logf func(format string, args ...any)
}

// Watcher publishes pending-interaction transitions for one city. Pass is
// safe to call concurrently with Poke; passes themselves are serialized.
type Watcher struct {
	deps Deps

	passMu      sync.Mutex
	published   map[string]Published
	probedAt    map[string]time.Time
	unsupported map[string]bool

	pokeMu   sync.Mutex
	forced   map[string]bool
	forceAll bool
	pokeCh   chan struct{}
}

// New returns a watcher that starts from published, the set already announced
// (see Rebuild).
func New(deps Deps, published map[string]Published) *Watcher {
	if deps.Logf == nil {
		deps.Logf = func(string, ...any) {}
	}
	if deps.Activity == nil {
		deps.Activity = func(string) (time.Time, bool) { return time.Time{}, false }
	}
	w := &Watcher{
		deps:        deps,
		published:   map[string]Published{},
		probedAt:    map[string]time.Time{},
		unsupported: map[string]bool{},
		forced:      map[string]bool{},
		pokeCh:      make(chan struct{}, 1),
	}
	for id, p := range published {
		w.published[id] = p
	}
	return w
}

// Poke asks for a pass now that re-probes sessionID whatever its activity, or
// every session when sessionID is empty: an interaction was just answered, so
// its clear should not wait for the cadence.
func (w *Watcher) Poke(sessionID string) {
	w.pokeMu.Lock()
	if sessionID == "" {
		w.forceAll = true
	} else {
		w.forced[sessionID] = true
	}
	w.pokeMu.Unlock()
	select {
	case w.pokeCh <- struct{}{}:
	default:
	}
}

// Run runs a pass at once, then every interval and on every Poke, until ctx
// ends.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		w.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.pokeCh:
		}
	}
}

// Published returns a copy of the announced set.
func (w *Watcher) Published() map[string]Published {
	w.passMu.Lock()
	defer w.passMu.Unlock()
	out := make(map[string]Published, len(w.published))
	for id, p := range w.published {
		out[id] = p
	}
	return out
}

func (w *Watcher) takeForced() (map[string]bool, bool) {
	w.pokeMu.Lock()
	defer w.pokeMu.Unlock()
	forced, all := w.forced, w.forceAll
	w.forced, w.forceAll = map[string]bool{}, false
	return forced, all
}

type probeJob struct {
	session  Session
	activity time.Time
	known    bool
	obs      Observation
}

// Pass runs one detection pass and publishes the transitions it finds.
func (w *Watcher) Pass(ctx context.Context) {
	w.passMu.Lock()
	defer w.passMu.Unlock()
	forced, forceAll := w.takeForced()

	listing, err := w.deps.List()
	if err != nil {
		w.deps.Logf("pending interactions: listing sessions: %v", err)
		// The pokes are not consumed by a pass that probed nothing.
		w.restoreForced(forced, forceAll)
		return
	}

	seen := make(map[string]bool, len(listing.Sessions))
	jobs := make([]*probeJob, 0, len(listing.Sessions))
	for _, s := range listing.Sessions {
		if s.ID == "" || seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		if w.unsupported[s.ID] {
			continue
		}
		at, known := w.deps.Activity(s.Name)
		if !forceAll && !forced[s.ID] && known {
			if last, probed := w.probedAt[s.ID]; probed && last.Equal(at) {
				// Nothing on the session's screen changed since the last
				// capture, so neither did its pending interaction.
				continue
			}
		}
		jobs = append(jobs, &probeJob{session: s, activity: at, known: known})
	}

	w.probe(ctx, jobs)

	for _, job := range jobs {
		w.apply(job)
	}
	for id := range w.probedAt {
		if !seen[id] {
			delete(w.probedAt, id)
		}
	}
	for id := range w.unsupported {
		if !seen[id] {
			delete(w.unsupported, id)
		}
	}
	if listing.Partial {
		return
	}
	gone := make([]string, 0)
	for id := range w.published {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	for _, id := range gone {
		w.publishCleared(Session{ID: id}, w.published[id], ReasonSessionGone)
	}
}

func (w *Watcher) restoreForced(forced map[string]bool, forceAll bool) {
	w.pokeMu.Lock()
	defer w.pokeMu.Unlock()
	w.forceAll = w.forceAll || forceAll
	for id := range forced {
		w.forced[id] = true
	}
}

func (w *Watcher) probe(ctx context.Context, jobs []*probeJob) {
	sem := make(chan struct{}, ProbeConcurrency)
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(job *probeJob) {
			defer wg.Done()
			defer func() { <-sem }()
			job.obs = w.deps.Probe(ctx, job.session.Name)
		}(job)
	}
	wg.Wait()
}

// apply diffs one probe against what was announced and publishes the
// transition, if any. Only a probe whose transitions all landed records the
// activity it saw, so a failed probe or publish is retried on the next pass.
func (w *Watcher) apply(job *probeJob) {
	id := job.session.ID
	if job.obs.Err != nil {
		// Unknown is not "none": keep what was announced.
		w.deps.Logf("pending interactions: probing session %s: %v", id, job.obs.Err)
		delete(w.probedAt, id)
		return
	}
	if !job.obs.Supported {
		w.unsupported[id] = true
		return
	}
	prev, had := w.published[id]
	cur := job.obs.Pending
	ok := true
	switch {
	case cur == nil && had:
		ok = w.publishCleared(job.session, prev, ReasonResolved)
	case cur != nil && !had:
		ok = w.publishPending(job.session, *cur)
	case cur != nil && had && cur.RequestID != prev.RequestID:
		ok = w.publishCleared(job.session, prev, ReasonReplaced) && w.publishPending(job.session, *cur)
	}
	if ok && job.known {
		w.probedAt[id] = job.activity
	} else {
		delete(w.probedAt, id)
	}
}

func (w *Watcher) publishPending(s Session, p runtime.PendingInteraction) bool {
	p.Options = append([]string(nil), p.Options...)
	if p.Metadata != nil {
		md := make(map[string]string, len(p.Metadata))
		for k, v := range p.Metadata {
			md[k] = v
		}
		p.Metadata = md
	}
	if err := w.deps.Publish(Transition{Type: events.SessionPending, Session: s, Pending: p}); err != nil {
		w.deps.Logf("pending interactions: %s for %s not recorded, retrying next pass: %v", events.SessionPending, s.ID, err)
		return false
	}
	w.published[s.ID] = Published{RequestID: p.RequestID, Kind: p.Kind}
	return true
}

func (w *Watcher) publishCleared(s Session, prev Published, reason string) bool {
	if err := w.deps.Publish(Transition{Type: events.SessionPendingCleared, Session: s, Previous: prev, Reason: reason}); err != nil {
		w.deps.Logf("pending interactions: %s for %s not recorded, retrying next pass: %v", events.SessionPendingCleared, s.ID, err)
		return false
	}
	delete(w.published, s.ID)
	return true
}

// pendingEventFields is the part of both transition payloads Rebuild reads.
type pendingEventFields struct {
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"`
}

// Rebuild returns the announced set the event log records: for each session,
// its last session.pending or session.pending_cleared event decides, in log
// (Seq) order. Events of other types and undecodable payloads are ignored.
func Rebuild(evs []events.Event) map[string]Published {
	sorted := append([]events.Event(nil), evs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	out := map[string]Published{}
	for _, e := range sorted {
		if e.Type != events.SessionPending && e.Type != events.SessionPendingCleared {
			continue
		}
		var f pendingEventFields
		if err := json.Unmarshal(e.Payload, &f); err != nil {
			continue
		}
		id := f.SessionID
		if id == "" {
			id = e.SessionID
		}
		if id == "" {
			continue
		}
		if e.Type == events.SessionPending {
			out[id] = Published{RequestID: f.RequestID, Kind: f.Kind}
		} else {
			delete(out, id)
		}
	}
	return out
}

// RebuildFrom reads both transition types from the log and rebuilds the
// announced set (see Rebuild).
func RebuildFrom(p events.Provider) (map[string]Published, error) {
	var all []events.Event
	for _, t := range []string{events.SessionPending, events.SessionPendingCleared} {
		evs, err := p.List(events.Filter{Type: t})
		if err != nil {
			return nil, err
		}
		all = append(all, evs...)
	}
	return Rebuild(all), nil
}
