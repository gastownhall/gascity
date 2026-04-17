// CLI client for the Gas City API. Phase 3 Fix 3a.
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
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
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

// Client is an HTTP client for the Gas City API server. It wraps the
// generated typed client so CLI commands can route writes through the API
// when a controller is running.
type Client struct {
	cw       *genclient.ClientWithResponses
	cityName string // non-empty for city-scoped clients; passed to every per-city call
}

// SessionSubmitResponse is the domain-facing shape of POST
// /v0/city/{cityName}/session/{id}/submit's 202 body. It intentionally
// shadows genclient.SessionSubmitOutputBody instead of re-exporting it
// so callers in cmd/gc do not depend on the generated-client package:
//
//  1. regenerating the client (different tool, renamed field) would
//     otherwise force a cascading edit across every CLI command; and
//  2. the wire uses a string for Intent but the domain uses the typed
//     session.SubmitIntent — this wrapper does the conversion in one
//     place and lets callers work with the strong type.
type SessionSubmitResponse struct {
	Status string               `json:"status"`
	ID     string               `json:"id"`
	Queued bool                 `json:"queued"`
	Intent session.SubmitIntent `json:"intent"`
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
		return &Client{}
	}
	return &Client{cw: cw, cityName: cityName}
}

// --- Lookup methods ---

// ListCities fetches the current set of cities managed by the supervisor.
func (c *Client) ListCities() ([]CityInfo, error) {
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
	if c.cw == nil {
		return nil, errClientUninitialized
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
	if c.cw == nil {
		return workspacesvc.Status{}, errClientUninitialized
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
	if c.cw == nil {
		return errClientUninitialized
	}
	resp, err := c.cw.PostV0CityByCityNameServiceByNameRestartWithResponse(context.Background(), c.cityName, name)
	return checkMutation(resp, err)
}

// SuspendCity suspends the city via PATCH /v0/city.
func (c *Client) SuspendCity() error { return c.patchCity(true) }

// ResumeCity resumes the city via PATCH /v0/city.
func (c *Client) ResumeCity() error { return c.patchCity(false) }

func (c *Client) patchCity(suspend bool) error {
	if c.cw == nil {
		return errClientUninitialized
	}
	resp, err := c.cw.PatchV0CityByCityNameWithResponse(context.Background(), c.cityName, genclient.PatchV0CityByCityNameJSONRequestBody{Suspended: &suspend})
	return checkMutation(resp, err)
}

// SuspendAgent suspends an agent via POST /v0/agent/{name}/suspend.
// Name can be qualified (e.g., "myrig/worker") — the server route uses
// {name...} which captures path segments including slashes.
func (c *Client) SuspendAgent(name string) error {
	return c.postAgentAction(name, "suspend")
}

// ResumeAgent resumes an agent via POST /v0/agent/{name}/resume.
func (c *Client) ResumeAgent(name string) error {
	return c.postAgentAction(name, "resume")
}

func (c *Client) postAgentAction(name, action string) error {
	if c.cw == nil {
		return errClientUninitialized
	}
	// The server's {name...} captures the full suffix including the
	// trailing /suspend or /resume; the handler strips it. We pass the
	// composite path as the "name" parameter to the generated client.
	resp, err := c.cw.PostV0CityByCityNameAgentByNameWithResponse(context.Background(), c.cityName, name+"/"+action)
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
	if c.cw == nil {
		return errClientUninitialized
	}
	resp, err := c.cw.PostV0CityByCityNameRigByNameByActionWithResponse(context.Background(), c.cityName, name, action)
	return checkMutation(resp, err)
}

// KillSession force-kills a session via POST /v0/session/{id}/kill.
func (c *Client) KillSession(id string) error {
	if c.cw == nil {
		return errClientUninitialized
	}
	resp, err := c.cw.PostV0CityByCityNameSessionByIdKillWithResponse(context.Background(), c.cityName, id)
	return checkMutation(resp, err)
}

// SubmitSession sends a semantic submit request to a session. The id may
// be either a bead ID or a resolvable session alias/name.
func (c *Client) SubmitSession(id, message string, intent session.SubmitIntent) (SessionSubmitResponse, error) {
	if c.cw == nil {
		return SessionSubmitResponse{}, errClientUninitialized
	}
	body := genclient.SubmitSessionJSONRequestBody{Message: message}
	if intent != "" {
		i := genclient.SessionSubmitInputBodyIntent(intent)
		body.Intent = &i
	}
	resp, err := c.cw.SubmitSessionWithResponse(context.Background(), c.cityName, id, body)
	if err != nil {
		return SessionSubmitResponse{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return SessionSubmitResponse{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), resp.ApplicationproblemJSONDefault); err != nil {
		return SessionSubmitResponse{}, err
	}
	// SubmitSession returns 202 Accepted on success.
	if resp.JSON202 == nil {
		return SessionSubmitResponse{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	out := SessionSubmitResponse{
		Status: resp.JSON202.Status,
		ID:     resp.JSON202.Id,
		Queued: resp.JSON202.Queued,
	}
	if resp.JSON202.Intent != "" {
		out.Intent = session.SubmitIntent(resp.JSON202.Intent)
	}
	return out, nil
}

// --- Adapter helpers ---

var errClientUninitialized = errors.New("api client not initialized")

// escapeName preserves slashes in qualified names for use in URL paths.
// The generated client's path-param escaping percent-encodes slashes;
// callers that want literal slashes (e.g., the city scope prefix) use
// this helper to keep them intact.
func escapeName(name string) string { return name }

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
