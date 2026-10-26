package api

// Session-related Huma input/output types.
//
// Extracted from huma_types.go to reduce file size and improve navigation.
// These types drive the OpenAPI spec for all /v0/session* endpoints.

import (
	"github.com/gastownhall/gascity/internal/session"
)

// SessionListInput is the Huma input for GET /v0/city/{cityName}/sessions.
// Keyset cursors made the old "cursor present but empty" distinction moot: an
// empty cursor is first-page paging, anything else must be a valid v1 token.
type SessionListInput struct {
	CityScope
	PaginationParam
	State    string `query:"state" required:"false" doc:"Filter by session state (e.g. active, closed)."`
	Template string `query:"template" required:"false" doc:"Filter by session template (agent qualified name)."`
	Peek     bool   `query:"peek" required:"false" doc:"Include last output preview."`
}

// CityPendingInput is the Huma input for GET /v0/city/{cityName}/pending.
type CityPendingInput struct {
	CityScope
}

// cityPendingEntry is one active session awaiting a human decision in the
// city-wide pending aggregate. It carries ids only; consumers fetch the full
// interaction via the per-session GET /v0/city/{cityName}/session/{id}/pending
// endpoint, keeping this snapshot minimal.
type cityPendingEntry struct {
	SessionID string `json:"session_id" doc:"Session ID awaiting a human decision."`
	RequestID string `json:"request_id" doc:"Pending interaction request ID."`
	Kind      string `json:"kind" doc:"Pending interaction kind (e.g. tool-approval, prompt-for-input)."`
}

// SessionGetInput is the Huma input for GET /v0/city/{cityName}/session/{id}.
type SessionGetInput struct {
	CityScope
	ID        string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Peek      bool   `query:"peek" required:"false" doc:"Include last output preview."`
	PeekLines int    `query:"peek_lines" required:"false" minimum:"0" maximum:"10000" doc:"Number of lines to include in the last output preview when peek=true. Defaults to 5."`
	ExactID   bool   `query:"exact_id" required:"false" doc:"Resolve {id} as an exact session bead id only: a single point read that also finds closed sessions and answers 404 when no bead has that id. Skips the alias, runtime session_name, configured-name and closed-session name lookups. For callers holding a durable id."`
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
	Metadata          map[string]string `json:"metadata,omitempty" doc:"External client metadata: real_world_app_* keys only (any other key is refused with 422). The generic bead API never writes a session row."`
}

// SessionCreateInput is the Huma input for POST /v0/city/{cityName}/sessions.
type SessionCreateInput struct {
	CityScope
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           sessionCreateBody
}

// asyncAcceptedBody is the response body for all async session 202 responses.
type asyncAcceptedBody struct {
	Status      string `json:"status" doc:"Async request status." example:"accepted"`
	RequestID   string `json:"request_id" doc:"Opaque correlation ID; clients must not parse it. When the server recorded the request as an operation it is the operation id and equals op.id, and a retry with the same Idempotency-Key against the same session gets the same request_id from either /submit or /messages. Watch the city event stream for request.result.session.create, request.result.session.message, request.result.session.submit, or request.failed with this request_id."`
	EventCursor string `json:"event_cursor" doc:"City event-stream sequence captured before the async request was accepted. Pass this value as after_seq to /v0/city/{cityName}/events/stream to receive the request result without replaying unrelated historical backlog, and to /v0/city/{cityName}/request/{requestId} to read its outcome. A value of 0 means the event log was empty."`
	// IdempotencyKey, Replayed and Op are set only when the server recorded
	// the request as an operation (an ops gate is auto or require).
	IdempotencyKey string          `json:"idempotency_key,omitempty" doc:"The Idempotency-Key the request was recorded under: the client's, or one the server minted when the request carried none. Retry with it to get this request's outcome instead of a second delivery."`
	Replayed       bool            `json:"replayed,omitempty" doc:"True when this answers a retry of a request that already finished: nothing was executed again, op carries the stored outcome, and the terminal event is emitted again after event_cursor. The retry's own options, resume included, took no effect, as that event's message says; to start the session, wake it, and to send again, use a new Idempotency-Key."`
	Op             *ConversationOp `json:"op,omitempty" doc:"The operation the request was recorded as."`
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

// SessionKillInput is the Huma input for POST
// /v0/city/{cityName}/session/{id}/kill.
type SessionKillInput struct {
	CityScope
	ID             string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Force          bool   `query:"force" required:"false" doc:"Kill even when the session has live background subagents, or past a hung holder of its runtime lease whose record expired, as gc session kill --force does."`
}

// SessionControlInput is the Huma input for body-less session control and
// lifecycle endpoints that honor Idempotency-Key (POST .../session/{id}/wake,
// .../suspend and .../reset).
type SessionControlInput struct {
	CityScope
	ID             string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
}

// SessionStopBody is the optional request body for POST
// .../session/{id}/stop (X11 D10). It admits fields it does not know, as the
// endpoint did before it declared a body: a client that sends one (T3's
// planned intent, BUILD-PLAN FK-1) is not refused, and the field is ignored.
type SessionStopBody struct {
	_    struct{} `json:"-" additionalProperties:"true"`
	Turn string   `json:"turn,omitempty" maxLength:"128" doc:"The turn to stop, as the client saw it (a turn id the session's runtime reported). Only that turn is interrupted: when another turn, or none, is running by then, nothing is sent and the request is refused with 409 turn_not_current. A runtime that does not record turn identity refuses a named turn with 501 turn_identity_unsupported. Omit it to stop whatever turn is running."`
}

// SessionStopInput is the Huma input for POST
// /v0/city/{cityName}/session/{id}/stop.
type SessionStopInput struct {
	CityScope
	ID             string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           *SessionStopBody
}

// SessionTranscriptInput is the Huma input for GET /v0/city/{cityName}/session/{id}/transcript.
type SessionTranscriptInput struct {
	CityScope
	TailParam
	ID              string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Format          string `query:"format" required:"false" enum:"conversation,raw,structured" doc:"Transcript format: conversation (default), raw, or structured."`
	IncludeThinking bool   `query:"include_thinking" required:"false" doc:"Include thinking block text and signature in structured responses. Defaults to false; both are redacted otherwise."`
	Before          string `query:"before" required:"false" doc:"Pagination cursor: return entries before this stable transcript entry ID."`
	After           string `query:"after" required:"false" doc:"Pagination cursor: return entries after this stable transcript entry ID."`
}

// SessionStreamInput is the Huma input for GET /v0/city/{cityName}/session/{id}/stream.
type SessionStreamInput struct {
	CityScope
	ID              string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Format          string `query:"format" required:"false" enum:"conversation,raw,structured" doc:"Transcript format: conversation (default), raw, or structured."`
	IncludeThinking bool   `query:"include_thinking" required:"false" doc:"Include thinking block text and signature in structured stream frames. Defaults to false; both are redacted otherwise."`
	AfterCursor     string `query:"after_cursor" required:"false" maxLength:"2048" doc:"Opaque structured transcript resume cursor from the REST snapshot. Last-Event-ID takes precedence on automatic SSE reconnect."`
	LastEventID     string `header:"Last-Event-ID" required:"false" maxLength:"2048" doc:"Opaque structured transcript resume cursor from the last received SSE frame. Takes precedence over after_cursor."`

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
	// Metadata is the external client's real_world_app_* keys, the only
	// session metadata a client writes (architecture B).
	Metadata map[string]string `json:"metadata,omitempty" doc:"External client metadata: real_world_app_* keys only (any other key is refused with 422); an empty value clears the key."`
}

// SessionPatchInput is the Huma input for PATCH /v0/city/{cityName}/session/{id}.
type SessionPatchInput struct {
	CityScope
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body SessionPatchBody
}

// SessionPermissionModeBody is the request body for updating the
// schema-backed permission_mode option on a session.
type SessionPermissionModeBody struct {
	_              struct{} `json:"-" additionalProperties:"false"`
	PermissionMode string   `json:"permission_mode" minLength:"1" pattern:"\\S" doc:"Provider schema value for the permission_mode option."`
}

// SessionPermissionModeInput is the Huma input for POST /v0/city/{cityName}/session/{id}/permission-mode.
type SessionPermissionModeInput struct {
	CityScope
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Body SessionPermissionModeBody
}

// SessionCloseInput is the Huma input for POST /v0/city/{cityName}/session/{id}/close.
type SessionCloseInput struct {
	CityScope
	ID             string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Delete         bool   `query:"delete" required:"false" doc:"Permanently delete bead after closing. Part of the request an Idempotency-Key names: a same-key retry that changes it is 422, after an API restart too while the close is recorded as an operation, and a replay never deletes. After a restart a same-key retry finds the recorded close by the session ID only: by an alias or session_name, or after a completed delete, it is 404."`
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
}

// SessionSubmitInput is the Huma input for POST /v0/city/{cityName}/session/{id}/submit.
type SessionSubmitInput struct {
	CityScope
	ID             string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           struct {
		Message string               `json:"message" minLength:"1" pattern:"\\S" doc:"Message text to submit."`
		Intent  session.SubmitIntent `json:"intent,omitempty" enum:"default,follow_up,interrupt_now" doc:"Submit intent; empty defaults to \"default\"."`
		Resume  bool                 `json:"resume,omitempty" doc:"Start or resume the session here if it is not running (an operator's own message; it consumes an operator's hold). Without it, a message to a session that is not running is queued for its next run (expiring after 24h) and the controller is asked to start it; the result's will_start says whether it will."`
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
	ID             string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           struct {
		Message string `json:"message" minLength:"1" pattern:"\\S" doc:"Message text to send."`
		Resume  bool   `json:"resume,omitempty" doc:"Start or resume the session here if it is not running (an operator's own message; it consumes an operator's hold). Without it, a message to a session that is not running is queued for its next run (expiring after 24h) and the controller is asked to start it; the result's will_start says whether it will."`
	}
}

// SessionMessageOutput is the Huma output for POST /v0/session/{id}/messages.
type SessionMessageOutput struct {
	Body asyncAcceptedBody
}

// SessionRespondInput is the Huma input for POST /v0/city/{cityName}/session/{id}/respond.
type SessionRespondInput struct {
	CityScope
	ID             string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           struct {
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
		// IdempotencyKey, Replayed and Op are set only when the server
		// recorded the request as an operation (ops.control is auto or
		// require).
		IdempotencyKey string          `json:"idempotency_key,omitempty" doc:"The key the request was recorded under: the client's Idempotency-Key, or one the server minted when the request carried none. Retry with it to get this request's outcome instead of a second response."`
		Replayed       bool            `json:"replayed,omitempty" doc:"True when this answers a retry of a request that already finished: nothing was executed again, and op carries the stored outcome."`
		Op             *ConversationOp `json:"op,omitempty" doc:"The operation the request was recorded as."`
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

// OKWithIDResponse is the success response of the session control and
// lifecycle commands that name the session they acted on (POST
// .../session/{id}/stop, .../wake, .../kill and .../reset).
type OKWithIDResponse struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"ok"`
		ID     string `json:"id,omitempty" doc:"Resource ID."`
		// IdempotencyKey, Replayed and Op are set only when the server
		// recorded the request as an operation (the command's ops gate is
		// auto or require).
		IdempotencyKey string          `json:"idempotency_key,omitempty" doc:"The key the request was recorded under: the client's Idempotency-Key, one the server minted when the request carried none, or, for a stop of a known turn, stop:<generation>:<turn>, so two stops of one turn are one operation. Retry with the Idempotency-Key to get this request's outcome instead of acting again."`
		Replayed       bool            `json:"replayed,omitempty" doc:"True when this answers a retry of a request that already finished: nothing was executed again, and op carries the stored outcome. A replayed stop never interrupts a later turn."`
		Op             *ConversationOp `json:"op,omitempty" doc:"The operation the request was recorded as."`
	}
}

// SessionCommandStatusBody is the success body of the session lifecycle
// commands that report only a status (POST .../session/{id}/suspend and
// .../close).
type SessionCommandStatusBody struct {
	Status string `json:"status" doc:"Operation result." example:"ok"`
	// IdempotencyKey, Replayed and Op are set only when the server recorded
	// the request as an operation (ops.lifecycle is auto or require).
	IdempotencyKey string          `json:"idempotency_key,omitempty" doc:"The key the request was recorded under: the client's Idempotency-Key, or one the server minted when the request carried none. Retry with it to get this request's outcome instead of acting again."`
	Replayed       bool            `json:"replayed,omitempty" doc:"True when this answers a retry of a request that already finished: nothing was executed again, and op carries the stored outcome."`
	Op             *ConversationOp `json:"op,omitempty" doc:"The operation the request was recorded as."`
}

// SessionCommandStatusOutput is the Huma output of the session commands
// whose body is a SessionCommandStatusBody.
type SessionCommandStatusOutput struct {
	Body SessionCommandStatusBody
}
