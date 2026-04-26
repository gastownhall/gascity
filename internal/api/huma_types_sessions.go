package api

// Session-related Huma input/output types.
//
// Extracted from huma_types.go to reduce file size and improve navigation.
// These types drive the OpenAPI spec for all /v0/session* endpoints.

import (
	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/session"
)

// SessionListInput is the Huma input for GET /v0/city/{cityName}/sessions.
type SessionListInput struct {
	CityScope
	PaginationParam
	State    string `query:"state" required:"false" doc:"Filter by session state (e.g. active, closed)."`
	Template string `query:"template" required:"false" doc:"Filter by session template (agent qualified name)."`
	Peek     bool   `query:"peek" required:"false" doc:"Include last output preview."`

	// cursorPresent is set by Resolve to distinguish "cursor absent" from
	// "cursor present but empty" in the query string. Huma gives "" for both.
	cursorPresent bool
}

// Resolve implements huma.Resolver to detect whether the cursor query
// parameter was explicitly provided (even as an empty string).
func (s *SessionListInput) Resolve(ctx huma.Context) []error {
	// huma.Context.URL() returns the parsed URL; check raw query for cursor key.
	u := ctx.URL()
	s.cursorPresent = u.Query().Has("cursor")
	return nil
}

// SessionGetInput is the Huma input for GET /v0/city/{cityName}/session/{id}.
type SessionGetInput struct {
	CityScope
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Peek bool   `query:"peek" required:"false" doc:"Include last output preview."`
}

// sessionCreateBody is the request body for POST /v0/sessions.
type sessionCreateBody struct {
	Kind              string            `json:"kind,omitempty" doc:"Session target kind: agent or provider."`
	Name              string            `json:"name,omitempty" doc:"Agent or provider name."`
	Alias             string            `json:"alias,omitempty" doc:"Optional session alias."`
	LegacySessionName *string           `json:"session_name,omitempty" doc:"Deprecated: use alias."`
	Message           string            `json:"message,omitempty" doc:"Initial message to send to the session."`
	Async             bool              `json:"async,omitempty" doc:"Create session asynchronously (agent only)."`
	Options           map[string]string `json:"options,omitempty" doc:"Provider/agent option overrides."`
	ProjectID         string            `json:"project_id,omitempty" doc:"Opaque project context identifier."`
	Title             string            `json:"title,omitempty" doc:"Session title."`
}

// SessionCreateInput is the Huma input for POST /v0/city/{cityName}/sessions.
type SessionCreateInput struct {
	CityScope
	Body sessionCreateBody
}

// asyncAcceptedBody is the response body for all async 202 responses.
// Contains only request_id for correlation — no domain data.
type asyncAcceptedBody struct {
	RequestID string `json:"request_id" doc:"Correlation ID. Watch /v0/events/stream for a request.result event with this request_id."`
}

// SessionCreateOutput is the Huma output for POST /v0/sessions.
type SessionCreateOutput struct {
	Status int `json:"-"`
	Body   asyncAcceptedBody
}

// SessionIDInput is a generic Huma input for session endpoints that only need {cityName}+{id}.
type SessionIDInput struct {
	CityScope
	ID string `path:"id" doc:"Session ID, alias, or runtime session_name."`
}

// SessionTranscriptInput is the Huma input for GET /v0/city/{cityName}/session/{id}/transcript.
type SessionTranscriptInput struct {
	CityScope
	TailParam
	ID     string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Format string `query:"format" required:"false" doc:"Transcript format: conversation (default) or raw."`
	Before string `query:"before" required:"false" doc:"Pagination cursor: return entries before this UUID."`
	After  string `query:"after" required:"false" doc:"Pagination cursor: return entries after this UUID."`
}

// SessionStreamInput is the Huma input for GET /v0/city/{cityName}/session/{id}/stream.
type SessionStreamInput struct {
	CityScope
	ID     string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Format string `query:"format" required:"false" doc:"Transcript format: conversation (default) or raw."`

	resolved *sessionStreamState
}

// SessionPatchBody is the request body for PATCH /v0/session/{id}.
//
// Title and Alias are pointers so the handler can distinguish "absent"
// (nil) from "provided with empty value" (*""):
//   - Title: if provided, must be non-empty (enforced via minLength:"1").
//   - Alias: if provided, may be any string including empty; empty clears.
//
// The sentinel `additionalProperties:"false"` tag instructs Huma's schema
// to reject unknown fields at validation time. Before Fix 3f this handler
// used an opaque raw-JSON body + manual field whitelist to achieve the
// same effect; the typed version pushes that contract into the spec.
type SessionPatchBody struct {
	_     struct{} `json:"-" additionalProperties:"false"`
	Title *string  `json:"title,omitempty" minLength:"1" doc:"Session title. If provided, must be non-empty."`
	Alias *string  `json:"alias,omitempty" doc:"Session alias. Empty string clears the alias."`
}

// SessionPatchInput is the Huma input for PATCH /v0/city/{cityName}/session/{id}.
type SessionPatchInput struct {
	CityScope
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body SessionPatchBody
}

// SessionCloseInput is the Huma input for POST /v0/city/{cityName}/session/{id}/close.
type SessionCloseInput struct {
	CityScope
	ID     string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Delete bool   `query:"delete" required:"false" doc:"Permanently delete bead after closing."`
}

// SessionSubmitInput is the Huma input for POST /v0/city/{cityName}/session/{id}/submit.
type SessionSubmitInput struct {
	CityScope
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body struct {
		Message string               `json:"message" minLength:"1" pattern:"\\S" doc:"Message text to submit."`
		Intent  session.SubmitIntent `json:"intent,omitempty" enum:"default,follow_up,interrupt_now" doc:"Submit intent; empty defaults to \"default\"."`
	}
}

// SessionSubmitOutput is the Huma output for POST /v0/session/{id}/submit.
type SessionSubmitOutput struct {
	Body asyncAcceptedBody
}

// SessionMessageInput is the Huma input for POST /v0/city/{cityName}/session/{id}/messages.
// Pattern \S requires at least one non-whitespace character so that
// whitespace-only messages are rejected at the validation layer.
type SessionMessageInput struct {
	CityScope
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body struct {
		Message string `json:"message" minLength:"1" pattern:"\\S" doc:"Message text to send."`
	}
}

// SessionMessageOutput is the Huma output for POST /v0/session/{id}/messages.
type SessionMessageOutput struct {
	Body asyncAcceptedBody
}

// SessionRespondInput is the Huma input for POST /v0/city/{cityName}/session/{id}/respond.
type SessionRespondInput struct {
	CityScope
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body struct {
		RequestID string            `json:"request_id,omitempty" doc:"Pending interaction request ID (optional)."`
		Action    string            `json:"action" minLength:"1" doc:"Response action (e.g. allow, deny)."`
		Text      string            `json:"text,omitempty" doc:"Optional response text."`
		Metadata  map[string]string `json:"metadata,omitempty" doc:"Optional response metadata."`
	}
}

// SessionRespondOutput is the Huma output for POST /v0/session/{id}/respond.
type SessionRespondOutput struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"accepted"`
		ID     string `json:"id" doc:"Session ID."`
	}
}

// SessionRenameInput is the Huma input for POST /v0/city/{cityName}/session/{id}/rename.
type SessionRenameInput struct {
	CityScope
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body struct {
		Title string `json:"title" minLength:"1" doc:"New session title."`
	}
}

// SessionAgentGetInput is the Huma input for GET /v0/city/{cityName}/session/{id}/agents/{agentId}.
type SessionAgentGetInput struct {
	CityScope
	ID      string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	AgentID string `path:"agentId" doc:"Subagent ID within the session."`
}

// OKWithIDResponse is a success response with an ID field.
type OKWithIDResponse struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"ok"`
		ID     string `json:"id,omitempty" doc:"Resource ID."`
	}
}
