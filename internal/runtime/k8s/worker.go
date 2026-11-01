package k8s

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/guard"
)

// WorkerProvider is the runtime provider gc uses inside a generated agent Pod.
//
// Those Pods hold no Kubernetes credentials, and the controller owns every
// session lifecycle, so the guard refuses lifecycle operations. Session
// metadata is the exception: it lives in the Pod's own tmux environment, the
// same one Provider reads and writes through pod exec. A worker can therefore
// signal the controller, for example with a drain acknowledgement, using only
// its local tmux server. Only the Pod's own session can be addressed.
type WorkerProvider struct {
	*guard.Provider
	session string
	tmux    func(args ...string) (string, error)
}

var _ runtime.Provider = (*WorkerProvider)(nil)

// NewWorkerProvider returns the provider for the session this Pod runs.
func NewWorkerProvider() *WorkerProvider {
	return &WorkerProvider{
		Provider: guard.New(),
		session:  strings.TrimSpace(os.Getenv("GC_SESSION_NAME")),
		tmux: func(args ...string) (string, error) {
			out, err := exec.Command("tmux", args...).Output()
			return string(out), err
		},
	}
}

func (p *WorkerProvider) own(name string) error {
	if p.session == "" || name != p.session {
		return fmt.Errorf("k8s worker %q cannot address session %q: %w", p.session, name, guard.ErrControllerOwnedRuntime)
	}
	return nil
}

// IsRunning reports whether name is this Pod's live session.
func (p *WorkerProvider) IsRunning(name string) bool {
	if p.own(name) != nil {
		return false
	}
	_, err := p.tmux("has-session", "-t", tmuxSession)
	return err == nil
}

// SetMeta writes a metadata value to this Pod's tmux environment.
func (p *WorkerProvider) SetMeta(name, key, value string) error {
	if err := p.own(name); err != nil {
		return err
	}
	if _, err := p.tmux("set-environment", "-t", tmuxSession, key, value); err != nil {
		return fmt.Errorf("setting k8s worker metadata %s: %w: %w", key, runtime.ErrSessionNotFound, err)
	}
	return nil
}

// GetMeta reads a metadata value from this Pod's tmux environment. An unset
// key reads as empty.
func (p *WorkerProvider) GetMeta(name, key string) (string, error) {
	if err := p.own(name); err != nil {
		return "", err
	}
	if _, err := p.tmux("has-session", "-t", tmuxSession); err != nil {
		return "", fmt.Errorf("reading k8s worker metadata %s: %w: %w", key, runtime.ErrSessionNotFound, err)
	}
	output, err := p.tmux("show-environment", "-t", tmuxSession, key)
	if err != nil {
		return "", nil //nolint:nilerr // tmux reports an unknown variable as an error.
	}
	if _, val, ok := strings.Cut(strings.TrimSpace(output), "="); ok {
		return val, nil
	}
	return "", nil
}

// RemoveMeta removes a metadata value from this Pod's tmux environment.
func (p *WorkerProvider) RemoveMeta(name, key string) error {
	if err := p.own(name); err != nil {
		return err
	}
	if _, err := p.tmux("set-environment", "-t", tmuxSession, "-u", key); err != nil {
		return fmt.Errorf("removing k8s worker metadata %s: %w: %w", key, runtime.ErrSessionNotFound, err)
	}
	return nil
}
