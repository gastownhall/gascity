package k8s

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/guard"
)

// PodSessionEnv names the environment variable the k8s provider sets in every
// pod it generates. Its value is the logical session name the pod hosts. A gc
// process that sees it non-empty is running inside that pod, and serves its
// k8s or hybrid session provider through the pod-local provider instead of a
// Kubernetes client. The provider owns this name: it drops config-supplied
// copies and appends the authoritative value itself.
const PodSessionEnv = "GC_K8S_POD_SESSION"

// podLocalTarget is the exact tmux target of the pod's one session on the
// pod's default tmux server: the session the controller reaches through pod
// exec.
const podLocalTarget = "=" + tmuxSession

// podLocalTmuxTimeout bounds each pod-local tmux command, as the tmux
// provider bounds each of its subprocesses: a stopped or wedged pod tmux
// server fails the drain-ack, drain-check or hook claim that asked instead of
// hanging it.
const podLocalTmuxTimeout = 30 * time.Second

// podLocalTmuxWaitDelay bounds how long a tmux command's output pipes may stay
// open after the client exits or its deadline kills it: the tmux client hands
// its stdio to the server, so a wedged server can hold them open forever.
const podLocalTmuxWaitDelay = 2 * time.Second

// tmuxRunner runs one tmux command with the given arguments and returns its
// stdout and stderr.
type tmuxRunner func(ctx context.Context, args ...string) (stdout, stderr string, err error)

// execTmux runs tmux on the pod's default server, without socket arguments.
func execTmux(ctx context.Context, args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	err := podLocalTmuxCommand(ctx, args, &stdout, &stderr).Run()
	return stdout.String(), stderr.String(), err
}

// podLocalTmuxCommand builds a tmux command on the pod's default server that
// captures stdout and stderr, with podLocalTmuxWaitDelay set.
func podLocalTmuxCommand(ctx context.Context, args []string, stdout, stderr *bytes.Buffer) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = podLocalTmuxWaitDelay
	return cmd
}

var _ runtime.Provider = (*PodLocal)(nil)

// PodLocal is the session provider a gc process inside a generated pod uses.
// It constructs no Kubernetes client: it serves only the pod's own session,
// reading and writing that session's tmux environment and probing its
// liveness on the pod's local tmux server.
//
// The controller owns every other runtime operation, so PodLocal refuses
// lifecycle and delivery operations for every name, and metadata access to
// any other session, with an error wrapping guard.ErrControllerOwnedRuntime;
// Stop's refusal also wraps runtime.ErrStopRefused. Each tmux command it runs
// is bounded by podLocalTmuxTimeout. It claims no capabilities and offers no
// fresh liveness observation, so a false probe of another session never
// confirms that session gone. Like the guard provider, it does not satisfy the
// shared provider conformance contract, which exercises a successful
// lifecycle.
type PodLocal struct {
	self    string
	run     tmuxRunner
	timeout time.Duration
}

// PodSession returns this process's PodSessionEnv value, trimmed: the logical
// session of the generated pod it runs in, or empty outside one.
func PodSession() string {
	return strings.TrimSpace(os.Getenv(PodSessionEnv))
}

// ValidateStaticConfig checks, without a Kubernetes client, the pod-creation
// settings a k8s provider reads from the environment: the scheduling fields
// (GC_K8S_NODE_SELECTOR, GC_K8S_TOLERATIONS, GC_K8S_AFFINITY) and the managed
// in-cluster service alias (GC_K8S_DOLT_HOST/GC_K8S_DOLT_PORT). It parses them
// exactly as NewProvider does. NewPodLocal deliberately does not call it, so a
// malformed setting the pod never uses cannot stop a running worker from
// acknowledging a drain.
func ValidateStaticConfig() error {
	if _, err := parseSchedulingEnv(); err != nil {
		return fmt.Errorf("k8s scheduling settings: %w", err)
	}
	if _, _, err := managedServiceAlias(); err != nil {
		return fmt.Errorf("k8s managed service alias: %w", err)
	}
	return nil
}

// NewPodLocal returns the pod-local provider serving the session self, the
// value of PodSessionEnv, on the pod's local tmux server. An empty self is an
// error. It constructs no Kubernetes client and reads no kubeconfig.
func NewPodLocal(self string) (*PodLocal, error) {
	return newPodLocal(self, execTmux)
}

// newPodLocal returns a pod-local provider serving the session self.
func newPodLocal(self string, run tmuxRunner) (*PodLocal, error) {
	self = strings.TrimSpace(self)
	if self == "" {
		return nil, fmt.Errorf("pod-local session provider: %s names no session", PodSessionEnv)
	}
	return &PodLocal{self: self, run: run, timeout: podLocalTmuxTimeout}, nil
}

// ownSession reports whether name addresses the pod's own session: the
// pod-local tmux name or the logical session name the pod hosts.
func (p *PodLocal) ownSession(name string) bool {
	return name == tmuxSession || name == p.self
}

// runTmux runs one tmux command for op on name under the pod-local deadline.
// A command the deadline ended returns an error wrapping
// runtime.ErrRuntimeUnavailable: the session could not be observed, which is
// neither a missing session nor a refusal.
func (p *PodLocal) runTmux(op, name string, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	stdout, stderr, err = p.run(ctx, args...)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stdout, stderr, fmt.Errorf("pod-local %s %q: tmux did not answer within %s: %w: %w", op, name, p.timeout, err, runtime.ErrRuntimeUnavailable)
	}
	return stdout, stderr, err
}

// tmux runs one tmux command against the pod's own session.
func (p *PodLocal) tmux(op, name string, args ...string) error {
	if _, stderr, err := p.runTmux(op, name, args...); err != nil {
		return podLocalError(op, name, stderr, err)
	}
	return nil
}

// podLocalError gives a failed tmux command its context, mapping a missing
// session or server to runtime.ErrSessionNotFound. A command the deadline
// ended already carries its context and runtime.ErrRuntimeUnavailable, and is
// returned as it is.
func podLocalError(op, name, stderr string, err error) error {
	if errors.Is(err, runtime.ErrRuntimeUnavailable) {
		return err
	}
	diag := strings.TrimSpace(stderr)
	if podLocalSessionMissing(diag) {
		return fmt.Errorf("pod-local %s %q: %s: %w", op, name, diag, runtime.ErrSessionNotFound)
	}
	if diag != "" {
		return fmt.Errorf("pod-local %s %q: tmux: %s: %w", op, name, diag, err)
	}
	return fmt.Errorf("pod-local %s %q: tmux: %w", op, name, err)
}

// podLocalSessionMissing reports whether a tmux diagnostic says the session or
// its server does not exist.
func podLocalSessionMissing(diag string) bool {
	for _, s := range []string{
		"no server running",
		"error connecting to",
		"server exited unexpectedly",
		"can't find session",
		"session not found",
		"no such session",
		"no current target",
	} {
		if strings.Contains(diag, s) {
			return true
		}
	}
	return false
}

// SetMeta stores key=value in the pod's own session tmux environment.
func (p *PodLocal) SetMeta(name, key, value string) error {
	if !p.ownSession(name) {
		return p.refuse("set-meta", name)
	}
	return p.tmux("set-meta", name, "set-environment", "-t", podLocalTarget, key, value)
}

// GetMeta reads key from the pod's own session tmux environment. A variable
// the session does not have, or has marked unset, reads as empty; a missing
// session or server is runtime.ErrSessionNotFound; a server that does not
// answer within the deadline is runtime.ErrRuntimeUnavailable; every other
// failure is returned.
func (p *PodLocal) GetMeta(name, key string) (string, error) {
	if !p.ownSession(name) {
		return "", p.refuse("get-meta", name)
	}
	stdout, stderr, err := p.runTmux("get-meta", name, "show-environment", "-t", podLocalTarget, key)
	if err != nil {
		diag := strings.TrimSpace(stderr)
		if strings.Contains(diag, "unknown variable") && !podLocalSessionMissing(diag) {
			return "", nil
		}
		return "", podLocalError("get-meta", name, stderr, err)
	}
	line := strings.TrimSuffix(strings.TrimSuffix(stdout, "\n"), "\r")
	if line == "-"+key {
		return "", nil
	}
	gotKey, val, ok := strings.Cut(line, "=")
	if !ok || gotKey != key {
		return "", fmt.Errorf("pod-local get-meta %q: unexpected tmux environment line for %s: %q", name, key, line)
	}
	return val, nil
}

// RemoveMeta unsets key in the pod's own session tmux environment.
func (p *PodLocal) RemoveMeta(name, key string) error {
	if !p.ownSession(name) {
		return p.refuse("remove-meta", name)
	}
	return p.tmux("remove-meta", name, "set-environment", "-t", podLocalTarget, "-u", key)
}

// IsRunning probes the pod's own session on the local tmux server. Every
// other session reads false: the pod observes no other runtime. A probe that
// fails, a deadline included, reads false.
func (p *PodLocal) IsRunning(name string) bool {
	if !p.ownSession(name) {
		return false
	}
	_, _, err := p.runTmux("is-running", name, "has-session", "-t", podLocalTarget)
	return err == nil
}

// refuse returns the controller-owned refusal for op on name. It runs
// nothing: the controller owns every runtime operation the pod-local provider
// does not serve.
func (p *PodLocal) refuse(op, name string) error {
	return fmt.Errorf("pod-local session provider for %q: runtime %s %q: %w", p.self, op, name, guard.ErrControllerOwnedRuntime)
}

// Start refuses: the controller starts sessions.
func (p *PodLocal) Start(_ context.Context, name string, _ runtime.Config) error {
	return p.refuse("start", name)
}

// Stop refuses with an error wrapping both guard.ErrControllerOwnedRuntime
// and runtime.ErrStopRefused: the session stays running.
func (p *PodLocal) Stop(name string) error {
	return fmt.Errorf("%w: %w", runtime.ErrStopRefused, p.refuse("stop", name))
}

// Interrupt refuses: the controller steers sessions.
func (p *PodLocal) Interrupt(name string) error { return p.refuse("interrupt", name) }

// IsAttached reports false: the pod observes no attachment.
func (p *PodLocal) IsAttached(string) bool { return false }

// Attach refuses: the controller owns attachment.
func (p *PodLocal) Attach(name string) error { return p.refuse("attach", name) }

// ProcessAlive reports false: the pod observes no runtime processes.
func (p *PodLocal) ProcessAlive(string, []string) bool { return false }

// Nudge refuses: the controller delivers input.
func (p *PodLocal) Nudge(name string, _ []runtime.ContentBlock) error {
	return p.refuse("nudge", name)
}

// Peek refuses: the controller observes output.
func (p *PodLocal) Peek(name string, _ int) (string, error) { return "", p.refuse("peek", name) }

// ListRunning refuses: the controller enumerates sessions.
func (p *PodLocal) ListRunning(prefix string) ([]string, error) {
	return nil, p.refuse("list-running", prefix)
}

// GetLastActivity refuses: the controller observes activity.
func (p *PodLocal) GetLastActivity(name string) (time.Time, error) {
	return time.Time{}, p.refuse("get-last-activity", name)
}

// ClearScrollback refuses: the controller owns the pane.
func (p *PodLocal) ClearScrollback(name string) error { return p.refuse("clear-scrollback", name) }

// CopyTo refuses: the controller stages files.
func (p *PodLocal) CopyTo(name, _, _ string) error { return p.refuse("copy-to", name) }

// SendKeys refuses: the controller delivers input.
func (p *PodLocal) SendKeys(name string, _ ...string) error { return p.refuse("send-keys", name) }

// RunLive refuses: the controller applies live session commands.
func (p *PodLocal) RunLive(name string, _ runtime.Config) error { return p.refuse("run-live", name) }

// Capabilities reports none.
func (p *PodLocal) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{}
}
