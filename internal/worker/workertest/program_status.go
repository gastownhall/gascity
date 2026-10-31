package workertest

import (
	"fmt"
	"strconv"
)

// ProgramStatusExpectation is a profile's OSC 7501 program-status
// declaration. Declared mirrors whether gc's provider data declares program
// status for the profile's family; a profile that declares it is held to
// every landed WC-PSTAT row.
type ProgramStatusExpectation struct {
	Declared bool
}

// programStatusPending names, for each WC-PSTAT row whose worker behavior
// has not landed, the slice that lands it. A row leaves this map in the
// change that lands its behavior and registers its check.
var programStatusPending = map[RequirementCode]string{
	RequirementProgramStatusSurfaces:           "ga-f4ovjm.18 (codex, claude), ga-f4ovjm.38 (other families)",
	RequirementProgramStatusPhase:              "ga-f4ovjm.16",
	RequirementProgramStatusPendingKindOnly:    "ga-f4ovjm.22",
	RequirementProgramStatusQueueBehindBlocked: "ga-f4ovjm.22",
	RequirementProgramStatusImmediateRefused:   "ga-f4ovjm.22",
	RequirementProgramStatusReadiness:          "ga-f4ovjm.23",
	RequirementProgramStatusTranscriptTurn:     "ga-f4ovjm.24",
}

// ProgramStatusPendingSlice returns the slice that lands code's behavior,
// and false once it has landed.
func ProgramStatusPendingSlice(code RequirementCode) (string, bool) {
	slice, ok := programStatusPending[code]
	return slice, ok
}

// ProgramStatusGate classifies profile for WC-PSTAT row code before the
// row's behavior runs. implementationDeclares is whether gc's provider data
// declares program status for the profile's family. A declaration that
// drifts from the profile's expectation fails; an undeclared profile is
// out of scope (Unsupported); a row whose behavior has not landed is
// pending (Unsupported, naming its slice). run is true only when the row
// must be checked.
func ProgramStatusGate(profile Profile, code RequirementCode, implementationDeclares bool) (result Result, run bool) {
	evidence := map[string]string{
		"provider":                profile.Provider,
		"profile_declares":        strconv.FormatBool(profile.ProgramStatus.Declared),
		"implementation_declares": strconv.FormatBool(implementationDeclares),
	}
	switch slice, pending := ProgramStatusPendingSlice(code); {
	case implementationDeclares != profile.ProgramStatus.Declared:
		return Fail(profile.ID, code, fmt.Sprintf(
			"program-status declaration drift: provider data declares=%t, profile expectation declared=%t",
			implementationDeclares, profile.ProgramStatus.Declared)).WithEvidence(evidence), false
	case !profile.ProgramStatus.Declared:
		return Unsupported(profile.ID, code,
			fmt.Sprintf("provider %q does not declare program status (out of scope)", profile.Provider)).WithEvidence(evidence), false
	case pending:
		evidence["pending_slice"] = slice
		return Unsupported(profile.ID, code,
			fmt.Sprintf("pending: the row's worker behavior lands in %s", slice)).WithEvidence(evidence), false
	default:
		return Result{}, true
	}
}
