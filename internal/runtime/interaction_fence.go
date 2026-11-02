package runtime

import (
	"context"
	"errors"
	"fmt"
)

// ErrInteractionGone reports a response naming an interaction the session
// no longer shows: none is pending, or another one is. Nothing was sent.
var ErrInteractionGone = errors.New("the interaction the response names is no longer pending")

// FencedInteractionProvider is implemented by runtimes that can answer a
// pending interaction only in the incarnation an input fence names (DES-O1
// §6.2): the answer's keys are conditional on the fence, as
// FencedInputProvider's input is. A fenced answer never reaches another
// incarnation: a miss is a *FenceMismatchError (Partial when a key of the
// answer was sent before it), and a runtime that cannot fence session name
// refuses with ErrInputFenceUnsupported before sending anything. An answer
// that fails for another reason before its key was sent may wrap
// ErrInputNotSent.
type FencedInteractionProvider interface {
	// ResponseFencing reports how the runtime fences answers to session
	// name. It is a static trait: it reads no runtime state.
	ResponseFencing(name string) InputFencing
	// RespondFenced answers as InteractionProvider.Respond does, only in
	// the incarnation fence names.
	RespondFenced(ctx context.Context, name string, fence InputFence, response InteractionResponse) error
}

// ResponseFencingFor reports how p fences answers to session name: none
// when p does not implement FencedInteractionProvider or reports a value
// this build does not know.
func ResponseFencingFor(p Provider, name string) InputFencing {
	fp, ok := p.(FencedInteractionProvider)
	if !ok {
		return InputFencingNone
	}
	switch v := fp.ResponseFencing(name); v {
	case InputFencingAtomic, InputFencingChecked:
		return v
	default:
		return InputFencingNone
	}
}

// RespondFencedVia answers through backend, the provider a composite routes
// session name to, under fence: a backend that cannot fence answers to name
// is ErrInputFenceUnsupported, so a composite never answers unfenced or
// through another backend.
func RespondFencedVia(ctx context.Context, backend Provider, name string, fence InputFence, response InteractionResponse) error {
	fp, ok := backend.(FencedInteractionProvider)
	if !ok || ResponseFencingFor(backend, name) == InputFencingNone {
		return fmt.Errorf("session %q: %w", name, ErrInputFenceUnsupported)
	}
	return fp.RespondFenced(ctx, name, fence, response)
}
