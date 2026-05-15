package doctor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// ProviderParityCheck flags providers used by configured agents whose
// capability fields are likely to silently no-op at runtime. The signal
// is structural — it inspects each provider after applying the same
// config resolution semantics used for runtime sessions, and warns when:
//
//   - ResumeFlag and ResumeCommand are both empty: every session restart
//     will silently drop the session-id and start a fresh process
//     (resolveResumeCommand short-circuits, gap 1 of #672).
//
// SupportsHooks=false is intentionally NOT flagged — many providers
// genuinely lack a hook surface and Gas Town has an alternative drain
// path (NeedsNudgePoller) for them. Flagging it would be noise.
//
// Each warning names the provider, what is missing, and the consequence.
type ProviderParityCheck struct {
	cfg *config.City
}

// NewProviderParityCheck creates a check that flags provider capability
// gaps for every provider referenced by a configured agent.
func NewProviderParityCheck(cfg *config.City) *ProviderParityCheck {
	return &ProviderParityCheck{cfg: cfg}
}

// Name returns the check identifier.
func (c *ProviderParityCheck) Name() string { return "provider-parity" }

// Run inspects each provider referenced by at least one agent and
// reports capability gaps as warnings.
func (c *ProviderParityCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no config; nothing to check"
		return r
	}

	providers := providersInUse(c.cfg)
	if len(providers) == 0 {
		r.Status = StatusOK
		r.Message = "no providers referenced by configured agents"
		return r
	}

	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)

	var details []string
	for _, name := range names {
		provider := providers[name]
		if !provider.hasResume {
			details = append(details, fmt.Sprintf(
				"provider %q has no ResumeFlag or ResumeCommand: session restarts will silently drop the session-id and start a fresh process",
				name))
		}
	}

	if len(details) == 0 {
		r.Status = StatusOK
		r.Message = "provider capabilities look complete"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d provider capability gap(s)", len(details))
	r.Details = details
	r.FixHint = "populate resume_flag (or resume_command) in the provider spec; see internal/config/provider.go for the built-in presets and gastownhall/gascity#672 (non-Claude provider parity)"
	return r
}

// CanFix returns false — provider capability fields are config-managed.
func (c *ProviderParityCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *ProviderParityCheck) Fix(_ *CheckContext) error { return nil }

type providerResumeState struct {
	hasResume bool
}

// providersInUse returns resume capability state for every provider used by
// at least one configured agent. It delegates provider inheritance and
// agent-level overrides to config.ResolveProvider, using a PATH lookup stub
// because provider-parity checks config semantics rather than host binaries.
// Agents that pin StartCommand are skipped because they bypass ProviderSpec.
func providersInUse(cfg *config.City) map[string]providerResumeState {
	out := map[string]providerResumeState{}

	addResolved := func(agent config.Agent) {
		resolved, err := config.ResolveProvider(&agent, &cfg.Workspace, cfg.Providers, providerParityLookPath)
		if err != nil {
			return
		}
		name := strings.TrimSpace(resolved.Name)
		if name == "" {
			return
		}
		hasResume := strings.TrimSpace(resolved.ResumeFlag) != "" || strings.TrimSpace(resolved.ResumeCommand) != ""
		current, seen := out[name]
		if !seen || (current.hasResume && !hasResume) {
			out[name] = providerResumeState{hasResume: hasResume}
		}
	}

	checkedAgentProvider := false
	for _, a := range cfg.Agents {
		if a.StartCommand != "" {
			continue
		}
		if strings.TrimSpace(a.Provider) == "" && strings.TrimSpace(cfg.Workspace.Provider) == "" {
			continue
		}
		checkedAgentProvider = true
		addResolved(a)
	}
	if !checkedAgentProvider && len(cfg.Agents) == 0 && strings.TrimSpace(cfg.Workspace.Provider) != "" {
		addResolved(config.Agent{})
	}
	return out
}

func providerParityLookPath(command string) (string, error) {
	return command, nil
}
