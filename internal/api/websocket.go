package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gorilla/websocket"
)

const wsProtocolVersion = "gc.v1alpha1"

// maxWSMessageSize is the maximum allowed inbound WebSocket message (10 MB).
const maxWSMessageSize = 10 << 20

type socketRequestEnvelope struct {
	Type           string             `json:"type"`
	ID             string             `json:"id"`
	Action         string             `json:"action"`
	IdempotencyKey string             `json:"idempotency_key,omitempty"`
	Scope          *socketScope       `json:"scope,omitempty"`
	Payload        json.RawMessage    `json:"payload,omitempty"`
	Watch          *socketWatchParams `json:"watch,omitempty"`
}

// socketWatchParams provides blocking query semantics over WebSocket,
// equivalent to HTTP ?index=X&wait=Y.
type socketWatchParams struct {
	Index uint64 `json:"index"`
	Wait  string `json:"wait,omitempty"` // duration string, e.g., "30s"
}

type socketScope struct {
	City string `json:"city,omitempty"`
}

type socketHelloEnvelope struct {
	Type              string   `json:"type"`
	Protocol          string   `json:"protocol"`
	ServerRole        string   `json:"server_role"`
	ReadOnly          bool     `json:"read_only"`
	Capabilities      []string `json:"capabilities"`
	SubscriptionKinds []string `json:"subscription_kinds,omitempty"`
}

type socketResponseEnvelope struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Index  uint64 `json:"index,omitempty"`
	Result any    `json:"result,omitempty"`
}

type socketErrorEnvelope struct {
	Type    string       `json:"type"`
	ID      string       `json:"id,omitempty"`
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Details []FieldError `json:"details,omitempty"`
}

type socketActionResult struct {
	Index      uint64
	Result     any
	AfterWrite func()
}

type socketNamePayload struct {
	Name string `json:"name"`
}

type socketIDPayload struct {
	ID string `json:"id"`
}

type socketSessionsListPayload struct {
	State    string `json:"state,omitempty"`
	Template string `json:"template,omitempty"`
	Peek     bool   `json:"peek,omitempty"`
	Limit    *int   `json:"limit,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
}

type socketAgentsListPayload struct {
	Pool    string `json:"pool,omitempty"`
	Rig     string `json:"rig,omitempty"`
	Running string `json:"running,omitempty"`
	Peek    bool   `json:"peek,omitempty"`
}

type socketBeadsListPayload struct {
	Status   string `json:"status,omitempty"`
	Type     string `json:"type,omitempty"`
	Label    string `json:"label,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	Rig      string `json:"rig,omitempty"`
	Limit    *int   `json:"limit,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
}

type socketMailListPayload struct {
	Agent  string `json:"agent,omitempty"`
	Status string `json:"status,omitempty"`
	Rig    string `json:"rig,omitempty"`
	Limit  *int   `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type socketMailGetPayload struct {
	ID  string `json:"id"`
	Rig string `json:"rig,omitempty"`
}

type socketMailCountPayload struct {
	Agent string `json:"agent,omitempty"`
	Rig   string `json:"rig,omitempty"`
}

type socketMailThreadPayload struct {
	ID  string `json:"id"`
	Rig string `json:"rig,omitempty"`
}

type socketEventsListPayload struct {
	Type  string `json:"type,omitempty"`
	Actor string `json:"actor,omitempty"`
	Since string `json:"since,omitempty"`
	Limit *int   `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type socketBeadAssignPayload struct {
	ID       string `json:"id"`
	Assignee string `json:"assignee"`
}

type socketBeadGraphPayload struct {
	RootID string `json:"root_id"`
}

type socketSessionTargetPayload struct {
	ID   string `json:"id"`
	Peek bool   `json:"peek,omitempty"`
}

type socketSessionSubmitPayload struct {
	ID      string               `json:"id"`
	Message string               `json:"message"`
	Intent  session.SubmitIntent `json:"intent,omitempty"`
}

type socketSessionTranscriptPayload struct {
	ID     string `json:"id"`
	Turns  int    `json:"turns,omitempty"` // most recent N turns (0=all)
	Before string `json:"before,omitempty"`
	Format string `json:"format,omitempty"`
}

type socketProvidersListPayload struct {
	View string `json:"view,omitempty"`
}

type socketSessionRenamePayload struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type socketSessionRespondPayload struct {
	ID        string            `json:"id"`
	RequestID string            `json:"request_id,omitempty"`
	Action    string            `json:"action"`
	Text      string            `json:"text,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type socketMailReplyPayload struct {
	ID      string `json:"id"`
	Rig     string `json:"rig,omitempty"`
	From    string `json:"from"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

type socketFormulaScopePayload struct {
	ScopeKind string `json:"scope_kind"`
	ScopeRef  string `json:"scope_ref"`
}

type socketFormulaFeedPayload struct {
	ScopeKind string `json:"scope_kind"`
	ScopeRef  string `json:"scope_ref"`
	Limit     int    `json:"limit,omitempty"`
}

type socketFormulaGetPayload struct {
	Name      string            `json:"name"`
	ScopeKind string            `json:"scope_kind"`
	ScopeRef  string            `json:"scope_ref"`
	Target    string            `json:"target"`
	Vars      map[string]string `json:"vars,omitempty"`
}

type socketFormulaRunsPayload struct {
	Name      string `json:"name"`
	ScopeKind string `json:"scope_kind"`
	ScopeRef  string `json:"scope_ref"`
	Limit     int    `json:"limit,omitempty"`
}

type socketOrdersHistoryPayload struct {
	ScopedName string `json:"scoped_name"`
	Limit      int    `json:"limit,omitempty"`
	Before     string `json:"before,omitempty"`
}

type socketOrdersFeedPayload struct {
	ScopeKind string `json:"scope_kind"`
	ScopeRef  string `json:"scope_ref"`
	Limit     int    `json:"limit,omitempty"`
}

type socketExtMsgBindingsPayload struct {
	SessionID string `json:"session_id"`
}

type socketWorkflowGetPayload struct {
	ID        string `json:"id"`
	ScopeKind string `json:"scope_kind,omitempty"`
	ScopeRef  string `json:"scope_ref,omitempty"`
}

type socketWorkflowDeletePayload struct {
	ID        string `json:"id"`
	ScopeKind string `json:"scope_kind,omitempty"`
	ScopeRef  string `json:"scope_ref,omitempty"`
	Delete    bool   `json:"delete,omitempty"`
}

type socketSessionPatchPayload struct {
	ID    string  `json:"id"`
	Title *string `json:"title,omitempty"`
	Alias *string `json:"alias,omitempty"`
}

type socketSessionAgentGetPayload struct {
	ID      string `json:"id"`
	AgentID string `json:"agent_id"`
}

type socketSessionMessagesPayload struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

type socketConvoyItemsPayload struct {
	ID    string   `json:"id"`
	Items []string `json:"items"`
}

type socketAgentUpdatePayload struct {
	Name      string `json:"name"`
	Provider  string `json:"provider,omitempty"`
	Scope     string `json:"scope,omitempty"`
	Suspended *bool  `json:"suspended,omitempty"`
}

type socketRigCreatePayload struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

type socketRigUpdatePayload struct {
	Name      string `json:"name"`
	Path      string `json:"path,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Suspended *bool  `json:"suspended,omitempty"`
}

type socketProviderCreatePayload struct {
	Name string             `json:"name"`
	Spec config.ProviderSpec `json:"spec"`
}

type socketProviderUpdatePayload struct {
	Name   string         `json:"name"`
	Update ProviderUpdate `json:"update"`
}

func socketPageParams(limit *int, cursor string, defaultLimit int) pageParams {
	pp := pageParams{
		Limit:    defaultLimit,
		IsPaging: strings.TrimSpace(cursor) != "",
	}
	if limit != nil {
		switch {
		case *limit == 0:
			pp.Limit = maxPaginationLimit
		case *limit > 0:
			pp.Limit = *limit
		}
	}
	if pp.Limit > maxPaginationLimit {
		pp.Limit = maxPaginationLimit
	}
	if cursor != "" {
		pp.Offset = decodeCursor(cursor)
	}
	return pp
}

type socketHandler interface {
	socketHello() socketHelloEnvelope
	handleSocketRequest(*socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope)
	startSocketSubscription(context.Context, *socketSession, *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope)
	stopSocketSubscription(*socketSession, *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope)
}

type socketConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

type httpError struct {
	status  int
	code    string
	message string
	details []FieldError
}

func (e httpError) Error() string { return e.message }

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return origin == "" || isLocalhostOrigin(origin)
	},
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	serveWebSocket(w, r, s)
}

func (sm *SupervisorMux) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	serveWebSocket(w, r, sm)
}

func serveWebSocket(w http.ResponseWriter, r *http.Request, handler socketHandler) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("api: websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	conn.SetReadLimit(maxWSMessageSize)

	hello := handler.socketHello()
	log.Printf("api: ws connected remote=%s role=%s read_only=%v", r.RemoteAddr, hello.ServerRole, hello.ReadOnly)
	telemetry.RecordWebSocketConnection(r.Context(), 1)

	sc := &socketConn{conn: conn}
	ss := newSocketSession(r.Context(), sc)
	defer ss.close()

	// Send appropriate close frame when the handler exits.
	// Default to normal closure; detect shutdown via request context.
	closeCode := websocket.CloseNormalClosure
	closeText := ""
	defer func() {
		_ = sc.writeClose(closeCode, closeText)
		log.Printf("api: ws disconnected remote=%s close_code=%d", r.RemoteAddr, closeCode)
		telemetry.RecordWebSocketConnection(r.Context(), -1)
	}()

	// Detect server shutdown via the request context and send close 1001.
	go func() {
		<-r.Context().Done()
		_ = sc.writeClose(websocket.CloseGoingAway, "server shutting down")
		ss.cancel()
	}()

	if err := conn.SetReadDeadline(time.Now().Add(socketPongWait)); err != nil {
		return
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(socketPongWait))
	})
	go ss.runPingLoop()

	if err := sc.writeJSON(hello); err != nil {
		return
	}

	for {
		var req socketRequestEnvelope
		if err := conn.ReadJSON(&req); err != nil {
			return
		}
		if req.Type != "request" {
			if err := sc.writeJSON(newSocketError(req.ID, "invalid", "message type must be request")); err != nil {
				return
			}
			continue
		}
		if req.ID == "" || req.Action == "" {
			if err := sc.writeJSON(newSocketError(req.ID, "invalid", "request id and action are required")); err != nil {
				return
			}
			continue
		}

		// Dispatch concurrently so the read loop can process the next
		// request immediately. The single-writer pattern (socketConn.mu)
		// serializes all outbound writes. Subscription start/stop must
		// still run synchronously to avoid races on the subscription map.
		reqCopy := req
		switch req.Action {
		case "subscription.start":
			start := time.Now()
			result, apiErr := handler.startSocketSubscription(ss.ctx, ss, &reqCopy)
			dur := time.Since(start)
			if apiErr != nil {
				log.Printf("api: ws req id=%s action=%s latency=%s err=%s/%s", reqCopy.ID, reqCopy.Action, dur.Round(time.Microsecond), apiErr.Code, apiErr.Message)
				if err := sc.writeJSON(apiErr); err != nil {
					return
				}
				continue
			}
			log.Printf("api: ws req id=%s action=%s latency=%s ok", reqCopy.ID, reqCopy.Action, dur.Round(time.Microsecond))
			if err := sc.writeJSON(socketResponseEnvelope{
				Type:   "response",
				ID:     reqCopy.ID,
				Index:  result.Index,
				Result: result.Result,
			}); err != nil {
				return
			}
			if result.AfterWrite != nil {
				result.AfterWrite()
			}
		case "subscription.stop":
			start := time.Now()
			result, apiErr := handler.stopSocketSubscription(ss, &reqCopy)
			dur := time.Since(start)
			if apiErr != nil {
				log.Printf("api: ws req id=%s action=%s latency=%s err=%s/%s", reqCopy.ID, reqCopy.Action, dur.Round(time.Microsecond), apiErr.Code, apiErr.Message)
				if err := sc.writeJSON(apiErr); err != nil {
					return
				}
				continue
			}
			log.Printf("api: ws req id=%s action=%s latency=%s ok", reqCopy.ID, reqCopy.Action, dur.Round(time.Microsecond))
			if err := sc.writeJSON(socketResponseEnvelope{
				Type:   "response",
				ID:     reqCopy.ID,
				Index:  result.Index,
				Result: result.Result,
			}); err != nil {
				return
			}
		default:
			go func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("api: ws dispatch panic for %s: %v", reqCopy.Action, r)
						closeCode = websocket.CloseInternalServerErr // 1011
						closeText = "internal server error"
						ss.cancel()
					}
				}()
				start := time.Now()
				result, apiErr := handler.handleSocketRequest(&reqCopy)

				dur := time.Since(start)
				if apiErr != nil {
					log.Printf("api: ws req id=%s action=%s latency=%s err=%s/%s", reqCopy.ID, reqCopy.Action, dur.Round(time.Microsecond), apiErr.Code, apiErr.Message)
					telemetry.RecordWebSocketRequest(context.Background(), reqCopy.Action, apiErr.Code, float64(dur.Milliseconds()))
					if err := sc.writeJSON(apiErr); err != nil {
						ss.cancel() // A3: cancel session on write error
					}
					return
				}
				log.Printf("api: ws req id=%s action=%s latency=%s ok", reqCopy.ID, reqCopy.Action, dur.Round(time.Microsecond))
				telemetry.RecordWebSocketRequest(context.Background(), reqCopy.Action, "", float64(dur.Milliseconds()))
				if err := sc.writeResponseChecked(reqCopy.ID, socketResponseEnvelope{
					Type:   "response",
					ID:     reqCopy.ID,
					Index:  result.Index,
					Result: result.Result,
				}); err != nil {
					ss.cancel() // A3: cancel session on write error
					return
				}
				if result.AfterWrite != nil {
					result.AfterWrite()
				}
			}()
		}
	}
}

// maxWSOutboundSize is the maximum allowed outbound WebSocket message (10 MB).
// Responses exceeding this are replaced with an error envelope.
const maxWSOutboundSize = 10 << 20

func (sc *socketConn) writeJSON(v any) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.conn.WriteJSON(v)
}

// writeResponseChecked writes a response envelope, replacing it with a
// size-correlated error if the marshaled payload exceeds the outbound limit.
// The error envelope preserves the request ID so concurrent clients can
// correlate the failure.
func (sc *socketConn) writeResponseChecked(reqID string, resp socketResponseEnvelope) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if len(data) > maxWSOutboundSize {
		log.Printf("api: ws outbound message too large (%d bytes) for req %s, sending error", len(data), reqID)
		return sc.conn.WriteJSON(socketErrorEnvelope{
			Type:    "error",
			ID:      reqID,
			Code:    "message_too_large",
			Message: "response exceeds maximum message size",
		})
	}
	return sc.conn.WriteMessage(websocket.TextMessage, data)
}

// writeClose sends a WebSocket close frame with the given code and text.
func (sc *socketConn) writeClose(code int, text string) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	msg := websocket.FormatCloseMessage(code, text)
	return sc.conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(5*time.Second))
}

func (s *Server) socketHello() socketHelloEnvelope {
	return socketHelloEnvelope{
		Type:       "hello",
		Protocol:   wsProtocolVersion,
		ServerRole: "city",
		ReadOnly:   s.readOnly,
		Capabilities: []string{
			"health.get",
			"status.get",
			"city.get",
			"city.patch",
			"config.get",
			"config.explain",
			"config.validate",
			"bead.create",
			"bead.close",
			"bead.update",
			"sessions.list",
			"beads.list",
			"beads.ready",
			"bead.get",
			"bead.deps",
			"bead.delete",
			"mail.list",
			"mail.get",
			"mail.count",
			"mail.thread",
			"mail.read",
			"mail.mark_unread",
			"mail.archive",
			"mail.reply",
			"mail.send",
			"events.list",
			"rigs.list",
			"convoys.list",
			"convoy.get",
			"sling.run",
			"services.list",
			"service.get",
			"service.restart",
			"packs.list",
			"providers.list",
			"provider.get",
			"agents.list",
			"agent.get",
			"agent.suspend",
			"agent.resume",
			"rig.get",
			"rig.suspend",
			"rig.resume",
			"rig.restart",
			"session.get",
			"session.suspend",
			"session.close",
			"session.wake",
			"session.rename",
			"session.respond",
			"session.kill",
			"session.pending",
			"session.submit",
			"session.transcript",
			"formulas.list",
			"formulas.feed",
			"formula.get",
			"formula.runs",
			"orders.list",
			"orders.check",
			"order.get",
			"order.enable",
			"order.disable",
			"orders.history",
			"order.history.detail",
			"extmsg.inbound",
			"extmsg.outbound",
			"extmsg.bindings.list",
			"extmsg.bind",
			"extmsg.unbind",
			"extmsg.groups.lookup",
			"extmsg.groups.ensure",
			"extmsg.participant.upsert",
			"extmsg.participant.remove",
			"extmsg.transcript.list",
			"extmsg.transcript.ack",
			"extmsg.adapters.list",
			"extmsg.adapters.register",
			"extmsg.adapters.unregister",
			"patches.agents.list",
			"patches.agent.get",
			"patches.agents.set",
			"patches.agent.delete",
			"patches.rigs.list",
			"patches.rig.get",
			"patches.rigs.set",
			"patches.rig.delete",
			"patches.providers.list",
			"patches.provider.get",
			"patches.providers.set",
			"patches.provider.delete",
			"agent.create",
			"agent.update",
			"agent.delete",
			"rig.create",
			"rig.update",
			"rig.delete",
			"provider.create",
			"provider.update",
			"provider.delete",
			"event.emit",
			"mail.delete",
			"convoy.create",
			"convoy.add",
			"convoy.remove",
			"convoy.check",
			"convoy.close",
			"convoy.delete",
			"session.stop",
			"session.messages",
			"session.agents.list",
			"session.agent.get",
			"session.create",
			"session.patch",
			"workflow.get",
			"workflow.delete",
			"orders.feed",
		},
		SubscriptionKinds: []string{"events", "session.stream"},
	}
}

func (sm *SupervisorMux) socketHello() socketHelloEnvelope {
	return socketHelloEnvelope{
		Type:       "hello",
		Protocol:   wsProtocolVersion,
		ServerRole: "supervisor",
		ReadOnly:   sm.readOnly,
		Capabilities: []string{
			"health.get",
			"cities.list",
			"city.get",
			"city.patch",
			"config.get",
			"config.explain",
			"config.validate",
			"bead.create",
			"bead.close",
			"bead.update",
			"status.get",
			"sessions.list",
			"beads.list",
			"beads.ready",
			"bead.get",
			"bead.deps",
			"bead.delete",
			"mail.list",
			"mail.get",
			"mail.count",
			"mail.thread",
			"mail.read",
			"mail.mark_unread",
			"mail.archive",
			"mail.reply",
			"mail.send",
			"events.list",
			"rigs.list",
			"convoys.list",
			"convoy.get",
			"sling.run",
			"services.list",
			"service.get",
			"service.restart",
			"packs.list",
			"providers.list",
			"provider.get",
			"agents.list",
			"agent.get",
			"agent.suspend",
			"agent.resume",
			"rig.get",
			"rig.suspend",
			"rig.resume",
			"rig.restart",
			"session.get",
			"session.suspend",
			"session.close",
			"session.wake",
			"session.rename",
			"session.respond",
			"session.kill",
			"session.pending",
			"session.submit",
			"session.transcript",
			"formulas.list",
			"formulas.feed",
			"formula.get",
			"formula.runs",
			"orders.list",
			"orders.check",
			"order.get",
			"order.enable",
			"order.disable",
			"orders.history",
			"order.history.detail",
			"extmsg.inbound",
			"extmsg.outbound",
			"extmsg.bindings.list",
			"extmsg.bind",
			"extmsg.unbind",
			"extmsg.groups.lookup",
			"extmsg.groups.ensure",
			"extmsg.participant.upsert",
			"extmsg.participant.remove",
			"extmsg.transcript.list",
			"extmsg.transcript.ack",
			"extmsg.adapters.list",
			"extmsg.adapters.register",
			"extmsg.adapters.unregister",
			"patches.agents.list",
			"patches.agent.get",
			"patches.agents.set",
			"patches.agent.delete",
			"patches.rigs.list",
			"patches.rig.get",
			"patches.rigs.set",
			"patches.rig.delete",
			"patches.providers.list",
			"patches.provider.get",
			"patches.providers.set",
			"patches.provider.delete",
			"agent.create",
			"agent.update",
			"agent.delete",
			"rig.create",
			"rig.update",
			"rig.delete",
			"provider.create",
			"provider.update",
			"provider.delete",
			"event.emit",
			"mail.delete",
			"convoy.create",
			"convoy.add",
			"convoy.remove",
			"convoy.check",
			"convoy.close",
			"convoy.delete",
			"session.stop",
			"session.messages",
			"session.agents.list",
			"session.agent.get",
			"session.create",
			"session.patch",
			"workflow.get",
			"workflow.delete",
			"orders.feed",
		},
		SubscriptionKinds: []string{"events", "session.stream"},
	}
}

func (s *Server) handleSocketRequest(req *socketRequestEnvelope) (result socketActionResult, apiErr *socketErrorEnvelope) {
	// Apply blocking watch semantics after the handler returns.
	defer func() {
		if apiErr == nil && req.Watch != nil && socketActionSupportsWatch(req.Action) {
			if ep := s.state.EventProvider(); ep != nil {
				bp := socketBlockingParams(req.Watch)
				if bp.HasIndex {
					idx := waitForChange(context.Background(), ep, bp)
					result.Index = idx
				}
			}
		}
	}()

	// On per-city servers, validate that scope.city matches (or is absent).
	if req.Scope != nil && req.Scope.City != "" {
		if cityName := s.state.CityName(); req.Scope.City != cityName {
			return socketActionResult{}, newSocketError(req.ID, "invalid",
				"scope.city "+req.Scope.City+" does not match this city "+cityName)
		}
	}
	switch req.Action {
	case "health.get":
		return socketActionResult{Result: s.healthResponse()}, nil
	case "status.get":
		return socketActionResult{Result: s.statusSnapshot(), Index: s.latestIndex()}, nil
	case "city.get":
		return socketActionResult{Result: s.cityGet(), Index: s.latestIndex()}, nil
	case "city.patch":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var body cityPatchRequest
		if err := decodeSocketPayload(req.Payload, &body); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if body.Suspended == nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "no fields to update")
		}
		if err := s.patchCitySuspended(*body.Suspended); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "ok"}}, nil
	case "config.get":
		return socketActionResult{Result: s.configGet(), Index: s.latestIndex()}, nil
	case "config.explain":
		return socketActionResult{Result: s.configExplain(), Index: s.latestIndex()}, nil
	case "config.validate":
		return socketActionResult{Result: s.configValidate(), Index: s.latestIndex()}, nil
	case "agents.list":
		var payload socketAgentsListPayload
		if err := decodeOptionalSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		items := s.listAgentResponses(payload.Pool, payload.Rig, payload.Running, payload.Peek)
		return socketActionResult{Result: listResponse{Items: items, Total: len(items)}, Index: s.latestIndex()}, nil
	case "agent.get":
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		cfg := s.state.Config()
		agentCfg, ok := findAgent(cfg, payload.Name)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "not_found", "agent "+payload.Name+" not found")
		}
		resp, _ := s.buildExpandedAgentResponse(agentCfg, expandedAgent{
			qualifiedName: payload.Name,
			rig:           agentCfg.Dir,
			suspended:     agentCfg.Suspended,
			provider:      agentCfg.Provider,
			description:   agentCfg.Description,
		}, false, "")
		return socketActionResult{Result: resp, Index: s.latestIndex()}, nil
	case "rig.get":
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		resp, ok := s.getRigResponse(payload.Name, false)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "not_found", "rig "+payload.Name+" not found")
		}
		return socketActionResult{Result: resp, Index: s.latestIndex()}, nil
	case "sessions.list":
		var payload socketSessionsListPayload
		if err := decodeOptionalSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		items, err := s.listSessionResponses(payload.State, payload.Template, payload.Peek)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		pp := socketPageParams(payload.Limit, payload.Cursor, maxPaginationLimit)
		if !pp.IsPaging {
			if pp.Limit < len(items) {
				items = items[:pp.Limit]
			}
			return socketActionResult{Result: listResponse{Items: items, Total: len(items)}, Index: s.latestIndex()}, nil
		}
		page, total, nextCursor := paginate(items, pp)
		if page == nil {
			page = []sessionResponse{}
		}
		return socketActionResult{Result: listResponse{Items: page, Total: total, NextCursor: nextCursor}, Index: s.latestIndex()}, nil
	case "beads.list":
		var payload socketBeadsListPayload
		if err := decodeOptionalSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		items := s.listBeads(beads.ListQuery{
			Status:   payload.Status,
			Type:     payload.Type,
			Label:    payload.Label,
			Assignee: payload.Assignee,
		}, payload.Rig, nil)
		pp := socketPageParams(payload.Limit, payload.Cursor, 50)
		if !pp.IsPaging {
			if pp.Limit < len(items) {
				items = items[:pp.Limit]
			}
			return socketActionResult{Result: listResponse{Items: items, Total: len(items)}, Index: s.latestIndex()}, nil
		}
		page, total, nextCursor := paginate(items, pp)
		if page == nil {
			page = []beads.Bead{}
		}
		return socketActionResult{Result: listResponse{Items: page, Total: total, NextCursor: nextCursor}, Index: s.latestIndex()}, nil
	case "beads.ready":
		items, err := s.listReadyBeads()
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: listResponse{Items: items, Total: len(items)}, Index: s.latestIndex()}, nil
	case "bead.get":
		var payload socketIDPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		bead, err := s.getBead(payload.ID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return socketActionResult{}, newSocketError(req.ID, "not_found", "bead "+payload.ID+" not found")
			}
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: bead, Index: s.latestIndex()}, nil
	case "bead.deps":
		var payload socketIDPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		resp, err := s.getBeadDeps(payload.ID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: resp, Index: s.latestIndex()}, nil
	case "beads.graph":
		var payload socketBeadGraphPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		resp, err := s.getBeadGraph(payload.RootID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: resp, Index: s.latestIndex()}, nil
	case "bead.create":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload beadCreateRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		// Idempotency check.
		if req.IdempotencyKey != "" {
			idemKey := socketScopedIdemKey(s.state.CityName(), req.Action, req.IdempotencyKey)
			bodyHash := hashBody(payload)
			cached, handled, idemErr := s.idem.checkIdempotent(idemKey, bodyHash)
			if idemErr != nil {
				idemErr.ID = req.ID
				return socketActionResult{}, idemErr
			}
			if handled {
				return socketActionResult{Result: cached}, nil
			}
			defer func() {
				// Store result on success for replay.
			}()
		}
		bead, err := s.createBead(payload)
		if err != nil {
			if req.IdempotencyKey != "" {
				s.idem.unreserve(socketScopedIdemKey(s.state.CityName(), req.Action, req.IdempotencyKey))
			}
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		if req.IdempotencyKey != "" {
			s.idem.storeResponse(socketScopedIdemKey(s.state.CityName(), req.Action, req.IdempotencyKey), hashBody(payload), 201, bead)
		}
		return socketActionResult{Result: bead}, nil
	case "bead.close":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketIDPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.closeBead(payload.ID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "bead.update":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketIDPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.updateBead(payload.ID, req.Payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "bead.reopen":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketIDPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.reopenBead(payload.ID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "bead.assign":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketBeadAssignPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.assignBead(payload.ID, payload.Assignee)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "bead.delete":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketIDPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := s.deleteBead(payload.ID); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "deleted"}}, nil
	case "mail.list":
		var payload socketMailListPayload
		if err := decodeOptionalSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		items, err := s.listMailMessages(payload.Agent, payload.Status, payload.Rig)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		pp := socketPageParams(payload.Limit, payload.Cursor, 50)
		if !pp.IsPaging {
			total := len(items)
			if pp.Limit < len(items) {
				items = items[:pp.Limit]
			}
			return socketActionResult{Result: listResponse{Items: items, Total: total}, Index: s.latestIndex()}, nil
		}
		page, total, nextCursor := paginate(items, pp)
		if page == nil {
			page = []mail.Message{}
		}
		return socketActionResult{Result: listResponse{Items: page, Total: total, NextCursor: nextCursor}, Index: s.latestIndex()}, nil
	case "mail.get":
		var payload socketMailGetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		msg, err := s.getMailMessage(payload.ID, payload.Rig)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: msg, Index: s.latestIndex()}, nil
	case "mail.count":
		var payload socketMailCountPayload
		if err := decodeOptionalSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.mailCount(payload.Agent, payload.Rig)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "mail.thread":
		var payload socketMailThreadPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.listMailThread(payload.ID, payload.Rig)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: listResponse{Items: result, Total: len(result)}, Index: s.latestIndex()}, nil
	case "mail.read":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketMailGetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.markMailRead(payload.ID, payload.Rig)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "mail.mark_unread":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketMailGetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.markMailUnread(payload.ID, payload.Rig)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "mail.archive":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketMailGetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.archiveMail(payload.ID, payload.Rig)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "mail.reply":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketMailReplyPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.replyMail(payload.ID, payload.Rig, mailReplyRequest{
			From:    payload.From,
			Subject: payload.Subject,
			Body:    payload.Body,
		})
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "mail.send":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload mailSendRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if req.IdempotencyKey != "" {
			idemKey := socketScopedIdemKey(s.state.CityName(), req.Action, req.IdempotencyKey)
			bodyHash := hashBody(payload)
			cached, handled, idemErr := s.idem.checkIdempotent(idemKey, bodyHash)
			if idemErr != nil {
				idemErr.ID = req.ID
				return socketActionResult{}, idemErr
			}
			if handled {
				return socketActionResult{Result: cached}, nil
			}
		}
		result, err := s.sendMail(payload)
		if err != nil {
			if req.IdempotencyKey != "" {
				s.idem.unreserve(socketScopedIdemKey(s.state.CityName(), req.Action, req.IdempotencyKey))
			}
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		if req.IdempotencyKey != "" {
			s.idem.storeResponse(socketScopedIdemKey(s.state.CityName(), req.Action, req.IdempotencyKey), hashBody(payload), 201, result)
		}
		return socketActionResult{Result: result}, nil
	case "events.list":
		var payload socketEventsListPayload
		if err := decodeOptionalSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		filter := events.Filter{Type: payload.Type, Actor: payload.Actor}
		if payload.Since != "" {
			d, err := time.ParseDuration(payload.Since)
			if err != nil {
				return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
			}
			filter.Since = time.Now().Add(-d)
		}
		items, err := s.listEvents(filter)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		pp := socketPageParams(payload.Limit, payload.Cursor, 100)
		if !pp.IsPaging {
			total := len(items)
			if pp.Limit < len(items) {
				items = items[:pp.Limit]
			}
			return socketActionResult{Result: listResponse{Items: items, Total: total}, Index: s.latestIndex()}, nil
		}
		page, total, nextCursor := paginate(items, pp)
		if page == nil {
			page = []events.Event{}
		}
		return socketActionResult{Result: listResponse{Items: page, Total: total, NextCursor: nextCursor}, Index: s.latestIndex()}, nil
	case "rigs.list":
		items := s.listRigResponses(false)
		return socketActionResult{Result: listResponse{Items: items, Total: len(items)}, Index: s.latestIndex()}, nil
	case "convoys.list":
		items := s.listConvoys()
		return socketActionResult{Result: listResponse{Items: items, Total: len(items)}, Index: s.latestIndex()}, nil
	case "convoy.get":
		var payload socketIDPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		snapshot, err := s.getConvoySnapshot(payload.ID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: snapshot, Index: s.latestIndex()}, nil
	case "sling.run":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload slingBody
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, _, err := s.runSling(context.Background(), payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "services.list":
		items := s.listServices()
		return socketActionResult{Result: listResponse{Items: items, Total: len(items)}}, nil
	case "service.get":
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		service, err := s.getService(payload.Name)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: service}, nil
	case "service.restart":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := s.restartService(payload.Name); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "ok", "action": "restart", "service": payload.Name}}, nil
	case "packs.list":
		return socketActionResult{Result: map[string]any{"packs": s.listPacks()}, Index: s.latestIndex()}, nil
	case "providers.list":
		var payload socketProvidersListPayload
		if err := decodeOptionalSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		items := s.listProviders(payload.View == "public")
		return socketActionResult{Result: listResponse{Items: items, Total: len(items)}, Index: s.latestIndex()}, nil
	case "provider.get":
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		provider, err := s.getProvider(payload.Name)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: provider, Index: s.latestIndex()}, nil
	case "formulas.list":
		var payload socketFormulaScopePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.listFormulas(payload.ScopeKind, payload.ScopeRef)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "formulas.feed":
		var payload socketFormulaFeedPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.getFormulaFeed(payload.ScopeKind, payload.ScopeRef, payload.Limit)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "formula.get":
		var payload socketFormulaGetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.getFormulaDetail(context.Background(), payload.Name, payload.ScopeKind, payload.ScopeRef, payload.Target, payload.Vars)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "formula.runs":
		var payload socketFormulaRunsPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.getFormulaRuns(payload.Name, payload.ScopeKind, payload.ScopeRef, payload.Limit)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "orders.list":
		aa := s.state.Orders()
		resp := make([]orderResponse, len(aa))
		for i, a := range aa {
			resp[i] = toOrderResponse(a)
		}
		return socketActionResult{Result: map[string]any{"orders": resp}, Index: s.latestIndex()}, nil
	case "order.get":
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		a, err := resolveOrder(s.state.Orders(), payload.Name)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: toOrderResponse(*a), Index: s.latestIndex()}, nil
	case "order.enable", "order.disable":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		a, err := resolveOrder(s.state.Orders(), payload.Name)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		enabled := req.Action == "order.enable"
		if enabled {
			err = sm.EnableOrder(a.Name, a.Rig)
		} else {
			err = sm.DisableOrder(a.Name, a.Rig)
		}
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		action := "enabled"
		if !enabled {
			action = "disabled"
		}
		return socketActionResult{Result: map[string]string{"status": action, "order": a.Name}}, nil
	case "orders.check":
		result := s.checkOrders()
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "orders.history":
		var payload socketOrdersHistoryPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.getOrderHistory(payload.ScopedName, payload.Limit, payload.Before)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "order.history.detail":
		var payload socketIDPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		store := s.state.CityBeadStore()
		if store == nil {
			return socketActionResult{}, newSocketError(req.ID, "unavailable", "no bead store configured")
		}
		bead, err := store.Get(payload.ID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: bead, Index: s.latestIndex()}, nil
	case "agent.suspend", "agent.resume":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		action := req.Action[len("agent."):]
		if err := s.applyAgentAction(payload.Name, action); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "ok"}}, nil
	case "rig.suspend", "rig.resume", "rig.restart":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		action := req.Action[len("rig."):]
		result, err := s.applyRigAction(payload.Name, action)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "session.kill":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketSessionTargetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.killSessionTarget(payload.ID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "session.get":
		var payload socketSessionTargetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.getSessionResponse(payload.ID, payload.Peek)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "session.suspend":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketSessionTargetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := s.suspendSessionTarget(payload.ID); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "ok"}}, nil
	case "session.close":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketSessionTargetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := s.closeSessionTarget(payload.ID); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "ok"}}, nil
	case "session.wake":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketSessionTargetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.wakeSessionTarget(context.Background(), payload.ID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "session.rename":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketSessionRenamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.renameSessionTarget(payload.ID, payload.Title)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "session.respond":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketSessionRespondPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.respondSessionTarget(payload.ID, sessionRespondRequest{
			RequestID: payload.RequestID,
			Action:    payload.Action,
			Text:      payload.Text,
			Metadata:  payload.Metadata,
		})
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "session.pending":
		var payload socketSessionTargetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.getSessionPending(payload.ID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "session.submit":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketSessionSubmitPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if payload.Intent == "" {
			payload.Intent = session.SubmitIntentDefault
		}
		result, err := s.submitSessionTarget(context.Background(), payload.ID, payload.Message, payload.Intent)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "session.transcript":
		var payload socketSessionTranscriptPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.getSessionTranscript(payload.ID, sessionTranscriptQuery{
			Tail:   payload.Turns,
			Before: payload.Before,
			Raw:    payload.Format == "raw",
		})
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "patches.agents.list":
		cfg := s.state.Config()
		patches := cfg.Patches.Agents
		if patches == nil {
			patches = []config.AgentPatch{}
		}
		return socketActionResult{Result: listResponse{Items: patches, Total: len(patches)}, Index: s.latestIndex()}, nil
	case "patches.agent.get":
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		cfg := s.state.Config()
		dir, base := config.ParseQualifiedName(payload.Name)
		for _, p := range cfg.Patches.Agents {
			if p.Dir == dir && p.Name == base {
				return socketActionResult{Result: p, Index: s.latestIndex()}, nil
			}
		}
		return socketActionResult{}, newSocketError(req.ID, "not_found", "agent patch "+payload.Name+" not found")
	case "patches.agents.set":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var patch config.AgentPatch
		if err := decodeSocketPayload(req.Payload, &patch); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if patch.Name == "" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "name is required")
		}
		if err := sm.SetAgentPatch(patch); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		qn := patch.Name
		if patch.Dir != "" {
			qn = patch.Dir + "/" + patch.Name
		}
		return socketActionResult{Result: map[string]string{"status": "ok", "agent_patch": qn}}, nil
	case "patches.agent.delete":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := sm.DeleteAgentPatch(payload.Name); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "deleted", "agent_patch": payload.Name}}, nil
	case "patches.rigs.list":
		cfg := s.state.Config()
		patches := cfg.Patches.Rigs
		if patches == nil {
			patches = []config.RigPatch{}
		}
		return socketActionResult{Result: listResponse{Items: patches, Total: len(patches)}, Index: s.latestIndex()}, nil
	case "patches.rig.get":
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		cfg := s.state.Config()
		for _, p := range cfg.Patches.Rigs {
			if p.Name == payload.Name {
				return socketActionResult{Result: p, Index: s.latestIndex()}, nil
			}
		}
		return socketActionResult{}, newSocketError(req.ID, "not_found", "rig patch "+payload.Name+" not found")
	case "patches.rigs.set":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var patch config.RigPatch
		if err := decodeSocketPayload(req.Payload, &patch); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if patch.Name == "" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "name is required")
		}
		if err := sm.SetRigPatch(patch); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "ok", "rig_patch": patch.Name}}, nil
	case "patches.rig.delete":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := sm.DeleteRigPatch(payload.Name); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "deleted", "rig_patch": payload.Name}}, nil
	case "patches.providers.list":
		cfg := s.state.Config()
		patches := cfg.Patches.Providers
		if patches == nil {
			patches = []config.ProviderPatch{}
		}
		return socketActionResult{Result: listResponse{Items: patches, Total: len(patches)}, Index: s.latestIndex()}, nil
	case "patches.provider.get":
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		cfg := s.state.Config()
		for _, p := range cfg.Patches.Providers {
			if p.Name == payload.Name {
				return socketActionResult{Result: p, Index: s.latestIndex()}, nil
			}
		}
		return socketActionResult{}, newSocketError(req.ID, "not_found", "provider patch "+payload.Name+" not found")
	case "patches.providers.set":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var patch config.ProviderPatch
		if err := decodeSocketPayload(req.Payload, &patch); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if patch.Name == "" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "name is required")
		}
		if err := sm.SetProviderPatch(patch); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "ok", "provider_patch": patch.Name}}, nil
	case "patches.provider.delete":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := sm.DeleteProviderPatch(payload.Name); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "deleted", "provider_patch": payload.Name}}, nil
	case "extmsg.inbound":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload extmsgInboundRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.processExtMsgInbound(context.Background(), payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "extmsg.outbound":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload extmsgOutboundRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.processExtMsgOutbound(context.Background(), payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "extmsg.bindings.list":
		var payload socketExtMsgBindingsPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.listExtMsgBindings(context.Background(), payload.SessionID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "extmsg.bind":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload extmsgBindRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.processExtMsgBind(context.Background(), payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "extmsg.unbind":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload extmsgUnbindRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.processExtMsgUnbind(context.Background(), payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "extmsg.groups.lookup":
		var payload extmsgGroupLookupRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.lookupExtMsgGroup(context.Background(), payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "extmsg.groups.ensure":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload extmsgGroupEnsureRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.ensureExtMsgGroup(context.Background(), payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "extmsg.participant.upsert":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload extmsgParticipantUpsertRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.upsertExtMsgParticipant(context.Background(), payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "extmsg.participant.remove":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload extmsgParticipantRemoveRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.removeExtMsgParticipant(context.Background(), payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "extmsg.transcript.list":
		var payload extmsgTranscriptListRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.listExtMsgTranscript(context.Background(), payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "extmsg.transcript.ack":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload extmsgTranscriptAckRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.ackExtMsgTranscript(context.Background(), payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "extmsg.adapters.list":
		result, err := s.listExtMsgAdapters()
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "extmsg.adapters.register":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload extmsgAdapterRegisterRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.registerExtMsgAdapter(payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "extmsg.adapters.unregister":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload extmsgAdapterUnregisterRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.unregisterExtMsgAdapter(payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "workflow.get":
		var payload socketWorkflowGetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if payload.ID == "" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "id is required")
		}
		// Validate optional scope params.
		if (payload.ScopeKind != "" || payload.ScopeRef != "") && (payload.ScopeKind == "" || payload.ScopeRef == "") {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "scope_kind and scope_ref must be provided together")
		}
		if payload.ScopeKind != "" && payload.ScopeKind != "city" && payload.ScopeKind != "rig" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "scope_kind must be 'city' or 'rig'")
		}
		index := s.latestIndex()
		snapshot, err := s.buildWorkflowSnapshot(payload.ID, payload.ScopeKind, payload.ScopeRef, index)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: snapshot, Index: index}, nil
	case "workflow.delete":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketWorkflowDeletePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if payload.ID == "" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "id is required")
		}
		result, err := s.deleteWorkflow(payload.ID, payload.ScopeKind, payload.ScopeRef, payload.Delete)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "orders.feed":
		var payload socketOrdersFeedPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.getOrdersFeed(payload.ScopeKind, payload.ScopeRef, payload.Limit)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	// --- Session operations ---
	case "session.patch":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketSessionPatchPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.patchSession(payload.ID, payload.Title, payload.Alias)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "session.create":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload sessionCreateRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if payload.Name == "" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "name is required")
		}
		if payload.Kind != "agent" && payload.Kind != "provider" {
			return socketActionResult{}, newSocketError(req.ID, "invalid_kind", "kind must be 'agent' or 'provider'")
		}
		// Delegate to HTTP handler via internal round-trip for now.
		// This preserves all complex session lifecycle logic identically.
		result, statusCode, err := s.createSessionInternal(context.Background(), payload, req.IdempotencyKey)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		_ = statusCode
		return socketActionResult{Result: result}, nil
	case "session.agents.list":
		var payload socketSessionTargetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.listSessionAgents(payload.ID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "session.agent.get":
		var payload socketSessionAgentGetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.getSessionAgent(payload.ID, payload.AgentID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "session.stop":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketSessionTargetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		store := s.state.CityBeadStore()
		if store == nil {
			return socketActionResult{}, newSocketError(req.ID, "unavailable", "no bead store configured")
		}
		id, err := s.resolveSessionIDWithConfig(store, payload.ID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		mgr := s.sessionManager(store)
		if err := mgr.StopTurn(id); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "ok", "id": id}}, nil
	case "session.messages":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketSessionMessagesPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if strings.TrimSpace(payload.Message) == "" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "message is required")
		}
		store := s.state.CityBeadStore()
		if store == nil {
			return socketActionResult{}, newSocketError(req.ID, "unavailable", "no bead store configured")
		}
		id, err := s.resolveSessionIDMaterializingNamedWithContext(context.Background(), store, payload.ID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		if err := s.sendUserMessageToSession(context.Background(), store, id, payload.Message); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "accepted", "id": id}}, nil
	// --- Convoy mutations ---
	case "convoy.create":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload convoyCreateRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.createConvoy(payload)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	case "convoy.add":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketConvoyItemsPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := s.convoyAddItems(payload.ID, payload.Items); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "updated"}}, nil
	case "convoy.remove":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketConvoyItemsPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := s.convoyRemoveItems(payload.ID, payload.Items); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "updated"}}, nil
	case "convoy.check":
		var payload socketIDPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		result, err := s.convoyCheck(payload.ID)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result, Index: s.latestIndex()}, nil
	case "convoy.close":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketIDPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := s.convoyClose(payload.ID); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "closed"}}, nil
	case "convoy.delete":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketIDPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := s.convoyDelete(payload.ID); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "deleted"}}, nil
	// --- Agent CRUD ---
	case "agent.create":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var payload agentCreateRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if payload.Name == "" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "name is required")
		}
		if payload.Provider == "" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "provider is required")
		}
		a := config.Agent{Name: payload.Name, Dir: payload.Dir, Provider: payload.Provider, Scope: payload.Scope}
		if err := sm.CreateAgent(a); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "created", "agent": a.QualifiedName()}}, nil
	case "agent.update":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var payload socketAgentUpdatePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := sm.UpdateAgent(payload.Name, AgentUpdate(agentUpdateRequest{Provider: payload.Provider, Scope: payload.Scope, Suspended: payload.Suspended})); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "updated", "agent": payload.Name}}, nil
	case "agent.delete":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := sm.DeleteAgent(payload.Name); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "deleted", "agent": payload.Name}}, nil
	// --- Rig CRUD ---
	case "rig.create":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var payload socketRigCreatePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if payload.Name == "" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "name is required")
		}
		if err := sm.CreateRig(config.Rig{Name: payload.Name, Path: payload.Path}); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "created", "rig": payload.Name}}, nil
	case "rig.update":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var payload socketRigUpdatePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := sm.UpdateRig(payload.Name, RigUpdate{Path: payload.Path, Prefix: payload.Prefix, Suspended: payload.Suspended}); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "updated", "rig": payload.Name}}, nil
	case "rig.delete":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := sm.DeleteRig(payload.Name); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "deleted", "rig": payload.Name}}, nil
	// --- Provider CRUD ---
	case "provider.create":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var payload socketProviderCreatePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if payload.Name == "" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "name is required")
		}
		if err := sm.CreateProvider(payload.Name, payload.Spec); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "created", "provider": payload.Name}}, nil
	case "provider.update":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var payload socketProviderUpdatePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := sm.UpdateProvider(payload.Name, payload.Update); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "updated", "provider": payload.Name}}, nil
	case "provider.delete":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		sm, ok := s.state.(StateMutator)
		if !ok {
			return socketActionResult{}, newSocketError(req.ID, "internal", "mutations not supported")
		}
		var payload socketNamePayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := sm.DeleteProvider(payload.Name); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "deleted", "provider": payload.Name}}, nil
	// --- Event emit ---
	case "event.emit":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload eventEmitRequest
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if payload.Type == "" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "type is required")
		}
		if payload.Actor == "" {
			return socketActionResult{}, newSocketError(req.ID, "invalid", "actor is required")
		}
		ep := s.state.EventProvider()
		if ep == nil {
			return socketActionResult{}, newSocketError(req.ID, "unavailable", "events not enabled")
		}
		ep.Record(events.Event{
			Type:    payload.Type,
			Actor:   payload.Actor,
			Subject: payload.Subject,
			Message: payload.Message,
		})
		return socketActionResult{Result: map[string]string{"status": "recorded"}}, nil
	case "mail.delete":
		if s.readOnly {
			return socketActionResult{}, newSocketError(req.ID, "read_only", "mutations disabled: server bound to non-localhost address")
		}
		var payload socketMailGetPayload
		if err := decodeSocketPayload(req.Payload, &payload); err != nil {
			return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
		}
		if err := s.deleteMail(payload.ID, payload.Rig); err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: map[string]string{"status": "deleted"}}, nil
	default:
		return socketActionResult{}, unknownSocketAction(req.ID, req.Action)
	}
}

func (sm *SupervisorMux) handleSocketRequest(req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope) {
	// Supervisor-level actions (no city scope required).
	switch req.Action {
	case "health.get":
		return socketActionResult{Result: sm.healthResponse()}, nil
	case "cities.list":
		return socketActionResult{Result: sm.citiesList()}, nil
	case "events.list":
		// Global events.list without scope aggregates from all cities.
		if req.Scope == nil || req.Scope.City == "" {
			result, err := sm.globalEventList(req)
			if err != nil {
				return socketActionResult{}, socketErrorFor(req.ID, err)
			}
			return socketActionResult{Result: result}, nil
		}
	}

	// City-scoped actions.
	if socketActionRequiresCityScope(req.Action) {
		cityName, apiErr := sm.resolveSocketCityTarget(req.Scope)
		if apiErr != nil {
			apiErr.ID = req.ID
			return socketActionResult{}, apiErr
		}
		state := sm.resolver.CityState(cityName)
		if state == nil {
			return socketActionResult{}, newSocketError(req.ID, "not_found", "city not found or not running: "+cityName)
		}
		cityReq := *req
		cityReq.Scope = nil
		srv := sm.getCityServer(cityName, state)
		return srv.handleSocketRequest(&cityReq)
	}

	return socketActionResult{}, unknownSocketAction(req.ID, req.Action)
}

func socketActionRequiresCityScope(action string) bool {
	switch action {
	case "city.patch",
		"city.get",
		"config.get",
		"config.explain",
		"config.validate",
		"agents.list",
		"agent.get",
		"bead.create",
		"bead.close",
		"bead.update",
		"status.get",
		"sessions.list",
		"beads.list",
		"beads.ready",
		"bead.get",
		"bead.deps",
		"beads.graph",
		"bead.reopen",
		"bead.assign",
		"bead.delete",
		"mail.list",
		"mail.get",
		"mail.count",
		"mail.thread",
		"mail.read",
		"mail.mark_unread",
		"mail.archive",
		"mail.reply",
		"mail.send",
		"events.list",
		"rigs.list",
		"convoys.list",
		"convoy.get",
		"sling.run",
		"services.list",
		"service.get",
		"service.restart",
		"packs.list",
		"providers.list",
		"provider.get",
		"agent.suspend",
		"agent.resume",
		"rig.get",
		"rig.suspend",
		"rig.resume",
		"rig.restart",
		"session.get",
		"session.suspend",
		"session.close",
		"session.wake",
		"session.rename",
		"session.respond",
		"session.kill",
		"session.pending",
		"session.submit",
		"session.transcript",
		"formulas.list",
		"formulas.feed",
		"formula.get",
		"formula.runs",
		"orders.list",
		"orders.check",
		"order.get",
		"order.enable",
		"order.disable",
		"orders.history",
		"order.history.detail",
		"extmsg.inbound",
		"extmsg.outbound",
		"extmsg.bindings.list",
		"extmsg.bind",
		"extmsg.unbind",
		"extmsg.groups.lookup",
		"extmsg.groups.ensure",
		"extmsg.participant.upsert",
		"extmsg.participant.remove",
		"extmsg.transcript.list",
		"extmsg.transcript.ack",
		"extmsg.adapters.list",
		"extmsg.adapters.register",
		"extmsg.adapters.unregister",
		"patches.agents.list",
		"patches.agent.get",
		"patches.agents.set",
		"patches.agent.delete",
		"patches.rigs.list",
		"patches.rig.get",
		"patches.rigs.set",
		"patches.rig.delete",
		"patches.providers.list",
		"patches.provider.get",
		"patches.providers.set",
		"patches.provider.delete",
		"agent.create",
		"agent.update",
		"agent.delete",
		"rig.create",
		"rig.update",
		"rig.delete",
		"provider.create",
		"provider.update",
		"provider.delete",
		"event.emit",
		"mail.delete",
		"convoy.create",
		"convoy.add",
		"convoy.remove",
		"convoy.check",
		"convoy.close",
		"convoy.delete",
		"session.stop",
		"session.messages",
		"session.agents.list",
		"session.agent.get",
		"session.create",
		"session.patch",
		"workflow.get",
		"workflow.delete",
		"orders.feed":
		return true
	default:
		return false
	}
}

func (sm *SupervisorMux) resolveSocketCityTarget(scope *socketScope) (string, *socketErrorEnvelope) {
	if scope != nil && scope.City != "" {
		return scope.City, nil
	}
	cities := sm.resolver.ListCities()
	running := make([]CityInfo, 0, len(cities))
	for _, city := range cities {
		if city.Running {
			running = append(running, city)
		}
	}
	switch len(running) {
	case 0:
		return "", newSocketError("", "no_cities", "no cities running")
	case 1:
		return running[0].Name, nil
	default:
		return "", newSocketError("", "city_required", "multiple cities running; use scope.city to specify which city")
	}
}

// socketBlockingParams converts WebSocket watch params into BlockingParams.
func socketBlockingParams(w *socketWatchParams) BlockingParams {
	if w == nil {
		return BlockingParams{}
	}
	bp := BlockingParams{Index: w.Index, HasIndex: true, Wait: defaultWait}
	if w.Wait != "" {
		if d, err := time.ParseDuration(w.Wait); err == nil && d > 0 {
			bp.Wait = d
		}
	}
	if bp.Wait > maxWait {
		bp.Wait = maxWait
	}
	return bp
}

// socketActionSupportsWatch returns true for actions that support blocking query semantics.
func socketActionSupportsWatch(action string) bool {
	switch action {
	case "beads.list", "beads.ready",
		"agents.list",
		"mail.list",
		"events.list",
		"rigs.list",
		"convoys.list",
		"status.get":
		return true
	default:
		return false
	}
}

func decodeSocketPayload(payload json.RawMessage, v any) error {
	if len(payload) == 0 {
		return errors.New("payload required")
	}
	return json.Unmarshal(payload, v)
}

func decodeOptionalSocketPayload(payload json.RawMessage, v any) error {
	if len(payload) == 0 {
		return nil
	}
	return json.Unmarshal(payload, v)
}

func socketErrorFor(id string, err error) *socketErrorEnvelope {
	var herr httpError
	var herrPtr *httpError
	if errors.As(err, &herr) {
		return newSocketErrorWithDetails(id, herr.code, herr.message, herr.details)
	}
	if errors.As(err, &herrPtr) {
		return newSocketErrorWithDetails(id, herrPtr.code, herrPtr.message, herrPtr.details)
	}
	switch {
	case errors.Is(err, beads.ErrNotFound), errors.Is(err, mail.ErrNotFound), errors.Is(err, errWorkflowNotFound):
		return newSocketError(id, "not_found", err.Error())
	case errors.Is(err, session.ErrAmbiguous), errors.Is(err, errConfiguredNamedSessionConflict):
		return newSocketError(id, "ambiguous", err.Error())
	case errors.Is(err, session.ErrSessionNotFound):
		return newSocketError(id, "not_found", err.Error())
	case errors.Is(err, session.ErrInvalidSessionName),
		errors.Is(err, session.ErrInvalidSessionAlias),
		errors.Is(err, session.ErrNotSession):
		return newSocketError(id, "invalid", err.Error())
	case errors.Is(err, session.ErrSessionNameExists),
		errors.Is(err, session.ErrSessionAliasExists),
		errors.Is(err, session.ErrPendingInteraction),
		errors.Is(err, session.ErrNoPendingInteraction),
		errors.Is(err, session.ErrInteractionMismatch),
		errors.Is(err, session.ErrSessionClosed),
		errors.Is(err, session.ErrResumeRequired):
		return newSocketError(id, "conflict", err.Error())
	case errors.Is(err, session.ErrInteractionUnsupported):
		return newSocketError(id, "unsupported", err.Error())
	}
	code := "internal"
	if errors.Is(err, websocket.ErrCloseSent) {
		code = "connection_closed"
	}
	return newSocketError(id, code, err.Error())
}

func newSocketError(id, code, message string) *socketErrorEnvelope {
	return newSocketErrorWithDetails(id, code, message, nil)
}

func newSocketErrorWithDetails(id, code, message string, details []FieldError) *socketErrorEnvelope {
	return &socketErrorEnvelope{
		Type:    "error",
		ID:      id,
		Code:    code,
		Message: message,
		Details: details,
	}
}

func unknownSocketAction(id, action string) *socketErrorEnvelope {
	return newSocketError(id, "not_found", "unknown action: "+action)
}
