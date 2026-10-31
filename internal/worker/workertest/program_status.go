package workertest

import (
	"fmt"
	"strconv"
)

// ProgramStatusDeclaration is a program-status declaration as gc arms it:
// whether capture is declared, the program name the capture reader accepts
// reports from, and whether the program runs under the probe-answering relay.
type ProgramStatusDeclaration struct {
	Declared bool
	App      string
	Relay    bool
}

// ProgramStatusExpectation is a profile's OSC 7501 program-status
// declaration and its program's recorded emission. The declaration mirrors
// gc's provider data for the profile's family; a profile that declares is
// held to every landed WC-PSTAT row.
type ProgramStatusExpectation struct {
	ProgramStatusDeclaration
	// Verified names the program build whose terminal output Emission
	// records (for example "codex-cli 0.165.0").
	Verified string
	// Emission is that build's OSC 7501 output at each turn boundary.
	Emission ProgramStatusEmission
}

// ProgramStatusEmission is a program's recorded OSC 7501 output as emitter
// script steps (package runtimetest/osc7501emitter/emit) at the boundaries
// WC-PSTAT-001 reads: from launch to its first report, at a turn's start,
// and at that turn's end. A probe-gated program's FirstReport includes the
// probe it writes before reporting: the steps are the bytes the pane sees
// with the relay answering that probe.
type ProgramStatusEmission struct {
	FirstReport []string
	TurnStart   []string
	TurnEnd     []string
}

// osc7501ProbeBELHex is the OSC 7501 support probe, ESC ] 7501 ; ? BEL, as
// an emitter raw step's hex: the bytes a probe-gated program writes before
// it reports.
const osc7501ProbeBELHex = "1b5d373530313b3f07"

// programStatusPending names, for each WC-PSTAT row whose worker behavior
// has not landed, the slice that lands it. A row leaves this map in the
// change that lands its behavior and registers its check.
var programStatusPending = map[RequirementCode]string{
	RequirementProgramStatusPhase:              "ga-f4ovjm.16",
	RequirementProgramStatusPendingKindOnly:    "ga-f4ovjm.22",
	RequirementProgramStatusQueueBehindBlocked: "ga-f4ovjm.22",
	RequirementProgramStatusImmediateRefused:   "ga-f4ovjm.22",
	RequirementProgramStatusReadiness:          "ga-f4ovjm.23",
	RequirementProgramStatusShadowIdentical:    "ga-f4ovjm.17",
	RequirementProgramStatusTranscriptTurn:     "ga-f4ovjm.24",
}

// ProgramStatusPendingSlice returns the slice that lands code's behavior,
// and false once it has landed.
func ProgramStatusPendingSlice(code RequirementCode) (string, bool) {
	slice, ok := programStatusPending[code]
	return slice, ok
}

// ProgramStatusGate classifies profile for WC-PSTAT row code before the
// row's behavior runs. implementation is the declaration gc's provider data
// arms for the profile's family. A declaration that drifts from the
// profile's expectation (declared, app or relay) fails; an undeclared
// profile is out of scope (Unsupported); a row whose behavior has not landed
// is pending (Unsupported, naming its slice). run is true only when the row
// must be checked.
func ProgramStatusGate(profile Profile, code RequirementCode, implementation ProgramStatusDeclaration) (result Result, run bool) {
	expected := profile.ProgramStatus.ProgramStatusDeclaration
	evidence := map[string]string{
		"provider":                profile.Provider,
		"profile_declares":        strconv.FormatBool(expected.Declared),
		"implementation_declares": strconv.FormatBool(implementation.Declared),
	}
	if implementation.Declared {
		evidence["implementation_app"] = implementation.App
		evidence["implementation_relay"] = strconv.FormatBool(implementation.Relay)
	}
	switch slice, pending := ProgramStatusPendingSlice(code); {
	case implementation != expected:
		return Fail(profile.ID, code, fmt.Sprintf(
			"program-status declaration drift: provider data %+v, profile expectation %+v",
			implementation, expected)).WithEvidence(evidence), false
	case !expected.Declared:
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
