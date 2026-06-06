package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

var (
	statusProviderCallTimeout    = 50 * time.Millisecond
	statusProviderTimeoutWarning = func() {
		fmt.Fprintln(os.Stderr, "gc status: runtime status probe timed out; using partial status")
	}
)

type statusProvider struct {
	base     runtime.Provider
	warnOnce sync.Once
}

func newBoundedStatusProvider(base runtime.Provider) runtime.Provider {
	if sp, ok := base.(*statusProvider); ok {
		return sp
	}
	return &statusProvider{base: base}
}

func boundedStatusCall[T any](p *statusProvider, fallback T, fn func() T) T {
	if statusProviderCallTimeout <= 0 {
		return fn()
	}
	resultCh := make(chan T, 1)
	go func() {
		resultCh <- fn()
	}()
	select {
	case result := <-resultCh:
		return result
	case <-time.After(statusProviderCallTimeout):
		p.warnOnce.Do(statusProviderTimeoutWarning)
		return fallback
	}
}

func (p *statusProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	return p.base.Start(ctx, name, cfg)
}

func (p *statusProvider) Stop(name string) error {
	return p.base.Stop(name)
}

func (p *statusProvider) Interrupt(name string) error {
	return p.base.Interrupt(name)
}

func (p *statusProvider) IsRunning(name string) bool {
	return boundedStatusCall(p, false, func() bool {
		return p.base.IsRunning(name)
	})
}

func (p *statusProvider) IsAttached(name string) bool {
	return boundedStatusCall(p, false, func() bool {
		return p.base.IsAttached(name)
	})
}

func (p *statusProvider) Attach(name string) error {
	return p.base.Attach(name)
}

func (p *statusProvider) ProcessAlive(name string, processNames []string) bool {
	return boundedStatusCall(p, false, func() bool {
		return p.base.ProcessAlive(name, processNames)
	})
}

func (p *statusProvider) ObserveLiveness(name string, processNames []string) runtime.Liveness {
	return boundedStatusCall(p, runtime.Liveness{}, func() runtime.Liveness {
		return runtime.ObserveLiveness(p.base, name, processNames)
	})
}

func (p *statusProvider) Nudge(name string, content []runtime.ContentBlock) error {
	return p.base.Nudge(name, content)
}

func (p *statusProvider) SetMeta(name, key, value string) error {
	return p.base.SetMeta(name, key, value)
}

func (p *statusProvider) GetMeta(name, key string) (string, error) {
	result := boundedStatusCall(p, struct {
		value string
		err   error
	}{}, func() struct {
		value string
		err   error
	} {
		value, err := p.base.GetMeta(name, key)
		return struct {
			value string
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) RemoveMeta(name, key string) error {
	return p.base.RemoveMeta(name, key)
}

func (p *statusProvider) Peek(name string, lines int) (string, error) {
	result := boundedStatusCall(p, struct {
		value string
		err   error
	}{}, func() struct {
		value string
		err   error
	} {
		value, err := p.base.Peek(name, lines)
		return struct {
			value string
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) ListRunning(prefix string) ([]string, error) {
	result := boundedStatusCall(p, struct {
		value []string
		err   error
	}{}, func() struct {
		value []string
		err   error
	} {
		value, err := p.base.ListRunning(prefix)
		return struct {
			value []string
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) RouteACP(name string) {
	if router, ok := p.base.(interface{ RouteACP(string) }); ok {
		router.RouteACP(name)
	}
}

func (p *statusProvider) GetLastActivity(name string) (time.Time, error) {
	result := boundedStatusCall(p, struct {
		value time.Time
		err   error
	}{}, func() struct {
		value time.Time
		err   error
	} {
		value, err := p.base.GetLastActivity(name)
		return struct {
			value time.Time
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) ClearScrollback(name string) error {
	return p.base.ClearScrollback(name)
}

func (p *statusProvider) CopyTo(name, src, relDst string) error {
	return p.base.CopyTo(name, src, relDst)
}

func (p *statusProvider) SendKeys(name string, keys ...string) error {
	return p.base.SendKeys(name, keys...)
}

func (p *statusProvider) RunLive(name string, cfg runtime.Config) error {
	return p.base.RunLive(name, cfg)
}

func (p *statusProvider) Capabilities() runtime.ProviderCapabilities {
	return p.base.Capabilities()
}

func (p *statusProvider) Pending(name string) (*runtime.PendingInteraction, error) {
	ip, ok := p.base.(runtime.InteractionProvider)
	if !ok {
		return nil, nil
	}
	result := boundedStatusCall(p, struct {
		value *runtime.PendingInteraction
		err   error
	}{}, func() struct {
		value *runtime.PendingInteraction
		err   error
	} {
		value, err := ip.Pending(name)
		return struct {
			value *runtime.PendingInteraction
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) Respond(name string, response runtime.InteractionResponse) error {
	ip, ok := p.base.(runtime.InteractionProvider)
	if !ok {
		return runtime.ErrInteractionUnsupported
	}
	return ip.Respond(name, response)
}
