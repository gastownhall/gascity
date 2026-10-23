package hybrid

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills a composite that answers unfenced, or through the other backend: a
// session routed to a backend that fences answers gets the fenced answer
// there, and one routed to a backend that cannot is refused with no answer
// on either.
func TestRespondFencedForwardsToTheRoutedBackend(t *testing.T) {
	fencing, unfenced := runtime.NewFake(), runtime.NewFake()
	unfenced.InputFencingValue = runtime.InputFencingNone
	p := New(fencing, unfenced, func(name string) bool { return name == "acp" })
	ctx := t.Context()
	fence := runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	env := map[string]string{"GC_INSTANCE_TOKEN": "tok"}
	for _, name := range []string{"local", "acp"} {
		if err := p.Start(ctx, name, runtime.Config{Env: env}); err != nil {
			t.Fatal(err)
		}
	}
	fencing.SetPendingInteraction("local", &runtime.PendingInteraction{RequestID: "r"})
	unfenced.SetPendingInteraction("acp", &runtime.PendingInteraction{RequestID: "r"})
	if got := runtime.ResponseFencingFor(p, "local"); got != runtime.InputFencingAtomic {
		t.Errorf("ResponseFencingFor(local) = %q, want atomic", got)
	}
	if got := runtime.ResponseFencingFor(p, "acp"); got != runtime.InputFencingNone {
		t.Errorf("ResponseFencingFor(acp) = %q, want none", got)
	}
	answer := runtime.InteractionResponse{RequestID: "r", Action: "approve"}
	if err := p.RespondFenced(ctx, "local", fence, answer); err != nil {
		t.Fatal(err)
	}
	if got := fencing.Responses["local"]; len(got) != 1 {
		t.Fatalf("fencing leaf answers %+v, want one", got)
	}
	if err := p.RespondFenced(ctx, "acp", fence, answer); !errors.Is(err, runtime.ErrInputFenceUnsupported) {
		t.Fatalf("RespondFenced(acp) = %v, want ErrInputFenceUnsupported", err)
	}
	if len(unfenced.Responses["acp"]) != 0 || len(fencing.Responses["acp"]) != 0 {
		t.Fatal("a refused fenced answer reached a backend")
	}
}
