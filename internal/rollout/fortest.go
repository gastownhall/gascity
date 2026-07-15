package rollout

// ForTestOption sets one gate on a Flags value under construction. There is
// exactly one With* constructor per registered gate, declared in that gate's
// file, so deleting a gate breaks its callers at COMPILE time.
type ForTestOption func(*flagsBuilder)

// flagsBuilder is the mutable, call-local Flags under construction — never
// package state.
type flagsBuilder struct {
	flags Flags
}

// defaultFlags is the single source of built-in defaults, shared by Resolve and
// ForTest. registry_test pins these values equal to the Spec.Default entries and
// to the config-accessor defaults, so the three homes cannot drift.
func defaultFlags() Flags {
	return Flags{
		beadsConditionalWrites: resolved[Mode]{value: Off, origin: OriginBuiltin},
		formulaV2:              resolved[bool]{value: true, origin: OriginBuiltin},
	}
}

// ForTest builds an immutable Flags from the built-in defaults plus typed
// overrides. It reads neither config nor env, holds no process-scoped state, and
// is safe under t.Parallel by construction (each call returns its own value).
func ForTest(opts ...ForTestOption) Flags {
	b := &flagsBuilder{flags: defaultFlags()}
	for _, o := range opts {
		o(b)
	}
	return b.flags
}
