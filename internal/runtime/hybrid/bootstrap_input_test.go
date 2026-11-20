package hybrid

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

type bootstrapLeaf struct {
	*runtime.Fake
	calls []string
}

func (p *bootstrapLeaf) ObserveBootstrapInput(_ context.Context, name string, req runtime.BootstrapInputRequest) (*runtime.BootstrapInputProof, error) {
	p.calls = append(p.calls, name)
	return &runtime.BootstrapInputProof{Fence: req.Fence, Token: name}, nil
}

func TestBootstrapInputOnlyConsultsRoutedBackend(t *testing.T) {
	local := &bootstrapLeaf{Fake: runtime.NewFake()}
	remote := &bootstrapLeaf{Fake: runtime.NewFake()}
	p := New(local, remote, func(name string) bool { return name == "remote" })
	req := runtime.BootstrapInputRequest{Family: "codex", Fence: runtime.InputFence{Generation: 1, Epoch: 1, Token: "instance"}}
	got, err := p.ObserveBootstrapInput(t.Context(), "remote", req)
	if err != nil || got == nil || got.Token != "remote" || len(local.calls) != 0 || len(remote.calls) != 1 {
		t.Fatalf("wrong routed bootstrap observation: %+v %v", got, err)
	}
	unsupported := runtime.NewFake()
	p2 := New(local, unsupported, func(name string) bool { return name == "remote" })
	got, err = p2.ObserveBootstrapInput(t.Context(), "remote", req)
	if err != nil || got != nil || len(local.calls) != 0 {
		t.Fatal("unsupported route fell through to other backend")
	}
}
