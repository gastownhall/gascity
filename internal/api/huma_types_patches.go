package api

import "github.com/gastownhall/gascity/internal/beads"

// Per-domain Huma input/output types for the patches handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_patches.go.

// --- Patch types ---

// AgentPatchListInput is the Huma input for GET /v0/city/{cityName}/patches/agents.
type AgentPatchListInput struct {
	CityScope
}

// AgentPatchGetInput is the Huma input for
// GET /v0/city/{cityName}/patches/agent/{base}.
type AgentPatchGetInput struct {
	CityScope
	Name string `path:"base" doc:"Agent patch name (unqualified)."`
}

// AgentPatchGetQualifiedInput is the Huma input for
// GET /v0/city/{cityName}/patches/agent/{dir}/{base}.
type AgentPatchGetQualifiedInput struct {
	CityScope
	Dir  string `path:"dir" doc:"Agent directory (rig name)."`
	Base string `path:"base" doc:"Agent base name."`
}

// QualifiedName joins dir and base into a canonical agent name.
func (i *AgentPatchGetQualifiedInput) QualifiedName() string {
	return joinAgentQualifiedName(i.Dir, i.Base)
}

// AgentPatchSetInput is the Huma input for PUT /v0/city/{cityName}/patches/agents.
type AgentPatchSetInput struct {
	CityScope
	Body struct {
		Dir       string            `json:"dir,omitempty" doc:"Agent directory scope."`
		Name      string            `json:"name,omitempty" doc:"Agent name."`
		Provider  *string           `json:"provider,omitempty" doc:"Override the agent's provider."`
		WorkDir   *string           `json:"work_dir,omitempty" doc:"Override session working directory."`
		TmuxAlias *string           `json:"tmux_alias,omitempty" doc:"Override tmux session name template."`
		Scope     *string           `json:"scope,omitempty" doc:"Override agent scope."`
		Suspended *bool             `json:"suspended,omitempty" doc:"Override suspended state."`
		Env       map[string]string `json:"env,omitempty" doc:"Override environment variables."`
	}
}

// AgentPatchDeleteInput is the Huma input for
// DELETE /v0/city/{cityName}/patches/agent/{base}.
type AgentPatchDeleteInput struct {
	CityScope
	Name string `path:"base" doc:"Agent patch name (unqualified)."`
}

// AgentPatchDeleteQualifiedInput is the Huma input for
// DELETE /v0/city/{cityName}/patches/agent/{dir}/{base}.
type AgentPatchDeleteQualifiedInput struct {
	CityScope
	Dir  string `path:"dir" doc:"Agent directory (rig name)."`
	Base string `path:"base" doc:"Agent base name."`
}

// QualifiedName joins dir and base into a canonical agent name.
func (i *AgentPatchDeleteQualifiedInput) QualifiedName() string {
	return joinAgentQualifiedName(i.Dir, i.Base)
}

// RigPatchListInput is the Huma input for GET /v0/city/{cityName}/patches/rigs.
type RigPatchListInput struct {
	CityScope
}

// RigPatchGetInput is the Huma input for GET /v0/city/{cityName}/patches/rig/{name}.
type RigPatchGetInput struct {
	CityScope
	Name string `path:"name" doc:"Rig patch name."`
}

// RigPatchSetInput is the Huma input for PUT /v0/city/{cityName}/patches/rigs.
type RigPatchSetInput struct {
	CityScope
	Body struct {
		Name          string  `json:"name,omitempty" doc:"Rig name."`
		Path          *string `json:"path,omitempty" doc:"Override filesystem path."`
		Prefix        *string `json:"prefix,omitempty" doc:"Override bead ID prefix."`
		DefaultBranch *string `json:"default_branch,omitempty" doc:"Override mainline branch."`
		Suspended     *bool   `json:"suspended,omitempty" doc:"Override suspended state."`
	}
}

// RigPatchDeleteInput is the Huma input for DELETE /v0/city/{cityName}/patches/rig/{name}.
type RigPatchDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Rig patch name."`
}

// ProviderPatchListInput is the Huma input for GET /v0/city/{cityName}/patches/providers.
type ProviderPatchListInput struct {
	CityScope
}

// ProviderPatchGetInput is the Huma input for GET /v0/city/{cityName}/patches/provider/{name}.
type ProviderPatchGetInput struct {
	CityScope
	Name string `path:"name" doc:"Provider patch name."`
}

// ProviderPatchSetInput is the Huma input for PUT /v0/city/{cityName}/patches/providers.
type ProviderPatchSetInput struct {
	CityScope
	Body struct {
		Name                 string            `json:"name,omitempty" doc:"Provider name."`
		Command              *string           `json:"command,omitempty" doc:"Override command binary."`
		ACPCommand           *string           `json:"acp_command,omitempty" doc:"Override ACP transport command binary."`
		Args                 []string          `json:"args,omitempty" doc:"Override command arguments."`
		ACPArgs              []string          `json:"acp_args,omitempty" doc:"Override ACP transport command arguments."`
		PromptMode           *string           `json:"prompt_mode,omitempty" doc:"Override prompt delivery mode."`
		PromptFlag           *string           `json:"prompt_flag,omitempty" doc:"Override prompt flag."`
		ReadyDelayMs         *int              `json:"ready_delay_ms,omitempty" doc:"Override ready delay in milliseconds."`
		AcceptStartupDialogs *bool             `json:"accept_startup_dialogs,omitempty" doc:"Override startup dialog acceptance behavior."`
		Env                  map[string]string `json:"env,omitempty" doc:"Override environment variables."`
	}
}

// ProviderPatchDeleteInput is the Huma input for DELETE /v0/city/{cityName}/patches/provider/{name}.
type ProviderPatchDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Provider patch name."`
}

// --- Patch response types ---

// PatchOKResponse is a success response for patch set operations.
type PatchOKResponse struct {
	Body struct {
		Status        string `json:"status" doc:"Operation result." example:"ok"`
		AgentPatch    string `json:"agent_patch,omitempty" doc:"Agent patch qualified name."`
		RigPatch      string `json:"rig_patch,omitempty" doc:"Rig patch name."`
		ProviderPatch string `json:"provider_patch,omitempty" doc:"Provider patch name."`
	}
}

// PatchDeletedResponse is a success response for patch delete operations.
type PatchDeletedResponse struct {
	Body struct {
		Status        string `json:"status" doc:"Operation result." example:"deleted"`
		AgentPatch    string `json:"agent_patch,omitempty" doc:"Agent patch qualified name."`
		RigPatch      string `json:"rig_patch,omitempty" doc:"Rig patch name."`
		ProviderPatch string `json:"provider_patch,omitempty" doc:"Provider patch name."`
	}
}

// StatusBody is the response body for GET /v0/status.
type StatusBody struct {
	Name                string                     `json:"name" doc:"City name."`
	Path                string                     `json:"path" doc:"City directory path."`
	Version             string                     `json:"version,omitempty" doc:"Server version."`
	UptimeSec           int                        `json:"uptime_sec" doc:"Server uptime in seconds."`
	Suspended           bool                       `json:"suspended" doc:"Whether the city is suspended."`
	AgentCount          int                        `json:"agent_count" doc:"Total agent count (deprecated, use agents.total)."`
	RigCount            int                        `json:"rig_count" doc:"Total rig count (deprecated, use rigs.total)."`
	Running             int                        `json:"running" doc:"Number of running agent processes."`
	Agents              StatusAgentCounts          `json:"agents" doc:"Agent state counts."`
	Rigs                StatusRigCounts            `json:"rigs" doc:"Rig state counts."`
	Work                StatusWorkCounts           `json:"work" doc:"Work item counts."`
	Mail                StatusMailCounts           `json:"mail" doc:"Mail counts."`
	StoreHealth         *StatusStoreHealth         `json:"store_health,omitempty" doc:"Dolt bead store health summary. Omitted when unavailable."`
	Beads               *beads.BeadsDiagnostic     `json:"beads,omitempty" doc:"Bead store selection diagnostic. Omitted when unavailable."`
	DoltVersion         string                     `json:"dolt_version,omitempty" doc:"Version of the dolt engine binary the supervisor drives. Omitted when the probe failed or the binary is unavailable."`
	BeadsVersion        string                     `json:"beads_version,omitempty" doc:"Version of the bd (beads) CLI the supervisor drives. Omitted when the probe failed or the binary is unavailable."`
	Partial             bool                       `json:"partial,omitempty" doc:"True when one or more status backing reads returned incomplete data."`
	PartialErrors       []string                   `json:"partial_errors,omitempty" doc:"Human-readable errors from incomplete status backing reads."`
	AgentDetails        []StatusAgentDetail        `json:"agent_details,omitempty" doc:"Per-agent state (for CLI status views). Empty when none."`
	RigDetails          []StatusRigDetail          `json:"rig_details,omitempty" doc:"Per-rig detail (for CLI status views). Empty when none."`
	NamedSessionDetails []StatusNamedSessionDetail `json:"named_session_details,omitempty" doc:"Per-named-session detail. Empty when none configured."`
	SessionCountsDetail *StatusSessionCountsDetail `json:"session_counts_detail,omitempty" doc:"Active/suspended session counts. Omitted when unavailable."`
	ConditionalWrites   *StatusConditionalWrites   `json:"conditional_writes,omitempty" doc:"Conditional-writes (CAS) rollout state: the daemon's boot-latched mode plus per-store capability verdicts. Omitted when the server predates the surface."`
}

// StatusConditionalWrites is the daemon's own latched conditional-writes
// snapshot: the boot-resolved mode, real per-store probe/latch verdicts, and
// retained rollout notices — never a re-derivation from config. Doctor and
// the dashboard render this same block, so they agree by construction.
type StatusConditionalWrites struct {
	Mode      string                               `json:"mode" enum:"off,auto,require" doc:"Boot-latched beads.conditional_writes mode."`
	Origin    string                               `json:"origin" enum:"builtin,config,env" doc:"Where the latched mode came from."`
	Effective string                               `json:"effective" enum:"off,active,degraded,fail_closed,pending_restart" doc:"Aggregate verdict: off (gate off), active (every store capable), degraded (auto with at least one incapable store), fail_closed (require with at least one incapable store — fenced writes on it refuse), pending_restart (on-disk config drifted from the latched mode)."`
	Stores    []StatusConditionalWriteStoreVerdict `json:"stores,omitempty" doc:"Per-store verdicts, one row per controller-owned store."`
	Notices   []StatusRolloutNotice                `json:"notices,omitempty" doc:"Retained rollout notices (env overrides, drift, invalid spellings)."`
}

// StatusConditionalWriteStoreVerdict is one store's conditional-writes
// capability as the write path sees it. Probe and Latch are independent so
// version-skew states stay legible: probe=capable latch=incapable means bd
// rejected a real fenced write at runtime and the fix is a restart to
// re-probe, not a bd upgrade.
type StatusConditionalWriteStoreVerdict struct {
	StoreID string `json:"store_id" doc:"Store scope: city, or rig/<name>."`
	Kind    string `json:"kind" doc:"Store kind in the degraded-event wire vocabulary (bd, native, caching, mem, file)."`
	Probe   string `json:"probe" enum:"capable,incapable,unprobed" doc:"Memoized capability-probe verdict. unprobed means no fenced write has exercised this store yet."`
	Latch   string `json:"latch" enum:"incapable,unlatched" doc:"Runtime unsupported latch: incapable after the store rejected a real fenced write; cleared only by restart."`
	Capable bool   `json:"capable" doc:"What the write path uses today: false only on a definitive incapable verdict."`
	Reason  string `json:"reason,omitempty" doc:"Incapable cause, verbatim from the probe or latch."`
}

// StatusRolloutNotice mirrors internal/rollout.Notice onto the typed wire.
type StatusRolloutNotice struct {
	Kind        string `json:"kind" doc:"Notice kind (env_overrides_config, pending_restart, invalid_value, ...)."`
	FlagKey     string `json:"flag_key" doc:"Rollout gate key the notice is about."`
	EnvVar      string `json:"env_var,omitempty" doc:"Environment variable involved, when env-related."`
	ConfigValue string `json:"config_value,omitempty" doc:"Raw config spelling; empty when unset."`
	EnvValue    string `json:"env_value,omitempty" doc:"Raw env spelling as found."`
	Message     string `json:"message" doc:"Human-readable line carrying the gate and the outcome."`
}

// StatusAgentDetail mirrors the CLI's StatusAgentJSON with the additional
// display hints (group name, scale label, session name) that the text
// formatter needs when rendering pool-expanded rows.
type StatusAgentDetail struct {
	Name          string `json:"name" doc:"Unqualified agent name (for pool instances, the per-instance short name like 'polecat-1')."`
	QualifiedName string `json:"qualified_name" doc:"Rig-qualified name when applicable, else the bare agent name."`
	Scope         string `json:"scope" doc:"city or rig."`
	Running       bool   `json:"running" doc:"Observed running state of the agent's session."`
	Suspended     bool   `json:"suspended" doc:"Whether the agent (or its rig) is suspended."`
	Draining      bool   `json:"draining,omitempty" doc:"True when the pool is draining this instance."`
	SessionName   string `json:"session_name,omitempty" doc:"tmux session name CLI drain-ops key on."`
	GroupName     string `json:"group_name,omitempty" doc:"Pool group label for expanded rows; same as QualifiedName for singletons."`
	ScaleLabel    string `json:"scale_label,omitempty" doc:"'scaled (min=N, max=M)' header emitted once per pool group."`
	Expanded      bool   `json:"expanded,omitempty" doc:"True when this row is a pool-expanded instance (renderer indents differently)."`
}

// StatusRigDetail mirrors the CLI's StatusRigJSON (name/path/suspended)
// so the API path can render the Rigs section without a separate /rigs call.
type StatusRigDetail struct {
	Name      string `json:"name" doc:"Rig name."`
	Path      string `json:"path" doc:"Rig directory path."`
	Suspended bool   `json:"suspended" doc:"Whether the rig is suspended (either explicitly or because all its agents are suspended)."`
}

// StatusNamedSessionDetail mirrors the CLI's Named sessions block so the
// API path can render it without a separate query.
type StatusNamedSessionDetail struct {
	Identity string `json:"identity" doc:"Qualified named-session identity."`
	Status   string `json:"status" doc:"Lifecycle status string (materialized, reserved-unmaterialized, etc.)."`
	Mode     string `json:"mode" doc:"Named-session mode (on-demand, always, etc.)."`
}

// StatusSessionCountsDetail mirrors the CLI's Sessions line
// (N active, M suspended).
type StatusSessionCountsDetail struct {
	Active    int `json:"active" doc:"Number of active sessions."`
	Suspended int `json:"suspended" doc:"Number of suspended sessions."`
}

// StatusStoreHealth summarizes the Dolt bead store's on-disk footprint
// and last maintenance run. Surfaced by GET /v0/status for operator
// dashboards; see ADR 0002 / bead ga-d5y design D9.
type StatusStoreHealth struct {
	Path         string  `json:"path" doc:"On-disk path of the Dolt store."`
	SizeBytes    int64   `json:"size_bytes" doc:"Total bytes of the store directory."`
	LiveRows     int     `json:"live_rows" doc:"Live bead row count."`
	RatioMB      float64 `json:"ratio_mb_per_row" doc:"Derived megabytes per row."`
	Warning      bool    `json:"warning" doc:"True when maintenance is overdue."`
	ThresholdMB  float64 `json:"threshold_mb_per_row" doc:"Ratio threshold; a ratio above this trips warning."`
	LastGCAt     string  `json:"last_gc_at,omitempty" doc:"RFC3339 timestamp of last maintenance run."`
	LastGCStatus string  `json:"last_gc_status,omitempty" doc:"Status of last maintenance run ('success' or 'failed')."`
}

// Session types moved to huma_types_sessions.go.
