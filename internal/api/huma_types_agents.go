package api

// Per-domain Huma input/output types for the agents handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_agents.go.

// joinAgentQualifiedName returns the canonical rig-qualified agent name
// from dir + base components. Shared by every qualified agent input
// type so the join logic lives in one place — embedding a mixin with
// path-tagged fields turns out to be invisible to the spec (Huma
// doesn't recurse into embedded path params the way it does for
// headers/queries), so the explicit Dir+Base fields stay on each input
// and this helper absorbs the duplication.
func joinAgentQualifiedName(dir, base string) string {
	if dir == "" {
		return base
	}
	return dir + "/" + base
}

// --- Agent types ---

// AgentListInput is the Huma input for GET /v0/city/{cityName}/agents.
type AgentListInput struct {
	CityScope
	BlockingParam
	Pool    string `query:"pool" required:"false" doc:"Filter by pool name."`
	Rig     string `query:"rig" required:"false" doc:"Filter by rig name."`
	Running string `query:"running" required:"false" enum:"true,false" doc:"Filter by running state. Omit to return all agents."`
	Peek    bool   `query:"peek" required:"false" doc:"Include last output preview."`
}

// AgentGetInput is the Huma input for GET /v0/city/{cityName}/agent/{base}.
// Agents can be addressed either by their unqualified name (this form) or by
// rig-qualified path segments (see AgentGetQualifiedInput). Qualified names
// never exceed two segments, so the two routes cover every real case without
// any trailing-path wildcard.
type AgentGetInput struct {
	CityScope
	Name string `path:"base" doc:"Agent name (unqualified, no rig)."`
}

// AgentGetQualifiedInput is the Huma input for GET /v0/city/{cityName}/agent/{dir}/{base}.
type AgentGetQualifiedInput struct {
	CityScope
	Dir  string `path:"dir" doc:"Agent directory (rig name)."`
	Base string `path:"base" doc:"Agent base name."`
}

// QualifiedName joins dir and base into a canonical agent name.
func (i *AgentGetQualifiedInput) QualifiedName() string {
	return joinAgentQualifiedName(i.Dir, i.Base)
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

// AgentUpdateInput is the Huma input for PATCH /v0/city/{cityName}/agent/{base}.
type AgentUpdateInput struct {
	CityScope
	Name string `path:"base" doc:"Agent name (unqualified)."`
	Body struct {
		Provider  string `json:"provider,omitempty" doc:"Provider name."`
		Scope     string `json:"scope,omitempty" doc:"Agent scope."`
		Suspended *bool  `json:"suspended,omitempty" doc:"Whether agent is suspended."`
	}
}

// AgentUpdateQualifiedInput is the Huma input for
// PATCH /v0/city/{cityName}/agent/{dir}/{base}.
type AgentUpdateQualifiedInput struct {
	CityScope
	Dir  string `path:"dir" doc:"Agent directory (rig name)."`
	Base string `path:"base" doc:"Agent base name."`
	Body struct {
		Provider  string `json:"provider,omitempty" doc:"Provider name."`
		Scope     string `json:"scope,omitempty" doc:"Agent scope."`
		Suspended *bool  `json:"suspended,omitempty" doc:"Whether agent is suspended."`
	}
}

// QualifiedName joins dir and base into a canonical agent name.
func (i *AgentUpdateQualifiedInput) QualifiedName() string {
	return joinAgentQualifiedName(i.Dir, i.Base)
}

// AgentDeleteInput is the Huma input for DELETE /v0/city/{cityName}/agent/{base}.
type AgentDeleteInput struct {
	CityScope
	Name string `path:"base" doc:"Agent name (unqualified)."`
}

// AgentDeleteQualifiedInput is the Huma input for
// DELETE /v0/city/{cityName}/agent/{dir}/{base}.
type AgentDeleteQualifiedInput struct {
	CityScope
	Dir  string `path:"dir" doc:"Agent directory (rig name)."`
	Base string `path:"base" doc:"Agent base name."`
}

// QualifiedName joins dir and base into a canonical agent name.
func (i *AgentDeleteQualifiedInput) QualifiedName() string {
	return joinAgentQualifiedName(i.Dir, i.Base)
}

// AgentActionInput is the Huma input for
// POST /v0/city/{cityName}/agent/{base}/{action}. Valid actions are
// suspend, resume, and (reserved) restart — matching the rig-action shape.
type AgentActionInput struct {
	CityScope
	Name   string `path:"base" doc:"Agent name (unqualified)."`
	Action string `path:"action" enum:"suspend,resume" doc:"Action to perform."`
}

// AgentActionQualifiedInput is the Huma input for
// POST /v0/city/{cityName}/agent/{dir}/{base}/{action}.
type AgentActionQualifiedInput struct {
	CityScope
	Dir    string `path:"dir" doc:"Agent directory (rig name)."`
	Base   string `path:"base" doc:"Agent base name."`
	Action string `path:"action" enum:"suspend,resume" doc:"Action to perform."`
}

// QualifiedName joins dir and base into a canonical agent name.
func (i *AgentActionQualifiedInput) QualifiedName() string {
	return joinAgentQualifiedName(i.Dir, i.Base)
}

// --- Agent output types ---

// AgentOutputInput is the Huma input for GET /v0/city/{cityName}/agent/{base}/output.
type AgentOutputInput struct {
	CityScope
	TailParam
	Name   string `path:"base" doc:"Agent base name."`
	Before string `query:"before" required:"false" doc:"Message UUID cursor for loading older messages."`
}

// AgentOutputQualifiedInput is the Huma input for GET /v0/city/{cityName}/agent/{dir}/{base}/output.
type AgentOutputQualifiedInput struct {
	CityScope
	TailParam
	Dir    string `path:"dir" doc:"Agent directory (rig name)."`
	Base   string `path:"base" doc:"Agent base name."`
	Before string `query:"before" required:"false" doc:"Message UUID cursor for loading older messages."`
}

// QualifiedName joins dir and base into a canonical agent name.
func (i *AgentOutputQualifiedInput) QualifiedName() string {
	return joinAgentQualifiedName(i.Dir, i.Base)
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

// QualifiedName joins dir and base into a canonical agent name.
func (i *AgentOutputStreamQualifiedInput) QualifiedName() string {
	return joinAgentQualifiedName(i.Dir, i.Base)
}
