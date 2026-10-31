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

// Arm derives a session's capture arm and the env to merge into its session
// environment. Both are empty unless the switch is shadow or on, the transport
// is not ACP, and the binding declares the protocol. The returned env is a copy
// the caller may mutate.
func (b ProgramStatusBinding) Arm(mode runtime.ProgramStatusMode, acp bool) (runtime.ProgramStatusArm, map[string]string) {
	if acp || !b.Declared() {
		return runtime.ProgramStatusArm{}, nil
	}
	if mode != runtime.ProgramStatusShadow && mode != runtime.ProgramStatusOn {
		return runtime.ProgramStatusArm{}, nil
	}
	arm := runtime.ProgramStatusArm{
		Capture: true,
		App:     b.App,
		Relay:   b.Relay != nil && *b.Relay,
	}
	return arm, maps.Clone(b.Env)
}

// clone returns a deep copy of the binding.
func (b ProgramStatusBinding) clone() ProgramStatusBinding {
	out := b
	out.Relay = cloneBoolPtr(b.Relay)
	out.Env = maps.Clone(b.Env)
	return out
}

// mergeProgramStatusBinding layers over on base: a non-empty protocol or app
// overrides, a non-nil relay overrides, and env merges per key with over
// winning (an empty value is copied verbatim, which withholds the variable).
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

// ProgramStatusArmFor is the one derivation every runtime.Config construction
// site uses: the resolved provider's binding under the city switch and the
// session transport. A nil city or provider yields a disarmed result.
func ProgramStatusArmFor(cfg *City, resolved *ResolvedProvider, transport string) (runtime.ProgramStatusArm, map[string]string) {
	if cfg == nil || resolved == nil {
		return runtime.ProgramStatusArm{}, nil
	}
	return resolved.ProgramStatus.Arm(cfg.Session.ProgramStatusMode(), transport == SessionTransportACP)
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
	if b.App != "" && !programstatus.ValidApp(b.App) {
		return fmt.Errorf("app %q must be 1 to %d letters, digits, '.', '_', '+' or '-'", b.App, programstatus.MaxApp)
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
