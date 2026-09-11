package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file implements runtime.SessionEventProvider over herdr's socket API.
//
// Unlike the CLI-verb client in client.go, the event stream talks to the
// session server's unix socket directly: `events.subscribe` holds its
// connection open as an NDJSON event stream, which a shell-out cannot carry.
// The server serves ONE request per connection (it answers, then closes), so
// the subscribe call owns a dedicated connection and any side lookups
// (agent.list) open their own short-lived ones.
//
// Wire facts, verified live against herdr 0.7.3:
//   - Every request needs a `params` field, even when empty.
//   - Subscribe ack: {"id":…,"result":{"type":"subscription_started"}}.
//   - Broadcast kinds stream as {"event":"pane_exited","data":{…}} — the
//     EventEnvelope schema, underscore names. The targeted kinds
//     (pane.output_matched / pane.agent_status_changed / pane.scroll_changed)
//     stream with their dot names and REQUIRE a pane_id filter — there is no
//     broadcast agent-status subscription, which is why the filter set must
//     be maintained per pane as sessions come and go.
//   - pane.output_changed is not a subscribable kind in 0.7.3 (the broadcast
//     EventKind exists, but events.subscribe rejects it).
//   - No frame carries a sequence number, so a dropped connection is an
//     undetectable gap: every (re)connect emits SessionEventResync and the
//     consumer reconciles from polled state.
//   - The server replays a backlog of the session's recent events to every
//     new subscription (observed live: the same pane_closed/agent_detected
//     frames re-delivered after each resubscribe). Frames carry no ids or
//     timestamps to filter the replay, so it is passed through; the
//     interface contract makes events level-triggered hints.

// sessionEventChanBuffer sizes the subscriber channel. A variable so unit
// tests can shrink it to pin the backpressure-coalescing contract.
var sessionEventChanBuffer = 256

const (
	// sessionEventMinBackoff..MaxBackoff bound the reconnect backoff after a
	// transport failure; a cycle that stayed healthy for
	// sessionEventHealthyCycle resets the backoff to the minimum.
	sessionEventMinBackoff   = 250 * time.Millisecond
	sessionEventMaxBackoff   = 5 * time.Second
	sessionEventHealthyCycle = 5 * time.Second
	// sessionEventRelistDebounce coalesces a burst of pane_created /
	// pane_agent_detected frames into one agent.list refresh.
	sessionEventRelistDebounce = 300 * time.Millisecond
	// sessionEventOpTimeout bounds each bounded socket op (dial, subscribe
	// ack, agent.list) so a wedged server cannot hang the stream loop; the
	// event stream itself legitimately idles indefinitely and carries no
	// deadline.
	sessionEventOpTimeout = 10 * time.Second
)

var (
	_ runtime.SessionEventProvider = (*Provider)(nil)

	// sockRequestID distinguishes concurrent requests in server logs; the
	// client never multiplexes, so it is not used for response routing.
	sockRequestID atomic.Uint64
)

// subscribeSub is one events.subscribe filter entry.
type subscribeSub struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id,omitempty"`
}

// subscribeParams is the events.subscribe params payload.
type subscribeParams struct {
	Subscriptions []subscribeSub `json:"subscriptions"`
}

// sockRequest is the socket API request envelope.
type sockRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

// SubscribeSessionEvents implements runtime.SessionEventProvider: it starts a
// self-healing subscription to herdr's event stream and translates pane/agent
// frames into session-attributed events. The stream does not require the
// session server to be up yet — it keeps dialing until the socket appears —
// and it never starts the server itself (that stays with Start /
// ConfigureServer; a sensor should not own the server lifecycle).
func (p *Provider) SubscribeSessionEvents(ctx context.Context) (<-chan runtime.SessionEvent, error) {
	ch := make(chan runtime.SessionEvent, sessionEventChanBuffer)
	go p.runSessionEventStream(ctx, ch)
	return ch, nil
}

// runSessionEventStream drives connection cycles until ctx is canceled. A
// cycle ending in resubscribe (the pane filter set grew) reconnects
// immediately; a transport failure reconnects with capped exponential
// backoff. Failures are logged once per streak, not per retry.
func (p *Provider) runSessionEventStream(ctx context.Context, ch chan runtime.SessionEvent) {
	defer close(ch)
	s := &sessionEventStream{c: p.c, ch: ch}
	backoff := sessionEventMinBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		resubscribe, err := s.runCycle(ctx)
		cycle := time.Since(start)
		if ctx.Err() != nil {
			return
		}
		if resubscribe {
			backoff = sessionEventMinBackoff
			continue
		}
		if err != nil && backoff == sessionEventMinBackoff {
			fmt.Fprintf(os.Stderr, "gc: herdr session-event stream for %q: %v (reconnecting)\n", p.c.session, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if cycle >= sessionEventHealthyCycle {
			backoff = sessionEventMinBackoff
			continue
		}
		backoff *= 2
		if backoff > sessionEventMaxBackoff {
			backoff = sessionEventMaxBackoff
		}
	}
}

// sessionEventStream is one subscriber's translation state.
type sessionEventStream struct {
	c  *client
	ch chan runtime.SessionEvent

	// paneNames maps pane id → gc session name. Rebuilt at cycle start and
	// merge-only within a cycle: entries are never deleted mid-cycle, so a
	// late pane_exited for an agent that already dropped out of the registry
	// (herdr reaps the record before the pane dies) still attributes.
	paneNames map[string]string
	// subscribed tracks pane ids covered by a per-pane agent-status filter in
	// the current cycle; a known agent pane missing from it forces a
	// resubscribe. Removals are lazy — herdr just never fires for a gone pane
	// — so only additions cycle the connection.
	subscribed map[string]bool
	// pendingResync records that events were dropped on a full channel; the
	// loss is surfaced as a SessionEventResync once the consumer drains.
	pendingResync bool
}

// runCycle runs one connection cycle: list agents, subscribe with the derived
// filter set, emit the leading resync, then translate frames until the
// transport fails (err), the filter set must grow (resubscribe), or ctx ends.
func (s *sessionEventStream) runCycle(ctx context.Context) (resubscribe bool, err error) {
	agents, err := s.c.sockAgentList(ctx)
	if err != nil {
		return false, err
	}
	s.paneNames = make(map[string]string, len(agents))
	s.subscribed = make(map[string]bool, len(agents))
	subs := []subscribeSub{
		{Type: "pane.created"},
		{Type: "pane.closed"},
		{Type: "pane.exited"},
		{Type: "pane.agent_detected"},
	}
	for _, a := range agents {
		if a.PaneID == "" || a.Name == "" {
			continue
		}
		s.paneNames[a.PaneID] = a.Name
		if !s.subscribed[a.PaneID] {
			s.subscribed[a.PaneID] = true
			subs = append(subs, subscribeSub{Type: "pane.agent_status_changed", PaneID: a.PaneID})
		}
	}

	// cctx scopes the connection and its reader goroutine to this cycle:
	// every exit path (transport error, resubscribe, parent cancel) cancels
	// it, which closes the conn (unblocking a pending ReadBytes) and releases
	// a reader blocked on handing over a line — otherwise each resubscribe
	// would strand one reader goroutine until the whole subscription ends.
	cctx, ccancel := context.WithCancel(ctx)
	defer ccancel()

	conn, err := s.c.dialSocket(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	stopAfter := context.AfterFunc(cctx, func() { _ = conn.Close() })
	defer stopAfter()

	_ = conn.SetDeadline(time.Now().Add(sessionEventOpTimeout))
	if err := sockSend(conn, "events.subscribe", subscribeParams{Subscriptions: subs}); err != nil {
		return false, fmt.Errorf("herdr events.subscribe: %w", err)
	}
	reader := bufio.NewReader(conn)
	if _, err := readSockResponse(reader); err != nil {
		return false, fmt.Errorf("herdr events.subscribe: %w", err)
	}
	// The stream idles indefinitely between events — no deadline from here.
	_ = conn.SetDeadline(time.Time{})

	// Stream established. Events since the last cycle are lost, so the first
	// delivery is the reconcile marker; it also subsumes any resync owed from
	// the previous cycle. This send intentionally blocks: there is no point
	// reading frames a full consumer cannot take, and ctx bounds the wait.
	select {
	case s.ch <- resyncEvent():
		s.pendingResync = false
	case <-ctx.Done():
		return false, ctx.Err()
	}

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				readErr <- err
				return
			}
			select {
			case lines <- line:
			case <-cctx.Done():
				return
			}
		}
	}()

	relist := time.NewTimer(sessionEventRelistDebounce)
	if !relist.Stop() {
		<-relist.C
	}
	defer relist.Stop()
	relistArmed := false

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case err := <-readErr:
			return false, err
		case <-relist.C:
			relistArmed = false
			s.tryPendingResync() // piggyback: a drained consumer gets its owed resync even in a quiet stream
			agents, err := s.c.sockAgentList(ctx)
			if err != nil {
				return false, err
			}
			for _, a := range agents {
				if a.PaneID == "" || a.Name == "" {
					continue
				}
				s.paneNames[a.PaneID] = a.Name
				if !s.subscribed[a.PaneID] {
					resubscribe = true
				}
			}
			if resubscribe {
				return true, nil
			}
		case line := <-lines:
			if s.handleFrame(line) && !relistArmed {
				relist.Reset(sessionEventRelistDebounce)
				relistArmed = true
			}
		}
	}
}

// handleFrame translates one wire frame into a SessionEvent. It reports
// whether the frame hints at a new agent pane (arm the debounced re-list).
// Unknown frame shapes and kinds are ignored for forward compatibility.
func (s *sessionEventStream) handleFrame(line []byte) (relistHint bool) {
	var f struct {
		Event string `json:"event"`
		Data  struct {
			PaneID      string `json:"pane_id"`
			AgentStatus string `json:"agent_status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &f); err != nil {
		return false
	}
	ev := runtime.SessionEvent{
		Session: s.paneNames[f.Data.PaneID],
		Ref:     f.Data.PaneID,
		Time:    time.Now(),
	}
	switch f.Event {
	case "pane_exited":
		ev.Kind = runtime.SessionEventExited
	case "pane_closed":
		ev.Kind = runtime.SessionEventClosed
	case "pane_agent_detected":
		// An agent registered — possibly a session started after this cycle's
		// filter set was built.
		ev.Kind = runtime.SessionEventAgentDetected
		relistHint = true
	case "pane.agent_status_changed":
		ev.Kind = runtime.SessionEventAgentStatus
		ev.AgentStatus = f.Data.AgentStatus
	case "pane_created":
		// Filter-maintenance hint only (the payload nests the pane object and
		// carries no agent mapping yet); consumers see the session once its
		// agent registers.
		return true
	default:
		return false
	}
	s.emit(ev)
	return relistHint
}

// emit delivers ev without ever blocking the read loop: an owed resync is
// flushed first, and a full channel converts the event into an owed resync
// (the event itself is dropped — the consumer reconciles on the resync).
func (s *sessionEventStream) emit(ev runtime.SessionEvent) {
	if !s.tryPendingResync() {
		return
	}
	select {
	case s.ch <- ev:
	default:
		s.pendingResync = true
	}
}

// tryPendingResync flushes an owed resync if any; false while one is still owed.
func (s *sessionEventStream) tryPendingResync() bool {
	if !s.pendingResync {
		return true
	}
	select {
	case s.ch <- resyncEvent():
		s.pendingResync = false
		return true
	default:
		return false
	}
}

func resyncEvent() runtime.SessionEvent {
	return runtime.SessionEvent{Kind: runtime.SessionEventResync, Time: time.Now()}
}

// ── socket-native request helpers ────────────────────────────────────────────

// dialSocket connects to this session server's unix socket.
func (c *client) dialSocket(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", c.socketPath())
}

// sockSend writes one request line. params must be non-nil — the server
// rejects requests without a params field.
func sockSend(conn net.Conn, method string, params any) error {
	b, err := json.Marshal(sockRequest{
		ID:     "gc-evt-" + strconv.FormatUint(sockRequestID.Add(1), 10),
		Method: method,
		Params: params,
	})
	if err != nil {
		return err
	}
	_, err = conn.Write(append(b, '\n'))
	return err
}

// readSockResponse reads one response line and unwraps the envelope.
func readSockResponse(r *bufio.Reader) (json.RawMessage, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
	}
	return env.Result, nil
}

// sockAgentList is agent.list over a dedicated short-lived connection (the
// server serves one request per connection), bounded by sessionEventOpTimeout.
func (c *client) sockAgentList(ctx context.Context) ([]agentInfo, error) {
	conn, err := c.dialSocket(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(sessionEventOpTimeout))
	if err := sockSend(conn, "agent.list", struct{}{}); err != nil {
		return nil, fmt.Errorf("herdr agent.list (socket): %w", err)
	}
	res, err := readSockResponse(bufio.NewReader(conn))
	if err != nil {
		return nil, fmt.Errorf("herdr agent.list (socket): %w", err)
	}
	var wrap struct {
		Agents []agentInfo `json:"agents"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return nil, fmt.Errorf("herdr agent.list (socket): decode: %w", err)
	}
	return wrap.Agents, nil
}
