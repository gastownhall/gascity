package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Pending interactions on the city event stream
//
// A pending monitor turns the snapshot GET /v0/city/{cityName}/pending serves
// into transitions on the city event log: session.pending when a session
// gains an interaction, session.pending_cleared when it loses it. Clients that
// watch the city (or supervisor) event stream learn about approvals without
// polling.
//
// Detection. A cycle runs the same probe GET /pending runs — the cached
// session read model plus one runtime Pending() probe per active session,
// fanned out cityPendingProbeConcurrency at a time — and diffs the result
// against what it last published, keyed by session ID and request ID. The
// probe is the runtime's own detector (for tmux, the pane scrape), so what
// counts as pending is exactly what GET /pending reports.
//
// Cost. The monitor runs only while at least one event stream for the city
// is open: the city stream and the supervisor stream each hold a lease, and
// the last lease to go stops the loop. With no watcher it costs nothing, as
// the per-session stream's stall-gated probe does. With watchers it costs one
// probe per active session every pendingMonitorInterval no matter how many
// clients watch — less than one client polling GET /pending every second,
// which is what it replaces. A successful POST .../respond pokes it so the
// clear lands immediately rather than on the next tick.
//
// Delivery. A transition is marked published only after the event log
// acknowledges the append (events.AckRecorder), so a dropped append is retried
// on the next cycle rather than lost, and while the monitor runs an unchanged
// interaction is never re-announced however many cycles see it. An exec:
// provider cannot acknowledge, so its appends are trusted as every emitter
// trusts Record, and one it drops is lost. Across a restart delivery is not
// exactly once: the published set is saved after a cycle's appends, not with
// them, so after a crash between the two, or a failed save, the next monitor
// diffs against a saved set that lags the log. It can then repeat a transition
// the log already holds, or miss one: the clear of an interaction it does not
// know was announced, or an interaction cleared and back under the same
// request ID. Consumers dedupe on session ID and request ID; one that must not
// miss a transition re-reads GET /pending when its stream reconnects. A probe
// error leaves that session's published state alone; a partial session listing
// suppresses the session_gone clears it cannot vouch for.
//
// Restarts and resumes. The published set is persisted under the city's
// .gc/runtime directory, so a monitor that starts — after a supervisor
// restart, or when a client returns after every watcher left — emits only the
// difference between the saved set and what is pending now. Every transition
// goes through the durable city event log, so a client resuming with
// Last-Event-ID receives the transitions it missed: those logged while it was
// away, plus the reconciling transitions the monitor emits when the client's
// own stream restarts it. Something that became pending and was answered while
// nothing watched the city is never announced; nobody could have acted on it.
// A client connecting without a cursor starts at the head of the log, so it
// reads GET /v0/city/{cityName}/pending once for the current set and applies
// events from there.

// pendingMonitorInterval is how often an active pending monitor probes.
var pendingMonitorInterval = 2 * time.Second

// pendingMonitorStateFile is the persisted published set, relative to the
// city's runtime data directory.
const pendingMonitorStateFile = "pending-interactions.json"

// publishedPending is what the monitor last announced for one session.
type publishedPending struct {
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"`
}

// pendingMonitorState is the on-disk form of the published set.
type pendingMonitorState struct {
	Sessions map[string]publishedPending `json:"sessions"`
}

// pendingMonitor publishes pending-interaction transitions for one city. One
// exists per city path per process (see pendingMonitorFor), shared by every
// Server built for that city, so two Servers for one city cannot both
// announce the same transition.
type pendingMonitor struct {
	cityPath string

	// mu guards the lease and loop fields.
	mu     sync.Mutex
	refs   int
	srv    *Server
	cancel context.CancelFunc
	done   chan struct{}
	poke   chan struct{}

	// cycleMu serializes cycles and guards loaded and published.
	cycleMu   sync.Mutex
	loaded    bool
	published map[string]publishedPending
}

func newPendingMonitor(cityPath string) *pendingMonitor {
	return &pendingMonitor{
		cityPath:  cityPath,
		poke:      make(chan struct{}, 1),
		published: map[string]publishedPending{},
	}
}

var pendingMonitors = struct {
	mu sync.Mutex
	m  map[string]*pendingMonitor
}{m: map[string]*pendingMonitor{}}

// pendingMonitorFor returns the process-wide monitor for cityPath.
func pendingMonitorFor(cityPath string) *pendingMonitor {
	pendingMonitors.mu.Lock()
	defer pendingMonitors.mu.Unlock()
	m, ok := pendingMonitors.m[cityPath]
	if !ok {
		m = newPendingMonitor(cityPath)
		pendingMonitors.m[cityPath] = m
	}
	return m
}

// acquirePendingMonitor starts (or joins) the city's pending monitor and
// returns the function that gives the lease back. Event-stream handlers hold a
// lease for as long as their client is connected.
func (s *Server) acquirePendingMonitor() func() {
	m := pendingMonitorFor(s.state.CityPath())
	m.acquire(s)
	var once sync.Once
	return func() { once.Do(m.release) }
}

// pokePendingMonitor asks a running monitor to probe now, e.g. right after an
// interaction was answered. It does nothing when no stream is watching.
func (s *Server) pokePendingMonitor() {
	pendingMonitorFor(s.state.CityPath()).wake()
}

func (m *pendingMonitor) acquire(srv *Server) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs++
	// The newest Server wins: after a city restart it carries the live State.
	m.srv = srv
	if m.refs > 1 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	go m.run(ctx, m.done)
}

func (m *pendingMonitor) release() {
	m.mu.Lock()
	m.refs--
	if m.refs > 0 {
		m.mu.Unlock()
		return
	}
	cancel, done := m.cancel, m.done
	m.cancel, m.done, m.srv = nil, nil, nil
	m.mu.Unlock()
	cancel()
	<-done
}

func (m *pendingMonitor) wake() {
	select {
	case m.poke <- struct{}{}:
	default:
	}
}

func (m *pendingMonitor) currentServer() *Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.srv
}

func (m *pendingMonitor) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(pendingMonitorInterval)
	defer ticker.Stop()
	for {
		if srv := m.currentServer(); srv != nil {
			m.cycle(srv)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.poke:
		}
	}
}

// cycle runs one detection pass and publishes the transitions it finds.
func (m *pendingMonitor) cycle(srv *Server) {
	m.cycleMu.Lock()
	defer m.cycleMu.Unlock()

	if !m.loaded {
		m.published = m.loadState()
		m.loaded = true
	}

	ep := srv.state.EventProvider()
	if ep == nil {
		return
	}
	snap, err := srv.probeCityPending()
	if err != nil {
		log.Printf("api: pending monitor %s: listing sessions: %v", m.cityPath, err)
		return
	}

	changed := false
	seen := make(map[string]bool, len(snap.rows))
	for _, row := range snap.rows {
		id := row.info.ID
		seen[id] = true
		if row.err != nil {
			// Unknown is not "none": keep what was published.
			log.Printf("api: pending monitor %s: probing session %s: %v", m.cityPath, id, row.err)
			continue
		}
		if !row.supported {
			continue
		}
		prev, had := m.published[id]
		cur := row.pending
		switch {
		case cur == nil && had:
			changed = m.publishCleared(ep, id, prev, PendingClearedResolved) || changed
		case cur != nil && !had:
			changed = m.publishPending(ep, row) || changed
		case cur != nil && had && cur.RequestID != prev.RequestID:
			if m.publishCleared(ep, id, prev, PendingClearedReplaced) {
				changed = true
				m.publishPending(ep, row)
			}
		}
	}
	if !snap.listingPartial {
		gone := make([]string, 0)
		for id := range m.published {
			if !seen[id] {
				gone = append(gone, id)
			}
		}
		sort.Strings(gone)
		for _, id := range gone {
			changed = m.publishCleared(ep, id, m.published[id], PendingClearedSessionGone) || changed
		}
	}
	if changed {
		m.saveState()
	}
}

func (m *pendingMonitor) publishPending(ep events.Provider, row cityPendingProbeRow) bool {
	p := row.pending
	payload := SessionPendingPayload{
		SessionID: row.info.ID,
		Template:  row.info.Template,
		Alias:     row.info.Alias,
		RequestID: p.RequestID,
		Kind:      p.Kind,
		Prompt:    p.Prompt,
		Options:   append([]string(nil), p.Options...),
		Metadata:  cloneStringMap(p.Metadata),
	}
	if !m.record(ep, events.SessionPending, row.info.ID, payload) {
		return false
	}
	m.published[row.info.ID] = publishedPending{RequestID: p.RequestID, Kind: p.Kind}
	return true
}

func (m *pendingMonitor) publishCleared(ep events.Provider, sessionID string, prev publishedPending, reason string) bool {
	payload := SessionPendingClearedPayload{
		SessionID: sessionID,
		RequestID: prev.RequestID,
		Kind:      prev.Kind,
		Reason:    reason,
	}
	if !m.record(ep, events.SessionPendingCleared, sessionID, payload) {
		return false
	}
	delete(m.published, sessionID)
	return true
}

// record appends one transition and reports whether the log acknowledged it.
// A recorder without acknowledgements is trusted, as every other emitter
// trusts Record.
func (m *pendingMonitor) record(ep events.Provider, eventType, sessionID string, payload events.Payload) bool {
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("api: pending monitor %s: marshal %s for %s: %v", m.cityPath, eventType, sessionID, err)
		return false
	}
	e := events.Event{
		Type:      eventType,
		Actor:     "api",
		Subject:   sessionID,
		SessionID: sessionID,
		Payload:   raw,
	}
	ack, ok := ep.(events.AckRecorder)
	if !ok {
		ep.Record(e)
		return true
	}
	if err := ack.RecordAck(e); err != nil {
		log.Printf("api: pending monitor %s: %s for %s not recorded, retrying next cycle: %v", m.cityPath, eventType, sessionID, err)
		return false
	}
	return true
}

func (m *pendingMonitor) statePath() string {
	if m.cityPath == "" {
		return ""
	}
	return filepath.Join(citylayout.RuntimeDataDir(m.cityPath), pendingMonitorStateFile)
}

// loadState reads the published set a previous monitor left. A missing file
// is an empty set; an unreadable one is logged and treated as empty, which can
// at worst re-announce an interaction or miss one clear for a client that
// does not reconcile against GET /pending.
func (m *pendingMonitor) loadState() map[string]publishedPending {
	out := map[string]publishedPending{}
	path := m.statePath()
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out
	}
	if err != nil {
		log.Printf("api: pending monitor %s: reading %s: %v (starting with no published interactions)", m.cityPath, path, err)
		return out
	}
	var st pendingMonitorState
	if err := json.Unmarshal(data, &st); err != nil {
		log.Printf("api: pending monitor %s: decoding %s: %v (starting with no published interactions)", m.cityPath, path, err)
		return out
	}
	for id, p := range st.Sessions {
		out[id] = p
	}
	return out
}

func (m *pendingMonitor) saveState() {
	path := m.statePath()
	if path == "" {
		return
	}
	if err := writePendingMonitorState(path, m.published); err != nil {
		log.Printf("api: pending monitor %s: %v (a restart may re-announce or miss a transition)", m.cityPath, err)
	}
}

func writePendingMonitorState(path string, published map[string]publishedPending) error {
	data, err := json.MarshalIndent(pendingMonitorState{Sessions: published}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding pending state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// pendingMonitorLeases is the set of pending-monitor leases one
// supervisor-scope event stream holds: one per running city it watches.
type pendingMonitorLeases struct {
	held map[string]heldPendingLease
}

type heldPendingLease struct {
	state   State
	release func()
}

func newPendingMonitorLeases() *pendingMonitorLeases {
	return &pendingMonitorLeases{held: map[string]heldPendingLease{}}
}

// syncPendingMonitorLeases makes leases match the running cities: it leases
// the monitor of every running city with an event provider and gives back
// the leases of cities that stopped or restarted (a restart re-leases through
// the new State's Server).
func (sm *SupervisorMux) syncPendingMonitorLeases(leases *pendingMonitorLeases) {
	wanted := map[string]State{}
	for _, c := range sm.resolver.ListCities() {
		if !c.Running {
			continue
		}
		st := sm.resolver.CityState(c.Name)
		if st == nil || st.EventProvider() == nil {
			continue
		}
		wanted[c.Name] = st
	}
	for name, h := range leases.held {
		if st, ok := wanted[name]; !ok || st != h.state {
			h.release()
			delete(leases.held, name)
		}
	}
	for name, st := range wanted {
		if _, ok := leases.held[name]; ok {
			continue
		}
		leases.held[name] = heldPendingLease{
			state:   st,
			release: sm.getCityServer(name, st).acquirePendingMonitor(),
		}
	}
}

// releaseAll gives back every lease.
func (leases *pendingMonitorLeases) releaseAll() {
	for name, h := range leases.held {
		h.release()
		delete(leases.held, name)
	}
}
