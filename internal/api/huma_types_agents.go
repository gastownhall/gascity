package api

// Per-domain Huma input/output types for the agents handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_agents.go.

// --- Agent types ---

// AgentListInput is the Huma input for GET /v0/city/{cityName}/agents.
type AgentListInput struct {
	CityScope
	BlockingParam
	Pool    string `query:"pool" required:"false" doc:"Filter by pool name."`
	Rig     string `query:"rig" required:"false" doc:"Filter by rig name."`
	Running string `query:"running" required:"false" doc:"Filter by running state (true/false)."`
	Peek    string `query:"peek" required:"false" doc:"Include last output preview (true/false)."`
}

// AgentGetInput is the Huma input for GET /v0/city/{cityName}/agent/{name}.
type AgentGetInput struct {
	CityScope
	Name string `path:"name" doc:"Agent qualified name."`
}

// AgentCreateInput is the Huma input for POST /v0/city/{cityName}/agents.
type AgentCreateInput struct {
	CityScope
	Body struct {
		Name     string `json:"name" doc:"Agent name." minLength:"1" example:"deacon-1"`
		Dir      string `json:"dir,omitempty" doc:"Working directory (rig name)."`
		Provider string `json:"provider" doc:"Provider name." minLength:"1" example:"claude"`
		Scope    string `json:"scope,omitempty" doc:"Agent scope."`
	}
}

// AgentUpdateInput is the Huma input for PATCH /v0/city/{cityName}/agent/{name}.
type AgentUpdateInput struct {
	CityScope
	Name string `path:"name" doc:"Agent qualified name."`
	Body struct {
		Provider  string `json:"provider,omitempty" doc:"Provider name."`
		Scope     string `json:"scope,omitempty" doc:"Agent scope."`
		Suspended *bool  `json:"suspended,omitempty" doc:"Whether agent is suspended."`
	}
}

// AgentDeleteInput is the Huma input for DELETE /v0/city/{cityName}/agent/{name}.
type AgentDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Agent qualified name."`
}

// AgentActionInput is the Huma input for POST /v0/city/{cityName}/agent/{name} (actions).
type AgentActionInput struct {
	CityScope
	Name string `path:"name" doc:"Agent qualified name with action suffix (e.g. myagent/suspend)."`
}

// --- Agent output types ---

// AgentOutputInput is the Huma input for GET /v0/city/{cityName}/agent/{base}/output.
type AgentOutputInput struct {
	CityScope
	Name   string `path:"base" doc:"Agent base name."`
	Tail   string `query:"tail" required:"false" doc:"Number of compaction segments to return (default 1, 0 = all)."`
	Before string `query:"before" required:"false" doc:"Message UUID cursor for loading older messages."`
}

// AgentOutputQualifiedInput is the Huma input for GET /v0/city/{cityName}/agent/{dir}/{base}/output.
type AgentOutputQualifiedInput struct {
	CityScope
	Dir    string `path:"dir" doc:"Agent directory (rig name)."`
	Base   string `path:"base" doc:"Agent base name."`
	Tail   string `query:"tail" required:"false" doc:"Number of compaction segments to return (default 1, 0 = all)."`
	Before string `query:"before" required:"false" doc:"Message UUID cursor for loading older messages."`
}

// QualifiedName returns the full qualified agent name from dir/base components.
func (i *AgentOutputQualifiedInput) QualifiedName() string {
	if i.Dir == "" {
		return i.Base
	}
	return i.Dir + "/" + i.Base
}

// AgentOutputStreamInput is the Huma input for GET /v0/city/{cityName}/agent/{base}/output/stream.
type AgentOutputStreamInput struct {
	CityScope
	Base string `path:"base" doc:"Agent base name."`
}

// AgentOutputStreamQualifiedInput is the Huma input for GET /v0/city/{cityName}/agent/{dir}/{base}/output/stream.
type AgentOutputStreamQualifiedInput struct {
	CityScope
	Dir  string `path:"dir" doc:"Agent directory (rig name)."`
	Base string `path:"base" doc:"Agent base name."`
}

// QualifiedName returns the full qualified agent name from dir/base components.
func (i *AgentOutputStreamQualifiedInput) QualifiedName() string {
	if i.Dir == "" {
		return i.Base
	}
	return i.Dir + "/" + i.Base
}

