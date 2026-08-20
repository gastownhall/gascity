package config

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/template"
)

func contextAdvisoryPtr[T any](v T) *T { return &v }

// ContextAdvisoryTier is one threshold and message in a context-pressure advisory.
type ContextAdvisoryTier struct {
	Threshold *int    `toml:"threshold,omitempty"`
	Message   *string `toml:"message,omitempty"`
	Enabled   *bool   `toml:"enabled,omitempty"`
}

// ContextAdvisory configures context-pressure guidance. Fields are pointers so
// agent settings can inherit individual global settings.
type ContextAdvisory struct {
	Enabled      *bool                 `toml:"enabled,omitempty"`
	WindowTokens *int                  `toml:"window_tokens,omitempty"`
	Tiers        []ContextAdvisoryTier `toml:"tiers,omitempty"`
}

// ResolvedContextAdvisoryTier is an enabled, complete advisory tier.
type ResolvedContextAdvisoryTier struct {
	Threshold int
	Message   string
}

// ContextAdvisoryPolicy is the effective context-pressure advisory.
type ContextAdvisoryPolicy struct {
	Enabled      bool
	WindowTokens int
	Tiers        []ResolvedContextAdvisoryTier
}

func cloneContextAdvisory(value *ContextAdvisory) *ContextAdvisory {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Enabled = copyBoolPtr(value.Enabled)
	copyValue.WindowTokens = copyIntPtr(value.WindowTokens)
	copyValue.Tiers = make([]ContextAdvisoryTier, len(value.Tiers))
	for i, tier := range value.Tiers {
		copyValue.Tiers[i] = ContextAdvisoryTier{
			Threshold: copyIntPtr(tier.Threshold),
			Message:   copyStringPtr(tier.Message),
			Enabled:   copyBoolPtr(tier.Enabled),
		}
	}
	return &copyValue
}

func mergeContextAdvisory(dst **ContextAdvisory, src *ContextAdvisory) {
	if src == nil {
		return
	}
	if *dst == nil {
		*dst = cloneContextAdvisory(src)
		return
	}
	if src.Enabled != nil {
		(*dst).Enabled = copyBoolPtr(src.Enabled)
	}
	if src.WindowTokens != nil {
		(*dst).WindowTokens = copyIntPtr(src.WindowTokens)
	}
	if src.Tiers != nil {
		(*dst).Tiers = cloneContextAdvisory(src).Tiers
	}
}

const (
	builtinContextAdvisoryMessage = "Context usage: {{.UsedK}}/{{.WindowK}} (~{{printf \"%.0f\" .Pct}}%). Approaching the recycle zone. Steer toward a clean seam: finish in-flight work, don't open new long-horizon tasks, and keep durable notes/work-items current so a handoff is cheap. Plan to hand off and reset before this climbs into the urgent band — a fresh session from durable notes outperforms riding lossy compaction."
	builtinContextUrgentMessage   = "Context usage: {{.UsedK}}/{{.WindowK}} (~{{printf \"%.0f\" .Pct}}%) — HIGH. Recycle this session now: reach a clean seam, run your handoff (durable notes + work-item updates + memory), then `gc session reset` yourself to resume fresh from that durable state. Repeated compaction degrades awareness — a clean reset beats running to compaction. Do this once you are at a seam; do NOT abandon work mid-step. (If an operator has told you to stay up, honor that and just hold at a clean seam instead of resetting.)"
)

// DefaultContextAdvisory returns the legacy advisory policy for zero config.
func DefaultContextAdvisory() ContextAdvisory {
	return ContextAdvisory{
		Enabled: contextAdvisoryPtr(true),
		Tiers: []ContextAdvisoryTier{
			{Threshold: contextAdvisoryPtr(60), Message: contextAdvisoryPtr(builtinContextAdvisoryMessage), Enabled: contextAdvisoryPtr(true)},
			{Threshold: contextAdvisoryPtr(80), Message: contextAdvisoryPtr(builtinContextUrgentMessage), Enabled: contextAdvisoryPtr(true)},
		},
	}
}

// ContextAdvisoryView is available to advisory message templates.
type ContextAdvisoryView struct {
	Tokens    int
	Window    int
	UsedK     string
	WindowK   string
	Pct       float64
	Threshold int
}

// ResolveContextAdvisory merges scopes from lowest to highest precedence.
func ResolveContextAdvisory(scopes ...*ContextAdvisory) ContextAdvisoryPolicy {
	var policy ContextAdvisoryPolicy
	var tiers []ContextAdvisoryTier
	for _, scope := range scopes {
		if scope == nil {
			continue
		}
		if scope.Enabled != nil {
			policy.Enabled = *scope.Enabled
		}
		if scope.WindowTokens != nil {
			policy.WindowTokens = *scope.WindowTokens
		}
		if scope.Tiers != nil {
			tiers = scope.Tiers
		}
	}
	for _, tier := range tiers {
		if tier.Threshold == nil || tier.Message == nil || (tier.Enabled != nil && !*tier.Enabled) {
			continue
		}
		policy.Tiers = append(policy.Tiers, ResolvedContextAdvisoryTier{Threshold: *tier.Threshold, Message: *tier.Message})
	}
	sort.SliceStable(policy.Tiers, func(i, j int) bool { return policy.Tiers[i].Threshold < policy.Tiers[j].Threshold })
	return policy
}

// Validate checks one authored advisory scope.
func (c *ContextAdvisory) Validate() error {
	if c == nil {
		return nil
	}
	if c.WindowTokens != nil && *c.WindowTokens < 0 {
		return fmt.Errorf("context_advisory window_tokens must be non-negative")
	}
	var previous *int
	for i, tier := range c.Tiers {
		if tier.Threshold == nil {
			return fmt.Errorf("context_advisory tier %d: threshold is required", i)
		}
		if *tier.Threshold < 0 || *tier.Threshold > 100 {
			return fmt.Errorf("context_advisory tier %d: threshold %d out of range (0-100)", i, *tier.Threshold)
		}
		if previous != nil && *tier.Threshold <= *previous {
			return fmt.Errorf("context_advisory tier %d: thresholds must ascend", i)
		}
		previous = tier.Threshold
		if tier.Message == nil || strings.TrimSpace(*tier.Message) == "" {
			return fmt.Errorf("context_advisory tier %d: message is required", i)
		}
		tmpl, err := template.New("context-advisory").Option("missingkey=error").Parse(*tier.Message)
		if err != nil {
			return fmt.Errorf("context_advisory tier %d: parsing message template: %w", i, err)
		}
		if err := tmpl.Execute(io.Discard, ContextAdvisoryView{}); err != nil {
			return fmt.Errorf("context_advisory tier %d: rendering message template: %w", i, err)
		}
	}
	return nil
}

func validateContextAdvisories(cfg *City) error {
	if cfg == nil {
		return nil
	}
	if err := cfg.AgentDefaults.ContextAdvisory.Validate(); err != nil {
		return fmt.Errorf("agent_defaults: %w", err)
	}
	for _, agent := range cfg.Agents {
		if err := agent.ContextAdvisory.Validate(); err != nil {
			return fmt.Errorf("agent %q: %w", agent.Name, err)
		}
	}
	return nil
}

// SelectTier returns the highest configured threshold reached by pct.
func (p ContextAdvisoryPolicy) SelectTier(pct float64) (ResolvedContextAdvisoryTier, bool) {
	if !p.Enabled {
		return ResolvedContextAdvisoryTier{}, false
	}
	var selected ResolvedContextAdvisoryTier
	found := false
	for i, tier := range p.Tiers {
		if pct < float64(tier.Threshold) || (i > 0 && pct == float64(tier.Threshold)) {
			break
		}
		selected, found = tier, true
	}
	return selected, found
}

// RenderTier renders a tier's operator-authored message without blocking hooks
// when an invalid template escapes configuration validation.
func RenderTier(tier ResolvedContextAdvisoryTier, view ContextAdvisoryView) string {
	tmpl, err := template.New("context-advisory").Parse(tier.Message)
	if err != nil {
		return tier.Message
	}
	var output strings.Builder
	if err := tmpl.Execute(&output, view); err != nil {
		return tier.Message
	}
	return output.String()
}
