package main

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// promptDeliveryBudgetDoctorCheck verifies every agent's rendered prompt
// clears promptDelivery's size budget for its resolved provider/runtime
// (ga-q8wgom.1.1/.2), so a fleet-wide oversized-prompt hard-fail is caught
// by `gc doctor` instead of surfacing only at session start for whichever
// agent happens to prime first. It reuses promptDelivery directly rather
// than re-deriving thresholds, byte-accounting, or delivery-mode selection.
type promptDeliveryBudgetDoctorCheck struct {
	cityPath string
	cfg      *config.City
	lookPath config.LookPathFunc
}

func newPromptDeliveryBudgetDoctorCheck(cityPath string, cfg *config.City, lookPath config.LookPathFunc) *promptDeliveryBudgetDoctorCheck {
	return &promptDeliveryBudgetDoctorCheck{cityPath: cityPath, cfg: cfg, lookPath: lookPath}
}

// Name implements doctor.Check.
func (*promptDeliveryBudgetDoctorCheck) Name() string { return "prompt-delivery-budget" }

// CanFix implements doctor.Check. Fixing means editing prompt templates or
// fragments, which belongs to the user, not an automated fix.
func (*promptDeliveryBudgetDoctorCheck) CanFix() bool { return false }

// WarmupEligible implements doctor.Check. Rendering every agent's prompt
// depends on full city/rig configuration (pack dirs, fragments, rig
// resolution, provider resolution), so this check is ineligible for
// `gc start` warm-up, which runs before that configuration is available.
func (*promptDeliveryBudgetDoctorCheck) WarmupEligible() bool { return false }

// Fix implements doctor.Check.
func (*promptDeliveryBudgetDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run implements doctor.Check. It surveys every non-suspended, template-bearing
// agent, mirroring the same resolve-provider -> render-prompt -> promptDelivery
// pipeline `gc prime --strict` runs per-agent (reportPromptDeliveryBudget in
// cmd_prime.go), but aggregates every agent to a single worst-case result
// (Error > Warning > OK) instead of stopping at the first agent primed.
func (c *promptDeliveryBudgetDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	if c.cfg == nil {
		return okCheck("prompt-delivery-budget", "no city config")
	}

	cityName := c.cfg.Workspace.Name
	topo := cityQueryTopology(c.cityPath, c.cfg)
	suspendState := loadSuspensionStateBestEffort(c.cityPath)

	agents := make([]config.Agent, len(c.cfg.Agents))
	copy(agents, c.cfg.Agents)
	sort.Slice(agents, func(i, j int) bool {
		qi, qj := agents[i].QualifiedName(), agents[j].QualifiedName()
		if qi != qj {
			return qi < qj
		}
		return agents[i].Name < agents[j].Name
	})

	var details []string
	worst := doctor.StatusOK
	note := func(status doctor.CheckStatus, msg string) {
		details = append(details, msg)
		if worst < status {
			worst = status
		}
	}

	for i := range agents {
		a := agents[i]
		if isAgentEffectivelySuspendedWith(c.cfg, c.cityPath, &a, suspendState) {
			continue
		}
		if a.PromptTemplate == "" {
			continue
		}
		// Launch never delivers a startup prompt to an agent whose prompt is
		// suppressed, so there is nothing to measure against the budget.
		if suppressStartupPromptForAgent(&a) {
			continue
		}

		// ResolveProvider's error is intentionally discarded: cmd_prime.go's
		// own per-agent loop never branches on it either, and every consumer
		// of `resolved` below (ResolveSessionCreateTransport, promptDelivery)
		// is nil-safe and falls back to default delivery semantics. Flagging
		// a provider that fails to resolve is provider-catalog's job (an
		// explicit-but-dangling [providers] reference) — duplicating that
		// judgment here would surface the same misconfiguration under two
		// check names, and would wrongly hard-fail agents that simply have
		// no provider configured yet, which provider-parity already treats
		// as out of scope rather than an error.
		resolved, _ := config.ResolveProvider(&a, &c.cfg.Workspace, c.cfg.Providers, c.lookPath)

		prompt, renderFailed := renderDoctorAgentPrompt(c.cityPath, cityName, c.cfg, topo, &a)
		// Measure the startup prompt launch would send: the beacon is
		// prepended exactly as resolveTemplate does. Its timestamp is
		// fixed-width, so the zero time yields the same byte count. The
		// render above uses the resolved workdir; only the assigned-skills
		// appendix launch may append (resolveTemplate Step 9b) is not
		// counted, since its gating depends on runtime materialization paths
		// that are only known at launch.
		if prompt != "" {
			beacon := runtime.FormatBeaconAt(cityName, a.QualifiedName(), false, time.Time{})
			prompt = composeStartupPrompt(beacon, prompt, false)
		}

		effProvider := effectiveSessionProvider(a.Session, c.cfg.Session.Provider)
		sessionTransport := config.ResolveSessionCreateTransport(a.Session, resolved)
		isACP := sessionTransport == config.SessionTransportACP

		delivery, dErr := promptDelivery(prompt, isACP, resolved, "", effProvider, c.cfg.Runtimes)
		// Report the configured mode in the same vocabulary as
		// reportPromptDeliveryBudget (gc prime --strict).
		configuredMode := delivery.ConfiguredMode
		switch {
		case isACP:
			configuredMode = "acp"
		case configuredMode == "":
			configuredMode = "arg"
		}
		switch {
		case dErr != nil:
			note(doctor.StatusError, fmt.Sprintf("%s: hard-fail: prompt exceeds the delivery budget for runtime %q and has no supported fallback (configured_mode=%s effective_mode=%s): %v", a.QualifiedName(), effProvider, configuredMode, delivery.EffectiveMode, dErr))
		case delivery.OversizedFallback:
			note(doctor.StatusWarning, fmt.Sprintf("%s: nudge-fallback: prompt exceeds the delivery budget for runtime %q; falls back to a post-start nudge (configured_mode=%s effective_mode=%s raw_bytes=%d raw_limit=%d argv_bytes=%d argv_limit=%d)", a.QualifiedName(), effProvider, configuredMode, delivery.EffectiveMode, delivery.RawBytes, maxPromptSuffixRawBytes, delivery.ArgvBytes, maxPromptSuffixQuotedBytes))
		case renderFailed:
			note(doctor.StatusWarning, fmt.Sprintf("%s: render warning: prompt template %q failed to render and fell back to raw text", a.QualifiedName(), a.PromptTemplate))
		}
	}

	switch worst {
	case doctor.StatusError:
		return errorCheck("prompt-delivery-budget",
			"one or more agent prompts hard-fail the delivery budget",
			"shrink the prompt template/fragments, or move the agent to a runtime with nudge-fallback support",
			details)
	case doctor.StatusWarning:
		return warnCheck("prompt-delivery-budget",
			"one or more agent prompts are oversized or failed to render",
			"shrink the prompt template/fragments to avoid relying on nudge fallback, or fix the reported template",
			details)
	default:
		return okCheck("prompt-delivery-budget", "all agent prompts clear their delivery budget")
	}
}

// renderDoctorAgentPrompt renders a's prompt template the way launch does:
// the agent's prompt context with the workdir launch would use, its
// effective fragments, and the pack dirs of its rig. renderFailed reports
// that the template failed to render and the result fell back to raw text.
// It creates no directory: the unvalidated workdir resolver is a pure path
// computation.
func renderDoctorAgentPrompt(cityPath, cityName string, cfg *config.City, topo config.QueryTopology, a *config.Agent) (prompt string, renderFailed bool) {
	ctx := buildAgentPromptContext(cityPath, cityName, a, cfg.Rigs, topo, io.Discard)
	if wd, err := resolveConfiguredWorkDirPathUnvalidated(cityPath, cityName, a.QualifiedName(), a, cfg.Rigs); err == nil {
		ctx.WorkDir = wd
		ctx.DefaultBranch = defaultBranchForRig(ctx.RigName, cfg.Rigs, ctx.WorkDir)
	}
	ctx.ProviderKey, ctx.ProviderDisplayName = providerInfoForAgent(a, &cfg.Workspace, cfg.Providers)
	ctx.InstructionsFile = instructionsFileForAgent(a, &cfg.Workspace, cfg.Providers)

	fragments := effectivePromptFragments(
		cfg.Workspace.GlobalFragments,
		a.InjectFragments,
		a.AppendFragments,
		a.InheritedAppendFragments,
		cfg.AgentDefaults.AppendFragments,
	)
	packDirs := cfg.PackDirsForRig(ctx.RigName)

	var renderErrs bytes.Buffer
	prompt = renderPrompt(fsys.OSFS{}, cityPath, cityName, a.PromptTemplate, ctx, cfg.Workspace.SessionTemplate, &renderErrs, packDirs, fragments, nil)
	return prompt, renderErrs.Len() > 0
}
