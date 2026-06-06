package api

// MaintenanceRunBody is the Huma wire shape for one completed or failed
// maintenance run. Mirrors MaintenanceRunView but lives in the generated
// OpenAPI surface so it survives round-tripping through the genclient.
type MaintenanceRunBody struct {
	StartedAt       string  `json:"started_at" doc:"RFC3339 timestamp when the run began."`
	FinishedAt      string  `json:"finished_at" doc:"RFC3339 timestamp when the run completed."`
	Stage           string  `json:"stage" doc:"Outcome stage: 'done' on success or 'backup'/'gc'/'smoke-test'/'prune' on failure."`
	Err             string  `json:"err,omitempty" doc:"Error message when Stage names a failing phase; empty on success."`
	BeforeBytes     int64   `json:"before_bytes" doc:"Store size in bytes before the run (0 when not measured)."`
	AfterBytes      int64   `json:"after_bytes" doc:"Store size in bytes after the run (0 when not measured)."`
	SnapshotPath    string  `json:"snapshot_path,omitempty" doc:"Absolute path to the snapshot directory created for this run."`
	DurationSeconds float64 `json:"duration_s" doc:"Elapsed wall-clock seconds between started_at and finished_at."`
}

// MaintenanceStatusBody is the response body for GET
// /v0/city/{city}/maintenance/status.
type MaintenanceStatusBody struct {
	Enabled       bool                 `json:"enabled" doc:"Whether [maintenance.dolt] enabled=true in city.toml."`
	IntervalSec   int64                `json:"interval_seconds" doc:"Configured scheduling interval in seconds (0 when disabled)."`
	InFlight      bool                 `json:"in_flight" doc:"True when a maintenance cycle is currently running."`
	InFlightStart string               `json:"in_flight_start,omitempty" doc:"RFC3339 start time of the in-flight run."`
	LastRun       *MaintenanceRunBody  `json:"last_run,omitempty" doc:"Most recent completed run, or null when none."`
	NextScheduled string               `json:"next_scheduled,omitempty" doc:"RFC3339 approximate next scheduled run time."`
	History       []MaintenanceRunBody `json:"history" doc:"Bounded ring of recent run outcomes (oldest first)."`
}

// MaintenanceStatusInput is the Huma input for the status handler.
type MaintenanceStatusInput struct {
	CityScope
}

// MaintenanceStatusOutput wraps the status body and carries the
// X-GC-Cache-Age-S header so the read-path CLI can surface the banner.
type MaintenanceStatusOutput struct {
	CacheAgeHeader float64 `header:"X-GC-Cache-Age-S"`
	Body           MaintenanceStatusBody
}

// MaintenanceTriggerBody is the response body for POST
// /v0/city/{city}/maintenance/dolt-gc. Accepted=true indicates the
// supervisor has taken the lease; Run is populated only when the caller
// set ?wait=true and the cycle completed synchronously.
type MaintenanceTriggerBody struct {
	Accepted  bool                `json:"accepted" doc:"True when the supervisor accepted the trigger (202) or completed it (200)."`
	StartedAt string              `json:"started_at,omitempty" doc:"RFC3339 start time of the triggered run; doubles as a run identifier for async callers."`
	Run       *MaintenanceRunBody `json:"run,omitempty" doc:"Full run summary, populated when the caller set ?wait=true."`
}

// MaintenanceTriggerInput is the Huma input for the trigger handler.
// Wait toggles synchronous execution.
type MaintenanceTriggerInput struct {
	CityScope
	Wait bool `query:"wait" doc:"When true, the handler blocks until the run completes and returns 200 with the full Run. When false (default), the handler returns 202 Accepted immediately."`
}

// MaintenanceTriggerOutput wraps MaintenanceTriggerBody. The endpoint is
// registered with DefaultStatus=202: sync (?wait=true) still returns 202,
// with the full Run populated in the body so callers distinguish by the
// body shape (Run present → cycle completed; StartedAt only → accepted,
// running in background). This keeps the OpenAPI spec minimal — one
// Responses entry — at the cost of a minor 200-vs-202 departure from the
// design doc.
type MaintenanceTriggerOutput struct {
	Body MaintenanceTriggerBody
}

// MaintenanceInProgressBody is the typed 409 Conflict body documented on
// the design doc for ga-d5y D8. The TypeField literal lets the CLI key off
// a stable discriminator when parsing problem responses; StartedAt conveys
// the in-flight run's start time so operators can tell whether the existing
// run is fresh or stuck.
type MaintenanceInProgressBody struct {
	TypeField string `json:"type" doc:"Stable discriminator; always 'maintenance-in-progress'."`
	StartedAt string `json:"started_at,omitempty" doc:"RFC3339 start time of the in-flight run."`
}
