package connect

import "errors"

// Code is a stable, machine-readable refusal code. Codes appear verbatim as
// `error.code` in `gc connect --json` output, so a published code is never
// renamed or reused.
type Code string

// Bundle refusals.
const (
	// CodeBundleInvalid means the bundle does not parse or breaks a structural rule.
	CodeBundleInvalid Code = "connect_bundle_invalid"
	// CodeBundleVersion means the bundle schema version is not one this gc decodes.
	CodeBundleVersion Code = "connect_bundle_version"
	// CodeContractUnsupported means the bundle asks for a worker-site contract this
	// gc does not implement.
	CodeContractUnsupported Code = "connect_contract_unsupported"
	// CodeBundleInsecureFile means the bundle file is readable beyond its owner.
	CodeBundleInsecureFile Code = "connect_bundle_insecure_file"
)

// City refusals.
const (
	// CodeNoCity means no city directory encloses the working directory.
	CodeNoCity Code = "connect_no_city"
	// CodeCityMismatch means the bundle names a different city than the one found.
	CodeCityMismatch Code = "connect_city_mismatch"
	// CodeControllerHost means the city directory belongs to a controller host.
	CodeControllerHost Code = "connect_controller_host"
	// CodeStorageMismatch means the box's [storage] assigns a class to a
	// different binding than the bundle's ledger for it.
	CodeStorageMismatch Code = "connect_storage_mismatch"
)

// Environment refusals.
const (
	// CodeCompetingSelector means the environment already selects a store some
	// other way.
	CodeCompetingSelector Code = "connect_competing_selector"
	// CodeIdentityIncomplete means the staged identity environment is missing.
	CodeIdentityIncomplete Code = "connect_identity_incomplete"
	// CodeIdentityMismatch means the staged identity disagrees with the bundle.
	CodeIdentityMismatch Code = "connect_identity_mismatch"
)

// Box-state refusals.
const (
	// CodeSessionConflict means the box is already connected for another session.
	CodeSessionConflict Code = "connect_session_conflict"
	// CodeGenerationStale means the bundle's generation is older than the box's.
	CodeGenerationStale Code = "connect_generation_stale"
	// CodeCohostedSession means another session already shares this box's HOME.
	CodeCohostedSession Code = "connect_cohosted_session"
)

// Workspace refusals.
const (
	// CodeWorkspaceUnexpected means a workspace exists that the bundle cannot cover.
	CodeWorkspaceUnexpected Code = "connect_workspace_unexpected"
	// CodeWorkspaceUnproven means a workspace's identity cannot be proven.
	CodeWorkspaceUnproven Code = "connect_workspace_unproven"
	// CodeWorkspaceProjectMismatch means a workspace belongs to another project.
	CodeWorkspaceProjectMismatch Code = "connect_workspace_project_mismatch"
)

// Ledger refusals.
const (
	// CodeLedgerUnreachable means the ledger endpoint cannot be reached.
	CodeLedgerUnreachable Code = "connect_ledger_unreachable"
	// CodeLedgerUnauthorized means the ledger rejects the bearer.
	CodeLedgerUnauthorized Code = "connect_ledger_unauthorized"
	// CodeLedgerProjectMismatch means the ledger serves a different project.
	CodeLedgerProjectMismatch Code = "connect_ledger_project_mismatch"
	// CodeWireIncompatible means the ledger speaks an incompatible wire version.
	CodeWireIncompatible Code = "connect_wire_incompatible"
	// CodeJournalRequired means the bundle requires a ledger journal that is off.
	CodeJournalRequired Code = "connect_journal_required"
)

// Final-check refusals.
const (
	// CodeJournalNeutralizeFailed means the box-local journal could not be turned off.
	CodeJournalNeutralizeFailed Code = "connect_journal_neutralize_failed"
	// CodeBdTooling means the bd tooling on the box is missing or unusable.
	CodeBdTooling Code = "connect_bd_tooling"
	// CodeSelfVerifyFailed means the post-write self-verification read failed.
	CodeSelfVerifyFailed Code = "connect_self_verify_failed"
)

// CodeNotConnected means `status` or `--refresh` found no worker-site descriptor.
const CodeNotConnected Code = "connect_not_connected"

// Codes lists every connect refusal code in the order §3.8 publishes them.
func Codes() []Code {
	return []Code{
		CodeBundleInvalid, CodeBundleVersion, CodeContractUnsupported, CodeBundleInsecureFile,
		CodeNoCity, CodeCityMismatch, CodeControllerHost, CodeStorageMismatch,
		CodeCompetingSelector, CodeIdentityIncomplete, CodeIdentityMismatch,
		CodeSessionConflict, CodeGenerationStale, CodeCohostedSession,
		CodeWorkspaceUnexpected, CodeWorkspaceUnproven, CodeWorkspaceProjectMismatch,
		CodeLedgerUnreachable, CodeLedgerUnauthorized, CodeLedgerProjectMismatch,
		CodeWireIncompatible, CodeJournalRequired,
		CodeJournalNeutralizeFailed, CodeBdTooling, CodeSelfVerifyFailed,
		CodeNotConnected,
	}
}

// Refusal is a typed connect refusal. Detail is human text; it never carries
// a secret (a bearer, a key, or URL userinfo).
type Refusal struct {
	Code   Code
	Detail string
	// Err is an optional underlying cause, reachable through errors.Unwrap.
	Err error
}

// Error renders the refusal as "<code>: <detail>".
func (r *Refusal) Error() string {
	if r.Detail == "" {
		return string(r.Code)
	}
	return string(r.Code) + ": " + r.Detail
}

// Unwrap returns the underlying cause, if any.
func (r *Refusal) Unwrap() error { return r.Err }

// CodeOf returns the code of the first *Refusal in err's chain.
func CodeOf(err error) (Code, bool) {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal.Code, true
	}
	return "", false
}

func refuse(code Code, detail string) *Refusal {
	return &Refusal{Code: code, Detail: detail}
}
