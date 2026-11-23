package runtime

// ProbeResult represents a bounded probe outcome for liveness checks.
// Distinguishes confirmed-alive, confirmed-dead, and unknown (timeout/error).
type ProbeResult int

const (
	// ProbeAlive means the process is confirmed alive.
	ProbeAlive ProbeResult = iota
	// ProbeDead means the process is confirmed dead or absent.
	ProbeDead
	// ProbeUnknown means liveness could not be determined (timeout or error).
	ProbeUnknown
)

// HostCapabilities describes what a runtime guarantees about its sessions'
// relationship to the controller's host. The zero value guarantees nothing,
// which is the safe reading for a runtime that does not say.
//
// Unlike [ProviderCapabilities], which a constructed provider reports, these
// are declared where the runtime's selection name is registered
// (internal/runtime/registry), so config-time decisions — skill and MCP
// materialization — can read them without constructing a provider.
type HostCapabilities struct {
	// SharesHostFilesystem is true when session processes run on the
	// controller's host as the controller's user and read its filesystem at
	// the same paths, so files the controller writes under an agent's scope
	// root are what the agent reads.
	SharesHostFilesystem bool
	// RunsHostPreStart is true when the runtime executes the start config's
	// PreStart commands on the controller host before launching the agent,
	// failing the start when one fails.
	RunsHostPreStart bool
}

// ProviderCapabilities describes what a runtime provider can report.
// Not all providers support all wake-reason inputs.
type ProviderCapabilities struct {
	// CanReportAttachment is true if IsAttached returns meaningful results.
	CanReportAttachment bool
	// CanReportActivity is true if GetLastActivity returns meaningful results.
	CanReportActivity bool
	// CanStream is true if the provider exposes the persistent `stream`
	// connection op (declared via the proc.stream protocol capability).
	CanStream bool
	// CanAttachTTY is true if the provider exposes an interactive PTY `attach`
	// connection op (declared via the tty.attach protocol capability).
	CanAttachTTY bool
	// NeedsClaimBackstop is true when the provider's startup claim nudge is
	// delivered as a runtime paste that can be lost with no relaunch/respawn
	// path to re-deliver it (herdr: Relaunch is deliberately unimplemented).
	// The reconciler keeps the stalled-claim nudge backstop
	// (nudgeStalledPoolClaims) active for such providers even when they
	// report activity — reporting activity restores idle visibility, not
	// startup-prompt redelivery.
	NeedsClaimBackstop bool
}

// SessionSleepCapability describes how safely a runtime can participate in
// automatic idle sleep.
type SessionSleepCapability string

const (
	// SessionSleepCapabilityDisabled means idle sleep should be treated as off.
	SessionSleepCapabilityDisabled SessionSleepCapability = "disabled"
	// SessionSleepCapabilityTimedOnly means the runtime can participate in
	// timer-based sleep for headless sessions but cannot guarantee interactive
	// prompt-boundary safety.
	SessionSleepCapabilityTimedOnly SessionSleepCapability = "timed_only"
	// SessionSleepCapabilityFull means the runtime supports safe interactive
	// idle sleep, including attachment-aware grace windows.
	SessionSleepCapabilityFull SessionSleepCapability = "full"
)

// SleepCapabilityProvider is an optional extension for providers that can
// report idle sleep capability for the routed backend of a specific session.
type SleepCapabilityProvider interface {
	SleepCapability(name string) SessionSleepCapability
}
