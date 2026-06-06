package contract

import "strings"

// PreflightCheckState is the state for one beads backend preflight check.
type PreflightCheckState string

const (
	// PreflightCheckPass means a check passed.
	PreflightCheckPass PreflightCheckState = "PASS"
	// PreflightCheckWarn means a check found degraded but usable state.
	PreflightCheckWarn PreflightCheckState = "WARN"
	// PreflightCheckFail means a check found a hard blocker.
	PreflightCheckFail PreflightCheckState = "FAIL"
)

// PreflightVerdict is the aggregate native-store eligibility result.
type PreflightVerdict string

const (
	// PreflightVerdictEligible means native store activation is allowed.
	PreflightVerdictEligible PreflightVerdict = "ELIGIBLE"
	// PreflightVerdictDegraded means native store activation requires explicit opt-in.
	PreflightVerdictDegraded PreflightVerdict = "DEGRADED"
	// PreflightVerdictBlocked means native store activation must not proceed.
	PreflightVerdictBlocked PreflightVerdict = "BLOCKED"
)

// PreflightFallback names the store path used when native activation is unavailable.
type PreflightFallback string

const (
	// PreflightFallbackNone means no fallback is needed.
	PreflightFallbackNone PreflightFallback = ""
	// PreflightFallbackBdStore means callers should keep using the bd-backed store.
	PreflightFallbackBdStore PreflightFallback = "BdStore"
)

// PreflightCheckID identifies a stable preflight check.
type PreflightCheckID string

const (
	// PreflightCheckProviderContract validates the configured beads provider contract.
	PreflightCheckProviderContract PreflightCheckID = "provider_contract"
	// PreflightCheckMetadataBackend validates the backend recorded in metadata.json.
	PreflightCheckMetadataBackend PreflightCheckID = "metadata_backend"
	// PreflightCheckBDContextAgreement validates agreement with bd context.
	PreflightCheckBDContextAgreement PreflightCheckID = "bd_context_agreement"
	// PreflightCheckDoltModeSafe validates that Dolt runs in a native-safe mode.
	PreflightCheckDoltModeSafe PreflightCheckID = "dolt_mode_safe"
	// PreflightCheckIdentityMatch validates metadata and database project identity.
	PreflightCheckIdentityMatch PreflightCheckID = "identity_match"
	// PreflightCheckVersionCompat validates the bd CLI and linked beads library version.
	PreflightCheckVersionCompat PreflightCheckID = "version_compat"
	// PreflightCheckContractShape validates backend-specific metadata field shape.
	PreflightCheckContractShape PreflightCheckID = "contract_shape"
)

// PreflightRepairPriority is the severity/ordering hint for one repair step.
type PreflightRepairPriority string

const (
	// PreflightRepairCritical is for repair actions that protect database identity.
	PreflightRepairCritical PreflightRepairPriority = "critical"
	// PreflightRepairRecommended is for non-critical cleanup actions.
	PreflightRepairRecommended PreflightRepairPriority = "recommended"
)

// PreflightResult is the typed diagnostic result for beads backend preflight.
type PreflightResult struct {
	Verdict             PreflightVerdict       `json:"verdict"`
	Scope               string                 `json:"scope"`
	Checks              []PreflightCheckResult `json:"checks"`
	RepairSteps         []PreflightRepairStep  `json:"repair_steps,omitempty"`
	NativeStoreEligible bool                   `json:"native_store_eligible"`
	Fallback            PreflightFallback      `json:"fallback,omitempty"`
	FallbackReason      string                 `json:"fallback_reason,omitempty"`
}

// NewPreflightResult returns result with all nested diagnostic details redacted.
func NewPreflightResult(result PreflightResult) PreflightResult {
	return result.Redacted()
}

// Redacted returns a copy of result with all check details sanitized.
func (r PreflightResult) Redacted() PreflightResult {
	if len(r.Checks) > 0 {
		checks := make([]PreflightCheckResult, len(r.Checks))
		for i, check := range r.Checks {
			checks[i] = check.Redacted()
		}
		r.Checks = checks
	}
	return r
}

// PreflightCheckResult is the typed diagnostic for one preflight check.
type PreflightCheckResult struct {
	ID      PreflightCheckID    `json:"id"`
	State   PreflightCheckState `json:"state"`
	Summary string              `json:"summary"`
	Details PreflightDetails    `json:"details"`
}

// NewPreflightCheckResult returns a check result with redacted details.
func NewPreflightCheckResult(id PreflightCheckID, state PreflightCheckState, summary string, details PreflightDetails) PreflightCheckResult {
	return PreflightCheckResult{
		ID:      id,
		State:   state,
		Summary: summary,
		Details: details.Redacted(),
	}
}

// Redacted returns a copy of check with sensitive detail values sanitized.
func (c PreflightCheckResult) Redacted() PreflightCheckResult {
	c.Details = c.Details.Redacted()
	return c
}

// PreflightDetails is the typed details payload for preflight diagnostics.
type PreflightDetails struct {
	Provider              string                 `json:"provider,omitempty"`
	MetadataBackend       string                 `json:"metadata_backend,omitempty"`
	BDContextBackend      string                 `json:"bd_context_backend,omitempty"`
	BDContextDoltMode     string                 `json:"bd_context_dolt_mode,omitempty"`
	BDVersion             string                 `json:"bd_version,omitempty"`
	BeadsLibraryVersion   string                 `json:"beads_library_version,omitempty"`
	SchemaVersion         int                    `json:"schema_version,omitempty"`
	HasPostgresDSN        *bool                  `json:"has_postgres_dsn,omitempty"`
	HasSplitFields        *bool                  `json:"has_split_fields,omitempty"`
	PostgresDSNRedacted   string                 `json:"postgres_dsn_redacted,omitempty"`
	PostgresPassword      string                 `json:"postgres_password,omitempty"`
	PostgresHost          string                 `json:"postgres_host,omitempty"`
	PostgresPort          string                 `json:"postgres_port,omitempty"`
	PostgresUser          string                 `json:"postgres_user,omitempty"`
	PostgresDatabase      string                 `json:"postgres_database,omitempty"`
	MetadataProjectID     string                 `json:"metadata_project_id,omitempty"`
	DBProjectID           string                 `json:"db_project_id,omitempty"`
	Expected              string                 `json:"expected,omitempty"`
	AuthToken             string                 `json:"auth_token,omitempty"`
	APIKey                string                 `json:"api_key,omitempty"`
	AdditionalDiagnostics []PreflightDetailField `json:"additional_diagnostics,omitempty"`
}

// Redacted returns a copy of details with secret-bearing fields sanitized.
func (d PreflightDetails) Redacted() PreflightDetails {
	d.PostgresDSNRedacted = redactPreflightDetail("postgres_dsn", d.PostgresDSNRedacted)
	d.PostgresPassword = redactPreflightDetail("postgres_password", d.PostgresPassword)
	d.AuthToken = redactPreflightDetail("auth_token", d.AuthToken)
	d.APIKey = redactPreflightDetail("api_key", d.APIKey)
	if len(d.AdditionalDiagnostics) > 0 {
		fields := make([]PreflightDetailField, len(d.AdditionalDiagnostics))
		for i, field := range d.AdditionalDiagnostics {
			fields[i] = field.Redacted()
		}
		d.AdditionalDiagnostics = fields
	}
	return d
}

// PreflightDetailField is a typed extension field for diagnostics not covered
// by the stable PreflightDetails fields.
type PreflightDetailField struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Redacted returns a copy of field with a sanitized value when its key is sensitive.
func (f PreflightDetailField) Redacted() PreflightDetailField {
	f.Value = redactPreflightDetail(f.Key, f.Value)
	return f
}

// PreflightRepairStep describes one operator-run repair action.
type PreflightRepairStep struct {
	CheckID  PreflightCheckID        `json:"check_id"`
	Priority PreflightRepairPriority `json:"priority"`
	Command  string                  `json:"command,omitempty"`
	Note     string                  `json:"note,omitempty"`
}

const preflightRedactedValue = "[REDACTED]"

func redactPreflightDetail(key, value string) string {
	if strings.TrimSpace(value) == "" {
		return value
	}
	normalized := strings.ToLower(strings.TrimSpace(key))
	if strings.Contains(normalized, "postgres_dsn") {
		return redactPreflightDSN(value)
	}
	if preflightDetailKeyIsSensitive(normalized) {
		return preflightRedactedValue
	}
	return value
}

func redactPreflightDSN(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	if scheme, _, ok := strings.Cut(value, "://"); ok {
		return scheme + "://" + preflightRedactedValue
	}
	return preflightRedactedValue
}

func preflightDetailKeyIsSensitive(key string) bool {
	for _, needle := range []string{"password", "passwd", "secret", "token", "key"} {
		if strings.Contains(key, needle) {
			return true
		}
	}
	return false
}
