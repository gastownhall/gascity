package main

import (
	"io"
	"log/slog"
	"maps"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// startupPromptCity delivers startup prompts to the launches a session Manager
// performs for one city's sessions: every create and every resume that starts a
// runtime (ga-4k4zfk, ga-vytkfv). It is installed as the Manager's
// session.LaunchFinalizer, so the prompt is rendered exactly when a runtime is
// about to start, from the config that launch assembled. The reconciler renders
// and delivers its own prompt and never reaches this seam.
type startupPromptCity struct {
	path   string
	cfg    *config.City
	stderr io.Writer
}

// finalize returns lc.Config with the startup prompt of the launched session
// delivered: through the route the provider's prompt mode and the prompt's size
// select for a create, and through the post-start nudge for a resume (an argv
// prompt behind a resume command would be submitted to the continued
// conversation as a new turn). The launch env carries
// GC_STARTUP_PROMPT_DELIVERED exactly when the launch delivers, so the
// SessionStart hook adds context without repeating a prompt it cannot inline past
// the provider's size cap. It applies the reconciler's promptDelivery policy and
// returns the config with no payload and no marker when nothing can or needs to
// be delivered: a closed session, a session with no agent config or prompt, a
// client's provider chat (isClientProviderChat), and a runtime that cannot be
// confirmed to carry a nudge (subprocess ignores cfg.Nudge, so marking the launch
// delivered would suppress the hook and lose the prompt). A non-nil error means
// the prompt cannot be delivered and the launch must not proceed.
//
// The prompt is rendered for the identity the launch env carries (GC_ALIAS, else
// GC_AGENT), never the session row's stored agent name: an unaliased pool-managed
// create has no agent name yet, and the template must show the agent the same
// identity its own gc prime would.
func (c startupPromptCity) finalize(lc session.LaunchContext) (runtime.Config, error) {
	info := lc.Info
	cfg := lc.Config
	if info.Closed {
		return withoutStartupPrompt(cfg), nil
	}
	agent := findAgentByTemplate(c.cfg, info.Template)
	if agent == nil {
		return withoutStartupPrompt(cfg), nil
	}
	// A client's provider chat gets no worker persona on any start path: the
	// hook (primeHookClientChat) and the reconciler send it the beacon alone.
	if isClientProviderChat(agent, info.SessionOrigin, info.Alias) {
		return withoutStartupPrompt(cfg), nil
	}
	expectsPrompt := agent.PromptTemplate != "" && !suppressStartupPromptForAgent(agent)
	runtimeName := effectiveSessionProvider(agent.Session, c.cfg.Session.Provider)
	if lc.Transport != config.SessionTransportACP && promptDeliverySupportFor(runtimeName, c.cfg.Runtimes) == promptDeliverySupportUnsupported {
		if expectsPrompt {
			c.warnNotDelivered(lc, agent, "runtime-cannot-carry-nudge")
		}
		return withoutStartupPrompt(cfg), nil
	}
	prompt := c.promptFor(agent, launchIdentity(lc, agent))
	if prompt == "" {
		return withoutStartupPrompt(cfg), nil
	}
	resolved, _ := resolveWorkerRuntimeProviderWithConfigAndMetadata(c.cfg, info, "", nil)
	launch := startupPromptFreshLaunch
	if lc.Kind == session.LaunchResume {
		launch = startupPromptResumeLaunch
	}
	plan, err := planStartupPrompt(startupPromptSource{
		TemplateName:     agent.QualifiedName(),
		SessionName:      info.SessionName,
		InstanceName:     info.AgentName,
		Prompt:           prompt,
		IsACP:            lc.Transport == config.SessionTransportACP,
		ResolvedProvider: resolved,
		HintNudge:        cfg.Nudge,
		RuntimeName:      runtimeName,
		CityRuntimes:     c.cfg.Runtimes,
	}, launch)
	if err != nil {
		return runtime.Config{}, err
	}
	plan.applyTo(&cfg)
	if !plan.MarkDelivered {
		c.warnNotDelivered(lc, agent, "plan-delivers-nothing")
		return cfg, nil
	}
	slog.Info("startup prompt finalized for launch",
		"origin", string(lc.Kind),
		"session_id", info.ID,
		"template", agent.QualifiedName(),
		"route", startupPromptRoute(cfg),
		"marker", cfg.Env[startupPromptDeliveredEnv] == "1",
		"prompt_bytes", len(prompt))
	return cfg, nil
}

// warnNotDelivered records a launch whose agent has a startup prompt that this
// launch does not deliver, so the SessionStart hook is its only carrier.
func (c startupPromptCity) warnNotDelivered(lc session.LaunchContext, agent *config.Agent, reason string) {
	slog.Warn("startup prompt not delivered by launch",
		"origin", string(lc.Kind),
		"session_id", lc.Info.ID,
		"template", agent.QualifiedName(),
		"reason", reason)
}

// withoutStartupPrompt is cfg for a launch that delivers no prompt: it carries
// no inherited delivered marker, which would suppress the hook's copy.
func withoutStartupPrompt(cfg runtime.Config) runtime.Config {
	if _, ok := cfg.Env[startupPromptDeliveredEnv]; !ok {
		return cfg
	}
	cfg.Env = maps.Clone(cfg.Env)
	delete(cfg.Env, startupPromptDeliveredEnv)
	return cfg
}

// startupPromptRoute names how cfg carries the startup prompt, for the launch
// record.
func startupPromptRoute(cfg runtime.Config) string {
	switch {
	case cfg.PromptFlag != "":
		return "flag"
	case cfg.PromptSuffix != "":
		return "argv"
	case cfg.Nudge != "":
		return "nudge"
	}
	return "none"
}

// launchIdentity is the session identity the launch env carries, the identity
// the agent's own gc prime reads from its environment.
func launchIdentity(lc session.LaunchContext, a *config.Agent) primeSessionIdentity {
	env := lc.Config.Env
	return primeSessionIdentity{
		Alias:   firstNonEmptyGCString(env["GC_ALIAS"], lc.Info.Alias),
		Agent:   firstNonEmptyGCString(env["GC_AGENT"], lc.Info.AgentName, a.QualifiedName()),
		Dir:     firstNonEmptyGCString(env["GC_DIR"], lc.Config.WorkDir, lc.Info.WorkDir),
		Rig:     env["GC_RIG"],
		RigRoot: env["GC_RIG_ROOT"],
		Branch:  env["GC_BRANCH"],
	}
}

// promptFor renders the prompt gc prime would emit for agent a running as
// identity, or "" when there is nothing to deliver. The template
// context is built from the session's own identity, never from the calling
// process's GC_* environment: a CLI or API resolver runs in an operator's shell
// or the controller, so ambient identity would render another agent's prompt.
// The result is gc prime's rendering, which the hook would otherwise carry: it
// has no beacon (the hook still emits one) and no assigned-skills appendix, and
// it uses the controller-safe query topology the reconciler renders with
// (cityQueryTopology reopens the storage binding, which a controller must not do).
func (c startupPromptCity) promptFor(a *config.Agent, identity primeSessionIdentity) string {
	if a.PromptTemplate == "" || suppressStartupPromptForAgent(a) {
		return ""
	}
	cityName := loadedCityName(c.cfg, c.path)
	ctx := buildPrimeContextWithIdentity(c.path, cityName, a, c.cfg.Rigs,
		config.QueryTopology{Beads: c.cfg.Beads}, identity, c.stderr)
	ctx.ProviderKey, ctx.ProviderDisplayName = providerInfoForAgent(a, &c.cfg.Workspace, c.cfg.Providers)
	ctx.InstructionsFile = instructionsFileForAgent(a, &c.cfg.Workspace, c.cfg.Providers)
	return renderPrimeAgentPrompt(c.path, cityName, c.cfg, a, ctx, c.stderr).Text
}
