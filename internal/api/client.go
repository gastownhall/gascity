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
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/events"
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

// ShouldFallback reports whether err indicates the CLI should fall back to
// direct file mutation. This is true for transport-level failures (connection
// refused, timeout), read-only API rejections (server bound to non-localhost,
// mutations disabled), and client-init failures (malformed base URL).
func ShouldFallback(err error) bool {
	if IsConnError(err) {
		return true
	}
	var ro *readOnlyError
	if errors.As(err, &ro) {
		return true
	}
	var ci *clientInitError
	return errors.As(err, &ci)
}

// Client is an HTTP client for the Gas City API server. It wraps the
// generated typed client so CLI commands can route writes through the API
// when a controller is running.
type Client struct {
	cw       *genclient.ClientWithResponses
	baseURL  string // stored for SSE stream connections
	cityName string // non-empty for city-scoped clients; passed to every per-city call
	initErr  error  // set when NewClient failed to build the transport (malformed baseURL, etc.)
}

const sessionMessageTimeout = 4 * time.Minute

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

// sseEnvelope is the JSON envelope of a typed event on the stream.
type sseEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// waitForEvent connects to the appropriate SSE stream, reads frames
// until it finds an event matching the given request_id (in success or
// failure payloads), and returns the envelope. The caller decodes the
// typed payload.
func (c *Client) waitForEvent(ctx context.Context, requestID string, successType, failOp, eventCursor string) (*sseEnvelope, error) {
	streamURL := c.baseURL + "/v0/events/stream"
	cursor := strings.TrimSpace(eventCursor)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-GC-Request", "true")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("SSE connect: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			detail = resp.Status
		}
		return nil, fmt.Errorf("SSE connect failed: %s: %s", resp.Status, detail)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var current sseEvent
	for scanner.Scan() {
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
			var env sseEnvelope
			if err := json.Unmarshal([]byte(current.Data), &env); err != nil {
				return nil, fmt.Errorf("decode SSE event: %w", err)
			}
			if env.Type == successType {
				matches, err := payloadContainsRequestID(env.Payload, requestID)
				if err != nil {
					return nil, fmt.Errorf("decode %s payload: %w", successType, err)
				}
				if matches {
					return &env, nil
				}
			}
			if env.Type == events.RequestFailed {
				matches, err := payloadMatchesRequest(env.Payload, requestID, failOp)
				if err != nil {
					return nil, fmt.Errorf("decode %s payload: %w", events.RequestFailed, err)
				}
				if matches {
					return &env, nil
				}
			}
			current = sseEvent{}
		}
	}
	if err := scanner.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("SSE scan: %w", err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return nil, fmt.Errorf("SSE stream closed before event for %s arrived", requestID)
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
	httpClient := &http.Client{Timeout: 10 * time.Second}
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
	if err := apiErrorFromResponse(resp.StatusCode(), resp.ApplicationproblemJSONDefault); err != nil {
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
	if err := apiErrorFromResponse(resp.StatusCode(), resp.ApplicationproblemJSONDefault); err != nil {
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
	if err := apiErrorFromResponse(resp.StatusCode(), resp.ApplicationproblemJSONDefault); err != nil {
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
	resp, err := c.cw.PostV0CityByCityNameRigByNameByActionWithResponse(context.Background(), c.cityName, name, action, nil)
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
	if err := apiErrorFromResponse(resp.StatusCode(), resp.ApplicationproblemJSONDefault); err != nil {
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
	return rv.Kind() == reflect.Ptr && rv.IsNil()
}

// pdOf extracts the generated client's decoded Problem Details pointer
// from any generated *WithResponse type. Every response wrapper has an
// `ApplicationproblemJSONDefault *ErrorModel` field produced by
// oapi-codegen from the spec's default `application/problem+json`
// response. Returns nil when the field is absent (no operation without
// the default response has been observed; the nil-safe return is
// defensive) or unpopulated (2xx, non-JSON error).
//
// This is spec-driven: the field exists because the spec declares the
// default error to be Problem Details, and the generator decoded it.
// No hand-written JSON parsing happens here or downstream.
func pdOf(resp any) *genclient.ErrorModel {
	if resp == nil {
		return nil
	}
	rv := reflect.ValueOf(resp)
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	f := rv.FieldByName("ApplicationproblemJSONDefault")
	if !f.IsValid() {
		return nil
	}
	pd, _ := f.Interface().(*genclient.ErrorModel)
	return pd
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
	if detail != "" {
		return fmt.Errorf("API error: %s", detail)
	}
	if title != "" {
		return fmt.Errorf("API error: %s", title)
	}
	return fmt.Errorf("API returned %d", status)
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
