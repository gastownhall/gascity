package contract

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

// PreflightBDContext is the bd-reported backend state for a beads scope.
type PreflightBDContext struct {
	Backend  string
	DoltMode string
}

// PreflightChecker evaluates whether a beads scope may use native storage.
type PreflightChecker struct {
	// FS reads .beads/metadata.json. A nil FS uses fsys.OSFS.
	FS fsys.FS
	// Provider is the already-resolved beads provider name from configuration.
	Provider string
	// BDContext reads bd context state for the scope.
	BDContext func(scope string) (PreflightBDContext, error)
	// DatabaseProjectID reads the authoritative database _project_id for the scope.
	DatabaseProjectID func(scope string) (string, bool, error)
}

// Check runs the beads backend preflight for scope and returns typed diagnostics.
func (c PreflightChecker) Check(scope string) (PreflightResult, error) {
	metadata, err := c.readMetadata(scope)
	if err != nil {
		return PreflightResult{}, err
	}

	checks := []PreflightCheckResult{
		c.checkProvider(),
		c.checkMetadataBackend(metadata),
		c.checkBDContextAgreement(scope, metadata),
		c.checkIdentityMatch(scope, metadata),
		c.checkContractShape(metadata),
	}
	verdict := preflightVerdictForChecks(checks)
	result := PreflightResult{
		Verdict:             verdict,
		Scope:               scope,
		Checks:              checks,
		RepairSteps:         preflightRepairSteps(checks),
		NativeStoreEligible: verdict == PreflightVerdictEligible,
	}
	if verdict != PreflightVerdictEligible {
		result.Fallback = PreflightFallbackBdStore
	}
	return NewPreflightResult(result), nil
}

func (c PreflightChecker) readMetadata(scope string) (preflightMetadata, error) {
	files := c.FS
	if files == nil {
		files = fsys.OSFS{}
	}
	path := filepath.Join(scope, ".beads", "metadata.json")
	data, err := files.ReadFile(path)
	if err != nil {
		return preflightMetadata{}, fmt.Errorf("read preflight metadata %s: %w", path, err)
	}
	var metadata preflightMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return preflightMetadata{}, fmt.Errorf("parse preflight metadata %s: %w", path, err)
	}
	return metadata.trimmed(), nil
}

func (c PreflightChecker) checkProvider() PreflightCheckResult {
	provider := strings.TrimSpace(c.Provider)
	details := PreflightDetails{Provider: provider}
	switch provider {
	case "bd", "exec:gc-beads-bd":
		return NewPreflightCheckResult(PreflightCheckProviderContract, PreflightCheckPass, "Provider exposes bd contract", details)
	case "":
		return NewPreflightCheckResult(PreflightCheckProviderContract, PreflightCheckFail, "Beads provider is not configured", details)
	default:
		return NewPreflightCheckResult(PreflightCheckProviderContract, PreflightCheckFail, fmt.Sprintf("Provider %q does not expose the bd contract", provider), details)
	}
}

func (c PreflightChecker) checkMetadataBackend(metadata preflightMetadata) PreflightCheckResult {
	hasDSN := metadata.hasPostgresDSN()
	hasSplit := metadata.hasPostgresSplitFields()
	details := PreflightDetails{
		MetadataBackend:     metadata.Backend,
		HasPostgresDSN:      boolPtr(hasDSN),
		HasSplitFields:      boolPtr(hasSplit),
		PostgresDSNRedacted: metadata.PostgresDSN,
		PostgresPassword:    metadata.PostgresPassword,
	}
	switch metadata.Backend {
	case "dolt":
		return NewPreflightCheckResult(PreflightCheckMetadataBackend, PreflightCheckPass, "Metadata backend is dolt", details)
	case "postgres":
		if hasDSN && !hasSplit {
			return NewPreflightCheckResult(PreflightCheckMetadataBackend, PreflightCheckWarn, "Metadata backend is postgres (postgres_dsn form)", details)
		}
		return NewPreflightCheckResult(PreflightCheckMetadataBackend, PreflightCheckFail, "Metadata backend is postgres; native store supports dolt only", details)
	case "":
		return NewPreflightCheckResult(PreflightCheckMetadataBackend, PreflightCheckFail, "Metadata backend is missing", details)
	default:
		return NewPreflightCheckResult(PreflightCheckMetadataBackend, PreflightCheckFail, fmt.Sprintf("Metadata backend %q is unsupported", metadata.Backend), details)
	}
}

func (c PreflightChecker) checkBDContextAgreement(scope string, metadata preflightMetadata) PreflightCheckResult {
	details := PreflightDetails{MetadataBackend: metadata.Backend}
	if c.BDContext == nil {
		return NewPreflightCheckResult(PreflightCheckBDContextAgreement, PreflightCheckFail, "bd context reader is not configured", details)
	}
	ctx, err := c.BDContext(scope)
	details.BDContextBackend = strings.TrimSpace(ctx.Backend)
	details.BDContextDoltMode = strings.TrimSpace(ctx.DoltMode)
	if err != nil {
		return NewPreflightCheckResult(PreflightCheckBDContextAgreement, PreflightCheckFail, "bd context is unreachable", details)
	}
	if details.MetadataBackend == "" || details.BDContextBackend == "" {
		return NewPreflightCheckResult(PreflightCheckBDContextAgreement, PreflightCheckFail, "bd context agreement cannot be determined", details)
	}
	if details.MetadataBackend != details.BDContextBackend {
		return NewPreflightCheckResult(PreflightCheckBDContextAgreement, PreflightCheckFail, fmt.Sprintf("Metadata backend=%s; bd context reports backend=%s", details.MetadataBackend, details.BDContextBackend), details)
	}
	if details.BDContextBackend != "dolt" {
		return NewPreflightCheckResult(PreflightCheckBDContextAgreement, PreflightCheckPass, "bd context agrees with metadata backend", details)
	}
	switch details.BDContextDoltMode {
	case "server":
		return NewPreflightCheckResult(PreflightCheckBDContextAgreement, PreflightCheckPass, "bd context reports dolt server mode", details)
	case "embedded":
		return NewPreflightCheckResult(PreflightCheckBDContextAgreement, PreflightCheckWarn, "bd context reports dolt embedded mode", details)
	default:
		return NewPreflightCheckResult(PreflightCheckBDContextAgreement, PreflightCheckFail, "bd context reports unsupported dolt mode", details)
	}
}

func (c PreflightChecker) checkIdentityMatch(scope string, metadata preflightMetadata) PreflightCheckResult {
	details := PreflightDetails{MetadataProjectID: metadata.ProjectID}
	if metadata.ProjectID == "" {
		return NewPreflightCheckResult(PreflightCheckIdentityMatch, PreflightCheckFail, "metadata project_id is missing", details)
	}
	if c.DatabaseProjectID == nil {
		return NewPreflightCheckResult(PreflightCheckIdentityMatch, PreflightCheckWarn, "database project_id reader is not configured", details)
	}
	dbProjectID, ok, err := c.DatabaseProjectID(scope)
	details.DBProjectID = strings.TrimSpace(dbProjectID)
	if err != nil || !ok || details.DBProjectID == "" {
		return NewPreflightCheckResult(PreflightCheckIdentityMatch, PreflightCheckWarn, "database project_id could not be confirmed", details)
	}
	if metadata.ProjectID != details.DBProjectID {
		return NewPreflightCheckResult(PreflightCheckIdentityMatch, PreflightCheckFail, "project_id mismatch", details)
	}
	return NewPreflightCheckResult(PreflightCheckIdentityMatch, PreflightCheckPass, "project_id matches", details)
}

func (c PreflightChecker) checkContractShape(metadata preflightMetadata) PreflightCheckResult {
	hasDSN := metadata.hasPostgresDSN()
	hasSplit := metadata.hasPostgresSplitFields()
	details := PreflightDetails{
		MetadataBackend:     metadata.Backend,
		HasPostgresDSN:      boolPtr(hasDSN),
		HasSplitFields:      boolPtr(hasSplit),
		PostgresDSNRedacted: metadata.PostgresDSN,
		PostgresPassword:    metadata.PostgresPassword,
		PostgresHost:        metadata.PostgresHost,
		PostgresPort:        metadata.PostgresPort,
		PostgresUser:        metadata.PostgresUser,
		PostgresDatabase:    metadata.PostgresDatabase,
	}
	if hasDSN && hasSplit {
		return NewPreflightCheckResult(PreflightCheckContractShape, PreflightCheckFail, "postgres_dsn and split postgres fields are both present", details)
	}
	switch metadata.Backend {
	case "dolt":
		if hasDSN || hasSplit {
			return NewPreflightCheckResult(PreflightCheckContractShape, PreflightCheckFail, "dolt metadata contains postgres fields", details)
		}
		return NewPreflightCheckResult(PreflightCheckContractShape, PreflightCheckPass, "Metadata uses dolt shape", details)
	case "postgres":
		if hasDSN {
			return NewPreflightCheckResult(PreflightCheckContractShape, PreflightCheckWarn, "postgres_dsn present; Gas City expects split fields", details)
		}
		if metadata.hasCompletePostgresSplitFields() {
			return NewPreflightCheckResult(PreflightCheckContractShape, PreflightCheckPass, "Metadata uses split postgres shape", details)
		}
		return NewPreflightCheckResult(PreflightCheckContractShape, PreflightCheckFail, "postgres metadata split fields are incomplete", details)
	case "":
		return NewPreflightCheckResult(PreflightCheckContractShape, PreflightCheckFail, "metadata backend is missing", details)
	default:
		return NewPreflightCheckResult(PreflightCheckContractShape, PreflightCheckFail, fmt.Sprintf("metadata backend %q has unsupported contract shape", metadata.Backend), details)
	}
}

type preflightMetadata struct {
	Backend          string `json:"backend"`
	DoltMode         string `json:"dolt_mode"`
	DoltDatabase     string `json:"dolt_database"`
	PostgresDSN      string `json:"postgres_dsn"`
	PostgresPassword string `json:"postgres_password"`
	PostgresHost     string `json:"postgres_host"`
	PostgresPort     string `json:"postgres_port"`
	PostgresUser     string `json:"postgres_user"`
	PostgresDatabase string `json:"postgres_database"`
	ProjectID        string `json:"project_id"`
}

func (m preflightMetadata) trimmed() preflightMetadata {
	m.Backend = strings.TrimSpace(m.Backend)
	m.DoltMode = strings.TrimSpace(m.DoltMode)
	m.DoltDatabase = strings.TrimSpace(m.DoltDatabase)
	m.PostgresDSN = strings.TrimSpace(m.PostgresDSN)
	m.PostgresPassword = strings.TrimSpace(m.PostgresPassword)
	m.PostgresHost = strings.TrimSpace(m.PostgresHost)
	m.PostgresPort = strings.TrimSpace(m.PostgresPort)
	m.PostgresUser = strings.TrimSpace(m.PostgresUser)
	m.PostgresDatabase = strings.TrimSpace(m.PostgresDatabase)
	m.ProjectID = strings.TrimSpace(m.ProjectID)
	return m
}

func (m preflightMetadata) hasPostgresDSN() bool {
	return m.PostgresDSN != ""
}

func (m preflightMetadata) hasPostgresSplitFields() bool {
	return m.PostgresHost != "" || m.PostgresPort != "" || m.PostgresUser != "" || m.PostgresDatabase != ""
}

func (m preflightMetadata) hasCompletePostgresSplitFields() bool {
	return m.PostgresHost != "" && m.PostgresPort != "" && m.PostgresUser != "" && m.PostgresDatabase != ""
}

func preflightVerdictForChecks(checks []PreflightCheckResult) PreflightVerdict {
	hasWarn := false
	for _, check := range checks {
		switch check.State {
		case PreflightCheckFail:
			return PreflightVerdictBlocked
		case PreflightCheckWarn:
			hasWarn = true
		}
	}
	if hasWarn {
		return PreflightVerdictDegraded
	}
	return PreflightVerdictEligible
}

func preflightRepairSteps(checks []PreflightCheckResult) []PreflightRepairStep {
	var steps []PreflightRepairStep
	for _, check := range checks {
		switch check.ID {
		case PreflightCheckMetadataBackend:
			if check.State == PreflightCheckFail {
				steps = append(steps, PreflightRepairStep{
					CheckID:  check.ID,
					Priority: PreflightRepairRecommended,
					Command:  "bd bootstrap",
					Note:     "Re-anchor metadata to the active beads backend, or continue using BdStore for postgres scopes.",
				})
			}
		case PreflightCheckBDContextAgreement:
			if check.State == PreflightCheckFail {
				steps = append(steps, PreflightRepairStep{
					CheckID:  check.ID,
					Priority: PreflightRepairRecommended,
					Command:  "bd context --json",
					Note:     "Inspect which .beads scope bd resolves before repairing metadata.",
				})
			}
		case PreflightCheckIdentityMatch:
			if check.State == PreflightCheckFail {
				steps = append(steps, PreflightRepairStep{
					CheckID:  check.ID,
					Priority: PreflightRepairCritical,
					Command:  "bd doctor --fix",
					Note:     "Identity mismatch is the highest-severity failure.",
				})
			}
		case PreflightCheckContractShape:
			if check.State == PreflightCheckFail || check.State == PreflightCheckWarn {
				steps = append(steps, PreflightRepairStep{
					CheckID:  check.ID,
					Priority: PreflightRepairRecommended,
					Command:  "bd bootstrap",
					Note:     "Rewrite metadata to the canonical backend field shape.",
				})
			}
		}
	}
	return steps
}

func boolPtr(value bool) *bool {
	return &value
}
