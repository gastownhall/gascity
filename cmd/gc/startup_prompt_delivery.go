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
	TemplateName     string
	SessionName      string
	InstanceName     string
	Prompt           string
	IsACP            bool
	ResolvedProvider *config.ResolvedProvider
	HintNudge        string
	RuntimeName      string
	CityRuntimes     map[string]config.DiscoveredRuntime
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
	delivery, err := promptDelivery(src.Prompt, src.IsACP, src.ResolvedProvider, src.HintNudge, src.RuntimeName, src.CityRuntimes)
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
		plan := resumeStartupPromptPlan(src.Prompt, src.HintNudge)
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
// the prompt rides the nudge and the marker is set whenever there is a prompt.
func resumeStartupPromptPlan(prompt, hintNudge string) startupPromptPlan {
	return startupPromptPlan{
		Nudge:         restartPromptNudge(prompt, hintNudge),
		MarkDelivered: strings.TrimSpace(prompt) != "",
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
		TemplateName:     tp.TemplateName,
		SessionName:      tp.SessionName,
		InstanceName:     tp.InstanceName,
		Prompt:           tp.Prompt,
		IsACP:            tp.IsACP,
		ResolvedProvider: tp.ResolvedProvider,
		HintNudge:        tp.Hints.Nudge,
		RuntimeName:      tp.EffectiveSessionProvider,
		CityRuntimes:     tp.CityRuntimes,
	}
}
