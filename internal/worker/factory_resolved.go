package worker

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ResolvedRuntime captures worker-owned launch inputs after a caller has
// resolved provider-specific runtime configuration.
type ResolvedRuntime struct {
	Command    string
	WorkDir    string
	Provider   string
	SessionEnv map[string]string
	Resume     sessionpkg.ProviderResume
	Hints      runtime.Config
}

// ResolvedSessionConfig describes a new session-backed worker handle whose
// runtime inputs have already been resolved by the caller.
type ResolvedSessionConfig struct {
	Alias        string
	ExplicitName string
	Template     string
	Title        string
	Transport    string
	Metadata     map[string]string
	Runtime      ResolvedRuntime
}

func normalizeResolvedRuntimeInput(input ResolvedRuntime) ResolvedRuntime {
	input.Command = strings.TrimSpace(input.Command)
	input.WorkDir = strings.TrimSpace(input.WorkDir)
	input.Provider = strings.TrimSpace(input.Provider)
	input.SessionEnv = cloneStringMap(input.SessionEnv)
	input.Hints = cloneRuntimeConfig(input.Hints)
	if input.WorkDir == "" {
		input.WorkDir = strings.TrimSpace(input.Hints.WorkDir)
	}
	if strings.TrimSpace(input.Hints.WorkDir) == "" {
		input.Hints.WorkDir = input.WorkDir
	}
	if input.Provider == "" && input.Command != "" {
		input.Provider = input.Command
		if idx := strings.IndexAny(input.Provider, " \t"); idx >= 0 {
			input.Provider = input.Provider[:idx]
		}
	}
	return input
}

// NormalizeResolvedRuntime trims, clones, and fills derived runtime fields
// used by session-backed worker construction.
func NormalizeResolvedRuntime(input ResolvedRuntime) (ResolvedRuntime, error) {
	input = normalizeResolvedRuntimeInput(input)
	if input.Command == "" {
		return ResolvedRuntime{}, fmt.Errorf("%w: command is required", ErrHandleConfig)
	}
	if input.Provider == "" {
		return ResolvedRuntime{}, fmt.Errorf("%w: provider is required", ErrHandleConfig)
	}
	return input, nil
}

// NormalizeResolvedSessionConfig trims, clones, and validates caller-resolved
// session creation inputs before they are translated into a worker SessionSpec.
func NormalizeResolvedSessionConfig(cfg ResolvedSessionConfig) (ResolvedSessionConfig, error) {
	runtime, err := NormalizeResolvedRuntime(cfg.Runtime)
	if err != nil {
		return ResolvedSessionConfig{}, err
	}
	cfg.Transport = strings.TrimSpace(cfg.Transport)
	cfg.Metadata = cloneStringMap(cfg.Metadata)
	cfg.Runtime = runtime
	return cfg, nil
}

// SessionSpecForResolvedRuntime translates resolved runtime inputs into the
// canonical worker session spec used by session-backed handles.
func SessionSpecForResolvedRuntime(cfg ResolvedSessionConfig) (SessionSpec, error) {
	cfg, err := NormalizeResolvedSessionConfig(cfg)
	if err != nil {
		return SessionSpec{}, err
	}

	return SessionSpec{
		Alias:        cfg.Alias,
		ExplicitName: cfg.ExplicitName,
		Template:     cfg.Template,
		Title:        cfg.Title,
		Command:      cfg.Runtime.Command,
		WorkDir:      cfg.Runtime.WorkDir,
		Provider:     cfg.Runtime.Provider,
		Transport:    cfg.Transport,
		Env:          cfg.Runtime.SessionEnv,
		Resume:       cfg.Runtime.Resume,
		Hints:        cfg.Runtime.Hints,
		Metadata:     cfg.Metadata,
	}, nil
}

func applyResolvedRuntimeToSessionSpec(spec *SessionSpec, runtime *ResolvedRuntime) {
	if spec == nil || runtime == nil {
		return
	}
	normalized := normalizeResolvedRuntimeInput(*runtime)

	if command := normalized.Command; command != "" {
		spec.Command = command
	}
	if provider := normalized.Provider; provider != "" {
		spec.Provider = provider
	}
	if workDir := normalized.WorkDir; workDir != "" {
		spec.WorkDir = workDir
	}

	spec.Env = normalized.SessionEnv
	spec.Resume = normalized.Resume
	spec.Hints = normalized.Hints
	if strings.TrimSpace(spec.Hints.WorkDir) == "" {
		spec.Hints.WorkDir = strings.TrimSpace(spec.WorkDir)
	}
}

// SessionForResolvedRuntime constructs a session-backed handle from caller-
// resolved runtime inputs without forcing the caller to rebuild SessionSpec.
func (f *Factory) SessionForResolvedRuntime(cfg ResolvedSessionConfig) (Handle, error) {
	spec, err := SessionSpecForResolvedRuntime(cfg)
	if err != nil {
		return nil, err
	}
	return f.Session(spec)
}
