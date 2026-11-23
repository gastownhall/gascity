package api

// WorkActiveStepInput is the Huma input for GET
// /v0/city/{cityName}/work/active-step: the identities a session's step
// reminder resolves for, and the work scope its own work lives in.
type WorkActiveStepInput struct {
	CityScope
	Assignee  []string `query:"assignee,explode" required:"true" minItems:"1" maxItems:"8" doc:"The session's identities, in the order the reminder tries them: its alias, its session name, its session id. Repeatable; one to eight."`
	ScopeRoot string   `query:"scope_root" required:"false" doc:"Directory of the work scope the session's work lives in, its rig root. Omitted or empty is the city."`
}

// WorkActiveStepOutput is the response of GET
// /v0/city/{cityName}/work/active-step.
type WorkActiveStepOutput struct {
	Body WorkActiveStepBody
}

// WorkActiveStepBody carries the answer: the session's current formula step,
// or none.
type WorkActiveStepBody struct {
	Step *ActiveWorkStep `json:"step,omitempty" doc:"The session's current formula step, exactly the bead its step reminder names; absent when there is none."`
}

// ActiveWorkStep is the bead a session's step reminder names, carrying what
// the reminder prints of it.
type ActiveWorkStep struct {
	ID          string `json:"id" doc:"The step bead's id."`
	Title       string `json:"title" doc:"The step bead's title."`
	Description string `json:"description" doc:"The step bead's description; empty when it has none."`
}
