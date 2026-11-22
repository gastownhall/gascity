package k8s

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/guard"
)

// PodSessionEnv is the reserved environment marker that selects the
// client-free pod-local session provider for worker-side gc commands. The
// k8s provider's buildPod is the only writer: it stamps the logical session
// name of the session the pod hosts, so the marker carries both the
// selection and the pod's own session identity in one value. A process
// carrying no value (absent, empty or whitespace) uses ordinary provider
// selection.
//
// The marker is a routing contract, not an authorization boundary: it keeps
// supported configuration and environment projection inputs from spoofing
// worker-pod identity, and says nothing about a process deliberately
// altering its own environment.
const PodSessionEnv = "GC_K8S_POD_SESSION"

// MarkedPodSession reads the pod-local marker from the process environment
// and reports the pod's own logical session name. ok is false when the
// process carries no marker: then ordinary provider selection applies.
func MarkedPodSession() (name string, ok bool) {
	name = strings.TrimSpace(os.Getenv(PodSessionEnv))
	return name, name != ""
}

// NewPodLocal constructs the client-free session provider a marked worker
// process resolves through the runtime registry. It builds no Kubernetes
// client, reads no kubeconfig and parses no pod-creation settings, so a
// worker pod with no API credentials can still construct its session
// provider; construction never contacts the API.
//
// The provider serves only the pod's own tmux session (the marker's session
// name and "main") for metadata and liveness reads; every other operation is
// a typed controller-owned refusal because the controller owns the
// lifecycle.
func NewPodLocal() (runtime.Provider, error) {
	self, ok := MarkedPodSession()
	if !ok {
		return nil, fmt.Errorf("constructing pod-local session provider: %s is not set to a session name", PodSessionEnv)
	}
	return &podLocalProvider{self: self}, nil
}

// podLocalProvider is the in-pod provider behind NewPodLocal: the same
// default-server tmux session the controller observes through pod exec,
// reached without a clientset or kubeconfig. Only own-session metadata and
// liveness are served; lifecycle operations and other sessions are refused
// because the controller owns them (guard.ErrControllerOwnedRuntime).
type podLocalProvider struct {
	self string
}

var _ runtime.Provider = (*podLocalProvider)(nil)

// refusal is the typed refusal for every operation the controller owns.
func podLocalRefusal(op, name string) error {
	return fmt.Errorf("pod-local runtime %s %q: %w", op, name, guard.ErrControllerOwnedRuntime)
}

// Start refuses with ErrControllerOwnedRuntime for every name.
func (*podLocalProvider) Start(_ context.Context, name string, _ runtime.Config) error {
	return podLocalRefusal("start", name)
}

// Stop refuses with ErrControllerOwnedRuntime for every name: the
// controller owns the session lifecycle.
func (p *podLocalProvider) Stop(name string) error {
	return podLocalRefusal("stop", name)
}

// Interrupt refuses with ErrControllerOwnedRuntime.
func (*podLocalProvider) Interrupt(name string) error {
	return podLocalRefusal("interrupt", name)
}

// IsRunning reports whether the pod's own tmux session exists; false for any
// other name.
func (*podLocalProvider) IsRunning(string) bool {
	return false
}

// IsAttached reports false: the worker's own terminal is not the runtime.
func (*podLocalProvider) IsAttached(string) bool { return false }

// Attach refuses with ErrControllerOwnedRuntime.
func (*podLocalProvider) Attach(name string) error {
	return podLocalRefusal("attach", name)
}

// ProcessAlive reports false for the restricted observation contract.
func (*podLocalProvider) ProcessAlive(string, []string) bool { return false }

// Nudge refuses with ErrControllerOwnedRuntime.
func (*podLocalProvider) Nudge(name string, _ []runtime.ContentBlock) error {
	return podLocalRefusal("nudge", name)
}

// SetMeta refuses with ErrControllerOwnedRuntime: the controller owns the
// session metadata writes.
func (*podLocalProvider) SetMeta(name, _, _ string) error {
	return podLocalRefusal("set-meta", name)
}

// GetMeta refuses with ErrControllerOwnedRuntime: a worker site observes no
// session metadata.
func (*podLocalProvider) GetMeta(name, _ string) (string, error) {
	return "", podLocalRefusal("get-meta", name)
}

// RemoveMeta refuses with ErrControllerOwnedRuntime: the controller owns the
// session metadata writes.
func (*podLocalProvider) RemoveMeta(name, _ string) error {
	return podLocalRefusal("remove-meta", name)
}

// Peek refuses with ErrControllerOwnedRuntime.
func (*podLocalProvider) Peek(name string, _ int) (string, error) {
	return "", podLocalRefusal("peek", name)
}

// ListRunning refuses with ErrControllerOwnedRuntime: the worker observes no
// sessions beyond its own.
func (*podLocalProvider) ListRunning(prefix string) ([]string, error) {
	return nil, podLocalRefusal("list-running", prefix)
}

// GetLastActivity refuses with ErrControllerOwnedRuntime.
func (*podLocalProvider) GetLastActivity(name string) (time.Time, error) {
	return time.Time{}, podLocalRefusal("get-last-activity", name)
}

// ClearScrollback refuses with ErrControllerOwnedRuntime.
func (*podLocalProvider) ClearScrollback(name string) error {
	return podLocalRefusal("clear-scrollback", name)
}

// CopyTo refuses with ErrControllerOwnedRuntime.
func (*podLocalProvider) CopyTo(name, _, _ string) error {
	return podLocalRefusal("copy-to", name)
}

// SendKeys refuses with ErrControllerOwnedRuntime.
func (*podLocalProvider) SendKeys(name string, _ ...string) error {
	return podLocalRefusal("send-keys", name)
}

// RunLive refuses with ErrControllerOwnedRuntime.
func (*podLocalProvider) RunLive(name string, _ runtime.Config) error {
	return podLocalRefusal("run-live", name)
}

// Capabilities reports none: the restricted contract guarantees nothing the
// reconciler may rely on.
func (*podLocalProvider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{}
}
