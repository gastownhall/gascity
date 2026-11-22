package k8s

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
	return &podLocalProvider{self: self, run: runPodLocalTmux}, nil
}

// podLocalRunner executes one tmux argv and returns its combined output
// with whatever error it failed under, so command diagnostics classify
// without a shell. Production runs exec.Command argument arrays; tests
// inject answers.
type podLocalRunner func(argv []string) (string, error)

// runPodLocalTmux is the production runner: the tmux binary on the default
// pod-local server, addressed through plain argument arrays. No -L or -S
// socket name is supplied — city-derived sockets are the controller's
// naming, and the pod-local server is the one the controller reaches
// through pod exec.
func runPodLocalTmux(argv []string) (string, error) {
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	return string(out), err
}

// podLocalProvider is the in-pod provider behind NewPodLocal: the same
// default-server tmux session the controller observes through pod exec,
// reached without a clientset or kubeconfig. Only own-session metadata and
// liveness are served; lifecycle operations and other sessions are refused
// because the controller owns them (guard.ErrControllerOwnedRuntime).
type podLocalProvider struct {
	self string
	run  podLocalRunner
}

var _ runtime.Provider = (*podLocalProvider)(nil)

// refusal is the typed refusal for every operation the controller owns.
func podLocalRefusal(op, name string) error {
	return fmt.Errorf("pod-local runtime %s %q: %w", op, name, guard.ErrControllerOwnedRuntime)
}

// servesOwnSession reports whether name addresses this pod's one tmux
// session: its logical name (the marker) or the container-local "main".
// Only that session's metadata and liveness are served at all.
func (p *podLocalProvider) servesOwnSession(name string) bool {
	name = strings.TrimSpace(name)
	return name == "main" || name == strings.TrimSpace(p.self)
}

// tmuxTarget is the exact own-session target every served operation uses:
// "=main" pins the name so a same-prefix session can never shadow it.
const podLocalTarget = "=main"

// metaCommand runs argv as a metadata operation on the own session and
// classifies its failure. A session or server that is not there answers
// runtime.ErrSessionNotFound — the answer existing drain-ack and read-back
// handling keys on — and any other failure keeps the command's own error
// and output as context.
func (p *podLocalProvider) metaCommand(op, name string, argv []string) error {
	out, err := p.run(argv)
	if err == nil {
		return nil
	}
	return podLocalClassify(op, name, out, err, false)
}

// podLocalClassify turns one command failure into the caller-visible one.
// missingVariableIsNoop reports whether tmux's missing-variable diagnostic
// is a benign no-op (a removal of a key that already holds nothing); with
// it false the diagnostic is a foreign failure and keeps its context.
func podLocalClassify(op, name, out string, err error, missingVariableIsNoop bool) error {
	switch {
	case missingVariableIsNoop && podLocalSays(out, "no such variable"):
		return nil
	case podLocalSessionGone(out):
		return fmt.Errorf("pod-local runtime %s %q: %w: %s", op, name, runtime.ErrSessionNotFound, podLocalFirstLine(out))
	default:
		return fmt.Errorf("pod-local runtime %s %q: %w: %s", op, name, err, podLocalFirstLine(out))
	}
}

// podLocalSays reports whether the command output carries needle.
func podLocalSays(out, needle string) bool {
	return strings.Contains(out, needle)
}

// podLocalSessionGone reports whether the command output is tmux saying the
// session or its server is missing — the definitive absence answers, not an
// error that merely mentions it.
func podLocalSessionGone(out string) bool {
	return podLocalSays(out, "no such session") || podLocalSays(out, "no server running") || podLocalSays(out, "error connecting to")
}

// podLocalFirstLine truncates a command's output to its first non-empty
// line, so an error's context stays one readable clause.
func podLocalFirstLine(out string) string {
	out = strings.TrimSpace(out)
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	if len(out) > 160 {
		out = out[:160]
	}
	return out
}

// Start refuses with ErrControllerOwnedRuntime for every name.
func (*podLocalProvider) Start(_ context.Context, name string, _ runtime.Config) error {
	return podLocalRefusal("start", name)
}

// Stop refuses with ErrControllerOwnedRuntime for every name: the
// controller owns the session lifecycle. The refusal also matches
// runtime.ErrStopRefused, so cleanup paths that absorb a gone session
// (StopForCleanup and its callers) keep treating it as a stop the worker
// could not perform rather than as success or as a session that is gone.
func (p *podLocalProvider) Stop(name string) error {
	return fmt.Errorf("%w: %w", runtime.ErrStopRefused, podLocalRefusal("stop", name))
}

// Interrupt refuses with ErrControllerOwnedRuntime.
func (*podLocalProvider) Interrupt(name string) error {
	return podLocalRefusal("interrupt", name)
}

// IsRunning reports whether the pod's own tmux session exists, probed with a
// local has-session against the exact "=main" target; false for any other
// name, without executing anything. The probe cannot answer with an error:
// a failed probe stays false rather than acquiring absence semantics the
// caller does not expect from a bool.
func (p *podLocalProvider) IsRunning(name string) bool {
	if !p.servesOwnSession(name) {
		return false
	}
	_, err := p.run([]string{"tmux", "has-session", "-t", podLocalTarget})
	return err == nil
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

// SetMeta stores a key-value pair in the pod's own tmux session
// environment — the same store the controller reads through pod exec, so a
// drain ack written here reaches the controller. Any other session is a
// refusal that executes nothing.
func (p *podLocalProvider) SetMeta(name, key, value string) error {
	if !p.servesOwnSession(name) {
		return podLocalRefusal("set-meta", name)
	}
	return p.metaCommand("set-meta", name,
		[]string{"tmux", "set-environment", "-t", podLocalTarget, key, value})
}

// GetMeta retrieves a metadata value from the pod's own tmux session
// environment. A missing variable reads as an empty value: tmux's
// explicitly-unset "-KEY" line and its missing-variable diagnostic both
// mean the key holds nothing. A missing session or server is
// runtime.ErrSessionNotFound; any other failure reaches the caller with
// context. The value keeps embedded "=" and whitespace: only tmux's
// trailing newline is stripped.
func (p *podLocalProvider) GetMeta(name, key string) (string, error) {
	if !p.servesOwnSession(name) {
		return "", podLocalRefusal("get-meta", name)
	}
	out, err := p.run([]string{"tmux", "show-environment", "-t", podLocalTarget, key})
	if err != nil {
		if podLocalSays(out, "no such variable") {
			return "", nil
		}
		return "", podLocalClassify("get-meta", name, out, err, false)
	}
	out = strings.TrimSuffix(out, "\n")
	if strings.HasPrefix(out, "-") {
		return "", nil // explicitly unset
	}
	_, val, ok := strings.Cut(out, "=")
	if !ok {
		return "", fmt.Errorf("pod-local runtime get-meta %q: tmux show-environment %q answered %q, not KEY=value", name, key, out)
	}
	return val, nil
}

// RemoveMeta removes a metadata key from the pod's own tmux session
// environment; a key that already holds nothing is a no-op so callers can
// clear unconditionally.
func (p *podLocalProvider) RemoveMeta(name, key string) error {
	if !p.servesOwnSession(name) {
		return podLocalRefusal("remove-meta", name)
	}
	out, err := p.run([]string{"tmux", "set-environment", "-t", podLocalTarget, "-u", key})
	if err == nil {
		return nil
	}
	return podLocalClassify("remove-meta", name, out, err, true)
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
