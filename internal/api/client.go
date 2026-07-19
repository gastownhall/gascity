// Package api contains the Gas City supervisor API and generated-client adapter.
//
// This file is a thin adapter over the generated client in
// internal/api/genclient. The adapter preserves the small surface that
// CLI commands depend on (Client, NewClient, NewCityScopedClient, the
// 14 mutation/lookup methods, ShouldFallback, IsConnError) while pushing
// all wire-level work (request construction, JSON serialization, URL
// escaping, Problem Details parsing) into the generated client.
//
// Regenerate the generated client by running `go generate ./internal/api/genclient`
// after server changes.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workspacesvc"
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

// clientInitError indicates the client failed to construct its generated
// transport (typically a malformed base URL). It is treated as a fallback
// condition so CLI ladders can fall through to direct file mutation.
type clientInitError struct {
	err error
}

func (e *clientInitError) Error() string {
	if e.err == nil {
		return "api: client not initialized"
	}
	return "api: client not initialized: " + e.err.Error()
}
func (e *clientInitError) Unwrap() error { return e.err }

// cacheNotLiveError indicates the supervisor returned 503 because its
// read-path CachingStore has not yet reached the live state. Read handlers
// return this shape during startup/reconcile rather than serve stale or
// empty data; the CLI classifies it as fallbackable so reads land on raw
// bd instead.
type cacheNotLiveError struct {
	msg string
}

func (e *cacheNotLiveError) Error() string {
	if e.msg == "" {
		return "cache not yet live"
	}
	return e.msg
}

// storeSlowError indicates the supervisor returned 503 because a mail read
// exceeded its internal store deadline. It is intentionally not fallbackable:
// the local store path is affected by the same contention.
type storeSlowError struct {
	msg string
}

// StoreSlowErrorCode is the stable problem-detail prefix for mail read
// timeouts that must not fall back to the local store path.
const StoreSlowErrorCode = "store_slow"

func (e *storeSlowError) Error() string {
	if e.msg == "" {
		return "store slow: try again when load drops"
	}
	return e.msg
}

// IsStoreSlowError reports whether err originated from an API mail store
// timeout. Callers must not fall back to the local store for this error.
func IsStoreSlowError(err error) bool {
	var sse *storeSlowError
	return errors.As(err, &sse)
}

// MaintenanceInProgressError indicates the supervisor returned 409 because
// a Dolt store maintenance cycle is already executing. StartedAt carries
// the in-flight run's start time from the server's typed body so CLI
// callers can display it verbatim. Callers classify it via IsMaintenanceInProgress.
type MaintenanceInProgressError struct {
	StartedAt string // RFC3339 UTC; empty when server did not include it
	msg       string
}

// Error implements the error interface. The rendered message always leads
// with "already in progress" so callers can grep for it reliably; the raw
// server detail (in e.msg) is retained for debugging but not shown in the
// user-facing text.
func (e *MaintenanceInProgressError) Error() string {
	if e == nil {
		return "<nil maintenance-in-progress>"
	}
	if e.StartedAt == "" {
		return "maintenance already in progress"
	}
	return fmt.Sprintf("maintenance already in progress (started %s)", e.StartedAt)
}

// IsMaintenanceInProgress reports whether err originates from a 409 with a
// maintenance-in-progress typed body, so the CLI can emit exit code 3 and
// a targeted message instead of a generic error.
func IsMaintenanceInProgress(err error) bool {
	var e *MaintenanceInProgressError
	return errors.As(err, &e)
}

// MaintenanceDisabledError indicates the server returned 503 because
// [maintenance.dolt] enabled=false in city.toml. The CLI surfaces this as
// a short message pointing at the runbook rather than rolling the 503 into
// the generic cache-not-live fallback bucket (no local fallback path
// exists for maintenance operations).
type MaintenanceDisabledError struct{}

// Error implements the error interface.
func (e *MaintenanceDisabledError) Error() string {
	return "maintenance disabled: set [maintenance.dolt] enabled = true in city.toml and restart the controller"
}

// IsMaintenanceDisabled reports whether err indicates the server rejected
// a maintenance request because the loop is not enabled.
func IsMaintenanceDisabled(err error) bool {
	var e *MaintenanceDisabledError
	return errors.As(err, &e)
}

// serverError indicates a generic 5xx API response without a recognized
// 503 detail prefix such as cache_not_live or store_slow. Read-path callers
// classify it as fallbackable via ShouldFallbackForRead so the CLI lands on
// direct bd when the supervisor is unhealthy. Mutation callers continue to
// surface it as a hard error (ShouldFallback returns false) because writes
// with unknown server-side state are unsafe to silently retry locally.
type serverError struct {
	status int
	msg    string
}

func (e *serverError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("API returned %d", e.status)
	}
	return e.msg
}

// Status reports the HTTP status carried by the server error (always 5xx).
func (e *serverError) Status() int { return e.status }

// IsServerError reports whether err originates from a 5xx API response the
// read-path CLI should treat as fallbackable. Independent of ShouldFallback
// so mutation paths retain their strict no-fallback-on-5xx semantics.
func IsServerError(err error) bool {
	var se *serverError
	return errors.As(err, &se)
}

// ShouldFallbackForRead reports whether err indicates a read-path command
// should fall back to direct bd. Read-path commands tolerate generic 5xx
// server errors (IsServerError) in addition to the cases ShouldFallback
// already covers.
//
// c is the client that produced err (nil-safe). Any error from a REMOTE client
// is non-fallbackable regardless of type: a remote read has no local store to
// fall back to, and silently reading a local store instead would be the exact
// hazard the remote-city design exists to prevent (gate G1). errors.As unwraps
// transport wrappers, so the remoteness of the error cannot be recovered from
// err alone — it must come from the client. Pass the client you called; pass
// nil for a pure error-classification check (treated as local).
func ShouldFallbackForRead(c *Client, err error) bool {
	if c.IsRemote() {
		return false
	}
	if ShouldFallback(c, err) {
		return true
	}
	if IsRouteMissing(err) {
		return true
	}
	return IsServerError(err)
}

// ShouldFallback reports whether err indicates the CLI should fall back to
// direct file mutation (or, for reads, to raw bd). True for transport-level
// failures (connection refused, timeout), read-only API rejections (server
// bound to non-localhost, mutations disabled), client-init failures
// (malformed base URL), and cache-not-live 503 responses during supervisor
// priming. Always false for a REMOTE client (gate G1); see ShouldFallbackForRead
// for why the client, not the error, carries remoteness. c is nil-safe.
func ShouldFallback(c *Client, err error) bool {
	if c.IsRemote() {
		return false
	}
	if IsConnError(err) {
		return true
	}
	var ro *readOnlyError
	if errors.As(err, &ro) {
		return true
	}
	var ci *clientInitError
	if errors.As(err, &ci) {
		return true
	}
	var cnl *cacheNotLiveError
	return errors.As(err, &cnl)
}

// FallbackReason returns a stable reason code for err when
// ShouldFallbackForRead(c, err) is true. The set is closed: "remote",
// "cache-not-live", "read-only", "client-init", "route-missing", "conn-refused".
// A REMOTE client yields "remote" — reported for observability, never used to
// pick a local path (the caller gates on ShouldFallbackForRead first, which
// returns false for remote, so a remote error is surfaced, not fallen back).
// "route-missing" is a new-CLI/old-server route gap (a 404 with no problem+json
// body). Generic 5xx server errors collapse to "conn-refused" since from the
// CLI's read-path perspective an unhealthy server is equivalent to an
// unreachable one. Non-fallbackable error types such as store_slow are
// intentionally absent from this set. Returns "unknown" for non-fallbackable
// errors so callers that invoke FallbackReason unconditionally produce a token
// instead of panicking; gate on ShouldFallbackForRead first to avoid that
// sentinel. c is nil-safe.
func FallbackReason(c *Client, err error) string {
	if c.IsRemote() {
		return "remote"
	}
	var cnl *cacheNotLiveError
	if errors.As(err, &cnl) {
		return "cache-not-live"
	}
	var ro *readOnlyError
	if errors.As(err, &ro) {
		return "read-only"
	}
	var ci *clientInitError
	if errors.As(err, &ci) {
		return "client-init"
	}
	if IsRouteMissing(err) {
		return "route-missing"
	}
	if IsConnError(err) || IsServerError(err) {
		return "conn-refused"
	}
	return "unknown"
}

// Client is an HTTP client for the Gas City API server. It wraps the
// generated typed client so CLI commands can route writes through the API
// when a controller is running.
type Client struct {
	cw       *genclient.ClientWithResponses
	baseURL  string // stored for SSE stream connections
	cityName string // non-empty for city-scoped clients; passed to every per-city call
	initErr  error  // set when NewClient failed to build the transport (malformed baseURL, etc.)

	// Remote-city fields (set only by NewRemoteCityScopedClient). isRemote makes
	// no-fallback a compiler-checkable instance property (gate G1): any error
	// from a remote client is non-fallbackable regardless of type. streamClient
	// is the dedicated SSE transport shape (Timeout:0 + CheckRedirect + TLS);
	// tokenSource is called live before every request AND every SSE (re)connect
	// so a per-attempt 401 re-mint takes effect (never captured once).
	isRemote     bool
	streamClient *http.Client
	tokenSource  TokenSource
	tokenMu      sync.Mutex
	// grantSource, when set, mints a single-use X-GC-City-Write grant for each
	// MUTATING request (gate G18). Like tokenSource it is invoked live per
	// request, never captured. nil means no grant is attached (a city that
	// authenticates on X-GC-Request alone, or one fronted by a bearer edge).
	grantSource GrantSource
}

// IsRemote reports whether this client targets a remote city over the control
// plane. Remote clients never fall back to a local store (gate G1).
func (c *Client) IsRemote() bool { return c != nil && c.isRemote }

// bearerToken returns the current transport bearer from the token source, or ""
// when no source is configured. The call is serialized so a non-reentrant
// source (e.g. one that execs a credential command) is safe under concurrent
// REST + SSE use.
func (c *Client) bearerToken() (string, error) {
	if c == nil || c.tokenSource == nil {
		return "", nil
	}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	return c.tokenSource()
}

const sessionMessageTimeout = 4 * time.Minute

// defaultClientTimeout is the overall HTTP timeout for control-plane client
// calls. The read paths (ListBeads, GetBead, GetStatus, ListMailInbox,
// ListConvoys, ...) pass context.Background() and rely solely on this ceiling,
// and several of them federate the city store plus every rig store — a
// dolt-backed rig store can take many seconds, so a 10s ceiling false-timed-out
// healthy-but-slow federated reads. Most calls return in milliseconds; this
// only bounds the slow federated reads and genuinely hung requests.
const defaultClientTimeout = 60 * time.Second

// SessionSubmitResponse is the domain-facing shape of a session submit result.
type SessionSubmitResponse struct {
	Status string               `json:"status"`
	ID     string               `json:"id"`
	Queued bool                 `json:"queued"`
	Intent session.SubmitIntent `json:"intent"`
}

// sseEvent is a parsed SSE frame from the event stream.
type sseEvent struct {
	Event string
	Data  string
}

// sseEnvelope is the JSON envelope of a typed event on the stream. Seq is the
// per-city monotonic sequence number the wire emits on every typed envelope
// (convoy_event_stream.go:135); a reconnecting wait resumes from the last
// consumed frame via after_seq=<seq>. Heartbeat frames carry no seq/type key,
// so they decode to Seq:0, Type:"" — skipped by the type match, and (because a
// cursor only advances on env.Seq > lastSeq) unable to regress the resume point.
type sseEnvelope struct {
	Seq     uint64          `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// sseConnectError is a non-2xx SSE connect response carried as a typed error so
// a reconnecting wait can classify the status (transient vs permanent) and honor
// Retry-After. Error() renders the exact string waitForEvent produced before the
// reconnect core was split out, so single-shot session waits stay byte-stable.
type sseConnectError struct {
	Status     int    // HTTP status code, for retry classification
	StatusLine string // raw resp.Status ("401 Unauthorized"), for byte-stable rendering
	RetryAfter string // Retry-After header value, if any
	Detail     string // response body detail (or the status line when the body was empty)
}

func (e *sseConnectError) Error() string {
	return fmt.Sprintf("SSE connect failed: %s: %s", e.StatusLine, e.Detail)
}

// ssePayloadDecodeError is a matching (success- or failed-type) frame whose
// typed payload failed to decode. It carries the frame's seq so a reconnecting
// caller can re-read the SAME frame once (a transient truncation decodes cleanly
// on the retry) and, if the identical seq fails to decode again, surface a
// permanent "malformed terminal event at seq N" error — instead of advancing the
// resume cursor past the terminal and hanging to the absolute watchdog.
//
// Its Error() delegates to the wrapped error so the single-shot session wait
// (waitForEvent) keeps its byte-stable "decode <type> payload: ..." string.
type ssePayloadDecodeError struct {
	Seq uint64
	Err error
}

func (e *ssePayloadDecodeError) Error() string { return e.Err.Error() }
func (e *ssePayloadDecodeError) Unwrap() error { return e.Err }

// waitForEvent connects to the appropriate SSE stream, reads frames until it
// finds an event matching the given request_id (in success or failure
// payloads), and returns the envelope. The caller decodes the typed payload.
//
// It is single-shot — one connect, scan to a match or die — and is the wait
// every session async op (SendSessionMessage, SubmitSession) transits, so its
// behavior and error strings are byte-stable. It delegates to waitForEventOnce
// with no progress tap and surfaces its error as-is. Rig-create uses the
// reconnecting waitForEventReconnecting instead.
func (c *Client) waitForEvent(ctx context.Context, requestID string, successType, failOp, eventCursor string) (*sseEnvelope, error) {
	env, _, _, err := c.waitForEventOnce(ctx, requestID, successType, failOp, eventCursor, nil)
	return env, err
}

// waitForEventOnce is one SSE connect-and-scan, the shared core of the
// single-shot waitForEvent and the reconnecting waitForEventReconnecting.
// afterSeq is the cursor for THIS connection (the 202 EventCursor on the first
// attempt, the last consumed seq on a reconnect). onEnvelope, when non-nil, is
// invoked for every decoded typed envelope before matching (the progress tap).
//
// It returns the matched envelope, the resume cursor (the max seq of a frame
// FULLY processed without error — a decode failure returns the PRE-frame cursor
// so the reconnect re-reads the failing frame rather than skipping past it),
// whether any line at all was scanned (a live-peer signal — including a ': ping'
// comment keepalive — that resets the reconnect budget), and any error. A non-2xx
// connect returns a *sseConnectError; a matching frame whose payload fails to
// decode returns an *ssePayloadDecodeError carrying its seq; a
// transport/scan/idle-watchdog failure returns a plain wrapped error (all treated
// as transient by the reconnecting caller).
func (c *Client) waitForEventOnce(ctx context.Context, requestID, successType, failOp, afterSeq string, onEnvelope func(*sseEnvelope)) (env *sseEnvelope, lastSeq uint64, sawFrame bool, err error) {
	streamURL := c.baseURL + "/v0/events/stream"
	cursor := strings.TrimSpace(afterSeq)
	if c.cityName != "" {
		if cursor == "" {
			cursor = "0"
		}
		streamURL = c.baseURL + "/v0/city/" + c.cityName + "/events/stream?after_seq=" + url.QueryEscape(cursor)
	} else {
		if cursor == "" {
			cursor = "0"
		}
		streamURL += "?after_cursor=" + url.QueryEscape(cursor)
	}
	// For a remote client, an idle watchdog cancels a stalled stream: the stream
	// transport has no hard http.Client.Timeout (a long-lived SSE stream must
	// not be capped), so a per-frame-reset timer is the only bound on a silent
	// connection. Local clients keep the caller's context unchanged.
	readCtx := ctx
	var resetIdle func()
	if c.streamClient != nil {
		var cancel context.CancelFunc
		readCtx, cancel = context.WithCancel(ctx)
		defer cancel()
		idle := time.AfterFunc(remoteStreamIdleTimeout, cancel)
		defer idle.Stop()
		resetIdle = func() { idle.Reset(remoteStreamIdleTimeout) }
	}

	req, err := http.NewRequestWithContext(readCtx, http.MethodGet, streamURL, nil)
	if err != nil {
		return nil, lastSeq, sawFrame, fmt.Errorf("build SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-GC-Request", "true")
	// Attach a fresh bearer per (re)connect so a rotated/re-minted credential
	// takes effect on reconnect. No-op for a local client (nil token source).
	if tok, terr := c.bearerToken(); terr != nil {
		return nil, lastSeq, sawFrame, terr
	} else if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	httpClient := c.streamClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, lastSeq, sawFrame, ctxErr
		}
		return nil, lastSeq, sawFrame, fmt.Errorf("SSE connect: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			detail = resp.Status
		}
		return nil, lastSeq, sawFrame, &sseConnectError{
			Status:     resp.StatusCode,
			StatusLine: resp.Status,
			RetryAfter: resp.Header.Get("Retry-After"),
			Detail:     detail,
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var current sseEvent
	for scanner.Scan() {
		// Any scanned line — a data/event line, a blank frame terminator, or a
		// ': ping' comment keepalive — is proof the peer is alive: reset both the
		// per-frame idle watchdog and the cross-connection silent-attempt budget
		// (sawFrame). An intermediary that emits only comment keepalives plus
		// periodic clean closes must not burn the silent budget on a live provision.
		sawFrame = true
		if resetIdle != nil {
			resetIdle()
		}
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			current.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			if current.Data == "" {
				current.Data = data
			} else {
				current.Data += "\n" + data
			}
		case line == "":
			if current.Data == "" {
				current = sseEvent{}
				continue
			}
			var evt sseEnvelope
			if uerr := json.Unmarshal([]byte(current.Data), &evt); uerr != nil {
				return nil, lastSeq, sawFrame, fmt.Errorf("decode SSE event: %w", uerr)
			}
			// The resume cursor (lastSeq) must advance ONLY for a frame this
			// connection fully processed. A matching frame whose payload fails to
			// decode returns below WITHOUT advancing lastSeq, so the reconnect
			// resumes at the pre-frame cursor and re-reads THIS frame (the server
			// delivers strictly-greater than after_seq).
			if onEnvelope != nil {
				onEnvelope(&evt)
			}
			if evt.Type == successType {
				matches, merr := payloadContainsRequestID(evt.Payload, requestID)
				if merr != nil {
					return nil, lastSeq, sawFrame, &ssePayloadDecodeError{Seq: evt.Seq, Err: fmt.Errorf("decode %s payload: %w", successType, merr)}
				}
				if matches {
					out := evt
					if evt.Seq > lastSeq {
						lastSeq = evt.Seq
					}
					return &out, lastSeq, sawFrame, nil
				}
			}
			if evt.Type == events.RequestFailed {
				matches, merr := payloadMatchesRequest(evt.Payload, requestID, failOp)
				if merr != nil {
					return nil, lastSeq, sawFrame, &ssePayloadDecodeError{Seq: evt.Seq, Err: fmt.Errorf("decode %s payload: %w", events.RequestFailed, merr)}
				}
				if matches {
					out := evt
					if evt.Seq > lastSeq {
						lastSeq = evt.Seq
					}
					return &out, lastSeq, sawFrame, nil
				}
			}
			// Fully processed (heartbeat, non-matching, or a decoded match for
			// another request_id): now it is safe to advance past this frame.
			if evt.Seq > lastSeq {
				lastSeq = evt.Seq
			}
			current = sseEvent{}
		}
	}
	if serr := scanner.Err(); serr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, lastSeq, sawFrame, ctxErr
		}
		return nil, lastSeq, sawFrame, fmt.Errorf("SSE scan: %w", serr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, lastSeq, sawFrame, ctxErr
	}
	return nil, lastSeq, sawFrame, fmt.Errorf("SSE stream closed before event for %s arrived", requestID)
}

func payloadContainsRequestID(raw json.RawMessage, requestID string) (bool, error) {
	// Success event types are per-operation, so the typed envelope selects the
	// operation and the payload only needs the unique correlation ID.
	var p struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return false, err
	}
	return p.RequestID == requestID, nil
}

func payloadMatchesRequest(raw json.RawMessage, requestID, operation string) (bool, error) {
	var p struct {
		RequestID string `json:"request_id"`
		Operation string `json:"operation"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return false, err
	}
	return p.RequestID == requestID && p.Operation == operation, nil
}

// NewClient creates a new supervisor-scope API client targeting the
// given base URL (e.g., "http://127.0.0.1:8080"). Supervisor-scope
// operations (ListCities, ListServices-via-city, etc.) work through
// this client; per-city calls require NewCityScopedClient.
func NewClient(baseURL string) *Client {
	return newClient(baseURL, "")
}

// NewCityScopedClient creates a client that targets per-city operations
// at "/v0/city/<cityName>/...". The generated client produces those
// paths natively — no prefix rewrite or path editor needed.
func NewCityScopedClient(baseURL, cityName string) *Client {
	return newClient(baseURL, cityName)
}

func newClient(baseURL, cityName string) *Client {
	httpClient := &http.Client{Timeout: defaultClientTimeout}
	cw, err := genclient.NewClientWithResponses(
		baseURL,
		genclient.WithHTTPClient(httpClient),
		genclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("X-GC-Request", "true")
			return nil
		}),
	)
	if err != nil {
		// genclient.NewClient only returns errors for malformed URLs;
		// the CLI hits this on misconfig — return a stub that errors on
		// every method rather than panicking.
		return &Client{initErr: &clientInitError{err: err}}
	}
	return &Client{cw: cw, baseURL: baseURL, cityName: cityName}
}

// requireCityScope reports an error if the client was constructed as a
// supervisor-scope client (empty cityName) but a per-city method was called.
// Centralizes the check so silent `/v0/city//...` request construction is
// impossible.
func (c *Client) requireCityScope() error {
	if c.initErr != nil {
		return c.initErr
	}
	if c.cw == nil {
		return errClientUninitialized
	}
	if c.cityName == "" {
		return fmt.Errorf("api: per-city call requires NewCityScopedClient; use NewCityScopedClient(baseURL, cityName)")
	}
	return nil
}

// --- Lookup methods ---

// ListCities fetches the current set of cities managed by the supervisor.
func (c *Client) ListCities() ([]CityInfo, error) {
	if c.initErr != nil {
		return nil, c.initErr
	}
	if c.cw == nil {
		return nil, errClientUninitialized
	}
	resp, err := c.cw.GetV0CitiesWithResponse(context.Background())
	if err != nil {
		return nil, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return nil, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return nil, err
	}
	if resp.JSON200 == nil || resp.JSON200.Items == nil {
		return []CityInfo{}, nil
	}
	items := *resp.JSON200.Items
	out := make([]CityInfo, 0, len(items))
	for _, ci := range items {
		out = append(out, cityInfoFromGen(ci))
	}
	return out, nil
}

// ListServices fetches the current workspace service statuses.
func (c *Client) ListServices() ([]workspacesvc.Status, error) {
	if err := c.requireCityScope(); err != nil {
		return nil, err
	}
	resp, err := c.cw.GetV0CityByCityNameServicesWithResponse(context.Background(), c.cityName)
	if err != nil {
		return nil, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return nil, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return nil, err
	}
	if resp.JSON200 == nil || resp.JSON200.Items == nil {
		return []workspacesvc.Status{}, nil
	}
	items := *resp.JSON200.Items
	out := make([]workspacesvc.Status, 0, len(items))
	for _, item := range items {
		out = append(out, workspaceStatusFromGen(item))
	}
	return out, nil
}

// GetOrderHistory fetches order run history via
// GET /v0/city/{cityName}/orders/history. scopedName is required (the
// handler returns 400 when empty); limit=0 selects the server default;
// before is an optional RFC3339 upper bound. The CachedRead.AgeSeconds
// field carries the supervisor CachingStore age so callers can surface
// _cache_age_s on --json output and a staleness banner on human output.
func (c *Client) GetOrderHistory(scopedName string, limit int, before string) (CachedRead[[]OrderHistoryView], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[[]OrderHistoryView]{}, err
	}
	params := &genclient.GetV0CityByCityNameOrdersHistoryParams{
		ScopedName: scopedName,
	}
	if limit > 0 {
		l := int64(limit)
		params.Limit = &l
	}
	if before != "" {
		params.Before = &before
	}
	resp, err := c.cw.GetV0CityByCityNameOrdersHistoryWithResponse(context.Background(), c.cityName, params)
	if err != nil {
		return CachedRead[[]OrderHistoryView]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[[]OrderHistoryView]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[[]OrderHistoryView]{}, err
	}
	return CachedRead[[]OrderHistoryView]{
		Body:       orderHistoryFromGenList(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// GetMaintenanceStatus fetches the Dolt store maintenance loop state via
// GET /v0/city/{cityName}/maintenance/status. The CachedRead.AgeSeconds
// field carries the supervisor CachingStore age from the X-GC-Cache-Age-S
// response header so callers can surface _cache_age_s on --json output
// and a staleness banner on human output. Returns
// *MaintenanceDisabledError when the loop is disabled in city.toml.
func (c *Client) GetMaintenanceStatus() (CachedRead[MaintenanceStatusView], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[MaintenanceStatusView]{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameMaintenanceStatusWithResponse(context.Background(), c.cityName)
	if err != nil {
		return CachedRead[MaintenanceStatusView]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[MaintenanceStatusView]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[MaintenanceStatusView]{}, err
	}
	return CachedRead[MaintenanceStatusView]{
		Body:       maintenanceStatusViewFromGen(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// TriggerMaintenanceDoltGC posts POST /v0/city/{cityName}/maintenance/dolt-gc.
// With wait=true the call blocks until the run completes and returns the
// full MaintenanceTriggerView; with wait=false it returns 202 Accepted with
// the synthesized started_at token. Returns *MaintenanceInProgressError on
// 409 Conflict and *MaintenanceDisabledError on 503 maintenance_disabled.
func (c *Client) TriggerMaintenanceDoltGC(wait bool) (MaintenanceTriggerView, error) {
	if err := c.requireCityScope(); err != nil {
		return MaintenanceTriggerView{}, err
	}
	params := &genclient.TriggerMaintenanceDoltGcParams{}
	if wait {
		w := true
		params.Wait = &w
	}
	resp, err := c.cw.TriggerMaintenanceDoltGcWithResponse(context.Background(), c.cityName, params)
	if err != nil {
		return MaintenanceTriggerView{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return MaintenanceTriggerView{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return MaintenanceTriggerView{}, err
	}
	return maintenanceTriggerViewFromGen(resp.JSON202), nil
}

// ListSessions fetches the current set of sessions via
// GET /v0/city/{cityName}/sessions. The stateFilter and templateFilter
// arguments correspond to the state/template query parameters (empty means
// omit). peek controls the optional last-output preview. The
// CachedRead.AgeSeconds field carries the supervisor CachingStore age from
// the X-GC-Cache-Age-S response header so callers can surface _cache_age_s
// on --json output and a staleness banner on human output.
func (c *Client) ListSessions(stateFilter, templateFilter string, peek bool) (CachedRead[[]SessionView], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[[]SessionView]{}, err
	}
	// gc session list means "all sessions". Walk the keyset pages until the
	// server stops minting next_cursor and merge them, so a fleet larger than
	// one server-cap page is fully listed instead of silently truncated at the
	// first page. Each page requests the 1000-row server cap to minimize round
	// trips; the cache age is taken from the first page.
	capLimit := int64(maxPaginationLimit)
	var (
		all        []SessionView
		ageSeconds float64
		cursor     string
	)
	for page := 0; ; page++ {
		params := &genclient.GetV0CityByCityNameSessionsParams{Limit: &capLimit}
		if stateFilter != "" {
			params.State = &stateFilter
		}
		if templateFilter != "" {
			params.Template = &templateFilter
		}
		if peek {
			params.Peek = &peek
		}
		if cursor != "" {
			params.Cursor = &cursor
		}
		resp, err := c.cw.GetV0CityByCityNameSessionsWithResponse(context.Background(), c.cityName, params)
		if err != nil {
			return CachedRead[[]SessionView]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
		}
		if resp == nil {
			return CachedRead[[]SessionView]{}, &connError{err: fmt.Errorf("nil response")}
		}
		if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
			return CachedRead[[]SessionView]{}, err
		}
		all = append(all, sessionsFromGenList(resp.JSON200)...)
		if page == 0 {
			ageSeconds = cacheAgeFromResponse(resp.HTTPResponse)
		}
		next := ""
		if resp.JSON200 != nil && resp.JSON200.NextCursor != nil {
			next = *resp.JSON200.NextCursor
		}
		// Stop at the last page. The equal-cursor guard is a safety net against
		// a server that fails to advance the cursor, so the walk can never spin
		// forever on a degenerate response.
		if next == "" || next == cursor {
			break
		}
		cursor = next
	}
	if all == nil {
		all = []SessionView{}
	}
	return CachedRead[[]SessionView]{
		Body:       all,
		AgeSeconds: ageSeconds,
	}, nil
}

// GetSession fetches one session by ID or alias via
// GET /v0/city/{cityName}/session/{id}. peek=true asks the server to include
// the last-output preview; peekLines selects the preview line count (0 means
// "use the server default").
func (c *Client) GetSession(id string, peek bool, peekLines int) (CachedRead[SessionView], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[SessionView]{}, err
	}
	params := &genclient.GetV0CityByCityNameSessionByIdParams{}
	if peek {
		params.Peek = &peek
	}
	if peekLines > 0 {
		pl := int64(peekLines)
		params.PeekLines = &pl
	}
	resp, err := c.cw.GetV0CityByCityNameSessionByIdWithResponse(context.Background(), c.cityName, id, params)
	if err != nil {
		return CachedRead[SessionView]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[SessionView]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[SessionView]{}, err
	}
	if resp.JSON200 == nil {
		return CachedRead[SessionView]{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	return CachedRead[SessionView]{
		Body:       sessionViewFromGen(*resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// ListRigs fetches the current set of configured rigs via
// GET /v0/city/{cityName}/rigs. The CachedRead.AgeSeconds field carries the
// supervisor CachingStore age from the X-GC-Cache-Age-S response header so
// callers can surface _cache_age_s on --json output and a staleness banner
// on human output.
func (c *Client) ListRigs() (CachedRead[[]RigView], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[[]RigView]{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameRigsWithResponse(context.Background(), c.cityName, &genclient.GetV0CityByCityNameRigsParams{})
	if err != nil {
		return CachedRead[[]RigView]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[[]RigView]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[[]RigView]{}, err
	}
	return CachedRead[[]RigView]{
		Body:       rigsFromGenList(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// ListConvoys fetches the open convoys across all rigs via
// GET /v0/city/{cityName}/convoys. The CachedRead.AgeSeconds field carries the
// supervisor CachingStore age from the X-GC-Cache-Age-S response header so
// callers can surface _cache_age_s on --json output and a staleness banner
// on human output.
func (c *Client) ListConvoys() (CachedRead[[]beads.Bead], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[[]beads.Bead]{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameConvoysWithResponse(context.Background(), c.cityName, &genclient.GetV0CityByCityNameConvoysParams{})
	if err != nil {
		return CachedRead[[]beads.Bead]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[[]beads.Bead]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[[]beads.Bead]{}, err
	}
	return CachedRead[[]beads.Bead]{
		Body:       convoysFromGenList(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// GetConvoy fetches one convoy by ID via
// GET /v0/city/{cityName}/convoy/{id}. Returns the convoy bead, its direct
// children, and progress counts. Workflow/graph convoys produce an empty
// Convoy (ID == "") — callers should treat that as "not a simple convoy" and
// fall back to the local path.
func (c *Client) GetConvoy(id string) (CachedRead[ConvoyStatusView], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[ConvoyStatusView]{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameConvoyByIdWithResponse(context.Background(), c.cityName, id)
	if err != nil {
		return CachedRead[ConvoyStatusView]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[ConvoyStatusView]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[ConvoyStatusView]{}, err
	}
	if resp.JSON200 == nil {
		return CachedRead[ConvoyStatusView]{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	return CachedRead[ConvoyStatusView]{
		Body:       convoyStatusFromGen(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// CheckConvoy fetches one convoy's completion status via
// GET /v0/city/{cityName}/convoy/{id}/check. Returns child totals and a
// Complete flag that is true when total > 0 and all children are closed.
func (c *Client) CheckConvoy(id string) (CachedRead[ConvoyCheckView], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[ConvoyCheckView]{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameConvoyByIdCheckWithResponse(context.Background(), c.cityName, id)
	if err != nil {
		return CachedRead[ConvoyCheckView]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[ConvoyCheckView]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[ConvoyCheckView]{}, err
	}
	if resp.JSON200 == nil {
		return CachedRead[ConvoyCheckView]{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	return CachedRead[ConvoyCheckView]{
		Body:       convoyCheckFromGen(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// ListBeadsOpts is the optional filter set for ListBeads. All fields are
// zero-valued by default; the server falls back to its own defaults when a
// field is empty. All mirrors the CLI --all flag and maps to the server's
// IncludeClosed query semantic.
type ListBeadsOpts struct {
	Status   string
	Type     string
	Label    string
	Assignee string
	Rig      string
	// Limit is a client-side TOTAL bound on the number of beads returned across
	// all pages. 0 means "drain every page" (unbounded), matching the offline
	// direct-bd lane (beads.ListQuery.Limit 0 = unlimited) so the two lanes have
	// the same coverage. A positive value stops paginating once reached.
	Limit int
	All   bool
}

// maxBeadDrainPages hard-bounds the cursor-drain loop. The seenCursors guard
// already aborts on a repeated cursor (immediate A,A or a longer A,B,A,B cycle),
// but a server returning infinitely many DISTINCT, strictly-advancing cursors
// with empty or all-duplicate pages is caught by neither that guard (cursors
// never repeat) nor opts.Limit (dedup stops `all` from growing, so a positive
// Limit is never reached). This cap fails loudly instead of spinning. At
// maxPaginationLimit (1000) beads/page it admits 100M distinct beads — orders of
// magnitude past any real city — so a legitimate drain never trips it.
const maxBeadDrainPages = 100_000

// listBeadsParams maps the caller's ListBeadsOpts filters onto the generated
// query-parameter struct. Split out of ListBeads so the branch-heavy filter
// mapping is a small, independently testable unit and does not inflate the
// pagination loop's complexity.
func listBeadsParams(opts ListBeadsOpts) *genclient.GetV0CityByCityNameBeadsParams {
	params := &genclient.GetV0CityByCityNameBeadsParams{}
	if opts.Status != "" {
		params.Status = &opts.Status
	}
	if opts.Type != "" {
		params.Type = &opts.Type
	}
	if opts.Label != "" {
		params.Label = &opts.Label
	}
	if opts.Assignee != "" {
		params.Assignee = &opts.Assignee
	}
	if opts.Rig != "" {
		params.Rig = &opts.Rig
	}
	if opts.All {
		t := true
		params.All = &t
	}
	return params
}

// ListBeads fetches beads across all rigs via
// GET /v0/city/{cityName}/beads. Server-side filters mirror the BeadListInput
// query parameters. The CachedRead.AgeSeconds field carries the supervisor
// CachingStore age from the X-GC-Cache-Age-S response header so callers can
// surface _cache_age_s on --json output and a staleness banner on human
// output.
func (c *Client) ListBeads(opts ListBeadsOpts) (CachedRead[[]beads.Bead], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[[]beads.Bead]{}, err
	}
	return c.drainBeadPages(listBeadsParams(opts), opts.Limit, maxBeadDrainPages)
}

// drainBeadPages follows next_cursor from the beads-list endpoint until the
// server reports no further pages, or (when limit > 0) until limit beads have
// been collected. Without this the server's page default (50) silently
// truncated the result, so an offline `gc beads list` in a >50-bead city showed
// only the first page while the direct-bd lane returned everything. Extracted
// from ListBeads so the pagination invariants — non-nil empty slice, cross-page
// ID dedup, and the cursor-repeat + hard-page bounds — live behind one focused,
// separately tested seam.
//
// Cost: with all=true the server runs bounded mode where every page after the
// first is a SEEKED page that disables the store's native LIMIT and re-hydrates
// O(H) matching history (server comment, gascity#3253), and the all=true
// response cache is keyed on cursor, so each page is a distinct cold rebuild. A
// cold `--all` drain is therefore ≈O(H²/pageLimit) server work — much costlier
// than the direct-bd single O(H) pass it matches. Correctness is unaffected;
// prefer filters over `--all` on very large cities.
func (c *Client) drainBeadPages(params *genclient.GetV0CityByCityNameBeadsParams, limit, maxPages int) (CachedRead[[]beads.Bead], error) {
	// Initialize non-nil: an empty city (or an all-filtered-out result) must
	// serialize as `[]`, not `null`. A nil []beads.Bead marshals to JSON null,
	// which breaks `--json` consumers (`jq '.beads[]'`) and diverges from the
	// direct-bd lane, which always emits an empty array.
	all := []beads.Bead{}
	var ageSeconds float64
	// Cursors are non-snapshot offsets into a created_at-DESC list rebuilt per
	// request, so a bead created mid-drain can shift the tail and re-appear at a
	// page boundary. Dedupe by ID so JSON/table output never carries a duplicate
	// (a closed/deleted bead can still be skipped — inherent to offset paging
	// without a snapshot token; the drain is still strictly better than the old
	// page-1 truncation). AgeSeconds comes from the first page.
	seen := make(map[string]bool)
	// seenCursors records every next_cursor already requested. Cursors must
	// strictly advance, so any repeat — an immediate A,A or a longer A,B,A,B
	// cycle — is a buggy or hostile server; abort instead of looping. This
	// catches repeated cursors only; maxBeadDrainPages backstops a distinct-
	// cursor flood.
	seenCursors := make(map[string]bool)
	for page := 0; ; page++ {
		if page >= maxPages {
			return CachedRead[[]beads.Bead]{}, fmt.Errorf("pagination exceeded %d pages; aborting", maxPages)
		}
		pageLimit := maxPaginationLimit
		if limit > 0 {
			remaining := limit - len(all)
			if remaining <= 0 {
				break
			}
			if remaining < pageLimit {
				pageLimit = remaining
			}
		}
		lim := int64(pageLimit)
		params.Limit = &lim

		resp, err := c.cw.GetV0CityByCityNameBeadsWithResponse(context.Background(), c.cityName, params)
		if err != nil {
			return CachedRead[[]beads.Bead]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
		}
		if resp == nil {
			return CachedRead[[]beads.Bead]{}, &connError{err: fmt.Errorf("nil response")}
		}
		if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
			return CachedRead[[]beads.Bead]{}, err
		}
		if page == 0 {
			ageSeconds = cacheAgeFromResponse(resp.HTTPResponse)
		}
		for _, b := range beadsFromGenList(resp.JSON200) {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			all = append(all, b)
		}

		next := ""
		if resp.JSON200 != nil && resp.JSON200.NextCursor != nil {
			next = *resp.JSON200.NextCursor
		}
		if next == "" {
			break
		}
		if seenCursors[next] {
			return CachedRead[[]beads.Bead]{}, fmt.Errorf("pagination cursor repeated (%q); aborting", next)
		}
		seenCursors[next] = true
		params.Cursor = &next
	}
	return CachedRead[[]beads.Bead]{Body: all, AgeSeconds: ageSeconds}, nil
}

// GetBead fetches one bead by ID via
// GET /v0/city/{cityName}/bead/{id}. Returns the bead detail with cache age
// so callers can attach _cache_age_s (JSON) or a staleness banner (human).
func (c *Client) GetBead(id string) (CachedRead[beads.Bead], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[beads.Bead]{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameBeadByIdWithResponse(context.Background(), c.cityName, id)
	if err != nil {
		return CachedRead[beads.Bead]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[beads.Bead]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[beads.Bead]{}, err
	}
	if resp.JSON200 == nil {
		return CachedRead[beads.Bead]{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	return CachedRead[beads.Bead]{
		Body:       beadFromGenPtr(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// GetStatus fetches the city-wide status snapshot via
// GET /v0/city/{cityName}/status. The CachedRead.AgeSeconds field carries
// the supervisor CachingStore age from the X-GC-Cache-Age-S response header
// so callers can surface _cache_age_s on --json output and a staleness
// banner on human output.
func (c *Client) GetStatus() (CachedRead[StatusView], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[StatusView]{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameStatusWithResponse(context.Background(), c.cityName, &genclient.GetV0CityByCityNameStatusParams{})
	if err != nil {
		return CachedRead[StatusView]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[StatusView]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[StatusView]{}, err
	}
	return CachedRead[StatusView]{
		Body:       statusViewFromGen(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// ListMailInbox fetches unread messages for the given agent recipient via
// GET /v0/city/{cityName}/mail. An empty agent lets the server choose the
// default caller identity (same resolution path the CLI would take locally).
// rig narrows the query to a single rig's provider when set. The returned
// MailListView preserves partial aggregate-read metadata so callers do not
// silently treat a degraded all-rig read as authoritative. The
// CachedRead.AgeSeconds field carries the supervisor CachingStore age so
// callers can surface _cache_age_s on --json output and a staleness banner
// on human output.
func (c *Client) ListMailInbox(agent, rig string) (CachedRead[MailListView], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[MailListView]{}, err
	}
	params := &genclient.GetV0CityByCityNameMailParams{}
	if agent != "" {
		params.Agent = &agent
	}
	if rig != "" {
		params.Rig = &rig
	}
	resp, err := c.cw.GetV0CityByCityNameMailWithResponse(context.Background(), c.cityName, params)
	if err != nil {
		return CachedRead[MailListView]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[MailListView]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[MailListView]{}, err
	}
	return CachedRead[MailListView]{
		Body:       mailListFromGen(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// GetMail fetches a single message by ID via GET /v0/city/{cityName}/mail/{id}.
// rig is an optional hint for O(1) lookup when the caller already knows which
// rig owns the message.
func (c *Client) GetMail(id, rig string) (CachedRead[mail.Message], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[mail.Message]{}, err
	}
	params := &genclient.GetV0CityByCityNameMailByIdParams{}
	if rig != "" {
		params.Rig = &rig
	}
	resp, err := c.cw.GetV0CityByCityNameMailByIdWithResponse(context.Background(), c.cityName, id, params)
	if err != nil {
		return CachedRead[mail.Message]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[mail.Message]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[mail.Message]{}, err
	}
	if resp.JSON200 == nil {
		return CachedRead[mail.Message]{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	return CachedRead[mail.Message]{
		Body:       mailMessageFromGen(*resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// CountMail fetches total/unread message counts via
// GET /v0/city/{cityName}/mail/count. An empty agent lets the server choose
// the default caller identity; rig narrows to a single rig's provider.
func (c *Client) CountMail(agent, rig string) (CachedRead[MailCountView], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[MailCountView]{}, err
	}
	params := &genclient.GetV0CityByCityNameMailCountParams{}
	if agent != "" {
		params.Agent = &agent
	}
	if rig != "" {
		params.Rig = &rig
	}
	resp, err := c.cw.GetV0CityByCityNameMailCountWithResponse(context.Background(), c.cityName, params)
	if err != nil {
		return CachedRead[MailCountView]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[MailCountView]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[MailCountView]{}, err
	}
	return CachedRead[MailCountView]{
		Body:       mailCountFromGen(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// GetService fetches one current workspace service status.
func (c *Client) GetService(name string) (workspacesvc.Status, error) {
	if err := c.requireCityScope(); err != nil {
		return workspacesvc.Status{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameServiceByNameWithResponse(context.Background(), c.cityName, name)
	if err != nil {
		return workspacesvc.Status{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return workspacesvc.Status{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return workspacesvc.Status{}, err
	}
	if resp.JSON200 == nil {
		return workspacesvc.Status{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	return workspaceStatusFromGen(*resp.JSON200), nil
}

// --- Mutation methods ---

// RestartService restarts a service via POST /v0/service/{name}/restart.
func (c *Client) RestartService(name string) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	resp, err := c.cw.PostV0CityByCityNameServiceByNameRestartWithResponse(context.Background(), c.cityName, name, nil)
	return checkMutation(resp, err)
}

// SuspendCity suspends the city via PATCH /v0/city.
func (c *Client) SuspendCity() error { return c.patchCity(true) }

// ResumeCity resumes the city via PATCH /v0/city.
func (c *Client) ResumeCity() error { return c.patchCity(false) }

func (c *Client) patchCity(suspend bool) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	resp, err := c.cw.PatchV0CityByCityNameWithResponse(context.Background(), c.cityName, nil, genclient.PatchV0CityByCityNameJSONRequestBody{Suspended: &suspend})
	return checkMutation(resp, err)
}

// SuspendAgent suspends an agent via POST /v0/agent/{base}/{action} (or
// the qualified form /agent/{dir}/{base}/{action}). Name can be
// qualified (e.g. "myrig/worker") — the client picks the right route.
func (c *Client) SuspendAgent(name string) error {
	return c.postAgentAction(name, "suspend")
}

// ResumeAgent resumes an agent via POST /v0/agent/{base}/{action} (or
// the qualified form).
func (c *Client) ResumeAgent(name string) error {
	return c.postAgentAction(name, "resume")
}

func (c *Client) postAgentAction(name, action string) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	// Agents can be addressed unqualified or rig-qualified. The server
	// exposes a distinct route for each shape — no trailing-path
	// wildcard, no client-side path-rewriting shim.
	if dir, base, ok := strings.Cut(name, "/"); ok {
		resp, err := c.cw.PostV0CityByCityNameAgentByDirByBaseByActionWithResponse(
			context.Background(), c.cityName, dir, base,
			genclient.PostV0CityByCityNameAgentByDirByBaseByActionParamsAction(action), nil)
		return checkMutation(resp, err)
	}
	resp, err := c.cw.PostV0CityByCityNameAgentByBaseByActionWithResponse(
		context.Background(), c.cityName, name,
		genclient.PostV0CityByCityNameAgentByBaseByActionParamsAction(action), nil)
	return checkMutation(resp, err)
}

// SuspendRig suspends a rig via POST /v0/rig/{name}/suspend.
func (c *Client) SuspendRig(name string) error { return c.postRigAction(name, "suspend") }

// ResumeRig resumes a rig via POST /v0/rig/{name}/resume.
func (c *Client) ResumeRig(name string) error { return c.postRigAction(name, "resume") }

// RestartRig restarts a rig via POST /v0/rig/{name}/restart.
// Kills all agents in the rig; the reconciler restarts them.
func (c *Client) RestartRig(name string) error { return c.postRigAction(name, "restart") }

func (c *Client) postRigAction(name, action string) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	resp, err := c.cw.PostV0CityByCityNameRigByNameByActionWithResponse(
		context.Background(), c.cityName, name,
		genclient.PostV0CityByCityNameRigByNameByActionParamsAction(action), nil)
	return checkMutation(resp, err)
}

// KillSession force-kills a session via POST /v0/session/{id}/kill.
func (c *Client) KillSession(id string) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	resp, err := c.cw.PostV0CityByCityNameSessionByIdKillWithResponse(context.Background(), c.cityName, id, nil)
	return checkMutation(resp, err)
}

// SendSessionMessage delivers a message to a session via the async
// POST /v0/city/{cityName}/session/{id}/messages endpoint. Internally
// handles the async protocol: POST → 202 + request_id → SSE event.
func (c *Client) SendSessionMessage(id, message string) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionMessageTimeout)
	defer cancel()
	resp, err := c.cw.SendSessionMessageWithResponse(ctx, c.cityName, id, nil, genclient.SendSessionMessageJSONRequestBody{
		Message: message,
	})
	if err != nil {
		return &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if err := checkMutation(resp, err); err != nil {
		return err
	}
	if resp.JSON202 == nil {
		return fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	requestID := resp.JSON202.RequestId

	env, err := c.waitForEvent(ctx, requestID, events.RequestResultSessionMessage, RequestOperationSessionMessage, resp.JSON202.EventCursor)
	if err != nil {
		return err
	}
	if env.Type == events.RequestFailed {
		var p RequestFailedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return fmt.Errorf("decode message failure: %w", err)
		}
		return fmt.Errorf("message failed: %s: %s", p.ErrorCode, p.ErrorMessage)
	}
	return nil
}

// SubmitSession sends a semantic submit request to a session. The id may
// be either a bead ID or a resolvable session alias/name. Internally
// handles the async protocol: POST → 202 + request_id → SSE event.
func (c *Client) SubmitSession(id, message string, intent session.SubmitIntent) (SessionSubmitResponse, error) {
	if err := c.requireCityScope(); err != nil {
		return SessionSubmitResponse{}, err
	}
	body := genclient.SubmitSessionJSONRequestBody{Message: message}
	if intent != "" {
		i := genclient.SubmitIntent(intent)
		body.Intent = &i
	}
	resp, err := c.cw.SubmitSessionWithResponse(context.Background(), c.cityName, id, nil, body)
	if err != nil {
		return SessionSubmitResponse{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return SessionSubmitResponse{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return SessionSubmitResponse{}, err
	}
	if resp.JSON202 == nil {
		return SessionSubmitResponse{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	requestID := resp.JSON202.RequestId

	ctx, cancel := context.WithTimeout(context.Background(), sessionMessageTimeout)
	defer cancel()
	env, err := c.waitForEvent(ctx, requestID, events.RequestResultSessionSubmit, RequestOperationSessionSubmit, resp.JSON202.EventCursor)
	if err != nil {
		return SessionSubmitResponse{}, err
	}
	if env.Type == events.RequestFailed {
		var p RequestFailedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return SessionSubmitResponse{}, fmt.Errorf("decode submit failure: %w", err)
		}
		return SessionSubmitResponse{}, fmt.Errorf("submit failed: %s: %s", p.ErrorCode, p.ErrorMessage)
	}
	var p SessionSubmitSucceededPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return SessionSubmitResponse{}, fmt.Errorf("decode submit result: %w", err)
	}
	return SessionSubmitResponse{
		Status: "accepted",
		ID:     p.SessionID,
		Queued: p.Queued,
		Intent: session.SubmitIntent(p.Intent),
	}, nil
}

// SlingRequest carries the parameters of a sling mutation for Client.Sling.
// It mirrors the SlingInput body: Target is required; exactly one of Bead or
// Formula selects the work.
type SlingRequest struct {
	Rig            string
	Target         string
	Bead           string
	Formula        string
	AttachedBeadID string
	Title          string
	Vars           map[string]string
	ScopeKind      string
	ScopeRef       string
	Force          bool
	Reassign       bool
	Merge          string
	NoConvoy       bool
	Owned          bool
	NoFormula      bool
}

// SlingResult is the outcome of a sling mutation.
type SlingResult struct {
	Status         string
	Target         string
	Formula        string
	Bead           string
	WorkflowID     string
	RootBeadID     string
	AttachedBeadID string
	Mode           string
	Warnings       []string
}

// Sling routes work to a target agent or pool over the control plane
// (POST /v0/city/{city}/sling). It is synchronous: the server materializes the
// work, hooks it, creates any auto-convoy, and returns the result. A remote
// client attaches the X-GC-City-Write grant automatically for this mutating
// request (gate G18); a remote error is non-fallbackable (gate G1).
func (c *Client) Sling(req SlingRequest) (SlingResult, error) {
	if err := c.requireCityScope(); err != nil {
		return SlingResult{}, err
	}
	body := genclient.PostV0CityByCityNameSlingJSONRequestBody{Target: req.Target}
	setStrPtr(&body.Rig, req.Rig)
	setStrPtr(&body.Bead, req.Bead)
	setStrPtr(&body.Formula, req.Formula)
	setStrPtr(&body.AttachedBeadId, req.AttachedBeadID)
	setStrPtr(&body.Title, req.Title)
	setStrPtr(&body.ScopeKind, req.ScopeKind)
	setStrPtr(&body.ScopeRef, req.ScopeRef)
	if req.Force {
		f := true
		body.Force = &f
	}
	if req.Reassign {
		r := true
		body.Reassign = &r
	}
	setStrPtr(&body.Merge, req.Merge)
	if req.NoConvoy {
		b := true
		body.NoConvoy = &b
	}
	if req.Owned {
		b := true
		body.Owned = &b
	}
	if req.NoFormula {
		b := true
		body.NoFormula = &b
	}
	if len(req.Vars) > 0 {
		v := req.Vars
		body.Vars = &v
	}
	params := &genclient.PostV0CityByCityNameSlingParams{XGCRequest: "true"}
	resp, err := c.cw.PostV0CityByCityNameSlingWithResponse(context.Background(), c.cityName, params, body)
	if err != nil {
		return SlingResult{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return SlingResult{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return SlingResult{}, err
	}
	if resp.JSON200 == nil {
		return SlingResult{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	r := resp.JSON200
	out := SlingResult{
		Status:         r.Status,
		Target:         r.Target,
		Formula:        derefStr(r.Formula),
		Bead:           derefStr(r.Bead),
		WorkflowID:     derefStr(r.WorkflowId),
		RootBeadID:     derefStr(r.RootBeadId),
		AttachedBeadID: derefStr(r.AttachedBeadId),
		Mode:           derefStr(r.Mode),
	}
	if r.Warnings != nil {
		out.Warnings = *r.Warnings
	}
	return out, nil
}

// setStrPtr points *dst at a copy of v when v is non-empty, leaving it nil
// otherwise, so an omitempty pointer field is only set for a present value.
func setStrPtr(dst **string, v string) {
	if v != "" {
		*dst = &v
	}
}

var errClientUninitialized = errors.New("api client not initialized")

// checkMutation handles the (resp, err) tuple from a generated mutation
// call and returns the (nil | connError | readOnlyError | generic error)
// shape that ShouldFallback understands. resp may be nil when transportErr
// is set (e.g. connection refused).
func checkMutation(resp interface{ StatusCode() int }, transportErr error) error {
	if transportErr != nil {
		return &connError{err: fmt.Errorf("request failed: %w", transportErr)}
	}
	if resp == nil || isNil(resp) {
		return &connError{err: fmt.Errorf("nil response")}
	}
	return apiErrorFromResponse(resp.StatusCode(), pdOf(resp))
}

// isNil reports whether an interface value holds a nil concrete value.
// Necessary because passing a typed nil pointer satisfies an interface
// without being == nil.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// pdOf extracts the generated client's decoded Problem Details pointer
// from any generated *WithResponse type. An operation that keeps the spec's
// catch-all error decodes it into `ApplicationproblemJSONDefault *ErrorModel`;
// an operation that enumerates its error statuses (the P12 error-contract
// pilot) decodes into `ApplicationproblemJSON<code> *ErrorModel` instead —
// exactly one of which the generator populates, the one matching the HTTP
// status. pdOf returns whichever ErrorModel field is set, so both spec shapes
// are handled uniformly. Returns nil when none is populated (2xx, non-JSON
// error, or an operation with no problem+json error at all).
//
// This is spec-driven: the fields exist because the spec declares the error
// responses to be Problem Details, and the generator decoded them. No
// hand-written JSON parsing happens here or downstream.
func pdOf(resp any) *genclient.ErrorModel {
	if resp == nil {
		return nil
	}
	rv := reflect.ValueOf(resp)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	// Prefer the catch-all field, then fall back to whichever per-status
	// ApplicationproblemJSON<code> field the generator populated.
	if f := rv.FieldByName("ApplicationproblemJSONDefault"); f.IsValid() {
		if pd, _ := f.Interface().(*genclient.ErrorModel); pd != nil {
			return pd
		}
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		if !strings.HasPrefix(rt.Field(i).Name, "ApplicationproblemJSON") {
			continue
		}
		if pd, _ := rv.Field(i).Interface().(*genclient.ErrorModel); pd != nil {
			return pd
		}
	}
	// Fallback: the server returned a status the operation did not enumerate,
	// so the generator has no field to decode the problem+json into (e.g. an
	// infrastructure or middleware 503 like cache_not_live on a read whose
	// declared contract is 404-only). Recover the detail from the raw response
	// body so read-path fallback classification still works. Guarded to bodies
	// that decode as a Problem Details document so 2xx/non-problem payloads do
	// not masquerade as errors.
	if bf := rv.FieldByName("Body"); bf.IsValid() {
		if body, ok := bf.Interface().([]byte); ok && len(body) > 0 {
			var pd genclient.ErrorModel
			if json.Unmarshal(body, &pd) == nil && (pd.Detail != nil || pd.Title != nil || pd.Code != nil) {
				return &pd
			}
		}
	}
	return nil
}

// apiErrorFromResponse returns nil for 2xx responses, a *readOnlyError
// for "read_only:" prefixed Problem Details, and a generic error
// otherwise. pd comes from the generated client's typed decode of the
// spec's default `application/problem+json` response — there is no
// hand-written JSON parsing.
func apiErrorFromResponse(status int, pd *genclient.ErrorModel) error {
	if status >= 200 && status < 300 {
		return nil
	}
	var detail, title string
	if pd != nil {
		if pd.Detail != nil {
			detail = *pd.Detail
		}
		if pd.Title != nil {
			title = *pd.Title
		}
	}
	if strings.HasPrefix(detail, "read_only") {
		msg := detail
		if msg == "" {
			msg = "mutations disabled (read-only server)"
		}
		return &readOnlyError{msg: msg}
	}
	if status == http.StatusServiceUnavailable {
		if strings.HasPrefix(detail, "cache_not_live") {
			msg := detail
			if msg == "" {
				msg = "cache not yet live"
			}
			return &cacheNotLiveError{msg: msg}
		}
		if strings.HasPrefix(detail, StoreSlowErrorCode) {
			msg := detail
			if msg == "" {
				msg = "store slow: try again when load drops"
			}
			return &storeSlowError{msg: msg}
		}
		if strings.HasPrefix(detail, "maintenance_disabled") {
			return &MaintenanceDisabledError{}
		}
	}
	if status == http.StatusConflict && strings.HasPrefix(detail, "maintenance-in-progress") {
		startedAt := extractMaintenanceStartedAt(detail)
		return &MaintenanceInProgressError{StartedAt: startedAt, msg: detail}
	}
	// Generic 5xx (500/501/502/504/... plus 503 without a cache_not_live,
	// store_slow, or maintenance_disabled prefix) wraps into a serverError so
	// read-path callers can classify it as fallbackable via
	// ShouldFallbackForRead. Mutation callers continue to see it as
	// non-fallbackable (ShouldFallback excludes it).
	if status >= 500 {
		msg := detail
		if msg == "" {
			msg = title
		}
		if msg == "" {
			return &serverError{status: status}
		}
		return &serverError{status: status, msg: fmt.Sprintf("API error: %s", msg)}
	}
	if detail != "" {
		return fmt.Errorf("API error: %s", detail)
	}
	if title != "" {
		return fmt.Errorf("API error: %s", title)
	}
	return fmt.Errorf("API returned %d", status)
}

// extractMaintenanceStartedAt parses the JSON body that the
// maintenance 409 handler appends after the "maintenance-in-progress: "
// prefix and returns the started_at field, or empty when absent or
// malformed. The server always emits this prefix via maintenanceConflictFromError,
// so a missing started_at means the in-flight run had a zero-value
// StartedAt (a race during supervisor startup) rather than a protocol
// violation.
func extractMaintenanceStartedAt(detail string) string {
	const prefix = "maintenance-in-progress: "
	idx := strings.Index(detail, prefix)
	if idx < 0 {
		return ""
	}
	payload := detail[idx+len(prefix):]
	var body struct {
		StartedAt string `json:"started_at"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		return ""
	}
	return body.StartedAt
}

// cityInfoFromGen copies the generated CityInfo (which uses pointer
// fields for omitempty semantics) into the local api.CityInfo
// (value-typed for callers' ergonomics).
func cityInfoFromGen(g genclient.CityInfo) CityInfo {
	out := CityInfo{
		Name:    g.Name,
		Path:    g.Path,
		Running: g.Running,
	}
	if g.Status != nil {
		out.Status = *g.Status
	}
	if g.Error != nil {
		out.Error = *g.Error
	}
	if g.PhasesCompleted != nil {
		out.PhasesCompleted = *g.PhasesCompleted
	}
	return out
}

// workspaceStatusFromGen copies a generated workspacesvc.Status into the
// local typed struct. Required fields are value-typed in the generated
// shape; optional fields are pointers.
func workspaceStatusFromGen(g genclient.Status) workspacesvc.Status {
	return workspacesvc.Status{
		ServiceName:      g.ServiceName,
		Kind:             derefStr(g.Kind),
		WorkflowContract: derefStr(g.WorkflowContract),
		MountPath:        g.MountPath,
		PublishMode:      g.PublishMode,
		Visibility:       derefStr(g.Visibility),
		Hostname:         derefStr(g.Hostname),
		StateRoot:        g.StateRoot,
		URL:              derefStr(g.Url),
		State:            derefStr(g.State),
		LocalState:       g.LocalState,
		PublicationState: g.PublicationState,
		Reason:           derefStr(g.Reason),
		AllowWebSockets:  derefBool(g.AllowWebsockets),
		UpdatedAt:        g.UpdatedAt,
	}
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

// ExtMsgBindSpec describes a bind or handoff request for an external
// conversation. Exactly one of SessionID and AgentName must be set;
// Replace rebinds a conversation whose active binding targets someone
// else instead of conflicting.
type ExtMsgBindSpec struct {
	Conversation extmsg.ConversationRef
	SessionID    string
	AgentName    string
	Replace      bool
}

// BindExtMsgConversation binds an external conversation to a session or a
// configured agent via POST /v0/extmsg/bind and returns the resulting
// binding record.
func (c *Client) BindExtMsgConversation(spec ExtMsgBindSpec) (extmsg.SessionBindingRecord, error) {
	if err := c.requireCityScope(); err != nil {
		return extmsg.SessionBindingRecord{}, err
	}
	conv := genclientConversationRef(spec.Conversation)
	body := genclient.PostV0CityByCityNameExtmsgBindJSONRequestBody{Conversation: &conv}
	if spec.SessionID != "" {
		body.SessionId = &spec.SessionID
	}
	if spec.AgentName != "" {
		body.AgentName = &spec.AgentName
	}
	if spec.Replace {
		body.Replace = &spec.Replace
	}
	resp, err := c.cw.PostV0CityByCityNameExtmsgBindWithResponse(context.Background(), c.cityName, nil, body)
	if err != nil {
		return extmsg.SessionBindingRecord{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return extmsg.SessionBindingRecord{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return extmsg.SessionBindingRecord{}, err
	}
	if resp.JSON200 == nil {
		return extmsg.SessionBindingRecord{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	return extmsgBindingRecordFromWire(*resp.JSON200), nil
}

// UnbindExtMsgConversation removes active external-conversation bindings
// via POST /v0/extmsg/unbind, filtered by conversation and/or session ID
// and agent name. It returns the bindings that were ended.
func (c *Client) UnbindExtMsgConversation(conversation *extmsg.ConversationRef, sessionID, agentName string) ([]extmsg.SessionBindingRecord, error) {
	if err := c.requireCityScope(); err != nil {
		return nil, err
	}
	body := genclient.PostV0CityByCityNameExtmsgUnbindJSONRequestBody{}
	if conversation != nil {
		conv := genclientConversationRef(*conversation)
		body.Conversation = &conv
	}
	if sessionID != "" {
		body.SessionId = &sessionID
	}
	if agentName != "" {
		body.AgentName = &agentName
	}
	resp, err := c.cw.PostV0CityByCityNameExtmsgUnbindWithResponse(context.Background(), c.cityName, nil, body)
	if err != nil {
		return nil, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return nil, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return nil, err
	}
	if resp.JSON200 == nil || resp.JSON200.Unbound == nil {
		return nil, nil
	}
	out := make([]extmsg.SessionBindingRecord, 0, len(*resp.JSON200.Unbound))
	for _, record := range *resp.JSON200.Unbound {
		out = append(out, extmsgBindingRecordFromWire(record))
	}
	return out, nil
}

func genclientConversationRef(ref extmsg.ConversationRef) genclient.ConversationRef {
	out := genclient.ConversationRef{
		ScopeId:        ref.ScopeID,
		Provider:       ref.Provider,
		AccountId:      ref.AccountID,
		ConversationId: ref.ConversationID,
		Kind:           genclient.ConversationKind(ref.Kind),
	}
	if ref.ParentConversationID != "" {
		out.ParentConversationId = &ref.ParentConversationID
	}
	return out
}

func extmsgBindingRecordFromWire(record genclient.SessionBindingRecord) extmsg.SessionBindingRecord {
	return extmsg.SessionBindingRecord{
		ID:            record.ID,
		SchemaVersion: int(record.SchemaVersion),
		Conversation: extmsg.ConversationRef{
			ScopeID:              record.Conversation.ScopeId,
			Provider:             record.Conversation.Provider,
			AccountID:            record.Conversation.AccountId,
			ConversationID:       record.Conversation.ConversationId,
			ParentConversationID: derefStr(record.Conversation.ParentConversationId),
			Kind:                 extmsg.ConversationKind(record.Conversation.Kind),
		},
		SessionID:         record.SessionID,
		SessionName:       record.SessionName,
		AgentName:         record.AgentName,
		Status:            extmsg.BindingStatus(record.Status),
		BoundAt:           record.BoundAt,
		ExpiresAt:         record.ExpiresAt,
		BindingGeneration: record.BindingGeneration,
		Metadata:          record.Metadata,
	}
}
