package api

// Session-related Huma input/output types.
//
// Extracted from huma_types.go to reduce file size and improve navigation.
// These types drive the OpenAPI spec for all /v0/session* endpoints.

import (
	"encoding/json"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/session"
)

// SessionListInput is the Huma input for GET /v0/sessions.
type SessionListInput struct {
	PaginationParam
	State    string `query:"state" required:"false" doc:"Filter by session state (e.g. active, closed)."`
	Template string `query:"template" required:"false" doc:"Filter by session template (agent qualified name)."`
	Peek     string `query:"peek" required:"false" doc:"Include last output preview (true/false)."`

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

// SessionGetInput is the Huma input for GET /v0/session/{id}.
type SessionGetInput struct {
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Peek string `query:"peek" required:"false" doc:"Include last output preview (true/false)."`
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

// SessionCreateInput is the Huma input for POST /v0/sessions.
type SessionCreateInput struct {
	Body sessionCreateBody
}

// SessionCreateOutput is the Huma output for POST /v0/sessions.
// Status allows the handler to return different HTTP status codes:
// 201 Created for provider sessions, 202 Accepted for agent sessions.
type SessionCreateOutput struct {
	Status int `json:"-"`
	Body   sessionResponse
}

// SessionIDInput is a generic Huma input for session endpoints that only need {id}.
type SessionIDInput struct {
	ID string `path:"id" doc:"Session ID, alias, or runtime session_name."`
}

// SessionTranscriptInput is the Huma input for GET /v0/session/{id}/transcript.
type SessionTranscriptInput struct {
	ID     string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Format string `query:"format" required:"false" doc:"Transcript format: conversation (default) or raw."`
	Tail   string `query:"tail" required:"false" doc:"Number of recent entries to return."`
	Before string `query:"before" required:"false" doc:"Pagination cursor: return entries before this UUID."`
}

// SessionStreamInput is the Huma input for GET /v0/session/{id}/stream.
type SessionStreamInput struct {
	ID     string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Format string `query:"format" required:"false" doc:"Transcript format: conversation (default) or raw."`
}

// SessionPatchInput is the Huma input for PATCH /v0/session/{id}.
// The body uses json.RawMessage so the handler can detect immutable fields
// (like "template") before Huma's strict struct validation rejects them.
type SessionPatchInput struct {
	ID   string          `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body json.RawMessage `doc:"JSON object with title and/or alias fields."`
}

// SessionCloseInput is the Huma input for POST /v0/session/{id}/close.
type SessionCloseInput struct {
	ID     string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Delete string `query:"delete" required:"false" doc:"Permanently delete bead after closing (true/false)."`
}

// SessionSubmitInput is the Huma input for POST /v0/session/{id}/submit.
type SessionSubmitInput struct {
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body struct {
		Message string               `json:"message,omitempty" doc:"Message text to submit."`
		Intent  session.SubmitIntent `json:"intent,omitempty" doc:"Submit intent: default, follow-up, or interrupt-now."`
	}
}

// SessionSubmitOutput is the Huma output for POST /v0/session/{id}/submit.
type SessionSubmitOutput struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"accepted"`
		ID     string `json:"id" doc:"Session ID."`
		Queued bool   `json:"queued" doc:"Whether the message was queued."`
		Intent string `json:"intent" doc:"Resolved submit intent."`
	}
}

// SessionMessageInput is the Huma input for POST /v0/session/{id}/messages.
type SessionMessageInput struct {
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body struct {
		Message string `json:"message,omitempty" doc:"Message text to send."`
	}
}

// SessionMessageOutput is the Huma output for POST /v0/session/{id}/messages.
type SessionMessageOutput struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"accepted"`
		ID     string `json:"id" doc:"Session ID."`
	}
}

// SessionRespondInput is the Huma input for POST /v0/session/{id}/respond.
type SessionRespondInput struct {
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body struct {
		RequestID string            `json:"request_id,omitempty" doc:"Pending interaction request ID."`
		Action    string            `json:"action,omitempty" doc:"Response action (e.g. allow, deny)."`
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

// SessionRenameInput is the Huma input for POST /v0/session/{id}/rename.
type SessionRenameInput struct {
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body struct {
		Title string `json:"title,omitempty" doc:"New session title."`
	}
}

// SessionAgentGetInput is the Huma input for GET /v0/session/{id}/agents/{agentId}.
type SessionAgentGetInput struct {
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
