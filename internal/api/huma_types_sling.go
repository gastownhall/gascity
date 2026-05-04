package api

// Per-domain Huma input/output types for the sling handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_sling.go.

// --- Sling types ---

// SlingInput is the Huma input for POST /v0/city/{cityName}/sling.
//
// `target` is a hard requirement (handler returns 400 when empty). The
// spec marks it required + minLength 1 so generated clients validate at
// the edge rather than only at runtime.
type SlingInput struct {
	CityScope
	Body struct {
		Rig            string            `json:"rig,omitempty" doc:"Rig name."`
		Target         string            `json:"target" minLength:"1" doc:"Target agent or pool."`
		Bead           string            `json:"bead,omitempty" doc:"Bead ID to sling."`
		Formula        string            `json:"formula,omitempty" doc:"Formula name for workflow launch."`
		AttachedBeadID string            `json:"attached_bead_id,omitempty" doc:"Bead ID to attach a formula to."`
		Title          string            `json:"title,omitempty" doc:"Workflow title."`
		Vars           map[string]string `json:"vars,omitempty" doc:"Formula variables."`
		ScopeKind      string            `json:"scope_kind,omitempty" doc:"Scope kind (city or rig)."`
		ScopeRef       string            `json:"scope_ref,omitempty" doc:"Scope reference."`
		Force          bool              `json:"force,omitempty" doc:"Bypass cross-rig guards; for direct bead routes, also bypass missing-bead validation. Formula-backed graph routes may replace existing live workflow roots but still require the source bead to exist."`
	}
}
