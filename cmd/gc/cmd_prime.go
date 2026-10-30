package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/spf13/cobra"
)

// defaultPrimePrompt is the run-once worker prompt output when no agent name
// matches a configured agent. This is for users who start Claude Code manually
// inside a rig without being a managed agent.
const defaultPrimePrompt = `# Gas City Agent

You are an agent in a Gas City workspace. Claim available work and execute it.

## Your tools

- ` + "`gc hook --claim --json`" + ` — find and atomically claim one work item
- ` + "`bd show <id>`" + ` — see details of a work item
- ` + "`bd close <id>`" + ` — mark work as done

## How to work

1. Claim work: ` + "`gc hook --claim --json`" + `
2. Read the claimed bead and execute the work described in its title
3. When done, close it: ` + "`bd close <id>`" + `
4. Check for more work. Repeat until the queue is empty.
`

const primeHookReadTimeout = 500 * time.Millisecond

var primeStdin = func() *os.File { return os.Stdin }

type primeHookInput struct {
	Source         string `json:"source"`
	SessionID      string `json:"session_id"`
	ConversationID string `json:"conversation_id"`
	HookEventName  string `json:"hook_event_name"`
}

type primeHookContext struct {
	Source            string
	HookEventName     string
	ProviderSessionID string
}

// newPrimeCmd creates the "gc prime [agent-name]" command.
func newPrimeCmd(stdout, stderr io.Writer) *cobra.Command {
	var hookMode bool
	var hookFormat string
	var strictMode bool
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "prime [agent-name]",
		Short: "Output the behavioral prompt for an agent",
		Long: `Outputs the behavioral prompt for an agent.

Use it to prime any CLI coding agent with city-aware instructions:
  claude "$(gc prime mayor)"
  codex --prompt "$(gc prime worker)"

Runtime hook profiles may call ` + "`gc prime --hook`" + `.
When agent-name is omitted, ` + "`GC_ALIAS`" + ` is used (falling back to ` + "`GC_AGENT`" + `).

If agent-name matches a configured agent with a prompt_template,
that template is output. Otherwise outputs a default worker prompt.

Pass --strict to fail on debugging mistakes instead of falling back to
the default prompt. Strict errors on:

  - no city config found
  - city config fails to load
  - no agent name given (from args, GC_ALIAS, or GC_AGENT)
  - agent name not in city config (typo detection — the main use case)
  - agent's prompt_template points at a file that cannot be read
  - agent's prompt_template fails to parse or execute

Without --strict, a prompt_template that cannot be read is reported on
stderr and the default prompt is output; one that fails to parse or
execute is reported on stderr and its raw unrendered text is output.

Strict does NOT error on agents whose config intentionally lacks a
prompt_template (a supported minimal config), on templates that render
to empty output from valid conditional logic, or on suspended states
(city or agent) — those are legitimate quiet states, not mistakes.

Fragments appended to the prompt (global_fragments, inject_fragments,
append_fragments) do not fail --strict: one that is missing or fails to
render is reported on stderr and left out of the prompt. gc lint
reports a pack's missing or failing inject_fragments and
append_fragments.`,
		Args: cobra.MaximumNArgs(1),
	}
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		if jsonOut {
			var buf strings.Builder
			// Preview only: a strings.Builder write never fails, so a
			// consuming run here would archive durable handoff mail before
			// the real stdout write — and even on success would eat the
			// continuation the next SessionStart hook must deliver.
			code, budget := doPrimeWithHookFormatOpts(args, &buf, stderr, hookMode, hookFormat, strictMode, false)
			if code != 0 {
				return errExit
			}
			agentName, _ := primeInvocationAgentName(args)
			return writeCLIJSONLineOrErr(stdout, stderr, "gc prime", primeJSONResult{
				SchemaVersion: "1",
				Agent:         agentName,
				Hook:          hookMode,
				HookFormat:    hookFormat,
				Content:       buf.String(),
				Bytes:         buf.Len(),
				PromptBudget:  budget,
			})
		}
		if doPrimeWithHookFormat(args, stdout, stderr, hookMode, hookFormat, strictMode) != 0 {
			return errExit
		}
		return nil
	}
	cmd.Flags().BoolVar(&hookMode, "hook", false, "compatibility mode for runtime hook invocations")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	cmd.Flags().BoolVar(&strictMode, "strict", false, "fail on missing city, missing or unknown agent, or a prompt_template that cannot be read or rendered instead of falling back")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON summary")
	return cmd
}

type primeJSONResult struct {
	SchemaVersion string            `json:"schema_version"`
	Agent         string            `json:"agent,omitempty"`
	Hook          bool              `json:"hook"`
	HookFormat    string            `json:"hook_format,omitempty"`
	Content       string            `json:"content"`
	Bytes         int               `json:"bytes"`
	PromptBudget  *promptBudgetJSON `json:"prompt_budget,omitempty"`
}

// promptBudgetJSON is the --strict --json report of the promptDelivery
// budget decision (ga-q8wgom.1.2). Populated by doPrimeWithHookFormatOpts.
type promptBudgetJSON struct {
	RawBytes          int    `json:"raw_bytes"`
	RawLimit          int    `json:"raw_limit"`
	ArgvBytes         int    `json:"argv_bytes"`
	ArgvLimit         int    `json:"argv_limit"`
	ConfiguredMode    string `json:"configured_mode"`
	EffectiveMode     string `json:"effective_mode"`
	Runtime           string `json:"runtime"`
	OversizedFallback bool   `json:"oversized_fallback"`
	HardFail          bool   `json:"hard_fail"`
}

// reportPromptDeliveryBudget computes and reports (on stderr) the
// promptDelivery budget decision for a --strict gc prime invocation
// (ga-q8wgom.1.2). It reuses promptDelivery/promptDeliverySupportFor
// (ga-q8wgom.1.1) rather than re-deriving delivery mode independently, so
// the strict diagnostic can never disagree with what a real launch does.
//
// The returned error is non-nil exactly when promptDelivery hard-fails (an
// oversized prompt on a runtime with no confirmed post-start delivery
// path) — the caller must treat that as a strict failure and must not
// write the rendered prompt to stdout.
func reportPromptDeliveryBudget(prompt string, a *config.Agent, cfg *config.City, resolved *config.ResolvedProvider, stderr io.Writer) (*promptBudgetJSON, error) {
	effProvider := effectiveSessionProvider(a.Session, cfg.Session.Provider)
	sessionTransport := config.ResolveSessionCreateTransport(a.Session, resolved)
	isACP := sessionTransport == config.SessionTransportACP

	delivery, dErr := promptDelivery(prompt, isACP, resolved, "", effProvider, cfg.Runtimes)

	configuredMode := "arg"
	switch {
	case isACP:
		configuredMode = "acp"
	case resolved != nil && resolved.PromptMode != "":
		configuredMode = resolved.PromptMode
	}

	effectiveMode := "argv"
	switch {
	case dErr != nil:
		effectiveMode = "hard-fail"
	case delivery.OversizedFallback:
		effectiveMode = "nudge-fallback"
	case isACP || configuredMode == "none":
		effectiveMode = "nudge"
	case delivery.PromptFlag != "":
		effectiveMode = "flag"
	}

	quoted := shellquote.Quote(prompt)
	budget := &promptBudgetJSON{
		RawBytes:          len(prompt),
		RawLimit:          maxPromptSuffixRawBytes,
		ArgvBytes:         len(quoted),
		ArgvLimit:         maxPromptSuffixQuotedBytes,
		ConfiguredMode:    configuredMode,
		EffectiveMode:     effectiveMode,
		Runtime:           effProvider,
		OversizedFallback: delivery.OversizedFallback,
		HardFail:          dErr != nil,
	}

	fmt.Fprintf(stderr, "gc prime: prompt budget: raw_bytes=%d raw_limit=%d argv_bytes=%d argv_limit=%d configured_mode=%s effective_mode=%s runtime=%s oversized_fallback=%t hard_fail=%t\n", //nolint:errcheck // diagnostics are best effort.
		budget.RawBytes, budget.RawLimit, budget.ArgvBytes, budget.ArgvLimit, budget.ConfiguredMode, budget.EffectiveMode, budget.Runtime, budget.OversizedFallback, budget.HardFail)
	if dErr != nil {
		fmt.Fprintf(stderr, "gc prime: %v\n", dErr) //nolint:errcheck // diagnostics are best effort.
		return budget, dErr
	}
	return budget, nil
}

// doPrime exists as the public non-strict entry point so callers don't
// need to know about the strict flag; its return type stays int because
// the caller shape matches other cmd/gc entry points.
func doPrime(args []string, stdout, stderr io.Writer) int { //nolint:unparam // strictMode=false means always returns 0
	return doPrimeWithMode(args, stdout, stderr, false, false)
}

// doPrimeWithMode's strict-mode contract: only states that would indicate
// a user mistake (missing city config, no agent name, unknown agent name,
// a prompt_template that cannot be read, parsed, or executed) error out. Supported minimal configs
// (agent with no prompt_template at all, or a template that legitimately
// renders to empty output via conditional logic) and intentional quiet
// states (suspended city/agent) remain silent even under --strict —
// strict is a debugging aid, not a stricter mode for the whole command.
//
// Hook-mode side effects under --strict are deferred until we know the
// invocation is not a strict failure, so a failing --strict cannot update
// provider resume metadata for an agent that doesn't exist. Suspended paths
// still run side effects because suspension is a legitimate quiet state, not a
// failure.
func doPrimeWithMode(args []string, stdout, stderr io.Writer, hookMode, strictMode bool) int {
	return doPrimeWithHookFormat(args, stdout, stderr, hookMode, "", strictMode)
}

func primeInvocationAgentName(args []string) (string, bool) {
	agentName := os.Getenv("GC_ALIAS")
	if agentName == "" {
		agentName = os.Getenv("GC_AGENT")
	}
	sessionTemplateContext := false
	if len(args) == 0 {
		template := strings.TrimSpace(os.Getenv("GC_TEMPLATE"))
		hasSessionContext := strings.TrimSpace(os.Getenv("GC_SESSION_NAME")) != "" ||
			strings.TrimSpace(os.Getenv("GC_SESSION_ID")) != ""
		if template != "" && hasSessionContext {
			agentName = template
			sessionTemplateContext = true
		}
	}
	if len(args) > 0 {
		agentName = args[0]
	}
	return strings.TrimSpace(agentName), sessionTemplateContext
}

func doPrimeWithHookFormat(args []string, stdout, stderr io.Writer, hookMode bool, hookFormat string, strictMode bool) int {
	code, _ := doPrimeWithHookFormatOpts(args, stdout, stderr, hookMode, hookFormat, strictMode, true)
	return code
}

// doPrimeWithHookFormatOpts is the full entry point. consumeHandoff=false makes
// the invocation non-destructive: durable auto-handoff mail is still rendered
// into the output, but is not archived. Preview callers (--json) pass false so
// that a diagnostic run cannot eat the continuation the real SessionStart hook
// is supposed to deliver.
//
// The second return value is the --strict prompt-delivery budget decision
// (ga-q8wgom.1.2), non-nil only when strictMode is true and an agent with a
// prompt_template was resolved.
func doPrimeWithHookFormatOpts(args []string, stdout, stderr io.Writer, hookMode bool, hookFormat string, strictMode, consumeHandoff bool) (int, *promptBudgetJSON) {
	// One composed city-config load for the whole invocation: the session
	// proof, the provider-key write, the mail and step-reminder injections and
	// every store open each loaded it again (see invocationConfigMemo).
	defer beginInvocationConfigMemo()()
	agentName, sessionTemplateContext := primeInvocationAgentName(args)
	var hookContext primeHookContext
	suppressHookPrompt := false
	if hookMode {
		hookContext = readPrimeHookContext()
		suppressHookPrompt = managedSessionHookPromptAlreadyDelivered(hookContext)
		// A hook invocation carrying no gc identity at all did not come from a
		// session gc started, so there is nothing to prime and nothing to
		// persist. gc stages each provider's hook overlay into the session work
		// directory — commonly a city or rig root — and those overlays are
		// ordinary directory-scoped provider config, so a human who opens the
		// same provider in that directory loads them too. Emitting the worker
		// persona there turns the human's own session into a queue worker that
		// ignores what they typed, then fails to claim for want of an agent
		// identity.
		//
		// The existing live-session gate below covers only SessionStart, so
		// providers whose hooks pass no event name bypassed it entirely and
		// whether a human got hijacked depended on which provider they opened.
		// This gate is deliberately weaker than a live-session lookup: manual
		// aliases, template fallbacks and strict-mode validation are all real
		// hook flows without a live session bead, and they still carry identity.
		//
		// Explicit `gc prime` (no --hook) is untouched: a human asking for the
		// prompt still gets it.
		if len(args) == 0 && !hookHasManagedIdentity() {
			writePrimePromptWithFormat(stdout, "", "", "", hookMode, hookFormat, false, "", nil)
			return 0, nil
		}
	}
	// In non-strict mode, hook side effects fire eagerly (existing behavior).
	// In strict mode, we defer them until after strict checks pass so that a
	// failing --strict invocation does not update provider resume metadata for
	// failed agent resolution or template validation.
	//
	// hookSideEffectCityPath is the city this invocation resolved, once it has;
	// the provider-key write then reuses it instead of resolving the ambient
	// city again (each resolveCity loads the full city config).
	var hookSideEffectCityPath string
	runHookSideEffects := func() {
		if !hookMode {
			return
		}
		persistPrimeHookProviderSessionKeyAtCity(hookContext.ProviderSessionID, hookSideEffectCityPath, stderr)
	}
	if !strictMode && !primeHookSessionStart(hookContext) {
		runHookSideEffects()
	}

	cityPath, err := resolveCity()
	if err != nil {
		if strictMode {
			fmt.Fprintf(stderr, "gc prime: no city config found: %v\n", err) //nolint:errcheck
			return 1, nil
		}
		if hookMode && primeHookSessionStart(hookContext) {
			writePrimePromptWithFormat(stdout, "", "", "", hookMode, hookFormat, false, "", nil)
			return 0, nil
		}
		injection := primeHookContextSuffix("", nil, nil, hookMode, hookContext, stderr, consumeHandoff)
		writePrimePromptWithFormat(stdout, "", "", defaultPrimePrompt, hookMode, hookFormat, suppressHookPrompt, injection.text, injection.afterDelivery)
		return 0, nil
	}
	hookSideEffectCityPath = cityPath
	// The step reminder is the slowest read a hook makes: in process it opens
	// the rig store at GC_RIG_ROOT and scans that rig by gc.routed_to once per
	// identity. Ask the controller for it now, so the request runs while this
	// invocation proves its session, loads its config and starts its poller.
	var stepLookup *wispStepLookup
	if hookMode {
		stepLookup = startWispStepLookup(cityPath, stderr)
		defer stepLookup.stop()
	}
	// Starting a SessionStart hook's poller below reaps the pollers directory
	// first; reap it now instead, alongside the hook's reads
	// (startNudgePollerReap). Other hook shapes reap where they start it.
	reapedEarly := hookMode && primeHookSessionStart(hookContext)
	if reapedEarly {
		defer startNudgePollerReap(cityPath)()
	}
	// A SessionStart hook proves its live managed session against the city
	// store; the SessionStart mail context below reuses that same handle rather
	// than resolving the city and opening its store again.
	var sessionStart *primeSessionStart
	if hookMode && primeHookSessionStart(hookContext) {
		sessionStart = primeHookLiveManagedSession(cityPath, stderr)
		if sessionStart == nil {
			writePrimePromptWithFormat(stdout, "", "", "", hookMode, hookFormat, false, "", nil)
			return 0, nil
		}
	}
	if !strictMode && primeHookSessionStart(hookContext) {
		runHookSideEffects()
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		if strictMode {
			fmt.Fprintf(stderr, "gc prime: loading city config: %v\n", err) //nolint:errcheck
			return 1, nil
		}
		injection := primeHookContextSuffix(cityPath, sessionStart, stepLookup, hookMode, hookContext, stderr, consumeHandoff)
		writePrimePromptWithFormat(stdout, "", "", defaultPrimePrompt, hookMode, hookFormat, suppressHookPrompt, injection.text, injection.afterDelivery)
		return 0, nil
	}
	resolveRigPaths(cityPath, cfg.Rigs)

	// The suspension predicates below take the city this invocation already
	// resolved; their ambient forms would resolve (and load) it again each time.
	// The runtime suspension state is still re-read at each decision point.
	if citySuspendedWithState(cfg, loadSuspensionStateBestEffort(cityPath)) {
		// Suspended is a legitimate quiet state, not a strict failure —
		// keep hook behavior consistent with non-strict (which already
		// ran side effects eagerly above).
		if strictMode {
			runHookSideEffects()
		}
		return 0, nil
	}

	cityName := loadedCityName(cfg, cityPath)
	if hookMode && strings.TrimSpace(agentName) == "" {
		agentName = primeHookAgentFromWorkDir(cfg)
	}

	// Look up agent in config. First try qualified identity resolution
	// (handles "rig/agent" and rig-context matching), then fall back to
	// bare template name lookup (handles "gc prime polecat" for pool agents
	// whose config name is "polecat" regardless of dir). Hook-driven manual
	// sessions may have GC_ALIAS set to a user-facing alias that is not an
	// agent name, so also try GC_TEMPLATE before falling back to the generic
	// run-once prompt.
	var resolvedAgents []config.Agent
	agentCandidates := primeAgentCandidates(agentName, hookMode, cityPath)
	for _, candidate := range agentCandidates {
		a, ok := resolveAgentIdentity(cfg, candidate, currentRigContext(cfg))
		if !ok {
			a, ok = findAgentByName(cfg, candidate)
		}
		if ok {
			resolvedAgents = append(resolvedAgents, a)
		}
	}

	// Strict preconditions: fail now, before any hook side effects or the
	// nudge poller start, so a failing --strict leaves no partial state.
	if strictMode {
		switch {
		case agentName == "":
			fmt.Fprintf(stderr, "gc prime: --strict requires an agent name (from args, GC_ALIAS, or GC_AGENT)\n") //nolint:errcheck
			return 1, nil
		case len(resolvedAgents) == 0:
			fmt.Fprintf(stderr, "gc prime: agent %q not found in city config\n", agentName) //nolint:errcheck
			return 1, nil
		}
		// A prompt_template that cannot be read, parsed, or executed is a
		// strict failure; an empty render from valid template logic is not.
		// Check it here, before hook side effects and before any stdout write.
		if !primeStrictTemplatesRender(cityPath, cityName, cfg, resolvedAgents, hookMode, sessionTemplateContext, stderr) {
			return 1, nil
		}
		// Strict preconditions passed; now it's safe to update provider resume metadata.
		runHookSideEffects()
	}

	for _, a := range resolvedAgents {
		if isAgentEffectivelySuspendedWith(cfg, cityPath, &a, loadSuspensionStateBestEffort(cityPath)) {
			return 0, nil
		}
		resolved, rErr := config.ResolveProvider(&a, &cfg.Workspace, cfg.Providers, exec.LookPath)
		if rErr == nil && hookMode {
			sessionName := os.Getenv("GC_SESSION_NAME")
			if sessionName == "" {
				sessionName = cliSessionName(cityPath, cityName, a.QualifiedName(), cfg.Workspace.SessionTemplate)
			}
			target := withNudgeTargetFenceFromLiveSession(openNudgeBeadStore(cityPath).Store, sessionStart.liveInfo(), nudgeTarget{
				cityPath:          cityPath,
				cityName:          cityName,
				cfg:               cfg,
				agent:             a,
				resolved:          resolved,
				sessionID:         os.Getenv("GC_SESSION_ID"),
				continuationEpoch: os.Getenv("GC_CONTINUATION_EPOCH"),
				sessionName:       sessionName,
			})
			if reapedEarly {
				startNudgePollerUnlessDispatched(target)
			} else {
				maybeStartNudgePoller(target)
			}
		}
		if primeHookClientChat(hookMode, &a) {
			// A client's provider chat gets no worker persona (see
			// isClientProviderChat); the controller's launch channel drops
			// it with the same predicate. Unlike the unmanaged-hook exit
			// above, this one deliberately comes after the nudge poller
			// starts: the chat is a live session gc started, so nudges
			// queued for it still reach it.
			writePrimePromptWithFormat(stdout, "", "", "", hookMode, hookFormat, false, "", nil)
			return 0, nil
		}
		ctx, rendered := primeAgentPrompt(cityPath, cityName, cfg, &a, hookMode || sessionTemplateContext, stderr)
		if rendered.Text == "" {
			// The prompt rendered empty (a legitimate, if unusual, minimal
			// config), the prompt_template could not be read (reported on
			// stderr), or no builtin prompt fits the agent: try the next
			// candidate, then the default fallback.
			continue
		}
		var budget *promptBudgetJSON
		if strictMode && a.PromptTemplate != "" {
			var budgetErr error
			budget, budgetErr = reportPromptDeliveryBudget(rendered.Text, &a, cfg, resolved, stderr)
			if budgetErr != nil {
				return 1, budget
			}
		}
		injection := primeHookContextSuffix(cityPath, sessionStart, stepLookup, hookMode, hookContext, stderr, consumeHandoff)
		writePrimePromptWithFormat(stdout, cityName, ctx.AgentName, rendered.Text, hookMode, hookFormat, suppressHookPrompt, injection.text, injection.afterDelivery)
		return 0, budget
	}

	// Fallback: default run-once prompt. Under strict, this is only reached
	// when no candidate's prompt renders: its prompt_template legitimately
	// renders empty, or it has none and no builtin worker prompt fits it.
	// Both are supported config shapes, so the default prompt is the correct
	// output even under --strict.
	injection := primeHookContextSuffix(cityPath, sessionStart, stepLookup, hookMode, hookContext, stderr, consumeHandoff)
	writePrimePromptWithFormat(stdout, cityName, agentName, defaultPrimePrompt, hookMode, hookFormat, suppressHookPrompt, injection.text, injection.afterDelivery)
	return 0, nil
}

func primeAgentCandidates(agentName string, hookMode bool, cityPath string) []string {
	var candidates []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range candidates {
			if existing == value {
				return
			}
		}
		candidates = append(candidates, value)
	}
	add(agentName)
	if hookMode {
		if gcTemplate := os.Getenv("GC_TEMPLATE"); strings.TrimSpace(gcTemplate) != "" {
			add(gcTemplate)
		} else {
			add(primeHookSessionTemplate(cityPath))
		}
	}
	return candidates
}

func primeHookSessionTemplate(cityPath string) string {
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	if cityPath == "" || sessionID == "" {
		return ""
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return ""
	}
	// Route the session-bead read through the session coordination-class store so
	// a [beads.classes.sessions] relocation reaches this prime hook. The
	// no-refresh config loader is deliberate: this hook fires frequently, and the
	// pack-refresh side effect of loadCityConfig is inappropriate on a hot path
	// (the completion path uses the same no-refresh loader for the same reason).
	// A failed load yields nil cfg, which cliSessionStore treats as identity.
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	sessStore := cliSessionStore(store, cfg, cityPath)
	// The front-door Get rejects a present-but-non-session bead (ErrSessionNotFound)
	// where the prior raw store.Get projected it; here that only tightens a
	// crafted/stale id to the empty-return path below — not a regression.
	info, err := sessionFrontDoor(sessStore).Get(sessionID)
	if err != nil {
		return ""
	}
	if template := strings.TrimSpace(info.Template); template != "" {
		return template
	}
	return strings.TrimSpace(info.CommonName)
}

func primeHookAgentFromWorkDir(cfg *config.City) string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	candidates := primeHookAgentCandidatesFromPath(cwd)
	if cfg != nil {
		rigContext := currentRigContext(cfg)
		for _, candidate := range candidates {
			if a, ok := resolveAgentIdentity(cfg, candidate, rigContext); ok {
				return a.QualifiedName()
			}
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	return candidates[len(candidates)-1]
}

func primeHookAgentCandidatesFromPath(path string) []string {
	clean := filepath.Clean(path)
	parts := strings.Split(clean, string(os.PathSeparator))
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == ".gc" && parts[i+1] == "agents" {
			remaining := parts[i+2:]
			candidates := make([]string, 0, len(remaining))
			for end := len(remaining); end >= 1; end-- {
				candidates = append(candidates, strings.Join(remaining[:end], "/"))
			}
			return candidates
		}
	}
	return nil
}

func prependHookBeacon(cityName, agentName, prompt string) string {
	if cityName == "" || agentName == "" {
		return prompt
	}
	beacon := runtime.FormatBeaconAt(cityName, agentName, false, time.Now())
	if prompt == "" {
		return beacon
	}
	return beacon + "\n\n" + prompt
}

func managedSessionHookPromptAlreadyDelivered(ctx primeHookContext) bool {
	if strings.TrimSpace(os.Getenv(startupPromptDeliveredEnv)) != "1" {
		return false
	}
	if strings.TrimSpace(os.Getenv(managedSessionHookEnv)) != "1" {
		return false
	}
	return strings.TrimSpace(ctx.HookEventName) == "SessionStart"
}

func primeHookSessionStart(ctx primeHookContext) bool {
	return strings.TrimSpace(ctx.HookEventName) == "SessionStart"
}

// hookIdentityEnv are the environment markers that show gc, rather than a
// human, started the process a hook is running inside. gc's session lifecycle
// sets the session and agent variables and its managed hook wrappers set
// GC_MANAGED_SESSION_HOOK. The codex and antigravity overlays do bake
// GC_MANAGED_SESSION_HOOK=1 into their staged `gc prime --hook` command, so a
// human-launched session there passes this gate — those two also set
// GC_HOOK_EVENT_NAME=SessionStart and stay covered by the live-session gate
// below.
var hookIdentityEnv = []string{
	"GC_SESSION_ID",
	"GC_SESSION_NAME",
	"GC_ALIAS",
	"GC_AGENT",
	"GC_TEMPLATE",
	managedSessionHookEnv,
}

// hookHasManagedIdentity reports whether this process carries any gc
// identity. Shared by every hook-injection entry point (prime --hook,
// mail check --inject, nudge drain --inject). It deliberately asks the weaker question than
// primeHookLiveManagedSessionStore: not "is there a live session bead" but "did
// gc start this at all", so real hook flows that legitimately have no session
// bead yet (manual aliases, template fallbacks, strict-mode validation) are not
// mistaken for a human-launched provider.
func hookHasManagedIdentity() bool {
	for _, key := range hookIdentityEnv {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}

// primeSessionStart is a SessionStart hook's proven live managed session: the
// city store the proof read, and the session it read. The rest of the one
// SessionStart invocation reuses both instead of opening the store or reading
// the session again.
type primeSessionStart struct {
	store beads.Store
	info  sessionpkg.Info
}

// primeHookLiveManagedSession returns the city store and the hook's session
// only after the session-class projection proves the hook's exact live
// managed-session identity, and nil otherwise. A closed session bead is said
// so on stderr: its SessionStart surfaces no mail, rather than a successor's.
func primeHookLiveManagedSession(cityPath string, stderr io.Writer) *primeSessionStart {
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	if sessionID == "" {
		return nil
	}
	sessionName := strings.TrimSpace(os.Getenv("GC_SESSION_NAME"))
	if sessionName == "" {
		return nil
	}
	// The store open loads the city config first (loadCityConfigForStoreOpen:
	// this loader, this writer), which runs the builtin readiness pass the
	// routing read needs. Loading it here instead lets the routing read run
	// alongside the open (prewarmCLIStorageRoutes); the open then reuses it.
	if _, err := loadCityConfig(cityPath, io.Discard); err == nil {
		defer prewarmCLIStorageRoutes(cityPath)()
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return nil
	}
	// Route the session-bead read through the session coordination-class store so
	// a [beads.classes.sessions] relocation reaches this prime hook, mirroring
	// primeHookSessionTemplate. The no-refresh config loader is deliberate on this
	// hot hook path; a failed load yields nil cfg, which cliSessionStore treats as
	// identity.
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	sessStore := cliSessionStore(store, cfg, cityPath)
	// The front-door Get rejects a present-but-non-session bead
	// (ErrSessionNotFound), folding in the removed IsSessionBeadOrRepairable guard.
	info, err := sessionFrontDoor(sessStore).Get(sessionID)
	if err != nil {
		return nil
	}
	if info.Closed {
		fmt.Fprintf(stderr, "gc prime: %v; surfacing no mail\n", closedHookSessionError(info.ID)) //nolint:errcheck // best-effort hook diagnostics
		return nil
	}
	// Use the RAW session_name mirror (SessionNameMetadata), not SessionName which
	// falls back to sessionNameFor(ID) and would loosen the exact-match semantics.
	if strings.TrimSpace(info.SessionNameMetadata) != sessionName {
		return nil
	}
	if template := strings.TrimSpace(os.Getenv("GC_TEMPLATE")); template != "" &&
		strings.TrimSpace(info.Template) != template {
		return nil
	}
	// MetadataState is the RAW state metadata; Info.State is blanked on closed
	// beads, so the raw mirror preserves the original exact comparison.
	switch sessionpkg.State(strings.TrimSpace(info.MetadataState)) {
	case sessionpkg.StateActive, sessionpkg.StateAwake, sessionpkg.StateCreating, sessionpkg.StateStartPending:
		return &primeSessionStart{store: store, info: info}
	default:
		return nil
	}
}

// liveInfo is the proven live session, or nil without one.
func (p *primeSessionStart) liveInfo() *sessionpkg.Info {
	if p == nil {
		return nil
	}
	return &p.info
}

// hookInlinePromptLimit is the size beyond which Claude Code stops inlining
// SessionStart hook output into the first turn and spills the rest to a file
// (about 10,000 characters on 2.1.283; ga-4k4zfk).
const hookInlinePromptLimit = 10_000

// logHookSoleCarrierPrompt records that a SessionStart hook is about to be the
// only carrier of a startup prompt too large for the provider to inline in full:
// no launch payload delivered it (GC_STARTUP_PROMPT_DELIVERED is unset), so the
// agent will see a truncated prompt. Sizes compare in bytes, which never
// under-reports characters. log is *slog.Logger's Warn method, passed as a value
// so a test can capture the record; sessionFallback names the record when the
// session name is not in the environment. The record never carries prompt content.
func logHookSoleCarrierPrompt(log func(msg string, args ...any), sessionFallback, prompt string) {
	if len(prompt) <= hookInlinePromptLimit || strings.TrimSpace(os.Getenv(startupPromptDeliveredEnv)) == "1" {
		return
	}
	sessionName := strings.TrimSpace(os.Getenv("GC_SESSION_NAME"))
	if sessionName == "" {
		sessionName = sessionFallback
	}
	log("startup prompt reaches the session only through the SessionStart hook and exceeds the provider's inline limit; it will be truncated",
		slog.String("session", sessionName),
		slog.Int("bytes", len(prompt)),
		slog.Int("limit", hookInlinePromptLimit),
	)
}

func writePrimePromptWithFormat(stdout io.Writer, cityName, agentName, prompt string, hookMode bool, hookFormat string, suppressPrompt bool, hookContextSuffix string, afterDelivery func()) {
	if hookMode && suppressPrompt {
		// Managed sessions receive the rendered startup prompt through the
		// launch payload or nudge path. SessionStart hooks add context only.
		prompt = ""
	}
	if hookMode {
		logHookSoleCarrierPrompt(slog.Default().Warn, agentName, prompt)
		prompt = prependHookBeacon(cityName, agentName, prompt)
		// The step reminder is hook-only context, not the startup prompt, so it
		// survives suppression — managed SessionStart hooks still carry it. Folded
		// into the single write below to keep exactly one provider hook context.
		prompt += hookContextSuffix
	}
	if hookMode && hookFormat != "" {
		if err := writeProviderHookContextForEvent(stdout, hookFormat, "SessionStart", prompt); err == nil && afterDelivery != nil {
			afterDelivery()
		}
		return
	}
	if _, err := fmt.Fprint(stdout, prompt); err == nil && afterDelivery != nil { //nolint:errcheck // best-effort stdout
		afterDelivery()
	}
}

func readPrimeHookContext() primeHookContext {
	ctx := primeHookContext{
		Source:        strings.TrimSpace(os.Getenv("GC_HOOK_SOURCE")),
		HookEventName: strings.TrimSpace(os.Getenv("GC_HOOK_EVENT_NAME")),
	}
	if input := readPrimeHookStdin(); input != nil {
		if source := strings.TrimSpace(input.Source); source != "" {
			ctx.Source = source
		}
		if event := strings.TrimSpace(input.HookEventName); event != "" {
			ctx.HookEventName = event
		}
		providerSessionID := strings.TrimSpace(input.SessionID)
		if providerSessionID == "" {
			providerSessionID = strings.TrimSpace(input.ConversationID)
		}
		if providerSessionID != "" {
			ctx.ProviderSessionID = providerSessionID
		}
	}
	return ctx
}

func readPrimeHookStdin() *primeHookInput {
	stdin := primeStdin()
	stat, err := stdin.Stat()
	if err != nil {
		return nil
	}
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		return nil
	}

	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		ch <- readResult{line: line, err: err}
	}()

	var line string
	select {
	case res := <-ch:
		if res.err != nil && res.line == "" {
			return nil
		}
		line = strings.TrimSpace(res.line)
	case <-time.After(primeHookReadTimeout):
		return nil
	}
	if line == "" {
		return nil
	}

	var input primeHookInput
	if err := json.Unmarshal([]byte(line), &input); err != nil {
		return nil
	}
	return &input
}

func persistPrimeHookProviderSessionKey(hookProviderSessionID string, stderr io.Writer) {
	persistPrimeHookProviderSessionKeyAtCity(hookProviderSessionID, "", stderr)
}

// persistPrimeHookProviderSessionKeyAtCity is persistPrimeHookProviderSessionKey
// for a caller that already resolved its city: a non-empty cityPath is used as
// is, and an empty one falls back to resolving the ambient city. The store open,
// read, and write stay independent of any handle the caller holds.
func persistPrimeHookProviderSessionKeyAtCity(hookProviderSessionID, cityPath string, stderr io.Writer) {
	gcSessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	providerSessionID := strings.TrimSpace(os.Getenv("GC_PROVIDER_SESSION_ID"))
	if providerSessionID == "" {
		providerSessionID = strings.TrimSpace(os.Getenv("GEMINI_SESSION_ID"))
	}
	fromHookStdin := false
	if providerSessionID == "" {
		providerSessionID = strings.TrimSpace(hookProviderSessionID)
		fromHookStdin = providerSessionID != ""
	}
	requiredProviderID := strings.TrimSpace(os.Getenv("GC_PROVIDER_SESSION_ID_REQUIRED"))
	warn := func(format string, args ...any) {
		if requiredProviderID == "" || stderr == nil {
			return
		}
		allArgs := append([]any{requiredProviderID}, args...)
		fmt.Fprintf(stderr, "gc prime --hook: provider session key not persisted for %s: "+format+"\n", allArgs...) //nolint:errcheck // hook diagnostics are best effort.
	}
	if gcSessionID == "" {
		// Provider overlays set GC_PROVIDER_SESSION_ID_REQUIRED on every session
		// they open, managed or not, so an absent GC_SESSION_ID is the ordinary
		// state of a human-launched provider in a directory gc has staged — not
		// a fault. Reporting it there surfaces an error banner in the provider
		// UI for a session that was never Gas City's to begin with. Keep the
		// diagnostic only where managed intent is evident, which is where a
		// missing GC_SESSION_ID really is broken.
		if hookHasManagedIdentity() {
			warn("GC_SESSION_ID is empty")
		}
		return
	}
	if providerSessionID == "" {
		warn("GC_PROVIDER_SESSION_ID/GEMINI_SESSION_ID/hook stdin session id is empty for session %q", gcSessionID)
		return
	}
	if gcSessionID == providerSessionID {
		warn("provider session id equals GC_SESSION_ID %q", gcSessionID)
		return
	}
	if cityPath == "" {
		resolved, err := resolveCity()
		if err != nil {
			warn("resolving city for session %q: %v", gcSessionID, err)
			return
		}
		cityPath = resolved
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		warn("opening city store for session %q: %v", gcSessionID, err)
		return
	}
	// Route the session_key write through the session coordination-class store so
	// a [beads.classes.sessions] relocation reaches it — otherwise the provider
	// resume key would silently land on the work store while the real session
	// bead lives in the relocated store. No-refresh loader on this hot hook path
	// (see primeHookSessionTemplate); nil cfg → cliSessionStore identity.
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	sessStore := cliSessionStore(store, cfg, cityPath)
	// WI-6 R5: route the read through the session front door → Info. Get wraps
	// absence as "loading session %q" and rejects non-session beads with
	// ErrSessionNotFound; on this hook path both surface through the existing
	// warn-and-return diagnostic (a foreign/absent bead never reaches the write),
	// and the codex guard now resolves the family off Info (Provider precedence:
	// builtin_ancestor → provider_kind → provider, all carried on Info).
	sessFront := sessionFrontDoor(sessStore)
	info, err := sessFront.Get(gcSessionID)
	if err != nil {
		// The front-door Get already wraps with `loading session %q`, carrying the
		// id — don't re-prefix (that would double-wrap the stderr).
		warn("%v", err)
		return
	}
	if fromHookStdin && !providerAcceptsHookStdinSessionID(sessionProviderFamily(info)) {
		warn("hook stdin provider session id is only accepted for codex/cursor/claude session %q", gcSessionID)
		return
	}
	if existing := strings.TrimSpace(info.SessionKey); existing != "" {
		return
	}
	if err := sessFront.SetMarker(gcSessionID, "session_key", providerSessionID); err != nil {
		warn("writing session_key for session %q: %v", gcSessionID, err)
		return
	}
	// Runs once per session (the empty-key check above guards re-entry).
	//
	// Opt-in only. A provider hook forwards its child's stderr into the agent's
	// terminal, so anything written here lands in the user's input box mid-turn.
	// Every failure above goes through warn; this is the one success path, and a
	// routine capture is not something the operator needs to be told about.
	if stderr != nil && gcDebugEnabled() {
		fmt.Fprintf(stderr, "gc prime --hook: persisted resume session_key for %s session %q\n", sessionProviderFamily(info), gcSessionID) //nolint:errcheck // hook diagnostics are best effort.
	}
}

// providerAcceptsHookStdinSessionID reports whether a provider family delivers
// its authoritative resume id on the SessionStart hook's stdin JSON. Codex,
// Cursor, and Claude all run through the settings.json `gc prime --hook` path
// and emit their own session id there, so it is the authoritative resume key;
// Cursor normalizes its conversation id into the session_id field. Other CLI
// providers surface it via env instead (GC_PROVIDER_SESSION_ID for the
// JS-plugin providers, GEMINI_SESSION_ID for gemini) and are handled above,
// before this stdin gate.
//
// Claude is belt and braces: `claude --session-id <uuid>` IS accepted by the
// current CLI, so the builtin profile now sets session_id_flag and gc mints the
// durable key up front. The empty-key guard below means this stdin capture then
// no-ops for a session that was minted normally — it stays as the fallback for
// any session that reached the hook without one.
func providerAcceptsHookStdinSessionID(family string) bool {
	switch family {
	case "codex", "cursor", "claude":
		return true
	default:
		return false
	}
}

// isPoolInstance reports whether a resolved agent (with Pool=nil) originated
// from a pool template. Checks if the agent's base name (without -N suffix)
// matches a configured pool agent in the same dir.
func isPoolInstance(cfg *config.City, a config.Agent) bool {
	for _, ca := range cfg.Agents {
		if !ca.SupportsInstanceExpansion() {
			continue
		}
		if ca.Dir != a.Dir {
			continue
		}
		prefix := ca.Name + "-"
		if strings.HasPrefix(a.Name, prefix) {
			return true
		}
	}
	return false
}

// findAgentByName looks up an agent by its bare config name, ignoring dir.
// This allows "gc prime polecat" to find an agent with name="polecat" even
// when it has dir="myrig". Also handles pool instance names: "polecat-3"
// strips the "-N" suffix to match the base pool agent "polecat".
// Returns the first match.
func findAgentByName(cfg *config.City, name string) (config.Agent, bool) {
	for _, a := range cfg.Agents {
		if a.Name == name {
			return a, true
		}
	}
	// Pool suffix stripping: "polecat-3" → try "polecat" if it's a pool.
	for _, a := range cfg.Agents {
		if a.SupportsInstanceExpansion() && !a.UsesCanonicalSingletonPoolIdentity() {
			sp := scaleParamsFor(&a)
			prefix := a.Name + "-"
			if strings.HasPrefix(name, prefix) {
				suffix := name[len(prefix):]
				isUnlimited := sp.Max < 0
				if n, err := strconv.Atoi(suffix); err == nil && n >= 1 && (isUnlimited || n <= sp.Max) {
					return a, true
				}
			}
		}
	}
	return config.Agent{}, false
}

// buildPrimeContext constructs a PromptContext for gc prime. Uses GC_*
// environment variables when running inside a managed session, falls back
// to currentRigContext when run manually.
func buildPrimeContext(cityPath, cityName string, a *config.Agent, rigs []config.Rig, stderr io.Writer) PromptContext {
	return buildPrimeContextFor(cityPath, cityName, a, rigs, config.QueryTopology{}, stderr)
}

// primeAgentPromptContext builds the PromptContext gc prime renders an
// agent's prompt_template with, including the agent's provider identity.
func primeAgentPromptContext(cityPath, cityName string, cfg *config.City, a *config.Agent, stderr io.Writer) PromptContext {
	ctx := buildPrimeContextFor(cityPath, cityName, a, cfg.Rigs, cityQueryTopology(cityPath, cfg), stderr)
	ctx.ProviderKey, ctx.ProviderDisplayName = providerInfoForAgent(a, &cfg.Workspace, cfg.Providers)
	ctx.InstructionsFile = instructionsFileForAgent(a, &cfg.Workspace, cfg.Providers)
	return ctx
}

// primeStrictTemplatesRender is gc prime --strict's prompt_template check. It
// renders, silently and before any hook side effect, the prompt of each
// resolved agent the output loop of doPrimeWithHookFormatOpts reaches, and
// stops where that loop stops: at a suspended agent (a quiet success), at a
// hook for a client's provider chat (no persona), and at the first agent whose
// prompt renders non-empty (the prompt gc prime outputs). When an agent's
// prompt_template cannot be read, parsed, or executed, it writes the check's
// warnings to stderr, then a line naming the template and that agent, and
// reports false. On success it drops the warnings; the output render prints
// them.
func primeStrictTemplatesRender(cityPath, cityName string, cfg *config.City, agents []config.Agent, hookMode, sessionTemplateContext bool, stderr io.Writer) bool {
	var warnings bytes.Buffer
	for _, a := range agents {
		if isAgentEffectivelySuspendedWith(cfg, cityPath, &a, loadSuspensionStateBestEffort(cityPath)) || primeHookClientChat(hookMode, &a) {
			return true
		}
		_, rendered := primeAgentPrompt(cityPath, cityName, cfg, &a, hookMode || sessionTemplateContext, &warnings)
		if rendered.Err != nil {
			_, _ = stderr.Write(warnings.Bytes())
			fmt.Fprintf(stderr, "gc prime: prompt_template %q for agent %q: %v\n", a.PromptTemplate, a.QualifiedName(), rendered.Err) //nolint:errcheck // best-effort stderr
			return false
		}
		if rendered.Text != "" {
			return true
		}
	}
	return true
}

// primeHookClientChat reports whether a hook invocation primes a client's
// provider chat of agent a, which gets no worker persona (see
// isClientProviderChat).
func primeHookClientChat(hookMode bool, a *config.Agent) bool {
	return hookMode && isClientProviderChat(a, os.Getenv("GC_SESSION_ORIGIN"), os.Getenv("GC_ALIAS"))
}

// primeAgentPrompt renders the prompt gc prime outputs for resolved agent a
// and returns it with the context it was rendered with. That prompt is a's
// prompt_template, or, when a has none, the core pack's builtin worker prompt
// that fits a (see primeBuiltinPromptFile). An empty Text sends gc prime on to
// its next candidate. Err reports a prompt_template that could not be used as
// written (see PromptRenderResult); a builtin prompt never sets it.
// sessionContext builds a's context for a builtin prompt too, as a hook or a
// session's template context renders it; otherwise a builtin prompt renders
// with the zero context.
func primeAgentPrompt(cityPath, cityName string, cfg *config.City, a *config.Agent, sessionContext bool, stderr io.Writer) (PromptContext, PromptRenderResult) {
	var ctx PromptContext
	if a.PromptTemplate != "" || sessionContext {
		ctx = primeAgentPromptContext(cityPath, cityName, cfg, a, stderr)
	}
	if a.PromptTemplate != "" {
		return ctx, renderPrimeAgentPrompt(cityPath, cityName, cfg, a, ctx, stderr)
	}
	promptFile := primeBuiltinPromptFile(cfg, a)
	if promptFile == "" {
		return ctx, PromptRenderResult{}
	}
	// The read goes through renderPrompt, not os.ReadFile, so a builtin prompt
	// that composes a core-pack template fragment (pool-worker pulls in
	// "claim-protocol") resolves it exactly as a prompt_template does.
	// graph-worker.md is a plain .md and renders verbatim.
	return ctx, PromptRenderResult{Text: renderPrompt(fsys.OSFS{}, cityPath, cityName, promptFile, ctx, cfg.Workspace.SessionTemplate, stderr,
		cfg.PackDirsForRig(ctx.RigName), nil, nil)}
}

// primeBuiltinPromptFile returns the builtin prompt gc prime outputs for an
// agent without a prompt_template, shipped by the core bootstrap pack and
// resolved from the composed pack dirs: graph-worker.md for every agent when
// formula_v2 is enabled, else pool-worker.template.md for pool agents. A pool
// instance has Pool=nil after resolution, so isPoolInstance matches it to its
// pool agent by name. It returns "" when the core pack is absent or no builtin
// prompt fits the agent.
func primeBuiltinPromptFile(cfg *config.City, a *config.Agent) string {
	coreDir := cfg.PackDirByName("core")
	switch {
	case coreDir == "":
		return ""
	case cfg.Daemon.FormulaV2Enabled():
		return filepath.Join(coreDir, "assets", "prompts", "graph-worker.md")
	case a.SupportsInstanceExpansion() || isPoolInstance(cfg, *a):
		return filepath.Join(coreDir, "assets", "prompts", "pool-worker.template.md")
	}
	return ""
}

// renderPrimeAgentPrompt renders an agent's prompt_template with the
// fragments and rig-scoped pack dirs gc prime uses.
func renderPrimeAgentPrompt(cityPath, cityName string, cfg *config.City, a *config.Agent, ctx PromptContext, stderr io.Writer) PromptRenderResult {
	fragments := effectivePromptFragments(
		cfg.Workspace.GlobalFragments,
		a.InjectFragments,
		a.AppendFragments,
		a.InheritedAppendFragments,
		cfg.AgentDefaults.AppendFragments,
	)
	return renderPromptWithMeta(fsys.OSFS{}, cityPath, cityName, a.PromptTemplate, ctx, cfg.Workspace.SessionTemplate, stderr,
		cfg.PackDirsForRig(ctx.RigName), fragments, nil)
}

func buildPrimeContextFor(cityPath, cityName string, a *config.Agent, rigs []config.Rig, topo config.QueryTopology, stderr io.Writer) PromptContext {
	return buildPrimeContextWithIdentity(cityPath, cityName, a, rigs, topo, primeSessionIdentityFromEnv(), stderr)
}

// primeSessionIdentity names the session a prompt context is built for. gc prime
// runs inside that session and reads it from the environment; a caller that is
// not running inside the session supplies the session's own values instead of its
// process environment.
type primeSessionIdentity struct {
	Alias   string // GC_ALIAS
	Agent   string // GC_AGENT
	Dir     string // GC_DIR
	Rig     string // GC_RIG
	RigRoot string // GC_RIG_ROOT
	Branch  string // GC_BRANCH
}

// primeSessionIdentityFromEnv returns the identity of the session this process
// runs inside.
func primeSessionIdentityFromEnv() primeSessionIdentity {
	return primeSessionIdentity{
		Alias:   os.Getenv("GC_ALIAS"),
		Agent:   os.Getenv("GC_AGENT"),
		Dir:     os.Getenv("GC_DIR"),
		Rig:     os.Getenv("GC_RIG"),
		RigRoot: os.Getenv("GC_RIG_ROOT"),
		Branch:  os.Getenv("GC_BRANCH"),
	}
}

// buildPrimeContextWithIdentity is buildPrimeContextFor for an explicit session
// identity.
func buildPrimeContextWithIdentity(cityPath, cityName string, a *config.Agent, rigs []config.Rig, topo config.QueryTopology, id primeSessionIdentity, stderr io.Writer) PromptContext {
	ctx := buildAgentPromptContext(cityPath, cityName, a, rigs, topo, stderr)

	// Agent identity: prefer GC_ALIAS, then GC_AGENT, else config.
	if id.Alias != "" {
		ctx.AgentName = id.Alias
	} else if id.Agent != "" {
		ctx.AgentName = id.Agent
	}

	// Working directory.
	if id.Dir != "" {
		ctx.WorkDir = id.Dir
	}

	// Rig context.
	if id.Rig != "" {
		ctx.RigName = id.Rig
		ctx.RigRoot = id.RigRoot
		if ctx.RigRoot == "" {
			ctx.RigRoot = rigRootForName(id.Rig, rigs)
		}
		ctx.IssuePrefix = findRigPrefix(id.Rig, rigs)
	}

	ctx.Branch = id.Branch
	if id.Dir != "" || id.Rig != "" {
		ctx.DefaultBranch = defaultBranchForRig(ctx.RigName, rigs, ctx.WorkDir)
	}
	return ctx
}

// buildAgentPromptContext constructs a PromptContext for an agent purely from
// configuration: the identity is the agent's qualified name, the rig is its
// configured rig, and no GC_* session environment is consulted. Callers that
// survey agents other than the current session (e.g. gc doctor) use this so
// an ambient session identity cannot leak into another agent's render.
func buildAgentPromptContext(cityPath, cityName string, a *config.Agent, rigs []config.Rig, topo config.QueryTopology, stderr io.Writer) PromptContext {
	ctx := PromptContext{
		CityRoot:      cityPath,
		AgentName:     a.QualifiedName(),
		TemplateName:  a.Name,
		BindingName:   a.BindingName,
		BindingPrefix: a.BindingPrefix(),
		ConfigDir:     resolveConfigDir(cityPath, a.SourceDir),
		Env:           a.Env,
	}

	if rigName := configuredRigName(cityPath, a, rigs); rigName != "" {
		ctx.RigName = rigName
		ctx.RigRoot = rigRootForName(rigName, rigs)
		ctx.IssuePrefix = findRigPrefix(rigName, rigs)
	}

	ctx.DefaultBranch = defaultBranchForRig(ctx.RigName, rigs, ctx.WorkDir)
	// The discovery placeholders run verbatim in the agent's own shell, so they
	// render against the prompt topology — federated for a cross-store-eligible
	// agent on a city with rigs — not the caller's topology. SlingQuery is not a
	// discovery reader and keeps the caller's view.
	promptTopo := promptQueryTopology(topo, a, rigs)
	ctx.WorkQuery = expandAgentCommandTemplate(cityPath, cityName, a, rigs, "work_query", a.EffectiveWorkQueryFor(promptTopo), stderr)
	ctx.AssignedInProgressQuery = expandAgentCommandTemplate(cityPath, cityName, a, rigs, "assigned_in_progress_query", a.EffectiveAssignedInProgressQueryFor(promptTopo), stderr)
	ctx.AssignedReadyQuery = expandAgentCommandTemplate(cityPath, cityName, a, rigs, "assigned_ready_query", a.EffectiveAssignedReadyQueryFor(promptTopo), stderr)
	ctx.RoutedPoolQuery = expandAgentCommandTemplate(cityPath, cityName, a, rigs, "routed_pool_query", a.EffectiveRoutedPoolQueryFor(promptTopo), stderr)
	ctx.SlingQuery = expandAgentCommandTemplate(cityPath, cityName, a, rigs, "sling_query", a.EffectiveSlingQuery(), stderr)
	return ctx
}
