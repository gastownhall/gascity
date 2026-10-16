// Package guard provides the runtime provider a worker site uses in place of
// a real one.
//
// On a worker site the controller owns every session runtime; a `gc` process
// running inside a box must never start, stop, or steer one. The guard makes
// that a typed refusal instead of a silent local effect: every operation that
// would act on, or report from, a runtime returns ErrControllerOwnedRuntime,
// the liveness probes answer false, and it claims no capabilities.
//
// The guard deliberately does not satisfy the shared runtime-provider
// conformance contract: that contract exercises a successful lifecycle, which
// a provider that refuses every operation cannot have.
package guard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ErrControllerOwnedRuntime is wrapped by every error the guard returns: the
// session runtime is owned by the controller, not by this worker site.
var ErrControllerOwnedRuntime = errors.New("session runtime is owned by the controller; a worker site cannot operate it")

// Provider is a runtime.Provider that refuses every runtime operation.
type Provider struct{}

var _ runtime.Provider = (*Provider)(nil)

// New returns a guard provider.
func New() *Provider { return &Provider{} }

func refuse(op, name string) error {
	return fmt.Errorf("runtime %s %q: %w", op, name, ErrControllerOwnedRuntime)
}

// Start refuses with ErrControllerOwnedRuntime.
func (*Provider) Start(_ context.Context, name string, _ runtime.Config) error {
	return refuse("start", name)
}

// Stop refuses with ErrControllerOwnedRuntime.
func (*Provider) Stop(name string) error { return refuse("stop", name) }

// Interrupt refuses with ErrControllerOwnedRuntime.
func (*Provider) Interrupt(name string) error { return refuse("interrupt", name) }

// IsRunning reports false: a worker site observes no runtime.
func (*Provider) IsRunning(string) bool { return false }

// IsAttached reports false: a worker site observes no runtime.
func (*Provider) IsAttached(string) bool { return false }

// Attach refuses with ErrControllerOwnedRuntime.
func (*Provider) Attach(name string) error { return refuse("attach", name) }

// ProcessAlive reports false: a worker site observes no runtime.
func (*Provider) ProcessAlive(string, []string) bool { return false }

// Nudge refuses with ErrControllerOwnedRuntime.
func (*Provider) Nudge(name string, _ []runtime.ContentBlock) error {
	return refuse("nudge", name)
}

// SetMeta refuses with ErrControllerOwnedRuntime.
func (*Provider) SetMeta(name, _, _ string) error { return refuse("set-meta", name) }

// GetMeta refuses with ErrControllerOwnedRuntime.
func (*Provider) GetMeta(name, _ string) (string, error) { return "", refuse("get-meta", name) }

// RemoveMeta refuses with ErrControllerOwnedRuntime.
func (*Provider) RemoveMeta(name, _ string) error { return refuse("remove-meta", name) }

// Peek refuses with ErrControllerOwnedRuntime.
func (*Provider) Peek(name string, _ int) (string, error) { return "", refuse("peek", name) }

// ListRunning refuses with ErrControllerOwnedRuntime.
func (*Provider) ListRunning(prefix string) ([]string, error) {
	return nil, refuse("list-running", prefix)
}

// GetLastActivity refuses with ErrControllerOwnedRuntime.
func (*Provider) GetLastActivity(name string) (time.Time, error) {
	return time.Time{}, refuse("get-last-activity", name)
}

// ClearScrollback refuses with ErrControllerOwnedRuntime.
func (*Provider) ClearScrollback(name string) error { return refuse("clear-scrollback", name) }

// CopyTo refuses with ErrControllerOwnedRuntime.
func (*Provider) CopyTo(name, _, _ string) error { return refuse("copy-to", name) }

// SendKeys refuses with ErrControllerOwnedRuntime.
func (*Provider) SendKeys(name string, _ ...string) error { return refuse("send-keys", name) }

// RunLive refuses with ErrControllerOwnedRuntime.
func (*Provider) RunLive(name string, _ runtime.Config) error { return refuse("run-live", name) }

// Capabilities reports none.
func (*Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{}
}
