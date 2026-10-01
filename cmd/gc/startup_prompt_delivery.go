package main

import (
	"errors"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// startupPromptLaunch is the kind of launch a startup prompt is delivered to.
type startupPromptLaunch int

const (
	// startupPromptFreshLaunch begins a conversation.
	startupPromptFreshLaunch startupPromptLaunch = iota
	// startupPromptResumeLaunch continues a conversation that already has history.
	startupPromptResumeLaunch
)

// startupPromptSource is what a launch builder knows about the prompt it must
// deliver.
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

// startupPromptPlan is how one launch carries its startup prompt.
type startupPromptPlan struct {
	PromptSuffix  string
	PromptFlag    string
	Nudge         string
	MarkDelivered bool
	Delivery      promptDeliveryResult
}

// planStartupPrompt decides how a launch of the given kind carries src's prompt.
func planStartupPrompt(_ startupPromptSource, _ startupPromptLaunch) (startupPromptPlan, error) {
	return startupPromptPlan{}, errors.New("planStartupPrompt: not implemented")
}

// applyTo writes the plan's payload and marker into cfg.
func (startupPromptPlan) applyTo(_ *runtime.Config) {}
