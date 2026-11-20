package runtime

import "context"

// BootstrapInputOrigin is launch provenance, never an observation of an idle
// turn. Only a fresh built-in conversation with no startup input may carry it.
// It is not a configuration/fingerprint or public API option.
type BootstrapInputOrigin string

// BootstrapInputFreshBuiltin permits a runtime to seed one first-input credit.
// Resume, adoption, fork, custom commands and already-prompted starts omit it.
const BootstrapInputFreshBuiltin BootstrapInputOrigin = "fresh_builtin_conversation"

// BootstrapInputRequest identifies the exact fresh native input incarnation.
type BootstrapInputRequest struct {
	Family string
	Fence  InputFence
}

// BootstrapInputProof names a runtime-owned, one-shot first-input credit. It
// grants no idle verdict or authority over a later native turn.
type BootstrapInputProof struct {
	Fence InputFence
	Token string `json:"-"`
}

// BootstrapInputObserver is optional. A nil proof means there is no safe
// bootstrap input opportunity; unsupported and unknown remain fail closed.
type BootstrapInputObserver interface {
	ObserveBootstrapInput(context.Context, string, BootstrapInputRequest) (*BootstrapInputProof, error)
}

// ObserveBootstrapInput asks only the selected runtime for a first-input proof.
func ObserveBootstrapInput(ctx context.Context, p Provider, name string, req BootstrapInputRequest) (*BootstrapInputProof, error) {
	leaf, _, known := ResolveBackend(p, name)
	if !known {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if observer, ok := leaf.(BootstrapInputObserver); ok {
		return observer.ObserveBootstrapInput(ctx, name, req)
	}
	return nil, nil
}

type bootstrapInputContextKey struct{}

// WithBootstrapInput requires the next fenced nudge to consume this exact
// credit atomically before any input. It never enables an unfenced input.
func WithBootstrapInput(ctx context.Context, proof *BootstrapInputProof) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if proof != nil {
		proofCopy := *proof
		proof = &proofCopy
	}
	return context.WithValue(ctx, bootstrapInputContextKey{}, proof)
}

// BootstrapInputFrom returns the first-input requirement carried by ctx.
func BootstrapInputFrom(ctx context.Context) (*BootstrapInputProof, bool) {
	if ctx == nil {
		return nil, false
	}
	proof, _ := ctx.Value(bootstrapInputContextKey{}).(*BootstrapInputProof)
	return proof, proof != nil
}
