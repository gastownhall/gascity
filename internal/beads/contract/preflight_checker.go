package contract

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

// PreflightBDContext is the bd-reported backend state for a beads scope.
type PreflightBDContext struct {
	Backend       string
	DoltMode      string
	BDVersion     string
	SchemaVersion int
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
	// BeadsLibraryVersion is the linked github.com/steveyegge/beads module
	// version. Empty means infer it from build info.
	BeadsLibraryVersion string
}

// Check runs the beads backend preflight for scope and returns typed diagnostics.
func (c PreflightChecker) Check(scope string) (PreflightResult, error) {
	metadata, err := c.readMetadata(scope)
	if err != nil {
		return PreflightResult{}, err
	}
	bdCtx, bdCtxErr := c.readBDContext(scope)

	checks := []PreflightCheckResult{
		c.checkProvider(),
		c.checkMetadataBackend(metadata),
		c.checkBDContextAgreement(metadata, bdCtx, bdCtxErr),
		c.checkDoltModeSafe(metadata, bdCtx, bdCtxErr),
		c.checkIdentityMatch(scope, metadata),
		c.checkVersionCompat(bdCtx, bdCtxErr),
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
		result.FallbackReason = preflightFallbackReason(checks)
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
	switch {
	case ProviderUsesBDContract(provider):
		return NewPreflightCheckResult(PreflightCheckProviderContract, PreflightCheckPass, "Provider exposes bd contract", details)
	case provider == "":
		return NewPreflightCheckResult(PreflightCheckProviderContract, PreflightCheckFail, "Beads provider is not configured", details)
	default:
		return NewPreflightCheckResult(PreflightCheckProviderContract, PreflightCheckFail, fmt.Sprintf("Provider %q does not expose the bd contract", provider), details)
	}
}

// ProviderUsesBDContract reports whether provider exposes the bd-compatible
// store contract needed for native-store preflight and fallback decisions.
func ProviderUsesBDContract(provider string) bool {
	provider = strings.TrimSpace(provider)
	if provider == "" || provider == "bd" {
		return true
	}
	if !strings.HasPrefix(provider, "exec:") {
		return false
	}
	base := strings.TrimSuffix(filepath.Base(strings.TrimPrefix(provider, "exec:")), ".sh")
	return base == "gc-beads-bd"
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

func (c PreflightChecker) readBDContext(scope string) (PreflightBDContext, error) {
	if c.BDContext == nil {
		return PreflightBDContext{}, fmt.Errorf("bd context reader is not configured")
	}
	ctx, err := c.BDContext(scope)
	ctx.Backend = strings.TrimSpace(ctx.Backend)
	ctx.DoltMode = strings.TrimSpace(ctx.DoltMode)
	ctx.BDVersion = strings.TrimSpace(ctx.BDVersion)
	return ctx, err
}

func (c PreflightChecker) checkBDContextAgreement(metadata preflightMetadata, ctx PreflightBDContext, err error) PreflightCheckResult {
	details := PreflightDetails{MetadataBackend: metadata.Backend}
	details.BDContextBackend = ctx.Backend
	if err != nil {
		return NewPreflightCheckResult(PreflightCheckBDContextAgreement, PreflightCheckFail, "bd context is unreachable", details)
	}
	if details.MetadataBackend == "" || details.BDContextBackend == "" {
		return NewPreflightCheckResult(PreflightCheckBDContextAgreement, PreflightCheckFail, "bd context agreement cannot be determined", details)
	}
	if details.MetadataBackend != details.BDContextBackend {
		return NewPreflightCheckResult(PreflightCheckBDContextAgreement, PreflightCheckFail, fmt.Sprintf("Metadata backend=%s; bd context reports backend=%s", details.MetadataBackend, details.BDContextBackend), details)
	}
	return NewPreflightCheckResult(PreflightCheckBDContextAgreement, PreflightCheckPass, "bd context agrees with metadata backend", details)
}

func (c PreflightChecker) checkDoltModeSafe(metadata preflightMetadata, ctx PreflightBDContext, err error) PreflightCheckResult {
	details := PreflightDetails{
		MetadataBackend:   metadata.Backend,
		BDContextBackend:  ctx.Backend,
		BDContextDoltMode: ctx.DoltMode,
	}
	if err != nil {
		return NewPreflightCheckResult(PreflightCheckDoltModeSafe, PreflightCheckFail, "bd context is unreachable", details)
	}
	if metadata.Backend != "dolt" || ctx.Backend != "dolt" {
		return NewPreflightCheckResult(PreflightCheckDoltModeSafe, PreflightCheckPass, "Dolt mode check is not required for non-dolt backend", details)
	}
	switch ctx.DoltMode {
	case "server":
		return NewPreflightCheckResult(PreflightCheckDoltModeSafe, PreflightCheckPass, "bd context reports dolt server mode", details)
	case "embedded":
		return NewPreflightCheckResult(PreflightCheckDoltModeSafe, PreflightCheckFail, "dolt_mode=embedded requires server mode or native_embedded build tag", details)
	default:
		return NewPreflightCheckResult(PreflightCheckDoltModeSafe, PreflightCheckFail, "bd context reports unsupported dolt mode", details)
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

func (c PreflightChecker) checkVersionCompat(ctx PreflightBDContext, err error) PreflightCheckResult {
	libraryVersion := strings.TrimPrefix(strings.TrimSpace(c.BeadsLibraryVersion), "v")
	if libraryVersion == "" {
		libraryVersion = strings.TrimPrefix(beadsModuleVersion(), "v")
	}
	details := PreflightDetails{
		BDVersion:           ctx.BDVersion,
		BeadsLibraryVersion: libraryVersion,
		SchemaVersion:       ctx.SchemaVersion,
	}
	if err != nil {
		return NewPreflightCheckResult(PreflightCheckVersionCompat, PreflightCheckFail, "bd context is unreachable", details)
	}
	if ctx.SchemaVersion <= 0 {
		return NewPreflightCheckResult(PreflightCheckVersionCompat, PreflightCheckFail, "bd context did not report a schema version", details)
	}
	if ctx.BDVersion == "" || libraryVersion == "" || libraryVersion == "(devel)" {
		return NewPreflightCheckResult(PreflightCheckVersionCompat, PreflightCheckWarn, "bd/beads version compatibility could not be confirmed", details)
	}
	if strings.TrimPrefix(ctx.BDVersion, "v") != libraryVersion {
		return NewPreflightCheckResult(PreflightCheckVersionCompat, PreflightCheckFail, "bd version differs from linked beads library version", details)
	}
	return NewPreflightCheckResult(PreflightCheckVersionCompat, PreflightCheckPass, "bd and linked beads library versions match", details)
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

func preflightFallbackReason(checks []PreflightCheckResult) string {
	for _, check := range checks {
		if check.State == PreflightCheckFail {
			return check.Summary
		}
	}
	for _, check := range checks {
		if check.State == PreflightCheckWarn {
			return check.Summary
		}
	}
	return ""
}

func beadsModuleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/steveyegge/beads" {
			if dep.Replace != nil && dep.Replace.Version != "" {
				return dep.Replace.Version
			}
			return dep.Version
		}
	}
	return ""
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
		case PreflightCheckDoltModeSafe:
			if check.State == PreflightCheckFail {
				steps = append(steps, PreflightRepairStep{
					CheckID:  check.ID,
					Priority: PreflightRepairRecommended,
					Command:  "bd context --json",
					Note:     "Native store activation requires Dolt server mode.",
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
		case PreflightCheckVersionCompat:
			if check.State == PreflightCheckFail {
				steps = append(steps, PreflightRepairStep{
					CheckID:  check.ID,
					Priority: PreflightRepairRecommended,
					Command:  "bd doctor",
					Note:     "Verify the installed bd CLI and linked beads library are compatible.",
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
