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
// policy and returns hints unchanged when nothing can or needs to be delivered:
// a closed session, a session with no agent config or prompt, and a runtime that
// cannot be confirmed to carry a nudge (subprocess ignores cfg.Nudge, so marking
// the launch delivered would suppress the hook and lose the prompt).
//
// Running sessions are NOT skipped. The worker factory resolves these hints once
// per handle, and an interrupting submit stops a running session and relaunches
// it with the same hints (Manager.interruptAndSubmitLocked), so the payload must
// already be there when a handle is built over a live runtime.
func (c startupPromptCity) applyTo(info session.Info, resolved *config.ResolvedProvider, transport string, hints runtime.Config) (runtime.Config, error) {
	if info.Closed {
		return hints, nil
	}
	agent := findAgentByTemplate(c.cfg, info.Template)
	if agent == nil {
		return hints, nil
	}
	runtimeName := effectiveSessionProvider(agent.Session, c.cfg.Session.Provider)
	if transport != config.SessionTransportACP && promptDeliverySupportFor(runtimeName, c.cfg.Runtimes) == promptDeliverySupportUnsupported {
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
		RuntimeName:      runtimeName,
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
// The result is gc prime's rendering, which the hook would otherwise carry: it
// has no beacon (the hook still emits one) and no assigned-skills appendix, and
// it uses the controller-safe query topology the reconciler renders with
// (cityQueryTopology reopens the storage binding, which a controller must not do).
func (c startupPromptCity) promptFor(a *config.Agent, info session.Info) string {
	if a.PromptTemplate == "" || suppressStartupPromptForAgent(a) {
		return ""
	}
	identity := primeSessionIdentity{
		Agent: firstNonEmptyGCString(info.AgentName, a.QualifiedName()),
		Dir:   info.WorkDir,
	}
	cityName := loadedCityName(c.cfg, c.path)
	ctx := buildPrimeContextWithIdentity(c.path, cityName, a, c.cfg.Rigs,
		config.QueryTopology{Beads: c.cfg.Beads}, identity, c.stderr)
	ctx.ProviderKey, ctx.ProviderDisplayName = providerInfoForAgent(a, &c.cfg.Workspace, c.cfg.Providers)
	ctx.InstructionsFile = instructionsFileForAgent(a, &c.cfg.Workspace, c.cfg.Providers)
	return renderAgentPromptTemplate(c.path, cityName, c.cfg, a, ctx, c.stderr)
}
