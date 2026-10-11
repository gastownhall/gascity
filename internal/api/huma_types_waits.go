package api

// Per-domain Huma input/output types for the durable-wait handler group.
// WaitView mirrors session.WaitInfo 1:1 so the wire projection carries exactly
// the bead-stored facts the CLI and dashboard render, with bead serialization
// confined to the client edge (client_waits.go) and the server handler
// (huma_handlers_waits.go).

// WaitView is the wire projection of a durable session wait. Optional fields
// carry omitempty so an unset value is absent rather than "" on the wire; the
// client edge (waitInfoFromGen) reconstitutes the session.WaitInfo.
type WaitView struct {
	ID              string   `json:"id" doc:"Wait bead ID."`
	SessionID       string   `json:"session_id" doc:"Session bead ID the wait is registered against."`
	SessionName     string   `json:"session_name,omitempty" doc:"Runtime session name recorded at registration."`
	Kind            string   `json:"kind" doc:"Wait kind, e.g. deps."`
	State           string   `json:"state" doc:"Wait lifecycle state (pending/ready/closed/...)."`
	DepIDs          []string `json:"dep_ids,omitempty" doc:"Dependency bead IDs the wait watches."`
	DepMode         string   `json:"dep_mode,omitempty" doc:"all or any."`
	RegisteredEpoch string   `json:"registered_epoch,omitempty" doc:"Session continuation epoch at registration."`
	DeliveryAttempt string   `json:"delivery_attempt,omitempty" doc:"Current delivery attempt counter."`
	NudgeID         string   `json:"nudge_id,omitempty" doc:"Shadow wait-nudge ID once dispatched."`
	ExpiresAt       string   `json:"expires_at,omitempty" doc:"Raw RFC3339 expiry string, kept verbatim."`
	Note            string   `json:"note,omitempty" doc:"Reminder text delivered when the wait is satisfied."`
	Status          string   `json:"status" doc:"Persisted bead status (open/closed)."`
	CreatedAt       string   `json:"created_at,omitempty" doc:"Bead creation time (RFC3339, UTC)."`
	Labels          []string `json:"labels,omitempty" doc:"Bead labels."`
}

// WaitListInput is the Huma input for GET /v0/city/{cityName}/waits.
type WaitListInput struct {
	CityScope
	State   string `query:"state" required:"false" doc:"Filter by wait state."`
	Session string `query:"session" required:"false" doc:"Filter by session ID."`
}

// WaitGetInput is the Huma input for GET /v0/city/{cityName}/wait/{id}.
type WaitGetInput struct {
	CityScope
	ID string `path:"id" doc:"Wait bead ID."`
}

// WaitListBody is the response body for GET /v0/city/{cityName}/waits.
// Partial/PartialErrors mirror the generic /beads list contract (ListBody): a
// degraded backing-store read surfaces the surviving rows with partial=true and
// the per-read error(s) rather than failing the whole request.
type WaitListBody struct {
	Waits         []WaitView `json:"waits" doc:"Durable session waits, newest first."`
	Capped        bool       `json:"capped" doc:"True when the lookup hit the per-scope cap and the list is partial."`
	Partial       bool       `json:"partial,omitempty" doc:"True when a backing store returned a partial result and the list may be incomplete."`
	PartialErrors []string   `json:"partial_errors,omitempty" doc:"Human-readable errors from the degraded wait lookup when partial is true."`
}

// WaitListOutput is the response envelope for GET /v0/city/{cityName}/waits.
type WaitListOutput struct {
	CacheAgeS float64 `header:"X-GC-Cache-Age-S" doc:"Age in seconds of the CachingStore snapshot that served this response (0 if not applicable)."`
	Body      WaitListBody
}

// WaitGetOutput is the response envelope for GET /v0/city/{cityName}/wait/{id}.
type WaitGetOutput struct {
	CacheAgeS float64 `header:"X-GC-Cache-Age-S" doc:"Age in seconds of the CachingStore snapshot that served this response (0 if not applicable)."`
	Body      WaitView
}
