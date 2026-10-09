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
		Bead           string            `json:"bead,omitempty" doc:"Bead or convoy ID to sling, like gc sling <target> <bead>. The target's default formula is cooked onto the bead unless no_formula is set; a convoy's open children are routed one by one."`
		Formula        string            `json:"formula,omitempty" doc:"Formula name. Alone, it launches the formula standalone (gc sling --formula). With attached_bead_id, it is attached to that bead (gc sling <target> <bead> --on <formula>)."`
		AttachedBeadID string            `json:"attached_bead_id,omitempty" doc:"Bead or convoy ID to attach formula to, in place of bead (gc sling --on)."`
		Title          string            `json:"title,omitempty" doc:"Workflow title (gc sling --title), for an explicit or default formula."`
		Vars           map[string]string `json:"vars,omitempty" doc:"Formula variables (gc sling --var), for an explicit or default formula."`
		ScopeKind      string            `json:"scope_kind,omitempty" doc:"Scope kind (city or rig)."`
		ScopeRef       string            `json:"scope_ref,omitempty" doc:"Scope reference."`
		Force          bool              `json:"force,omitempty" doc:"Bypass cross-rig guards; for direct bead routes, also bypass missing-bead validation. Formula-backed graph routes may replace existing live workflow roots but still require the source bead to exist."`
		Reassign       bool              `json:"reassign,omitempty" doc:"Clear any existing human assignee on the bead before routing, so a bead claimed via bd update --claim is handed to the target's pool."`
		Merge          string            `json:"merge,omitempty" doc:"Merge strategy: direct, mr, or local."`
		NoConvoy       bool              `json:"no_convoy,omitempty" doc:"Do not create an auto-convoy for the routed bead."`
		Owned          bool              `json:"owned,omitempty" doc:"Mark the routed bead as owned by the target."`
		NoFormula      bool              `json:"no_formula,omitempty" doc:"Suppress the target's default_sling_formula and route the raw bead (gc sling --no-formula)."`
	}
}
