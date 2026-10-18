package runtime

// SteerMode describes how a runtime delivers input into a session's running
// agent turn: a steer (DES-O1 §10.4). Whether the agent accepts such input
// is the agent provider family's trait, which internal/session combines with
// this one.
type SteerMode string

const (
	// SteerNone means the runtime cannot deliver input into a running turn:
	// input waits for the turn's boundary. It is the mode assumed for a
	// runtime that does not implement [SteerCapabilityProvider].
	SteerNone SteerMode = "none"
	// SteerKeystroke means the runtime types into the agent's terminal
	// without waiting for idle ([ImmediateNudgeProvider], or a fenced nudge
	// with Immediate set); the agent CLI decides whether the running turn
	// takes the text.
	SteerKeystroke SteerMode = "keystroke"
	// SteerProtocol means the runtime steers the running turn through a
	// protocol, for every agent provider it hosts. Its immediate nudge
	// ([ImmediateNudgeProvider], or a fenced nudge with Immediate set) is
	// that steer: a caller steers every mode through the immediate nudge, so
	// a runtime that reports this mode must route its immediate nudge into
	// the running turn. No runtime deduplicates an immediate nudge yet, so a
	// steer is not deduplicated even where a runtime's keyed nudge is; the
	// conversation lane steers no runtime that deduplicates.
	SteerProtocol SteerMode = "protocol"
)

// SteerCapabilityProvider is an optional extension for runtimes that report
// how they deliver input into the running turn of the routed backend of a
// specific session. Without it a runtime is taken to be [SteerNone].
type SteerCapabilityProvider interface {
	SteerMode(name string) SteerMode
}

// SteerModeFor reports how p delivers input into session name's running
// turn: the mode p reports, or [SteerNone] when p is nil, does not implement
// [SteerCapabilityProvider], or reports a mode this build does not know.
func SteerModeFor(p Provider, name string) SteerMode {
	sp, ok := p.(SteerCapabilityProvider)
	if !ok {
		return SteerNone
	}
	switch mode := sp.SteerMode(name); mode {
	case SteerKeystroke, SteerProtocol:
		return mode
	default:
		return SteerNone
	}
}
