package runtime

import (
	"errors"
	"fmt"
	"testing"
)

// A capacity refusal is still a startup death: consumers that already branch
// on ErrSessionDiedDuringStartup (the lifecycle's post-start death check, the
// stale-key recovery) must keep seeing it, and the message must stay exactly
// the provider's so logs and pane excerpts are unchanged.
func TestCapacityError_MatchesSentinelAndCausePreservesMessage(t *testing.T) {
	cause := fmt.Errorf("%w: session %q; last pane output:\ntunnel down", ErrSessionDiedDuringStartup, "worker-1")
	capErr := &CapacityError{ExitCode: ExitCodeTempFail, Source: CapacitySourceExitStatus, Err: cause}

	if got, want := capErr.Error(), cause.Error(); got != want {
		t.Fatalf("Error() = %q, want the cause's message %q", got, want)
	}
	wrapped := fmt.Errorf("resuming session: %w", fmt.Errorf("starting %q: %w", "worker-1", capErr))
	for _, target := range []error{ErrProviderCapacity, ErrSessionDiedDuringStartup} {
		if !errors.Is(wrapped, target) {
			t.Errorf("errors.Is(wrapped, %v) = false, want true", target)
		}
	}
	if !IsProviderCapacity(wrapped) {
		t.Error("IsProviderCapacity(wrapped) = false, want true")
	}
	var got *CapacityError
	if !errors.As(wrapped, &got) || got.ExitCode != ExitCodeTempFail {
		t.Fatalf("errors.As(*CapacityError) = %v (%+v), want ExitCode %d", got != nil, got, ExitCodeTempFail)
	}
	if IsProviderCapacity(cause) {
		t.Error("IsProviderCapacity(plain startup death) = true, want false")
	}
}
