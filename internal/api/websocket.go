package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gorilla/websocket"
)

const wsProtocolVersion = "gc.v1alpha1"

// maxWSMessageSize is the maximum allowed inbound WebSocket message (10 MB).
const maxWSMessageSize = 10 << 20

// RequestEnvelope is the client-to-server request message.
// This is an exported type — it IS the protocol contract. The AsyncAPI
// spec is generated directly from this struct via reflection.
type RequestEnvelope struct {
	Type           string          `json:"type" description:"Must be 'request'"`
	ID             string          `json:"id" description:"Client-assigned correlation ID"`
	Action         string          `json:"action" description:"Dotted action name (e.g. 'beads.list')"`
	IdempotencyKey string          `json:"idempotency_key,omitempty" description:"Deduplication key for mutation replay"`
	Scope          *Scope          `json:"scope,omitempty" description:"City targeting for supervisor connections"`
	Payload        json.RawMessage `json:"payload,omitempty" description:"Action-specific request payload"`
	Watch          *WatchParams    `json:"watch,omitempty" description:"Blocking query parameters"`

	// Framework-internal fields (not serialized).
	dispatchCtx   context.Context `json:"-"`
	dispatchIndex uint64          `json:"-"`
}

// WatchParams provides blocking query semantics over WebSocket.
type WatchParams struct {
	Index uint64 `json:"index" description:"Block until server index exceeds this value"`
	Wait  string `json:"wait,omitempty" description:"Maximum wait duration (e.g. '30s')"`
}

// Scope targets a specific city on supervisor connections.
type Scope struct {
	City string `json:"city,omitempty" description:"City name for supervisor-scoped requests"`
}

// HelloEnvelope is sent by the server immediately after WebSocket upgrade.
type HelloEnvelope struct {
	Type              string   `json:"type" description:"Must be 'hello'"`
	Protocol          string   `json:"protocol" description:"Protocol version (e.g. 'gc.v1alpha1')"`
	ServerRole        string   `json:"server_role" description:"'city' or 'supervisor'"`
	ReadOnly          bool     `json:"read_only" description:"True if mutations are disabled"`
	Capabilities      []string `json:"capabilities" description:"Sorted list of supported action names"`
	SubscriptionKinds []string `json:"subscription_kinds,omitempty" description:"Supported subscription types"`
}

// ResponseEnvelope is the server-to-client response for a successful action.
type ResponseEnvelope struct {
	Type   string `json:"type" description:"Must be 'response'"`
	ID     string `json:"id" description:"Correlation ID matching the request"`
	Index  uint64 `json:"index,omitempty" description:"Server event index for watch semantics"`
	Result any    `json:"result,omitempty" description:"Action-specific response payload"`
}

// ErrorEnvelope is sent by the server when a request fails.
type ErrorEnvelope struct {
	Type    string       `json:"type" description:"Must be 'error'"`
	ID      string       `json:"id,omitempty" description:"Correlation ID (empty for connection-level errors)"`
	Code    string       `json:"code" description:"Machine-readable error code"`
	Message string       `json:"message" description:"Human-readable error message"`
	Details []FieldError `json:"details,omitempty" description:"Per-field validation errors"`
}

// Backward-compatible aliases for internal code.
type socketRequestEnvelope = RequestEnvelope
type socketWatchParams = WatchParams
type socketScope = Scope
type socketHelloEnvelope = HelloEnvelope
type socketResponseEnvelope = ResponseEnvelope
type socketErrorEnvelope = ErrorEnvelope

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
	// Protected by closeMu since dispatch goroutines may set these on panic.
	var closeMu sync.Mutex
	closeCode := websocket.CloseNormalClosure
	closeText := ""
	defer func() {
		closeMu.Lock()
		code, text := closeCode, closeText
		closeMu.Unlock()
		_ = sc.writeClose(code, text)
		log.Printf("api: ws disconnected remote=%s close_code=%d", r.RemoteAddr, code)
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
				telemetry.RecordWebSocketRequest(context.Background(), reqCopy.Action, apiErr.Code, float64(dur.Milliseconds()))
				if err := sc.writeJSON(apiErr); err != nil {
					return
				}
				continue
			}
			log.Printf("api: ws req id=%s action=%s latency=%s ok", reqCopy.ID, reqCopy.Action, dur.Round(time.Microsecond))
			telemetry.RecordWebSocketRequest(context.Background(), reqCopy.Action, "", float64(dur.Milliseconds()))
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
				telemetry.RecordWebSocketRequest(context.Background(), reqCopy.Action, apiErr.Code, float64(dur.Milliseconds()))
				if err := sc.writeJSON(apiErr); err != nil {
					return
				}
				continue
			}
			log.Printf("api: ws req id=%s action=%s latency=%s ok", reqCopy.ID, reqCopy.Action, dur.Round(time.Microsecond))
			telemetry.RecordWebSocketRequest(context.Background(), reqCopy.Action, "", float64(dur.Milliseconds()))
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
						closeMu.Lock()
						closeCode = websocket.CloseInternalServerErr // 1011
						closeText = "internal server error"
						closeMu.Unlock()
						ss.cancel()
					}
				}()
				reqCopy.dispatchCtx = ss.ctx
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
	// Marshal outside the lock to avoid holding the mutex during serialization.
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if len(data) > maxWSOutboundSize {
		log.Printf("api: ws outbound message too large (%d bytes) for req %s, sending error", len(data), reqID)
		return sc.writeJSON(socketErrorEnvelope{
			Type:    "error",
			ID:      reqID,
			Code:    "message_too_large",
			Message: "response exceeds maximum message size",
		})
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
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
		Type:              "hello",
		Protocol:          wsProtocolVersion,
		ServerRole:        "city",
		ReadOnly:          s.readOnly,
		Capabilities:      actionTableCapabilities(),
		SubscriptionKinds: []string{"events", "session.stream"},
	}
}

func (sm *SupervisorMux) socketHello() socketHelloEnvelope {
	caps := actionTableCapabilities()
	// Supervisor also supports cities.list (not a per-city action).
	hasCities := false
	for _, c := range caps {
		if c == "cities.list" {
			hasCities = true
			break
		}
	}
	if !hasCities {
		caps = append(caps, "cities.list")
		sort.Strings(caps)
	}
	return socketHelloEnvelope{
		Type:              "hello",
		Protocol:          wsProtocolVersion,
		ServerRole:        "supervisor",
		ReadOnly:          sm.readOnly,
		Capabilities:      caps,
		SubscriptionKinds: []string{"events", "session.stream"},
	}
}

func (s *Server) handleSocketRequest(req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope) {
	return s.dispatchAction(req)
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
	return actionTableRequiresCityScope(action)
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
	return actionTableSupportsWatch(action)
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
