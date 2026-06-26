package runtimetest

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

type startFailProvider struct {
	runtime.Provider
	err error
}

func (p startFailProvider) Start(_ context.Context, _ string, _ runtime.Config) error {
	return p.err
}

func TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors(t *testing.T) {
	startErr := errors.New("environmental start failure")
	provider := startFailProvider{Provider: runtime.NewFake(), err: startErr}
	var counter int64

	RunProviderTestsWithOptions(t, func(_ *testing.T) (runtime.Provider, runtime.Config, string) {
		id := atomic.AddInt64(&counter, 1)
		return provider, runtime.Config{}, fmt.Sprintf("skip-start-%d", id)
	}, Options{
		SkipStartError: func(err error) (string, bool) {
			if errors.Is(err, startErr) {
				return "provider environment unavailable", true
			}
			return "", false
		},
	})
}
