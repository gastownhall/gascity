package workertest

import (
	"fmt"
	"strconv"
	"strings"
)

// ProgramStatusExpectation is a profile's OSC 7501 program-status
// declaration. Declared mirrors whether gc's provider data declares program
// status for the profile's family; a profile that declares it is held to
// every landed WC-PSTAT row.
type ProgramStatusExpectation struct {
	Declared bool
	// Capture is the family's program output, as gc's capture reads it: the
	// pane output stream a pipe-pane -O tap recorded while the program ran
	// under tmux with its declared env, against a local mock API, through
	// startup, a turn and exit. Relative to this package. Set when Declared.
	Capture string
	// Program names the program and version Capture was taken from.
	Program string
}

// Profile IDs of families whose program status is held to the WC-PSTAT rows
// but that have no phase-1 transcript fixtures.
const (
	ProfileOmpTmuxCLI  ProfileID = "omp/tmux-cli"  // Oh My Pi
	ProfileGrokTmuxCLI ProfileID = "grok/tmux-cli" // Grok Build
)

// ProgramStatusProfiles returns the profiles held to the WC-PSTAT rows:
// every phase-1 profile, then the families that declare program status
// without phase-1 transcript fixtures.
func ProgramStatusProfiles() []Profile {
	return append(Phase1Profiles(),
		Profile{
			ID:       ProfileOmpTmuxCLI,
			Provider: "omp/tmux-cli",
			ProgramStatus: ProgramStatusExpectation{
				Declared: true,
				Capture:  "testdata/program_status/omp-18.14.1.bin",
				Program:  "omp 18.14.1 (npm @oh-my-pi/pi-coding-agent) on bun 1.4.4",
			},
		},
		Profile{
			ID:       ProfileGrokTmuxCLI,
			Provider: "grok/tmux-cli",
			ProgramStatus: ProgramStatusExpectation{
				Declared: true,
				Capture:  "testdata/program_status/grok-1.0.70.bin",
				Program:  "grok 1.0.70 (grok-build-public-artifacts linux-x86_64)",
			},
		},
	)
}

// ProfileFamily returns the provider family a profile runs: its Provider up
// to the first "/".
func ProfileFamily(profile Profile) string {
	family, _, _ := strings.Cut(profile.Provider, "/")
	return family
}

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
