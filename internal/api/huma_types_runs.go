package api

import "github.com/danielgtaylor/huma/v2"

// RunStatus is the closed lifecycle state of a run on the canonical Run
// resource. It converges the several run-status vocabularies the API historically
// exposed (workflow-run projection, orders monitor feed, dashboard lane phases)
// into ONE machine-branchable enum. The schema is closed (Huma emits `enum`), so
// a consumer can switch exhaustively.
//
// The full set is fixed here from the first slice so no schema break lands as the
// derivation grows: `waiting` is emitted for a run blocked on an open dependency;
// `canceling`/`canceled` are emitted once cancellation (POST .../cancel) is
// wired. `canceled` is always a terminal outcome distinct from `failed` and
// `skipped`.
type RunStatus string

const (
	// RunStatusPending is a created run that has not started any work.
	RunStatusPending RunStatus = "pending"
	// RunStatusActive is a run with work in progress.
	RunStatusActive RunStatus = "active"
	// RunStatusWaiting is a run the projection classifies as blocked (its work
	// is not progressing). Richer dependency/gate derivation is future work.
	RunStatusWaiting RunStatus = "waiting"
	// RunStatusCanceling is a run for which cancellation was requested but has
	// not yet reached a terminal state.
	RunStatusCanceling RunStatus = "canceling"
	// RunStatusCompleted is a run that finished successfully.
	RunStatusCompleted RunStatus = "completed"
	// RunStatusFailed is a run that finished with a failure outcome.
	RunStatusFailed RunStatus = "failed"
	// RunStatusCanceled is a run that terminated because it was canceled.
	RunStatusCanceled RunStatus = "canceled"
	// RunStatusSkipped is a run that terminated as skipped (no-op teardown).
	RunStatusSkipped RunStatus = "skipped"
)

// Schema registers RunStatus as a named, closed string enum in the OpenAPI
// components so every field of this type renders a `$ref` to one schema instead
// of an inlined bare string.
func (RunStatus) Schema(r huma.Registry) *huma.Schema {
	return registerNamedEnum(r, "RunStatus",
		"Closed lifecycle state of a run.",
		string(RunStatusPending), string(RunStatusActive), string(RunStatusWaiting),
		string(RunStatusCanceling), string(RunStatusCompleted), string(RunStatusFailed),
		string(RunStatusCanceled), string(RunStatusSkipped),
	)
}

// RunStepStatus is the closed lifecycle state of a single run step (a child bead
// of the run). It is a step-level projection of the same terminal outcomes used
// by RunStatus.
type RunStepStatus string

const (
	// RunStepStatusPending is a step that has not started.
	RunStepStatusPending RunStepStatus = "pending"
	// RunStepStatusActive is a step in progress.
	RunStepStatusActive RunStepStatus = "active"
	// RunStepStatusBlocked is a step waiting on an unmet dependency.
	RunStepStatusBlocked RunStepStatus = "blocked"
	// RunStepStatusCompleted is a step that finished successfully.
	RunStepStatusCompleted RunStepStatus = "completed"
	// RunStepStatusFailed is a step that finished with a failure outcome.
	RunStepStatusFailed RunStepStatus = "failed"
	// RunStepStatusSkipped is a step that terminated as skipped.
	RunStepStatusSkipped RunStepStatus = "skipped"
	// RunStepStatusCanceled is a step closed because its run was canceled.
	RunStepStatusCanceled RunStepStatus = "canceled"
)

// Schema registers RunStepStatus as a named, closed string enum.
func (RunStepStatus) Schema(r huma.Registry) *huma.Schema {
	return registerNamedEnum(r, "RunStepStatus",
		"Closed lifecycle state of a run step.",
		string(RunStepStatusPending), string(RunStepStatusActive), string(RunStepStatusBlocked),
		string(RunStepStatusCompleted), string(RunStepStatusFailed), string(RunStepStatusSkipped),
		string(RunStepStatusCanceled),
	)
}

// RunScope is the resolved scope a run executes under.
type RunScope struct {
	Kind string `json:"kind,omitempty" doc:"Scope kind (city or rig), when resolved."`
	Ref  string `json:"ref,omitempty" doc:"Scope reference within the kind, when resolved."`
}

// RunLastError carries the structured failure reason for a terminal run, sourced
// from the run root's close outcome metadata. Absent while the run is non-terminal
// or succeeded.
type RunLastError struct {
	Code    string `json:"code" doc:"Machine-readable outcome code (e.g. fail, skipped, canceled)."`
	Message string `json:"message,omitempty" doc:"Human-readable failure detail, when available."`
}

// Run is the canonical typed projection of one execution. It is the ONE run shape
// the API exposes, sourced from the city event log (.gc/events.jsonl) via the run
// projection.
type Run struct {
	RunID     string        `json:"run_id" doc:"Stable run identifier (the run root bead id)."`
	Formula   string        `json:"formula,omitempty" doc:"Formula name driving the run, when known."`
	Title     string        `json:"title" doc:"Human-readable run title."`
	Status    RunStatus     `json:"status" doc:"Closed lifecycle status."`
	Target    string        `json:"target,omitempty" doc:"Where the run is routed (rig/target), when known."`
	Scope     RunScope      `json:"scope" doc:"Resolved run scope."`
	StartedAt string        `json:"started_at,omitempty" doc:"RFC3339 run start time (root creation)."`
	UpdatedAt string        `json:"updated_at,omitempty" doc:"RFC3339 time of the run's most recent activity."`
	LastError *RunLastError `json:"last_error,omitempty" doc:"Structured failure reason for a terminal run."`
}

// RunStep is one step of a run (a child bead), projected to a stable shape.
type RunStep struct {
	ID       string        `json:"id" doc:"Step (child bead) identifier."`
	Title    string        `json:"title" doc:"Step title."`
	Status   RunStepStatus `json:"status" doc:"Closed step lifecycle status."`
	Kind     string        `json:"kind,omitempty" doc:"Step kind (bead type)."`
	Assignee string        `json:"assignee,omitempty" doc:"Current assignee, when set."`
}

// RunRef is the lightweight run reference a launch endpoint returns in its body,
// pointing the caller at the canonical Run resource. The Location header on the
// same response carries the URL; this stanza carries the ids for a client reading
// the body. It is emitted only when the launch produced an addressable run (a
// graph-workflow sling); launches that produce no single run (order dispatch,
// wisps) point their Location at the runs list instead.
type RunRef struct {
	RunID  string    `json:"run_id" doc:"Run identifier; GET /v0/city/{cityName}/runs/{run_id} for detail."`
	Kind   string    `json:"kind" enum:"sling,order" doc:"Launch mechanism that produced the run."`
	Status RunStatus `json:"status" doc:"Closed lifecycle status at response time (a just-launched run is pending)."`
}

// RunKindSling marks a run launched via POST /sling.
const RunKindSling = "sling"

// RunsListInput is the request for GET /v0/city/{cityName}/runs.
type RunsListInput struct {
	CityScope
	Limit int `query:"limit" minimum:"0" doc:"Maximum runs to return (0 uses the server default)."`
}

// RunsListOutput is the response body for the run list.
type RunsListOutput struct {
	Body struct {
		Runs          []Run    `json:"runs" doc:"Runs in the city, newest activity first."`
		Partial       bool     `json:"partial,omitempty" doc:"True when some runs could not be fully projected."`
		PartialErrors []string `json:"partial_errors,omitempty" doc:"Reasons the projection was partial."`
	}
}

// RunGetInput is the request for GET /v0/city/{cityName}/runs/{run_id}.
type RunGetInput struct {
	CityScope
	RunID string `path:"run_id" minLength:"1" pattern:"\\S" doc:"Run identifier."`
}

// RunGetOutput is the response body for a single run.
type RunGetOutput struct {
	Body Run
}

// RunStepsInput is the request for GET /v0/city/{cityName}/runs/{run_id}/steps.
type RunStepsInput struct {
	CityScope
	RunID string `path:"run_id" minLength:"1" pattern:"\\S" doc:"Run identifier."`
}

// RunStepsOutput is the response body for a run's steps.
type RunStepsOutput struct {
	Body struct {
		RunID string    `json:"run_id" doc:"Run identifier the steps belong to."`
		Steps []RunStep `json:"steps" doc:"Steps of the run."`
	}
}

// RunCancelInput is the request for POST /v0/city/{cityName}/runs/{run_id}/cancel.
type RunCancelInput struct {
	CityScope
	RunID string `path:"run_id" minLength:"1" pattern:"\\S" doc:"Run identifier."`
}

// RunCancelOutput is the response body for a run cancel (HTTP 202).
type RunCancelOutput struct {
	Body struct {
		RunID  string    `json:"run_id" doc:"The canceled run."`
		Status RunStatus `json:"status" doc:"Run status after the cancel wind-down."`
		Closed int       `json:"closed" doc:"Count of the run's beads closed by the cancel."`
	}
}
