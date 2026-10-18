package session

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// steerFamilies are the agent provider families whose CLI takes text typed
// while a turn runs into that turn, so a keystroke runtime can steer them
// (DES-O1 §10.4). Every other family gets a steer at its next boundary.
var steerFamilies = map[string]bool{
	"claude": true,
	"codex":  true,
}

// SupportsSteer reports whether a session of agent provider family (as
// [ProviderFamilyFromMetadata] resolves it) on transport takes a steer from
// a runtime that steers in mode ([runtime.SteerModeFor]): a protocol steer
// for every family, a keystroke steer for a family in the steer table and
// never over ACP, whose prompts are whole turns.
func SupportsSteer(family, transport string, mode runtime.SteerMode) bool {
	switch mode {
	case runtime.SteerProtocol:
		return true
	case runtime.SteerKeystroke:
		return strings.TrimSpace(transport) != "acp" && steerFamilies[sessionlog.ProviderFamily(family)]
	default:
		return false
	}
}

// supportsSteerForMetadata is [SupportsSteer] for a session row's metadata.
func supportsSteerForMetadata(metadata map[string]string, mode runtime.SteerMode) bool {
	b := beads.Bead{Metadata: metadata}
	return SupportsSteer(providerKind(b), transportFromMetadata(b), mode)
}
