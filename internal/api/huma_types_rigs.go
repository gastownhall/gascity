package api

// Per-domain Huma input/output types for the rigs handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_rigs.go.

// --- Rig types ---

// RigListInput is the Huma input for GET /v0/city/{cityName}/rigs.
type RigListInput struct {
	CityScope
	BlockingParam
	Git bool `query:"git" required:"false" doc:"Include git status."`
}

// RigGetInput is the Huma input for GET /v0/city/{cityName}/rig/{name}.
type RigGetInput struct {
	CityScope
	Name string `path:"name" doc:"Rig name."`
	Git  bool   `query:"git" required:"false" doc:"Include git status."`
}

// RigCreateInput is the Huma input for POST /v0/city/{cityName}/rigs. Body is
// the shared RigCreateBody (owned by the idempotency slice) so the request_id
// digest is computed over the exact wire body. Path is optional at the schema
// level because a git_url clone derives it server-side; the sync (git_url
// absent) branch enforces path presence in the handler, preserving the prior
// 422-on-missing-path contract. The Idempotency-Key header applies the S2
// create-idempotency contract to the synchronous (git_url-absent) path; the
// async git_url path carries its own idempotency via the request_id admission
// state machine.
type RigCreateInput struct {
	CityScope
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries (synchronous create)."`
	Body           RigCreateBody
}

// RigCreateOutput is the unified Huma output for POST /v0/city/{cityName}/rigs.
// Huma binds exactly one output type per operation, so the three success shapes
// (201 created / 202 accepted / 200 exists) share one struct discriminated by
// Body.Status. Status is the runtime HTTP code Huma reads from the json:"-"
// field, mirroring SessionCreateOutput.
type RigCreateOutput struct {
	Status int `json:"-"` // runtime code: 200 | 201 | 202
	Body   RigCreateResponseBody
}

// RigCreateResponseBody is the union success body for rig create. Fields not
// relevant to a given status are omitted (omitempty).
type RigCreateResponseBody struct {
	Status        string `json:"status" enum:"created,accepted,exists" doc:"created (201 sync), accepted (202 async provisioning), exists (200 idempotent replay)."`
	Rig           string `json:"rig,omitempty" doc:"Rig name (created/exists)."`
	RequestID     string `json:"request_id,omitempty" doc:"Correlation ID; echo of the request's request_id, or a server-minted id on 202."`
	EventCursor   string `json:"event_cursor,omitempty" doc:"City event-stream cursor captured before accept (202 only); pass as after_seq to the events stream to receive request.result.rig.create / rig.provision.progress / request.failed without replaying unrelated backlog."`
	Prefix        string `json:"prefix,omitempty" doc:"Resolved session-name prefix (created/exists)."`
	DefaultBranch string `json:"default_branch,omitempty" doc:"Resolved mainline branch (created/exists)."`
}

// RigUpdateInput is the Huma input for PATCH /v0/city/{cityName}/rig/{name}.
type RigUpdateInput struct {
	CityScope
	Name string `path:"name" doc:"Rig name."`
	Body struct {
		Path          string `json:"path,omitempty" doc:"Filesystem path."`
		Prefix        string `json:"prefix,omitempty" doc:"Session name prefix."`
		DefaultBranch string `json:"default_branch,omitempty" doc:"Mainline branch (e.g. main, master)."`
		Suspended     *bool  `json:"suspended,omitempty" doc:"Whether rig is suspended."`
	}
}

// RigDeleteInput is the Huma input for DELETE /v0/city/{cityName}/rig/{name}.
type RigDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Rig name."`
}

// RigActionInput is the Huma input for POST /v0/city/{cityName}/rig/{name}/{action}.
type RigActionInput struct {
	CityScope
	Name   string `path:"name" doc:"Rig name."`
	Action string `path:"action" enum:"suspend,resume,restart" doc:"Action to perform."`
}

// RigActionResponse is the response for rig actions (suspend/resume/restart).
type RigActionResponse struct {
	Body RigActionBody
}

// RigActionBody is the JSON body for rig action responses.
type RigActionBody struct {
	Status string   `json:"status" doc:"Operation result (ok, partial, failed)." example:"ok"`
	Action string   `json:"action" doc:"Action that was performed."`
	Rig    string   `json:"rig" doc:"Rig name."`
	Killed []string `json:"killed,omitempty" doc:"Agents that were killed (restart only)."`
	Failed []string `json:"failed,omitempty" doc:"Agents that failed to stop (restart only)."`
}
