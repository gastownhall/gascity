package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
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

// ShouldFallback reports whether err indicates the CLI should fall back to
// direct file mutation. This is true for transport-level failures (connection
// refused, timeout) and for read-only API rejections (server bound to
// non-localhost, mutations disabled).
func ShouldFallback(err error) bool {
	if IsConnError(err) {
		return true
	}
	var ro *readOnlyError
	return errors.As(err, &ro)
}

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
	nextReqID   uint64
	// Concurrent WebSocket transport.
	wsReaderDone  chan struct{}
	pending       sync.Map // map[string]chan wsClientResult
	// Subscriptions: routing event frames to callbacks.
	subMu    sync.Mutex
	subs     map[string]func(SubscriptionEvent)
	eventBuf []SubscriptionEvent // buffered events for not-yet-registered subscriptions
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

// GetJSON fetches a JSON resource and returns the raw response body.
// Routes are mapped to WS actions via socketGetAction.
func (c *Client) GetJSON(path string) ([]byte, error) {
	action, payload, ok := socketGetAction(path)
	if !ok {
		return nil, fmt.Errorf("no WS action for path: %s", path)
	}
	raw, _, err := c.doSocketRaw(action, nil, payload)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// PostJSON sends a JSON mutation request and returns the raw response body.
// Routes are mapped to WS actions via socketPostAction.
func (c *Client) PostJSON(path string, payload any) ([]byte, error) {
	action, socketPayload, ok, err := socketPostAction(path, payload)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no WS action for path: %s", path)
	}
	raw, _, err := c.doSocketRaw(action, nil, socketPayload)
	if err != nil {
		return nil, err
	}
	return raw, nil
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

// doMutation sends a mutation request and checks for errors.
func (c *Client) doMutation(method, path string, body any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.urlForPath(path), bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-GC-Request", "true")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	// Parse error response.
	var apiErr struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil {
		return fmt.Errorf("API returned %d", resp.StatusCode)
	}

	// Read-only rejection: server is bound non-localhost and rejects mutations.
	// CLI should fall back to direct file mutation.
	if apiErr.Error == "read_only" {
		msg := apiErr.Message
		if msg == "" {
			msg = "mutations disabled (read-only server)"
		}
		return &readOnlyError{msg: msg}
	}

	if apiErr.Message != "" {
		return fmt.Errorf("API error: %s", apiErr.Message)
	}
	if apiErr.Error != "" {
		return fmt.Errorf("API error: %s", apiErr.Error)
	}
	return fmt.Errorf("API returned %d", resp.StatusCode)
}

func (c *Client) doGet(path string, out any) error {
	body, err := c.doGetRaw(path)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) doGetRaw(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.urlForPath(path), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("read response: %w", readErr)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}

	var apiErr struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &apiErr); err != nil {
		return nil, fmt.Errorf("API returned %d", resp.StatusCode)
	}
	if apiErr.Message != "" {
		return nil, fmt.Errorf("API error: %s", apiErr.Message)
	}
	if apiErr.Error != "" {
		return nil, fmt.Errorf("API error: %s", apiErr.Error)
	}
	return nil, fmt.Errorf("API returned %d", resp.StatusCode)
}

func (c *Client) doPostJSON(path string, body any, out any) error {
	raw, err := c.doPostRaw(path, body)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) doPostRaw(path string, body any) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(http.MethodPost, c.urlForPath(path), bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-GC-Request", "true")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("read response: %w", readErr)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}

	var apiErr struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &apiErr); err != nil {
		return nil, fmt.Errorf("API returned %d", resp.StatusCode)
	}

	if apiErr.Error == "read_only" {
		msg := apiErr.Message
		if msg == "" {
			msg = "mutations disabled (read-only server)"
		}
		return nil, &readOnlyError{msg: msg}
	}
	if apiErr.Message != "" {
		return nil, fmt.Errorf("API error: %s", apiErr.Message)
	}
	if apiErr.Error != "" {
		return nil, fmt.Errorf("API error: %s", apiErr.Error)
	}
	return nil, fmt.Errorf("API returned %d", resp.StatusCode)
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
	var resp struct {
		SubscriptionID string `json:"subscription_id"`
	}
	used, err := c.doSocketJSON("subscription.start", nil, payload, &resp)
	if err != nil {
		return "", err
	}
	if !used {
		return "", fmt.Errorf("websocket not available for subscriptions")
	}
	if resp.SubscriptionID == "" {
		return "", fmt.Errorf("server returned empty subscription_id")
	}

	// Register the callback under subMu and drain any buffered events
	// that arrived between the response and this registration.
	c.subMu.Lock()
	if c.subs == nil {
		c.subs = make(map[string]func(SubscriptionEvent))
	}
	c.subs[resp.SubscriptionID] = callback
	var kept []SubscriptionEvent
	for _, evt := range c.eventBuf {
		if evt.SubscriptionID == resp.SubscriptionID {
			callback(evt)
		} else {
			kept = append(kept, evt)
		}
	}
	c.eventBuf = kept
	c.subMu.Unlock()

	// Auto-cleanup when caller's ctx is cancelled.
	go func() {
		<-ctx.Done()
		c.subMu.Lock()
		delete(c.subs, resp.SubscriptionID)
		c.subMu.Unlock()
		// Best-effort server-side cleanup.
		_, _ = c.doSocketJSON("subscription.stop", nil, map[string]any{
			"subscription_id": resp.SubscriptionID,
		}, nil)
	}()

	return resp.SubscriptionID, nil
}

// Unsubscribe stops a subscription by ID.
func (c *Client) Unsubscribe(subscriptionID string) error {
	c.subMu.Lock()
	delete(c.subs, subscriptionID)
	c.subMu.Unlock()
	_, err := c.doSocketJSON("subscription.stop", nil, map[string]any{
		"subscription_id": subscriptionID,
	}, nil)
	return err
}

func (c *Client) doSocketRequest(action string, scope *socketScope, payload any) (socketClientResponseEnvelope, bool, error) {
	c.wsMu.Lock()

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
		var raw map[string]json.RawMessage
		if err := conn.ReadJSON(&raw); err != nil {
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
			c.wsConn = nil
			c.wsFailCount++
			c.wsBackoff = time.Now().Add(wsBackoffDuration(c.wsFailCount))
			c.wsMu.Unlock()
			return
		}

		var msgType string
		if t, ok := raw["type"]; ok {
			_ = json.Unmarshal(t, &msgType)
		}

		switch msgType {
		case "response":
			var resp socketClientResponseEnvelope
			if err := decodeRawMessage(raw, &resp); err != nil {
				continue
			}
			if val, ok := c.pending.LoadAndDelete(resp.ID); ok {
				val.(chan wsClientResult) <- wsClientResult{resp: resp}
			}
		case "error":
			var resp socketErrorEnvelope
			if err := decodeRawMessage(raw, &resp); err != nil {
				continue
			}
			goErr := wsSocketErrorToGoError(resp)
			if val, ok := c.pending.LoadAndDelete(resp.ID); ok {
				val.(chan wsClientResult) <- wsClientResult{err: goErr}
			}
		case "event":
			var evt SubscriptionEvent
			if err := decodeRawMessage(raw, &evt); err != nil {
				continue
			}
			c.subMu.Lock()
			if cb, ok := c.subs[evt.SubscriptionID]; ok {
				c.subMu.Unlock()
				cb(evt)
			} else {
				c.eventBuf = append(c.eventBuf, evt)
				c.subMu.Unlock()
			}
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

func decodeRawMessage(raw map[string]json.RawMessage, out any) error {
	data, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal websocket envelope: %w", err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode websocket envelope: %w", err)
	}
	return nil
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

func socketGetAction(path string) (string, any, bool) {
	u, err := url.Parse(path)
	if err != nil {
		return "", nil, false
	}
	switch {
	case u.Path == "/v0/city":
		return "city.get", nil, true
	case u.Path == "/v0/config":
		return "config.get", nil, true
	case u.Path == "/v0/config/explain":
		return "config.explain", nil, true
	case u.Path == "/v0/config/validate":
		return "config.validate", nil, true
	case u.Path == "/v0/packs":
		return "packs.list", nil, true
	case u.Path == "/v0/providers":
		payload := map[string]any{}
		if v := strings.TrimSpace(u.Query().Get("view")); v != "" {
			payload["view"] = v
		}
		return "providers.list", payload, true
	case strings.HasPrefix(u.Path, "/v0/provider/"):
		name := strings.TrimPrefix(u.Path, "/v0/provider/")
		if name == "" {
			return "", nil, false
		}
		return "provider.get", map[string]any{"name": name}, true
	case u.Path == "/v0/status":
		return "status.get", nil, true
	case u.Path == "/v0/agents":
		payload := map[string]any{}
		for _, key := range []string{"pool", "rig", "running"} {
			if v := strings.TrimSpace(u.Query().Get(key)); v != "" {
				payload[key] = v
			}
		}
		if u.Query().Get("peek") == "true" {
			payload["peek"] = true
		}
		return "agents.list", payload, true
	case strings.HasPrefix(u.Path, "/v0/agent/"):
		name := strings.TrimPrefix(u.Path, "/v0/agent/")
		if name == "" {
			return "", nil, false
		}
		return "agent.get", map[string]any{"name": name}, true
	case strings.HasPrefix(u.Path, "/v0/rig/"):
		name := strings.TrimPrefix(u.Path, "/v0/rig/")
		if name == "" {
			return "", nil, false
		}
		return "rig.get", map[string]any{"name": name}, true
	case u.Path == "/v0/services":
		return "services.list", nil, true
	case strings.HasPrefix(u.Path, "/v0/service/"):
		name := strings.TrimPrefix(u.Path, "/v0/service/")
		if name == "" {
			return "", nil, false
		}
		return "service.get", map[string]any{"name": name}, true
	case u.Path == "/v0/sessions":
		payload := map[string]any{}
		if v := strings.TrimSpace(u.Query().Get("state")); v != "" {
			payload["state"] = v
		}
		if v := strings.TrimSpace(u.Query().Get("template")); v != "" {
			payload["template"] = v
		}
		if u.Query().Get("peek") == "true" {
			payload["peek"] = true
		}
		applySocketPaginationQuery(payload, u.Query())
		return "sessions.list", payload, true
	case u.Path == "/v0/beads":
		payload := map[string]any{}
		for _, key := range []string{"status", "type", "label", "assignee", "rig"} {
			if v := strings.TrimSpace(u.Query().Get(key)); v != "" {
				payload[key] = v
			}
		}
		applySocketPaginationQuery(payload, u.Query())
		return "beads.list", payload, true
	case u.Path == "/v0/beads/ready":
		return "beads.ready", nil, true
	case strings.HasPrefix(u.Path, "/v0/beads/graph/"):
		rootID := strings.TrimPrefix(u.Path, "/v0/beads/graph/")
		if rootID == "" {
			return "", nil, false
		}
		return "beads.graph", map[string]any{"root_id": rootID}, true
	case strings.HasSuffix(u.Path, "/deps") && strings.HasPrefix(u.Path, "/v0/bead/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/bead/"), "/deps")
		if id == "" {
			return "", nil, false
		}
		return "bead.deps", map[string]any{"id": id}, true
	case strings.HasPrefix(u.Path, "/v0/bead/"):
		id := strings.TrimPrefix(u.Path, "/v0/bead/")
		if id == "" {
			return "", nil, false
		}
		return "bead.get", map[string]any{"id": id}, true
	case u.Path == "/v0/mail":
		payload := map[string]any{}
		for _, key := range []string{"agent", "status", "rig"} {
			if v := strings.TrimSpace(u.Query().Get(key)); v != "" {
				payload[key] = v
			}
		}
		applySocketPaginationQuery(payload, u.Query())
		return "mail.list", payload, true
	case u.Path == "/v0/mail/count":
		payload := map[string]any{}
		for _, key := range []string{"agent", "rig"} {
			if v := strings.TrimSpace(u.Query().Get(key)); v != "" {
				payload[key] = v
			}
		}
		return "mail.count", payload, true
	case strings.HasPrefix(u.Path, "/v0/mail/thread/"):
		id := strings.TrimPrefix(u.Path, "/v0/mail/thread/")
		if id == "" {
			return "", nil, false
		}
		payload := map[string]any{"id": id}
		if v := strings.TrimSpace(u.Query().Get("rig")); v != "" {
			payload["rig"] = v
		}
		return "mail.thread", payload, true
	case strings.HasPrefix(u.Path, "/v0/mail/"):
		id := strings.TrimPrefix(u.Path, "/v0/mail/")
		if id == "" {
			return "", nil, false
		}
		payload := map[string]any{"id": id}
		if v := strings.TrimSpace(u.Query().Get("rig")); v != "" {
			payload["rig"] = v
		}
		return "mail.get", payload, true
	case u.Path == "/v0/events":
		payload := map[string]any{}
		for _, key := range []string{"type", "actor", "since"} {
			if v := strings.TrimSpace(u.Query().Get(key)); v != "" {
				payload[key] = v
			}
		}
		applySocketPaginationQuery(payload, u.Query())
		return "events.list", payload, true
	case u.Path == "/v0/rigs":
		payload := map[string]any{}
		if u.Query().Get("git") == "true" {
			payload["git"] = true
		}
		return "rigs.list", payload, true
	case u.Path == "/v0/convoys":
		return "convoys.list", nil, true
	case strings.HasPrefix(u.Path, "/v0/convoy/"):
		id := strings.TrimPrefix(u.Path, "/v0/convoy/")
		if id == "" {
			return "", nil, false
		}
		return "convoy.get", map[string]any{"id": id}, true
	case strings.HasSuffix(u.Path, "/pending") && strings.HasPrefix(u.Path, "/v0/session/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/session/"), "/pending")
		if id == "" {
			return "", nil, false
		}
		return "session.pending", map[string]any{"id": id}, true
	case strings.HasSuffix(u.Path, "/transcript") && strings.HasPrefix(u.Path, "/v0/session/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/session/"), "/transcript")
		if id == "" {
			return "", nil, false
		}
		payload := map[string]any{"id": id}
		if v := strings.TrimSpace(u.Query().Get("before")); v != "" {
			payload["before"] = v
		}
		if v := strings.TrimSpace(u.Query().Get("format")); v != "" {
			payload["format"] = v
		}
		if v := strings.TrimSpace(u.Query().Get("tail")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return "", nil, false
			}
			payload["tail"] = n
		}
		return "session.transcript", payload, true
	case strings.HasPrefix(u.Path, "/v0/session/"):
		id := strings.TrimPrefix(u.Path, "/v0/session/")
		if id == "" || strings.Contains(id, "/") {
			return "", nil, false
		}
		payload := map[string]any{"id": id}
		if u.Query().Get("peek") == "true" {
			payload["peek"] = true
		}
		return "session.get", payload, true
	default:
		return "", nil, false
	}
}

func applySocketPaginationQuery(payload map[string]any, q url.Values) {
	if payload == nil {
		return
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			payload["limit"] = n
		}
	}
	if v := strings.TrimSpace(q.Get("cursor")); v != "" {
		payload["cursor"] = v
	}
}

func socketPostAction(path string, payload any) (string, any, bool, error) {
	u, err := url.Parse(path)
	if err != nil {
		return "", nil, false, nil
	}
	switch {
	case u.Path == "/v0/beads":
		merged, err := mergeSocketPayload(nil, payload)
		return "bead.create", merged, true, err
	case strings.HasSuffix(u.Path, "/close") && strings.HasPrefix(u.Path, "/v0/bead/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/bead/"), "/close")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "bead.close", merged, true, err
	case strings.HasSuffix(u.Path, "/update") && strings.HasPrefix(u.Path, "/v0/bead/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/bead/"), "/update")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "bead.update", merged, true, err
	case u.Path == "/v0/mail":
		merged, err := mergeSocketPayload(nil, payload)
		return "mail.send", merged, true, err
	case strings.HasSuffix(u.Path, "/read") && strings.HasPrefix(u.Path, "/v0/mail/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/mail/"), "/read")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "mail.read", merged, true, err
	case strings.HasSuffix(u.Path, "/reply") && strings.HasPrefix(u.Path, "/v0/mail/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/mail/"), "/reply")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "mail.reply", merged, true, err
	case strings.HasSuffix(u.Path, "/mark-unread") && strings.HasPrefix(u.Path, "/v0/mail/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/mail/"), "/mark-unread")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "mail.mark_unread", merged, true, err
	case strings.HasSuffix(u.Path, "/archive") && strings.HasPrefix(u.Path, "/v0/mail/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/mail/"), "/archive")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "mail.archive", merged, true, err
	case u.Path == "/v0/sling":
		merged, err := mergeSocketPayload(nil, payload)
		return "sling.run", merged, true, err
	case strings.HasSuffix(u.Path, "/reopen") && strings.HasPrefix(u.Path, "/v0/bead/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/bead/"), "/reopen")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "bead.reopen", merged, true, err
	case strings.HasSuffix(u.Path, "/assign") && strings.HasPrefix(u.Path, "/v0/bead/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/bead/"), "/assign")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "bead.assign", merged, true, err
	case strings.HasSuffix(u.Path, "/wake") && strings.HasPrefix(u.Path, "/v0/session/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/session/"), "/wake")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "session.wake", merged, true, err
	case strings.HasSuffix(u.Path, "/rename") && strings.HasPrefix(u.Path, "/v0/session/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/session/"), "/rename")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "session.rename", merged, true, err
	case strings.HasSuffix(u.Path, "/respond") && strings.HasPrefix(u.Path, "/v0/session/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/session/"), "/respond")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "session.respond", merged, true, err
	case strings.HasSuffix(u.Path, "/suspend") && strings.HasPrefix(u.Path, "/v0/session/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/session/"), "/suspend")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "session.suspend", merged, true, err
	case strings.HasSuffix(u.Path, "/close") && strings.HasPrefix(u.Path, "/v0/session/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/session/"), "/close")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "session.close", merged, true, err
	case strings.HasSuffix(u.Path, "/submit") && strings.HasPrefix(u.Path, "/v0/session/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/session/"), "/submit")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "session.submit", merged, true, err
	case strings.HasSuffix(u.Path, "/kill") && strings.HasPrefix(u.Path, "/v0/session/"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/session/"), "/kill")
		if id == "" {
			return "", nil, false, nil
		}
		merged, err := mergeSocketPayload(map[string]any{"id": id}, payload)
		return "session.kill", merged, true, err
	case strings.HasSuffix(u.Path, "/suspend") && strings.HasPrefix(u.Path, "/v0/agent/"):
		name := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/agent/"), "/suspend")
		if name == "" {
			return "", nil, false, nil
		}
		return "agent.suspend", map[string]any{"name": unescapeName(name)}, true, nil
	case strings.HasSuffix(u.Path, "/resume") && strings.HasPrefix(u.Path, "/v0/agent/"):
		name := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/agent/"), "/resume")
		if name == "" {
			return "", nil, false, nil
		}
		return "agent.resume", map[string]any{"name": unescapeName(name)}, true, nil
	case strings.HasSuffix(u.Path, "/suspend") && strings.HasPrefix(u.Path, "/v0/rig/"):
		name := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/rig/"), "/suspend")
		if name == "" {
			return "", nil, false, nil
		}
		return "rig.suspend", map[string]any{"name": unescapeName(name)}, true, nil
	case strings.HasSuffix(u.Path, "/resume") && strings.HasPrefix(u.Path, "/v0/rig/"):
		name := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/rig/"), "/resume")
		if name == "" {
			return "", nil, false, nil
		}
		return "rig.resume", map[string]any{"name": unescapeName(name)}, true, nil
	case strings.HasSuffix(u.Path, "/restart") && strings.HasPrefix(u.Path, "/v0/rig/"):
		name := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/rig/"), "/restart")
		if name == "" {
			return "", nil, false, nil
		}
		return "rig.restart", map[string]any{"name": unescapeName(name)}, true, nil
	case strings.HasSuffix(u.Path, "/restart") && strings.HasPrefix(u.Path, "/v0/service/"):
		name := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v0/service/"), "/restart")
		if name == "" {
			return "", nil, false, nil
		}
		return "service.restart", map[string]any{"name": name}, true, nil
	default:
		return "", nil, false, nil
	}
}

func mergeSocketPayload(base map[string]any, payload any) (map[string]any, error) {
	if base == nil {
		base = map[string]any{}
	}
	if payload == nil {
		return base, nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal socket payload: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return base, nil
	}
	var extra map[string]any
	if err := json.Unmarshal(data, &extra); err != nil {
		return nil, fmt.Errorf("decode socket payload: %w", err)
	}
	for k, v := range extra {
		base[k] = v
	}
	return base, nil
}
