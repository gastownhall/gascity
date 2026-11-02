package main

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// startupPromptLaunch is the kind of launch a startup prompt is delivered to.
type startupPromptLaunch int

const (
	// startupPromptFreshLaunch begins a conversation: promptDelivery routes the
	// prompt through the argv suffix, the argv flag or the nudge, by provider
	// prompt mode and prompt size.
	startupPromptFreshLaunch startupPromptLaunch = iota
	// startupPromptResumeLaunch continues a conversation that already has
	// history. The prompt is re-delivered as the post-start nudge and never
	// through argv: an argv prompt behind a resume command would be submitted to
	// the continued conversation as a new turn.
	startupPromptResumeLaunch
)

// startupPromptSource is what a launch builder knows about the prompt it must
// deliver. TemplateParams projects onto it for the reconciler; the worker
// resolvers build it from the session and its agent.
type startupPromptSource struct {
	TemplateName string
	SessionName  string
	InstanceName string
	Prompt       string
	// Beacon is the bare identity line (`[city] agent • ts`, no prime
	// instruction) that opens a resume turn when the provider hook supplies
	// the role (roleSuppliedByHook). The worker resolvers leave it empty: they
	// carry no configured nudge, so that resume sends no turn at all. Were
	// they to carry one, it would go out without a beacon.
	Beacon string
	IsACP  bool
	// HookEnabled is config.AgentHasHooks for the launching agent: its
	// provider hooks are installed.
	HookEnabled bool
	// ClientProviderChat marks a client's provider chat (isClientProviderChat),
	// whose hook emits no persona.
	ClientProviderChat bool
	ResolvedProvider   *config.ResolvedProvider
	HintNudge          string
	RuntimeName        string
	// SessionRuntime is the runtime a non-ACP session runs on: the city's
	// session provider, whatever the agent's session override says.
	SessionRuntime string
	CityRuntimes   map[string]config.DiscoveredRuntime
}

// roleSuppliedByHook reports whether the provider hook supplies the rendered
// role prompt to every model generation of this launch, on fresh starts as
// well as resumes, so no launch submits it as a user turn. The hook decorates
// generations but never starts one; activation is delivered separately
// (hookNudge, resumeNudge and the reconciler's fresh-start kickoff).
//
// Every condition must hold. The resolved provider's builtin family stages a
// per-turn role hook (config.ResolvedProvider.HookSuppliesRolePerTurn);
// providers whose hooks prime once at SessionStart (pi, codex, antigravity,
// claude via settings) are excluded by that family fact, not by
// SupportsHooks, which is true for them as well. The hook is installed for
// this agent (HookEnabled: true by default for opencode/mimocode, whose
// overlay is staged for every launch; an explicit hooks_installed = false
// means no plugin, so the nudge stays the only carrier). The session is not
// ACP, whose transport never loads the CLI plugin. And the session is not a
// client's provider chat: its hook writes no persona (primeHookClientChat),
// so it keeps its beacon-only turns. Last, the session runs on a local
// terminal runtime, checked below.
func (s startupPromptSource) roleSuppliedByHook() bool {
	if !s.HookEnabled || s.IsACP || s.ClientProviderChat || !s.ResolvedProvider.HookSuppliesRolePerTurn() {
		return false
	}
	// Only the local terminal runtimes are known to stage the provider overlay
	// in the same filesystem where the CLI loads its plugin. That is the
	// city's runtime: session = "tmux" picks the terminal transport, not the
	// runtime. Remote and custom runtimes keep the explicit role carrier
	// rather than assuming hook presence.
	switch strings.TrimSpace(s.SessionRuntime) {
	case "", "tmux", "herdr":
		return true
	default:
		return false
	}
}

// hookNudge supplies activation without persisting the hook-supplied role as
// a user message: the beacon and the configured nudge. A beacon alone does
// not justify starting a turn, so with no configured nudge it is "".
func (s startupPromptSource) hookNudge() string {
	if strings.TrimSpace(s.HintNudge) == "" {
		return ""
	}
	return restartPromptNudge(s.Beacon, s.HintNudge)
}

// delivery selects the per-generation hook as the role carrier where it
// supplies the role, reported as effective mode "hook". Other launches keep
// promptDelivery's launch-time routing. Role bytes carried by the hook never
// enter argv and need no argv-size fallback. A fresh launch, gc prime --strict
// and gc doctor all route through it, so none of them can disagree about the
// carrier.
func (s startupPromptSource) delivery() (promptDeliveryResult, error) {
	if s.roleSuppliedByHook() {
		res := promptDeliveryResult{
			Nudge:         s.hookNudge(),
			Delivered:     strings.TrimSpace(s.Prompt) != "",
			EffectiveMode: "hook",
		}
		if s.ResolvedProvider != nil {
			res.ConfiguredMode = s.ResolvedProvider.PromptMode
		}
		return res, nil
	}
	return promptDelivery(s.Prompt, s.IsACP, s.ResolvedProvider, s.HintNudge, s.RuntimeName, s.CityRuntimes)
}

// agentStartupPromptSource is the source a fresh launch of agent a would plan
// the rendered prompt with, built from config alone: gc prime --strict and gc
// doctor measure the delivery budget through it. resolved may be nil.
func agentStartupPromptSource(cfg *config.City, a *config.Agent, resolved *config.ResolvedProvider, prompt string) startupPromptSource {
	providerName := ""
	if resolved != nil {
		providerName = resolved.Name
	}
	return startupPromptSource{
		TemplateName:     a.QualifiedName(),
		Prompt:           prompt,
		IsACP:            config.ResolveSessionCreateTransport(a.Session, resolved) == config.SessionTransportACP,
		HookEnabled:      config.AgentHasHooks(a, &cfg.Workspace, providerName, cfg.Providers),
		ResolvedProvider: resolved,
		RuntimeName:      effectiveSessionProvider(a.Session, cfg.Session.Provider),
		SessionRuntime:   cfg.Session.Provider,
		CityRuntimes:     cfg.Runtimes,
	}
}

// resumeNudge is the restart turn delivered to a resumed provider
// conversation. The rendered prompt rides along so a restarted session does
// not land idle, unless the provider hook re-supplies it to every generation:
// replaying it there would persist another copy of the role as a user message
// on every wake.
//
// On that hook-primed branch the turn is the beacon followed by the configured
// nudge: the nudge is the reason for the turn, and the beacon identifies the
// wake. With no configured nudge there is no restart turn. The role is already
// in the system prompt through the plugin, so a content-free wake turn only
// buys a generation that acknowledges and waits; the next human message, queued
// nudge or claim backstop (nudgeStalledPoolClaims) starts a real turn. tmux and
// herdr skip an empty nudge, so "" delivers nothing.
func (s startupPromptSource) resumeNudge() string {
	if s.roleSuppliedByHook() {
		return s.hookNudge()
	}
	return restartPromptNudge(s.Prompt, s.HintNudge)
}

// configuredMode names the prompt-delivery mode the source's provider is
// configured for, for the oversized-prompt log record.
func (s startupPromptSource) configuredMode() string {
	switch {
	case s.IsACP:
		return "acp"
	case s.ResolvedProvider != nil && s.ResolvedProvider.PromptMode != "":
		return s.ResolvedProvider.PromptMode
	}
	return "arg"
}

// startupPromptPlan is how one launch carries its startup prompt. applyTo is the
// only writer of the launch payload and the delivered marker, so a launch cannot
// carry one without the other: a non-empty prompt with no payload and no marker
// would leave the SessionStart hook as the sole, size-limited carrier
// (ga-4k4zfk).
type startupPromptPlan struct {
	PromptSuffix string
	PromptFlag   string
	Nudge        string
	// MarkDelivered reports whether the launch sets GC_STARTUP_PROMPT_DELIVERED,
	// which tells the SessionStart hook to add context without repeating the
	// prompt. Like promptDeliveryResult.Delivered it means delivery was selected
	// for this launch, not that the agent consumed the prompt.
	MarkDelivered bool
	// Delivery is promptDelivery's pure routing decision. The reconciler stamps
	// its priming markers from it, never from the env marker, which a resume
	// launch sets for hook consumption even though it delivers nothing new.
	Delivery promptDeliveryResult
	launch   startupPromptLaunch
}

// planStartupPrompt decides how a launch of the given kind carries src's prompt.
// Every launch runs promptDelivery first, so the oversized-prompt policy
// (fallback to the nudge, or a hard failure on a runtime with no fallback,
// ga-q8wgom.1.1) is identical for every launch builder.
func planStartupPrompt(src startupPromptSource, launch startupPromptLaunch) (startupPromptPlan, error) {
	delivery, err := src.delivery()
	if err != nil {
		logOversizedPromptDelivery(slog.Default().Error,
			"startup prompt exceeds argv-safety threshold; no fallback delivery available",
			src, "hard-fail")
		return startupPromptPlan{}, fmt.Errorf("template %q (session %q): %w", src.TemplateName, src.SessionName, err)
	}
	if delivery.OversizedFallback {
		logOversizedPromptDelivery(slog.Default().Warn,
			"startup prompt exceeds argv-safety threshold; falling back to nudge delivery",
			src, "nudge-fallback")
	}
	if launch == startupPromptResumeLaunch {
		plan := resumeStartupPromptPlan(src)
		plan.Delivery = delivery
		return plan, nil
	}
	return startupPromptPlan{
		PromptSuffix:  delivery.PromptSuffix,
		PromptFlag:    delivery.PromptFlag,
		Nudge:         delivery.Nudge,
		MarkDelivered: delivery.Delivered,
		Delivery:      delivery,
		launch:        launch,
	}, nil
}

// resumeStartupPromptPlan is the plan for a launch that continues a conversation:
// the restart turn (src.resumeNudge) rides the nudge and the marker is set
// whenever there is a prompt. A session whose hook supplies the role is still
// primed, through the hook rather than the nudge, so the marker is set for it
// too.
func resumeStartupPromptPlan(src startupPromptSource) startupPromptPlan {
	return startupPromptPlan{
		Nudge:         src.resumeNudge(),
		MarkDelivered: strings.TrimSpace(src.Prompt) != "",
		launch:        startupPromptResumeLaunch,
	}
}

// applyTo writes the plan's payload and marker into cfg. cfg.Env must be owned
// by the caller: it is written in place, and allocated when the plan marks a
// launch that has no env. A resume launch also strips a stale marker, so a
// resume that delivers nothing never inherits one.
func (p startupPromptPlan) applyTo(cfg *runtime.Config) {
	cfg.PromptSuffix = p.PromptSuffix
	cfg.PromptFlag = p.PromptFlag
	cfg.Nudge = p.Nudge
	if p.launch == startupPromptResumeLaunch {
		delete(cfg.Env, startupPromptDeliveredEnv)
	}
	if p.MarkDelivered {
		if cfg.Env == nil {
			cfg.Env = map[string]string{}
		}
		cfg.Env[startupPromptDeliveredEnv] = "1"
	}
}

// startupPromptSource projects the reconciler's resolved template onto the
// inputs of the shared delivery decision.
func (tp TemplateParams) startupPromptSource() startupPromptSource {
	return startupPromptSource{
		TemplateName:       tp.TemplateName,
		SessionName:        tp.SessionName,
		InstanceName:       tp.InstanceName,
		Prompt:             tp.Prompt,
		Beacon:             tp.Beacon,
		IsACP:              tp.IsACP,
		HookEnabled:        tp.HookEnabled,
		ClientProviderChat: tp.ClientProviderChat,
		ResolvedProvider:   tp.ResolvedProvider,
		HintNudge:          tp.Hints.Nudge,
		RuntimeName:        tp.EffectiveSessionProvider,
		SessionRuntime:     tp.CitySessionProvider,
		CityRuntimes:       tp.CityRuntimes,
	}
}
