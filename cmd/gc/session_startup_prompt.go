package main

import (
	"io"
	"maps"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// startupPromptCity delivers startup prompts to launches of one city's sessions
// that gc starts outside the reconciler: the CLI and API worker-factory resume
// resolvers (ga-4k4zfk). The reconciler renders and delivers its own prompt.
type startupPromptCity struct {
	path   string
	cfg    *config.City
	stderr io.Writer
}

// applyTo returns hints with the startup prompt of the session described by
// info delivered for a launch that resumes it. resolved and transport are the
// provider and transport the caller resolved for this launch. The prompt rides
// the post-start nudge and the launch env carries GC_STARTUP_PROMPT_DELIVERED,
// so the SessionStart hook adds context without repeating a prompt it cannot
// inline past the provider's size cap. It applies the reconciler's promptDelivery
// policy, including the oversized-prompt hard failure. A session with no agent
// config, or whose agent has no prompt, is returned unchanged, and so is one
// whose runtime is already up: the worker factory resolves these hints on every
// handle it builds, and only a launch consumes them.
func (c startupPromptCity) applyTo(info session.Info, resolved *config.ResolvedProvider, transport string, hints runtime.Config) (runtime.Config, error) {
	// The factory enriches info with a live runtime probe before it resolves
	// hints, so StateActive here means the runtime is up and Manager.ensureRunning
	// returns without launching. Every other state may launch and gets the prompt.
	if info.State == session.StateActive {
		return hints, nil
	}
	agent := findAgentByTemplate(c.cfg, info.Template)
	if agent == nil {
		return hints, nil
	}
	prompt := c.promptFor(agent, info)
	if prompt == "" {
		return hints, nil
	}
	plan, err := planStartupPrompt(startupPromptSource{
		TemplateName:     agent.QualifiedName(),
		SessionName:      info.SessionName,
		InstanceName:     info.AgentName,
		Prompt:           prompt,
		IsACP:            transport == config.SessionTransportACP,
		ResolvedProvider: resolved,
		HintNudge:        hints.Nudge,
		RuntimeName:      effectiveSessionProvider(agent.Session, c.cfg.Session.Provider),
		CityRuntimes:     c.cfg.Runtimes,
	}, startupPromptResumeLaunch)
	if err != nil {
		return runtime.Config{}, err
	}
	// hints.Env can alias the caller's session env; the marker belongs to this
	// launch only.
	hints.Env = maps.Clone(hints.Env)
	plan.applyTo(&hints)
	return hints, nil
}

// promptFor renders the prompt gc prime would emit for agent a in the session
// described by info, or "" when there is nothing to deliver. The template
// context is built from the session's own identity, never from the calling
// process's GC_* environment: a CLI or API resolver runs in an operator's shell
// or the controller, so ambient identity would render another agent's prompt.
// Its query topology is the controller-safe one the reconciler renders with, so
// every launch of a session carries the same prompt.
func (c startupPromptCity) promptFor(a *config.Agent, info session.Info) string {
	if a.PromptTemplate == "" || suppressStartupPromptForAgent(a) {
		return ""
	}
	sessionEnv := map[string]string{
		"GC_AGENT": firstNonEmptyGCString(info.AgentName, a.QualifiedName()),
		"GC_DIR":   info.WorkDir,
	}
	cityName := loadedCityName(c.cfg, c.path)
	ctx := buildPrimeContextWithEnv(c.path, cityName, a, c.cfg.Rigs,
		config.QueryTopology{Beads: c.cfg.Beads}, func(key string) string { return sessionEnv[key] }, c.stderr)
	ctx.ProviderKey, ctx.ProviderDisplayName = providerInfoForAgent(a, &c.cfg.Workspace, c.cfg.Providers)
	ctx.InstructionsFile = instructionsFileForAgent(a, &c.cfg.Workspace, c.cfg.Providers)
	return renderAgentPromptTemplate(c.path, cityName, c.cfg, a, ctx, c.stderr)
}
