package worker

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// BootstrapInputHandle observes a runtime-owned, one-shot initial input proof.
// An absent proof is unsupported or unproven, never permission to send.
type BootstrapInputHandle interface {
	ObserveBootstrapInput(context.Context, runtime.InputFence) (*runtime.BootstrapInputProof, error)
}

// ObserveBootstrapInput asks only an explicitly supporting handle for an exact
// incarnation proof. Ordinary liveness and missing history do not grant one.
func ObserveBootstrapInput(ctx context.Context, h Handle, fence runtime.InputFence) (*runtime.BootstrapInputProof, error) {
	if observer, ok := h.(BootstrapInputHandle); ok {
		return observer.ObserveBootstrapInput(ctx, fence)
	}
	return nil, nil
}

// ObserveBootstrapInput observes this runtime handle's resolved native family.
func (h *RuntimeHandle) ObserveBootstrapInput(ctx context.Context, fence runtime.InputFence) (*runtime.BootstrapInputProof, error) {
	return runtime.ObserveBootstrapInput(ctx, h.provider, h.sessionName, runtime.BootstrapInputRequest{Family: h.providerName, Fence: fence})
}
