package runtime

import (
	"context"
	"errors"
	"testing"
)

// Kills a response-fencing read that trusts a provider without the
// interface or an unknown value, and a fake that answers a misfenced
// response.
func TestResponseFencing(t *testing.T) {
	f := NewFake()
	if got := ResponseFencingFor(f, "s"); got != InputFencingAtomic {
		t.Fatalf("ResponseFencingFor(fake) = %q, want atomic", got)
	}
	f.InputFencingValue = "from_the_future"
	if got := ResponseFencingFor(f, "s"); got != InputFencingNone {
		t.Fatalf("ResponseFencingFor(unknown value) = %q, want none", got)
	}
	f.InputFencingValue = ""
	if got := ResponseFencingFor(Provider(nil), "s"); got != InputFencingNone {
		t.Fatalf("ResponseFencingFor(nil) = %q, want none", got)
	}
	plain := struct{ Provider }{f}
	if got := ResponseFencingFor(plain, "s"); got != InputFencingNone {
		t.Fatalf("ResponseFencingFor(provider without the interface) = %q, want none", got)
	}
	ctx := context.Background()
	fence := InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	answer := InteractionResponse{RequestID: "r", Action: "approve"}
	if err := RespondFencedVia(ctx, plain, "s", fence, answer); !errors.Is(err, ErrInputFenceUnsupported) {
		t.Fatalf("RespondFencedVia(provider without the interface) = %v, want ErrInputFenceUnsupported", err)
	}
	if err := f.Start(ctx, "s", Config{Env: map[string]string{InstanceTokenEnv: "tok"}}); err != nil {
		t.Fatal(err)
	}
	f.SetPendingInteraction("s", &PendingInteraction{RequestID: "r"})
	other := fence
	other.Token = "other"
	if err := RespondFencedVia(ctx, f, "s", other, answer); !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("RespondFenced(other incarnation) = %v, want a fence miss", err)
	}
	if err := RespondFencedVia(ctx, f, "s", fence, answer); err != nil {
		t.Fatalf("RespondFenced(own incarnation) = %v", err)
	}
	if got := f.Responses["s"]; len(got) != 1 {
		t.Fatalf("answers %+v, want one", got)
	}
	if err := f.Respond("s", answer); !errors.Is(err, ErrInteractionGone) {
		t.Fatalf("Respond with nothing pending = %v, want ErrInteractionGone", err)
	}
}
