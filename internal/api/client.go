package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workspacesvc"
	"github.com/gorilla/websocket"
)

// connError wraps transport-level errors (connection refused, timeout, etc.)
// to distinguish them from API-level error responses.
type connError struct {
	err error
}

func (e *connError) Error() string { return e.err.Error() }
func (e *connError) Unwrap() error { return e.err }

// IsConnError reports whether err is a transport-level connection failure
// (e.g., connection refused, timeout) rather than an API-level error response.
func IsConnError(err error) bool {
	var ce *connError
	return errors.As(err, &ce)
}

// readOnlyError indicates the API server rejected a mutation because it's
// running in read-only mode (non-localhost bind).
type readOnlyError struct {
	msg string
}

func (e *readOnlyError) Error() string { return e.msg }

// wsClientResult carries either a response or an error from the background
// reader to the waiting request goroutine.
type wsClientResult struct {
	resp socketClientResponseEnvelope
	err  error
}

// Client is a WebSocket client for the Gas City API server.
// All API operations go through the persistent WebSocket connection.
// The client auto-reconnects with exponential backoff on failure.
type Client struct {
	baseURL     string
	scopePrefix string
	socketScope *socketScope
	httpClient  *http.Client // retained for health/readiness probes only
	wsMu        sync.Mutex
	wsConn      *websocket.Conn
	wsFailCount int
	wsBackoff   time.Time // don't attempt WS before this time
	wsClosed    bool
	nextReqID   uint64
	// Concurrent WebSocket transport.
	wsReaderDone chan struct{}
	pending      sync.Map // map[string]chan wsClientResult
	// Subscriptions: routing event frames to callbacks.
	subMu           sync.Mutex
	subs            map[string]*clientSubscription // stable client ID -> subscription state
	subServerIndex  map[string]string              // server subscription ID -> stable client ID
	eventBuf        map[string][]SubscriptionEvent // buffered by server subscription ID
	reconnectActive bool
}

type clientSubscription struct {
	id         string
	serverID   string
	scope      *socketScope
	payload    map[string]any
	callback   func(SubscriptionEvent)
	ctx        context.Context
	lastIndex  uint64
	lastCursor string
}

// SessionSubmitResponse mirrors POST /v0/session/{id}/submit.
type SessionSubmitResponse struct {
	Status string               `json:"status"`
	ID     string               `json:"id"`
	Queued bool                 `json:"queued"`
	Intent session.SubmitIntent `json:"intent"`
}

// NewClient creates a new API client targeting the given base URL
// (e.g., "http://127.0.0.1:8080").
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		subs:           make(map[string]*clientSubscription),
		subServerIndex: make(map[string]string),
		eventBuf:       make(map[string][]SubscriptionEvent),
	}
}

// NewCityScopedClient creates a client that routes requests through the
// supervisor's city-scoped API namespace for the given city name.
func NewCityScopedClient(baseURL, cityName string) *Client {
	c := NewClient(baseURL)
	c.scopePrefix = "/v0/city/" + escapeName(cityName)
	c.socketScope = &socketScope{City: cityName}
	return c
}

// ListCities fetches the current set of cities managed by the supervisor.
func (c *Client) ListCities() ([]CityInfo, error) {
	var resp struct {
		Items []CityInfo `json:"items"`
	}
	if _, err := c.doSocketJSON("cities.list", nil, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// ListServices fetches the current workspace service statuses.
func (c *Client) ListServices() ([]workspacesvc.Status, error) {
	var resp struct {
		Items []workspacesvc.Status `json:"items"`
	}
	if _, err := c.doSocketJSON("services.list", nil, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// GetService fetches one current workspace service status.
func (c *Client) GetService(name string) (workspacesvc.Status, error) {
	var resp workspacesvc.Status
	if _, err := c.doSocketJSON("service.get", nil, map[string]any{"name": name}, &resp); err != nil {
		return workspacesvc.Status{}, err
	}
	return resp, nil
}

// RestartService restarts a service.
func (c *Client) RestartService(name string) error {
	_, err := c.doSocketJSON("service.restart", nil, map[string]any{"name": name}, nil)
	return err
}

// SuspendCity suspends the city via PATCH /v0/city.
func (c *Client) SuspendCity() error {
	return c.patchCity(true)
}

// ResumeCity resumes the city via PATCH /v0/city.
func (c *Client) ResumeCity() error {
	return c.patchCity(false)
}

func (c *Client) patchCity(suspend bool) error {
	_, err := c.doSocketJSON("city.patch", nil, map[string]any{"suspended": suspend}, nil)
	return err
}

// SuspendAgent suspends an agent.
func (c *Client) SuspendAgent(name string) error {
	_, err := c.doSocketJSON("agent.suspend", nil, map[string]any{"name": name}, nil)
	return err
}

// ResumeAgent resumes a suspended agent.
func (c *Client) ResumeAgent(name string) error {
	_, err := c.doSocketJSON("agent.resume", nil, map[string]any{"name": name}, nil)
	return err
}

// SuspendRig suspends a rig.
func (c *Client) SuspendRig(name string) error {
	_, err := c.doSocketJSON("rig.suspend", nil, map[string]any{"name": name}, nil)
	return err
}

// ResumeRig resumes a suspended rig.
func (c *Client) ResumeRig(name string) error {
	_, err := c.doSocketJSON("rig.resume", nil, map[string]any{"name": name}, nil)
	return err
}

// RestartRig restarts a rig. Kills all agents; the reconciler restarts them.
func (c *Client) RestartRig(name string) error {
	_, err := c.doSocketJSON("rig.restart", nil, map[string]any{"name": name}, nil)
	return err
}

// KillSession force-kills a session.
func (c *Client) KillSession(id string) error {
	_, err := c.doSocketJSON("session.kill", nil, map[string]any{"id": id}, nil)
	return err
}

// SubmitSession sends a semantic submit request to a session.
// The id may be either a bead ID or a resolvable session alias/name.
func (c *Client) SubmitSession(id, message string, intent session.SubmitIntent) (SessionSubmitResponse, error) {
	payload := map[string]any{
		"id":      id,
		"message": message,
	}
	if intent != "" {
		payload["intent"] = intent
	}
	var resp SessionSubmitResponse
	if _, err := c.doSocketJSON("session.submit", nil, payload, &resp); err != nil {
		return SessionSubmitResponse{}, err
	}
	return resp, nil
}

// escapeName escapes each segment of a potentially qualified name (e.g.,
// "myrig/worker") for use in URL paths. Slashes are preserved as path
// separators; other URL metacharacters (#, ?, etc.) are percent-encoded.
func escapeName(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func unescapeName(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		unescaped, err := url.PathUnescape(p)
		if err == nil {
			parts[i] = unescaped
		}
	}
	return strings.Join(parts, "/")
}

func (c *Client) doSocketJSON(action string, scope *socketScope, payload any, out any) (bool, error) {
	resp, handled, err := c.doSocketRequest(action, c.effectiveSocketScope(scope), payload)
	if !handled || err != nil {
		return handled, err
	}
	if out == nil || len(resp.Result) == 0 {
		return true, nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return true, fmt.Errorf("decode websocket response: %w", err)
	}
	return true, nil
}

func (c *Client) doSocketRaw(action string, scope *socketScope, payload any) ([]byte, bool, error) {
	resp, handled, err := c.doSocketRequest(action, c.effectiveSocketScope(scope), payload)
	if !handled || err != nil {
		return nil, handled, err
	}
	return append([]byte(nil), resp.Result...), true, nil
}

type socketClientResponseEnvelope struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Index  uint64          `json:"index,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// wsBackoffDuration returns the backoff duration for the given failure count.
func wsBackoffDuration(failCount int) time.Duration {
	d := time.Second
	for i := 1; i < failCount && d < 30*time.Second; i++ {
		d *= 2
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// Close shuts down the WebSocket connection and waits for the reader to exit.
func (c *Client) Close() {
	c.wsMu.Lock()
	conn := c.wsConn
	done := c.wsReaderDone
	c.wsConn = nil
	c.wsClosed = true
	c.wsMu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	// Wait for the reader goroutine to finish AFTER releasing wsMu,
	// since wsReadLoop acquires wsMu on connection death.
	if done != nil {
		<-done
	}
}

// SubscriptionEvent represents an event received via a WebSocket subscription.
type SubscriptionEvent struct {
	SubscriptionID string          `json:"subscription_id"`
	EventType      string          `json:"event_type"`
	Index          uint64          `json:"index,omitempty"`
	Cursor         string          `json:"cursor,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

// SubscribeEvents starts an event subscription and delivers events to the
// callback until ctx is cancelled or Unsubscribe is called. Returns the
// subscription ID assigned by the server.
func (c *Client) SubscribeEvents(ctx context.Context, afterSeq uint64, callback func(SubscriptionEvent)) (string, error) {
	payload := map[string]any{"kind": "events"}
	if afterSeq > 0 {
		payload["after_seq"] = afterSeq
	}
	return c.startSubscription(ctx, payload, callback)
}

// SubscribeSessionStream starts a session stream subscription and delivers
// events to the callback. The target identifies the session (bead ID or name).
// Format is optional ("text", "jsonl", etc.). Turns controls how many recent
// turns to replay (0 = all).
func (c *Client) SubscribeSessionStream(ctx context.Context, target, format string, turns int, callback func(SubscriptionEvent)) (string, error) {
	payload := map[string]any{
		"kind":   "session.stream",
		"target": target,
	}
	if format != "" {
		payload["format"] = format
	}
	if turns > 0 {
		payload["turns"] = turns
	}
	return c.startSubscription(ctx, payload, callback)
}

func (c *Client) startSubscription(ctx context.Context, payload map[string]any, callback func(SubscriptionEvent)) (string, error) {
	serverID, used, err := c.startSubscriptionOnSocket(c.effectiveSocketScope(nil), payload)
	if err != nil {
		return "", err
	}
	if !used {
		return "", fmt.Errorf("websocket not available for subscriptions")
	}
	sub := &clientSubscription{
		id:       serverID,
		serverID: serverID,
		scope:    cloneSocketScope(c.effectiveSocketScope(nil)),
		payload:  cloneAnyMap(payload),
		callback: callback,
		ctx:      ctx,
	}
	buffered := c.registerSubscription(sub)

	go func() {
		<-ctx.Done()
		_ = c.unsubscribeSubscription(sub.id, true)
	}()

	for _, evt := range buffered {
		callback(evt)
	}

	return sub.id, nil
}

// Unsubscribe stops a subscription by ID.
func (c *Client) Unsubscribe(subscriptionID string) error {
	return c.unsubscribeSubscription(subscriptionID, false)
}

func (c *Client) unsubscribeSubscription(subscriptionID string, bestEffort bool) error {
	sub, serverID := c.removeSubscription(subscriptionID)
	if sub == nil {
		if bestEffort {
			return nil
		}
		return fmt.Errorf("subscription not found: %s", subscriptionID)
	}
	if serverID == "" {
		return nil
	}
	_, err := c.stopSubscriptionOnSocket(serverID)
	if bestEffort && IsConnError(err) {
		return nil
	}
	return err
}

func (c *Client) registerSubscription(sub *clientSubscription) []SubscriptionEvent {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	c.subs[sub.id] = sub
	if sub.serverID != "" {
		c.subServerIndex[sub.serverID] = sub.id
	}
	buffered := c.takeBufferedEventsLocked(sub.serverID)
	for i := range buffered {
		buffered[i].SubscriptionID = sub.id
		c.updateSubscriptionCursorLocked(sub, buffered[i])
	}
	return buffered
}

func (c *Client) removeSubscription(subscriptionID string) (*clientSubscription, string) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	sub := c.subs[subscriptionID]
	if sub == nil {
		return nil, ""
	}
	delete(c.subs, subscriptionID)
	serverID := sub.serverID
	if serverID != "" {
		delete(c.subServerIndex, serverID)
		delete(c.eventBuf, serverID)
	}
	sub.serverID = ""
	return sub, serverID
}

func (c *Client) startSubscriptionOnSocket(scope *socketScope, payload map[string]any) (string, bool, error) {
	var resp struct {
		SubscriptionID string `json:"subscription_id"`
	}
	used, err := c.doSocketJSON("subscription.start", scope, payload, &resp)
	if err != nil {
		return "", used, err
	}
	if resp.SubscriptionID == "" {
		return "", used, fmt.Errorf("server returned empty subscription_id")
	}
	return resp.SubscriptionID, used, nil
}

func (c *Client) stopSubscriptionOnSocket(subscriptionID string) (bool, error) {
	return c.doSocketJSON("subscription.stop", nil, map[string]any{
		"subscription_id": subscriptionID,
	}, nil)
}

func (c *Client) doSocketRequest(action string, scope *socketScope, payload any) (socketClientResponseEnvelope, bool, error) {
	c.wsMu.Lock()
	if c.wsClosed {
		c.wsMu.Unlock()
		return socketClientResponseEnvelope{}, true, &connError{err: fmt.Errorf("websocket client closed")}
	}

	// Backoff: if we've failed recently, return error (no HTTP fallback).
	if !c.wsBackoff.IsZero() && time.Now().Before(c.wsBackoff) {
		c.wsMu.Unlock()
		return socketClientResponseEnvelope{}, true, &connError{err: fmt.Errorf("websocket in backoff (next retry in %s)", time.Until(c.wsBackoff).Truncate(time.Millisecond))}
	}

	if err := c.ensureWSConnLocked(); err != nil {
		c.wsFailCount++
		c.wsBackoff = time.Now().Add(wsBackoffDuration(c.wsFailCount))
		c.wsMu.Unlock()
		log.Printf("api: ws connect failed (attempt %d, backoff %s): %v", c.wsFailCount, wsBackoffDuration(c.wsFailCount), err)
		return socketClientResponseEnvelope{}, true, &connError{err: fmt.Errorf("websocket connect failed: %w", err)}
	}
	// Successful connection — reset backoff.
	c.wsFailCount = 0
	c.wsBackoff = time.Time{}

	c.nextReqID++
	reqID := fmt.Sprintf("cli-%d", c.nextReqID)
	req := socketRequestEnvelope{
		Type:   "request",
		ID:     reqID,
		Action: action,
		Scope:  scope,
	}
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			c.wsMu.Unlock()
			return socketClientResponseEnvelope{}, true, fmt.Errorf("marshal websocket payload: %w", err)
		}
		req.Payload = data
	}

	// Register pending channel before writing so the reader can route the response.
	ch := make(chan wsClientResult, 1)
	c.pending.Store(reqID, ch)

	if err := c.wsConn.WriteJSON(req); err != nil {
		c.pending.Delete(reqID)
		_ = c.wsConn.Close()
		c.wsConn = nil
		c.wsFailCount++
		c.wsBackoff = time.Now().Add(wsBackoffDuration(c.wsFailCount))
		c.wsMu.Unlock()
		return socketClientResponseEnvelope{}, true, &connError{err: fmt.Errorf("websocket write failed: %w", err)}
	}

	// Unlock immediately after write — the background reader will route the response.
	c.wsMu.Unlock()

	// Wait for correlated response with timeout.
	select {
	case result := <-ch:
		if result.err != nil {
			return socketClientResponseEnvelope{}, true, result.err
		}
		return result.resp, true, nil
	case <-time.After(30 * time.Second):
		c.pending.Delete(reqID)
		return socketClientResponseEnvelope{}, true, &connError{err: fmt.Errorf("websocket request timeout")}
	}
}

// wsReadLoop is the background reader goroutine. It reads all incoming
// messages and dispatches responses/errors to the appropriate pending
// request channel by ID. The conn parameter is captured at launch time
// so the loop is safe from concurrent Close() setting c.wsConn to nil.
func (c *Client) wsReadLoop(conn *websocket.Conn) {
	defer close(c.wsReaderDone)
	for {
		_, rawBytes, err := conn.ReadMessage()
		if err != nil {
			// Connection died — notify all pending requests.
			connErr := &connError{err: fmt.Errorf("websocket read failed: %w", err)}
			c.pending.Range(func(key, val any) bool {
				ch := val.(chan wsClientResult)
				select {
				case ch <- wsClientResult{err: connErr}:
				default:
				}
				return true
			})
			c.wsMu.Lock()
			// Only clear wsConn if this reader's connection is still the active one.
			// A concurrent reconnection may have already replaced it.
			if c.wsConn == conn {
				c.wsConn = nil
				c.wsFailCount++
				c.wsBackoff = time.Now().Add(wsBackoffDuration(c.wsFailCount))
			}
			c.wsMu.Unlock()
			c.handleDisconnectedSubscriptions()
			return
		}

		// Extract the message type with a minimal partial unmarshal.
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(rawBytes, &envelope); err != nil {
			log.Printf("api: ws client: invalid envelope: %v", err)
			continue
		}

		switch envelope.Type {
		case "response":
			var resp socketClientResponseEnvelope
			if err := json.Unmarshal(rawBytes, &resp); err != nil {
				log.Printf("api: ws client: invalid response frame: %v", err)
				continue
			}
			if val, ok := c.pending.LoadAndDelete(resp.ID); ok {
				val.(chan wsClientResult) <- wsClientResult{resp: resp}
			}
		case "error":
			var resp socketErrorEnvelope
			if err := json.Unmarshal(rawBytes, &resp); err != nil {
				log.Printf("api: ws client: invalid error frame: %v", err)
				continue
			}
			goErr := wsSocketErrorToGoError(resp)
			if val, ok := c.pending.LoadAndDelete(resp.ID); ok {
				val.(chan wsClientResult) <- wsClientResult{err: goErr}
			}
		case "event":
			var evt SubscriptionEvent
			if err := json.Unmarshal(rawBytes, &evt); err != nil {
				log.Printf("api: ws client: invalid event frame: %v", err)
				continue
			}
			c.routeSubscriptionEvent(evt)
		default:
			// Ignore unknown message types (e.g., pings handled by gorilla).
		}
	}
}

// wsSocketErrorToGoError converts a WebSocket error envelope to a Go error.
func wsSocketErrorToGoError(resp socketErrorEnvelope) error {
	if resp.Code == "read_only" {
		msg := resp.Message
		if msg == "" {
			msg = "mutations disabled (read-only server)"
		}
		return &readOnlyError{msg: msg}
	}
	if resp.Message != "" {
		return fmt.Errorf("API error: %s", resp.Message)
	}
	if resp.Code != "" {
		return fmt.Errorf("API error: %s", resp.Code)
	}
	return fmt.Errorf("API error")
}

func (c *Client) ensureWSConnLocked() error {
	if c.wsClosed {
		return fmt.Errorf("websocket client closed")
	}
	if c.wsConn != nil {
		return nil
	}
	wsURL, err := websocketURLForBase(c.baseURL)
	if err != nil {
		return err
	}
	header := http.Header{}
	header.Set("Origin", "http://localhost")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("websocket handshake failed: %s", resp.Status)
		}
		return err
	}
	var hello socketHelloEnvelope
	if err := conn.ReadJSON(&hello); err != nil {
		_ = conn.Close()
		return err
	}
	if hello.Type != "hello" {
		_ = conn.Close()
		return fmt.Errorf("unexpected websocket hello type: %s", hello.Type)
	}
	c.wsConn = conn
	c.wsReaderDone = make(chan struct{})
	go c.wsReadLoop(conn)
	return nil
}

func websocketURLForBase(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base url: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported base url scheme: %s", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v0/ws"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func (c *Client) urlForPath(path string) string {
	if c.scopePrefix != "" && strings.HasPrefix(path, "/v0/") {
		return c.baseURL + c.scopePrefix + strings.TrimPrefix(path, "/v0")
	}
	return c.baseURL + path
}

func (c *Client) effectiveSocketScope(scope *socketScope) *socketScope {
	if scope != nil {
		return scope
	}
	return c.socketScope
}

func (c *Client) routeSubscriptionEvent(evt SubscriptionEvent) {
	cb, routed, shouldBuffer := c.routeSubscriptionEventLocked(evt)
	if shouldBuffer || cb == nil {
		return
	}
	cb(routed)
}

func (c *Client) routeSubscriptionEventLocked(evt SubscriptionEvent) (func(SubscriptionEvent), SubscriptionEvent, bool) {
	c.subMu.Lock()
	defer c.subMu.Unlock()

	stableID, ok := c.subServerIndex[evt.SubscriptionID]
	if !ok {
		const maxEventBufPerSubscription = 128
		buf := append(c.eventBuf[evt.SubscriptionID], evt)
		if len(buf) > maxEventBufPerSubscription {
			buf = buf[len(buf)-maxEventBufPerSubscription:]
		}
		c.eventBuf[evt.SubscriptionID] = buf
		return nil, SubscriptionEvent{}, true
	}

	sub := c.subs[stableID]
	if sub == nil {
		delete(c.subServerIndex, evt.SubscriptionID)
		return nil, SubscriptionEvent{}, true
	}
	c.updateSubscriptionCursorLocked(sub, evt)
	evt.SubscriptionID = stableID
	return sub.callback, evt, false
}

func (c *Client) updateSubscriptionCursorLocked(sub *clientSubscription, evt SubscriptionEvent) {
	if evt.Index > 0 {
		sub.lastIndex = evt.Index
	}
	if evt.Cursor != "" {
		sub.lastCursor = evt.Cursor
	}
}

func (c *Client) takeBufferedEventsLocked(serverID string) []SubscriptionEvent {
	if serverID == "" {
		return nil
	}
	buffered := append([]SubscriptionEvent(nil), c.eventBuf[serverID]...)
	delete(c.eventBuf, serverID)
	return buffered
}

func (c *Client) handleDisconnectedSubscriptions() {
	c.subMu.Lock()
	if len(c.subs) == 0 || c.wsClosed {
		c.subServerIndex = make(map[string]string)
		c.eventBuf = make(map[string][]SubscriptionEvent)
		c.subMu.Unlock()
		return
	}
	for _, sub := range c.subs {
		sub.serverID = ""
	}
	c.subServerIndex = make(map[string]string)
	c.eventBuf = make(map[string][]SubscriptionEvent)
	if c.reconnectActive {
		c.subMu.Unlock()
		return
	}
	c.reconnectActive = true
	c.subMu.Unlock()

	go c.reconnectSubscriptionsLoop()
}

func (c *Client) reconnectSubscriptionsLoop() {
	defer func() {
		c.subMu.Lock()
		c.reconnectActive = false
		c.subMu.Unlock()
	}()

	for {
		if c.isClosed() {
			return
		}
		snapshot := c.subscriptionSnapshot()
		if len(snapshot) == 0 {
			return
		}
		if !c.waitForReconnectBackoff() {
			return
		}
		if err := c.reconnectWS(); err != nil {
			continue
		}
		if c.resubscribeAll(snapshot) {
			return
		}
	}
}

func (c *Client) isClosed() bool {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	return c.wsClosed
}

func (c *Client) waitForReconnectBackoff() bool {
	for {
		c.wsMu.Lock()
		if c.wsClosed {
			c.wsMu.Unlock()
			return false
		}
		wait := time.Until(c.wsBackoff)
		c.wsMu.Unlock()
		if wait <= 0 {
			return true
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		}
	}
}

func (c *Client) reconnectWS() error {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()

	if c.wsClosed {
		return fmt.Errorf("websocket client closed")
	}
	if c.wsConn != nil {
		return nil
	}
	if err := c.ensureWSConnLocked(); err != nil {
		c.wsFailCount++
		c.wsBackoff = time.Now().Add(wsBackoffDuration(c.wsFailCount))
		log.Printf("api: ws reconnect failed (attempt %d, backoff %s): %v", c.wsFailCount, wsBackoffDuration(c.wsFailCount), err)
		return err
	}
	c.wsFailCount = 0
	c.wsBackoff = time.Time{}
	return nil
}

func (c *Client) resubscribeAll(snapshot []*clientSubscription) bool {
	for _, sub := range snapshot {
		if !c.subscriptionStillActive(sub.id, sub.ctx) {
			continue
		}
		payload := c.resumePayload(sub)
		serverID, _, err := c.startSubscriptionOnSocket(sub.scope, payload)
		if err != nil {
			if IsConnError(err) {
				return false
			}
			log.Printf("api: ws resubscribe failed id=%s kind=%v: %v", sub.id, payload["kind"], err)
			return false
		}
		buffered, current := c.remapSubscription(sub.id, serverID)
		if !current {
			_, _ = c.stopSubscriptionOnSocket(serverID)
			continue
		}
		for _, evt := range buffered {
			sub.callback(evt)
		}
	}
	return true
}

func (c *Client) subscriptionSnapshot() []*clientSubscription {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	snapshot := make([]*clientSubscription, 0, len(c.subs))
	for _, sub := range c.subs {
		snapshot = append(snapshot, &clientSubscription{
			id:         sub.id,
			serverID:   sub.serverID,
			scope:      cloneSocketScope(sub.scope),
			payload:    cloneAnyMap(sub.payload),
			callback:   sub.callback,
			ctx:        sub.ctx,
			lastIndex:  sub.lastIndex,
			lastCursor: sub.lastCursor,
		})
	}
	return snapshot
}

func (c *Client) subscriptionStillActive(subscriptionID string, ctx context.Context) bool {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return false
		default:
		}
	}
	c.subMu.Lock()
	defer c.subMu.Unlock()
	_, ok := c.subs[subscriptionID]
	return ok
}

func (c *Client) resumePayload(sub *clientSubscription) map[string]any {
	payload := cloneAnyMap(sub.payload)
	switch payload["kind"] {
	case "events":
		if sub.lastCursor != "" {
			payload["after_cursor"] = sub.lastCursor
			delete(payload, "after_seq")
		} else if sub.lastIndex > 0 {
			payload["after_seq"] = sub.lastIndex
		}
	case "session.stream":
		if sub.lastCursor != "" {
			payload["after_cursor"] = sub.lastCursor
			delete(payload, "after_seq")
		}
	}
	return payload
}

func (c *Client) remapSubscription(subscriptionID, serverID string) ([]SubscriptionEvent, bool) {
	c.subMu.Lock()
	defer c.subMu.Unlock()

	sub := c.subs[subscriptionID]
	if sub == nil {
		return nil, false
	}
	if sub.ctx != nil {
		select {
		case <-sub.ctx.Done():
			return nil, false
		default:
		}
	}
	if sub.serverID != "" {
		delete(c.subServerIndex, sub.serverID)
	}
	sub.serverID = serverID
	c.subServerIndex[serverID] = subscriptionID
	buffered := c.takeBufferedEventsLocked(serverID)
	for i := range buffered {
		buffered[i].SubscriptionID = subscriptionID
		c.updateSubscriptionCursorLocked(sub, buffered[i])
	}
	return buffered, true
}

func cloneAnyMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func cloneSocketScope(scope *socketScope) *socketScope {
	if scope == nil {
		return nil
	}
	clone := *scope
	return &clone
}
