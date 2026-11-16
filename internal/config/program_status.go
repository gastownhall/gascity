package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// Program-status protocol values for [providers.<p>.program_status] protocol.
// The empty string inherits the base layer's value.
const (
	// ProgramStatusProtocolOSC7501 declares that the provider's program reports
	// its own state with the OSC 7501 Program Status Protocol.
	ProgramStatusProtocolOSC7501 = "osc7501"
	// ProgramStatusProtocolNone explicitly undeclares an inherited declaration.
	ProgramStatusProtocolNone = "none"
)

// ProgramStatusBinding is a provider's program-status declaration: whether its
// program reports state over OSC 7501 and how gc captures those reports. It is
// plain data; ProgramStatusBinding.Arm turns it into a session's capture arm
// under the city switch.
type ProgramStatusBinding struct {
	// Protocol declares the status protocol: "osc7501" declares, "none"
	// explicitly undeclares an inherited declaration, and "" inherits.
	Protocol string `toml:"protocol,omitempty" jsonschema:"enum=osc7501,enum=none"`
	// App is the program name the capture reader accepts reports from; reports
	// naming another program are dropped. Empty accepts any program name.
	App string `toml:"app,omitempty"`
	// Relay wraps the agent in the probe-answering relay, for programs that
	// only report after their support probe is answered. Unset inherits.
	Relay *bool `toml:"relay,omitempty"`
	// HoldMidTurn declares that the program aborts its running turn when
	// input is typed into it mid-turn. It governs the conversation lane's
	// keystroke boundary and the bypass of the quiescence gate that a
	// delivery starved on a never-quiet session takes. Under the [session]
	// program_status switch on, the lane's quiescence boundary stays closed,
	// and a delivery at it is held right before it is typed, while a fresh
	// read of the program's own record shows a live working record; the lane
	// takes the starved bypass only on such a read that decides and shows no
	// live working record. Under off and shadow the lane never takes the
	// starved bypass. Legacy delivery defers the bypass under on while such a
	// read shows a live working record, and otherwise takes it as without
	// the declaration. Every other delivery is unchanged. It reaches a pane
	// when its capture is armed (create, respawn, heal), and heal keeps it
	// current. Unset inherits.
	HoldMidTurn *bool `toml:"hold_mid_turn,omitempty"`
	// DecideStartup opts the startup ladder into deciding with the program's
	// own record under the [session] program_status switch on: a turn start
	// past the record read before the first send confirms the startup nudge,
	// and a live blocked record holds the send (nothing is typed into the
	// dialog) and, once the ladder runs out, hands the nudge back to be
	// queued for the session. Without it the ladder decides from the screen
	// alone, exactly as under off. Declare it only for a program whose
	// records were shown to agree with the ladder's outcomes. Unset inherits.
	DecideStartup *bool `toml:"decide_startup,omitempty"`
	// DecideSettle opts the interrupt settle into deciding with the program's
	// own record under the [session] program_status switch on: the idle wait
	// that follows an interrupt gc sent returns at the first root boundary
	// past the pre-interrupt record, and a dialog the program reports since
	// ends it as a pending interaction, so no further keys are sent. Without
	// it the wait decides from the screen alone, exactly as under off. It
	// reaches a pane when its capture is armed (create, respawn, heal), and
	// heal keeps it current. Declare it only for a program whose records were
	// shown to agree with the settle's outcomes. Unset inherits.
	DecideSettle *bool `toml:"decide_settle,omitempty"`
	// Env is forced into the session environment when capture is armed (for
	// example PI_PROGRAM_STATUS = "1"). It is not part of the launch
	// fingerprint, so changing it does not relaunch running agents: keys gc
	// sets itself (GC_*, BEADS_*, BD_*, GT_ROOT) are rejected. Keys merge per
	// layer; an empty value withholds the variable.
	Env map[string]string `toml:"env,omitempty"`
}

// Declared reports whether the binding declares the OSC 7501 protocol.
func (b ProgramStatusBinding) Declared() bool {
	return b.Protocol == ProgramStatusProtocolOSC7501
}

// Declaration is the session's capture declaration, independent of the city
// switch: empty when the transport is ACP or the binding does not declare the
// protocol. It is what a start carries to the leaf, whose live switch decides
// whether the declaration arms the pane, so a start resolved before the switch
// moved still follows it in either direction.
func (b ProgramStatusBinding) Declaration(acp bool) runtime.ProgramStatusArm {
	if acp || !b.Declared() {
		return runtime.ProgramStatusArm{}
	}
	return runtime.ProgramStatusArm{
		Capture:       true,
		App:           b.App,
		Relay:         b.Relay != nil && *b.Relay,
		HoldMidTurn:   b.HoldMidTurn != nil && *b.HoldMidTurn,
		DecideStartup: b.DecideStartup != nil && *b.DecideStartup,
		DecideSettle:  b.DecideSettle != nil && *b.DecideSettle,
	}
}

// Arm derives a session's capture arm and the env to merge into its session
// environment under switch mode. Both are empty unless the switch is shadow or
// on, the transport is not ACP, and the binding declares the protocol. The
// returned env is a copy the caller may mutate.
func (b ProgramStatusBinding) Arm(mode runtime.ProgramStatusMode, acp bool) (runtime.ProgramStatusArm, map[string]string) {
	arm := b.Declaration(acp)
	if !arm.Capture || (mode != runtime.ProgramStatusShadow && mode != runtime.ProgramStatusOn) {
		return runtime.ProgramStatusArm{}, nil
	}
	return arm, maps.Clone(b.Env)
}

// Launch derives what a start carries: the switch-independent capture
// declaration (Declaration), which the leaf arms only under its live switch,
// and the forced env, which follows switch mode at resolution time. The env is
// never part of the launch fingerprint, and neither is the declaration. The
// returned env is a copy the caller may mutate.
func (b ProgramStatusBinding) Launch(mode runtime.ProgramStatusMode, acp bool) (runtime.ProgramStatusArm, map[string]string) {
	_, env := b.Arm(mode, acp)
	return b.Declaration(acp), env
}

// clone returns a deep copy of the binding.
func (b ProgramStatusBinding) clone() ProgramStatusBinding {
	out := b
	out.Relay = cloneBoolPtr(b.Relay)
	out.HoldMidTurn = cloneBoolPtr(b.HoldMidTurn)
	out.DecideStartup = cloneBoolPtr(b.DecideStartup)
	out.DecideSettle = cloneBoolPtr(b.DecideSettle)
	out.Env = maps.Clone(b.Env)
	return out
}

// mergeProgramStatusBinding layers over on base: a non-empty protocol or app
// overrides, a non-nil relay, hold_mid_turn, decide_startup or decide_settle
// overrides, and env merges per key with over winning (an empty value is copied verbatim, which withholds the variable).
func mergeProgramStatusBinding(base, over ProgramStatusBinding) ProgramStatusBinding {
	out := base.clone()
	if over.Protocol != "" {
		out.Protocol = over.Protocol
	}
	if over.App != "" {
		out.App = over.App
	}
	if over.Relay != nil {
		out.Relay = cloneBoolPtr(over.Relay)
	}
	if over.HoldMidTurn != nil {
		out.HoldMidTurn = cloneBoolPtr(over.HoldMidTurn)
	}
	if over.DecideStartup != nil {
		out.DecideStartup = cloneBoolPtr(over.DecideStartup)
	}
	if over.DecideSettle != nil {
		out.DecideSettle = cloneBoolPtr(over.DecideSettle)
	}
	if over.Env != nil {
		merged := make(map[string]string, len(out.Env)+len(over.Env))
		maps.Copy(merged, out.Env)
		maps.Copy(merged, over.Env)
		out.Env = merged
	}
	return out
}

// ProgramStatusMode returns the [session] program_status switch, reading the
// unset value as off. An out-of-enum value is rejected at load, so any other
// value reaching here also reads as off.
func (s SessionConfig) ProgramStatusMode() runtime.ProgramStatusMode {
	switch mode := runtime.ProgramStatusMode(s.ProgramStatus); mode {
	case runtime.ProgramStatusShadow, runtime.ProgramStatusOn:
		return mode
	default:
		return runtime.ProgramStatusOff
	}
}

// ProgramStatusArmFor is the capture arm a session wants under the city
// switch and its transport: what heal converges a running pane to and what a
// decision under the switch reads. A nil city or provider yields a disarmed
// result. Starts use ProgramStatusLaunchFor.
func ProgramStatusArmFor(cfg *City, resolved *ResolvedProvider, transport string) (runtime.ProgramStatusArm, map[string]string) {
	if cfg == nil || resolved == nil {
		return runtime.ProgramStatusArm{}, nil
	}
	return resolved.ProgramStatus.Arm(cfg.Session.ProgramStatusMode(), transport == SessionTransportACP)
}

// ProgramStatusLaunchFor is the one derivation every runtime.Config
// construction site uses for a start: the resolved provider's
// switch-independent capture declaration, which the leaf arms only while its
// live switch is shadow or on, and the forced env under the city switch
// (ProgramStatusBinding.Launch). A nil city or provider yields neither.
func ProgramStatusLaunchFor(cfg *City, resolved *ResolvedProvider, transport string) (runtime.ProgramStatusArm, map[string]string) {
	if cfg == nil || resolved == nil {
		return runtime.ProgramStatusArm{}, nil
	}
	return resolved.ProgramStatus.Launch(cfg.Session.ProgramStatusMode(), transport == SessionTransportACP)
}

var (
	programStatusEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// programStatusReservedEnvKeyPattern matches the session env gc writes
	// itself: identity and city anchors (GC_*, GT_ROOT) and the beads scope
	// and bd pins (BEADS_*, BD_*). Every key the launch fingerprint hashes is
	// among them, so a binding env that avoids them can neither override a
	// session's identity nor move its fingerprint.
	programStatusReservedEnvKeyPattern = regexp.MustCompile(`^(?:(?:GC|BEADS|BD)_|GT_ROOT$)`)
	programStatusModes                 = []string{"", string(runtime.ProgramStatusOff), string(runtime.ProgramStatusShadow), string(runtime.ProgramStatusOn)}
	programStatusProtocols             = []string{"", ProgramStatusProtocolOSC7501, ProgramStatusProtocolNone}
)

// validateProgramStatus rejects an out-of-enum [session] program_status switch
// and malformed [providers.<p>.program_status] bindings. Parse runs it on its
// single layer and LoadWithIncludesOptions runs it again on the composed root,
// because a fragment may supply any of these fields. A typo must never read as
// off: an operator who wrote "shadwo" would believe the shadow is running.
func validateProgramStatus(cfg *City) error {
	if !slices.Contains(programStatusModes, cfg.Session.ProgramStatus) {
		return fmt.Errorf("session.program_status: %q is not one of off, shadow, on", cfg.Session.ProgramStatus)
	}
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validateProgramStatusBinding(cfg.Providers[name].ProgramStatus); err != nil {
			return fmt.Errorf("providers.%s.program_status: %w", name, err)
		}
	}
	return nil
}

func validateProgramStatusBinding(b ProgramStatusBinding) error {
	if !slices.Contains(programStatusProtocols, b.Protocol) {
		return fmt.Errorf("protocol %q is not one of %s, %s", b.Protocol, ProgramStatusProtocolOSC7501, ProgramStatusProtocolNone)
	}
	// The codec's app name is the one rule: an app the leaf could not arm
	// must not load and then run unarmed.
	if b.App != "" && !programstatus.ValidApp(b.App) {
		return fmt.Errorf("app %q must be at most %d letters, digits, '.', '_', '+' or '-'", b.App, programstatus.MaxApp)
	}
	keys := make([]string, 0, len(b.Env))
	for k := range b.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !programStatusEnvKeyPattern.MatchString(k) {
			return fmt.Errorf("env key %q is not a valid environment variable name", k)
		}
		if programStatusReservedEnvKeyPattern.MatchString(k) {
			return fmt.Errorf("env key %q is reserved: gc sets GC_*, BEADS_*, BD_* and GT_ROOT itself", k)
		}
	}
	return nil
}
