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

// RigCreateInput is the Huma input for POST /v0/city/{cityName}/rigs.
type RigCreateInput struct {
	CityScope
	Body struct {
		Name          string `json:"name" doc:"Rig name." minLength:"1"`
		Path          string `json:"path" doc:"Filesystem path." minLength:"1"`
		Prefix        string `json:"prefix,omitempty" doc:"Session name prefix."`
		DefaultBranch string `json:"default_branch,omitempty" doc:"Mainline branch (e.g. main, master). Auto-detected when omitted."`
	}
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
	Action string `path:"action" doc:"Action to perform (suspend, resume, restart)."`
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
