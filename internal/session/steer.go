package session

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// steerFamilies are the agent provider families whose CLI joins text typed
// while a turn runs into that turn, so a keystroke runtime can steer them
// (DES-O1 §10.4). It is empty: no family is keystroke-steerable yet.
//
//   - codex: tmux's nudge flow ends with Escape, which Codex reads as cancel,
//     so a typed steer interrupts the running turn instead of joining it.
//     Codex returns once tmux has a steer path that sends no Escape into a
//     busy pane, pinned by a tmux unit test of the keys a steer sends, and
//     X11's live check passes.
//   - claude: typed input during a turn is queued for the next turn, not
//     joined, and a busy pane never shows the idle-to-busy edge tmux
//     confirms a submit by.
//
// A family is added only once the runtime can confirm a steer without a turn
// start. Every family not here gets a steer per its on_unsupported policy.
var steerFamilies = map[string]bool{}

// supportsSteer reports whether a session of agent provider family on
// transport takes a steer from a runtime that steers in mode
// ([runtime.SteerModeFor]): a protocol steer for every family, a keystroke
// steer for a family in the steer table and never over ACP, whose prompts are
// whole turns. family may be a raw provider name such as "Codex":
// supportsSteer normalizes it with [sessionlog.ProviderFamily], which leaves
// a family [ProviderFamilyFromInfo] already normalized unchanged.
func supportsSteer(family, transport string, mode runtime.SteerMode) bool {
	switch mode {
	case runtime.SteerProtocol:
		return true
	case runtime.SteerKeystroke:
		return strings.TrimSpace(transport) != "acp" && steerFamilies[sessionlog.ProviderFamily(family)]
	default:
		return false
	}
}

// SupportsSteerForInfo reports whether the session that info describes, as
// projected from its row, takes a steer from a runtime that steers in mode.
// It reads only the row's provider family ([ProviderFamilyFromInfo]) and
// transport, so every reader of the capability agrees: session responses
// through [SubmissionCapabilitiesForRuntime], and the conversation lane while
// it runs the conversation live. A lane that does not run it live takes no
// steer ([SubmissionCapabilities]).
func SupportsSteerForInfo(info Info, mode runtime.SteerMode) bool {
	return supportsSteer(ProviderFamilyFromInfo(info, ""), normalizeTransport(info.Provider, info.TransportMetadata), mode)
}

// supportsSteerForMetadata is [SupportsSteerForInfo] for a session row's
// metadata.
func supportsSteerForMetadata(metadata map[string]string, mode runtime.SteerMode) bool {
	return SupportsSteerForInfo(infoFromPersistedBead(beads.Bead{Metadata: metadata}), mode)
}
