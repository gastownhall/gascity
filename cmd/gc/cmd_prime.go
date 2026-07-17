package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
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
	Source        string `json:"source"`
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
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

Pass --strict to fail on debugging mistakes instead of silently falling
back to the default prompt. Strict errors on:

  - no city config found
  - city config fails to load
  - no agent name given (from args, GC_ALIAS, or GC_AGENT)
  - agent name not in city config (typo detection — the main use case)
  - agent's prompt_template points at a file that cannot be read

Strict does NOT error on agents whose config intentionally lacks a
prompt_template (a supported minimal config), on templates that render
to empty output from valid conditional logic, or on suspended states
(city or agent) — those are legitimate quiet states, not mistakes.`,
		Args: cobra.MaximumNArgs(1),
	}
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		if jsonOut {
			var buf strings.Builder
			if doPrimeWithHookFormat(args, &buf, stderr, hookMode, hookFormat, strictMode) != 0 {
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
			})
		}
		if doPrimeWithHookFormat(args, stdout, stderr, hookMode, hookFormat, strictMode) != 0 {
			return errExit
		}
		return nil
	}
	cmd.Flags().BoolVar(&hookMode, "hook", false, "compatibility mode for runtime hook invocations")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	cmd.Flags().BoolVar(&strictMode, "strict", false, "fail on missing city, missing or unknown agent, or unreadable prompt_template instead of falling back to the default prompt")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON summary")
	return cmd
}

type primeJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	Agent         string `json:"agent,omitempty"`
	Hook          bool   `json:"hook"`
	HookFormat    string `json:"hook_format,omitempty"`
	Content       string `json:"content"`
	Bytes         int    `json:"bytes"`
}

// doPrime exists as the public non-strict entry point so callers don't
// need to know about the strict flag; its return type stays int because
// the caller shape matches other cmd/gc entry points.
func doPrime(args []string, stdout, stderr io.Writer) int { //nolint:unparam // strictMode=false means always returns 0
	return doPrimeWithMode(args, stdout, stderr, false, false)
}

// doPrimeWithMode's strict-mode contract: only states that would indicate
// a user mistake (missing city config, no agent name, unknown agent name,
// unreadable prompt_template file) error out. Supported minimal configs
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
	agentName, sessionTemplateContext := primeInvocationAgentName(args)
	var hookContext primeHookContext
	suppressHookPrompt := false
	if hookMode {
		hookContext = readPrimeHookContext()
		suppressHookPrompt = managedSessionHookPromptAlreadyDelivered(hookContext)
	}
	// In non-strict mode, hook side effects fire eagerly (existing behavior).
	// In strict mode, we defer them until after strict checks pass so that a
	// failing --strict invocation does not update provider resume metadata for
	// failed agent resolution or template validation.
	runHookSideEffects := func() {
		if !hookMode {
			return
		}
		persistPrimeHookProviderSessionKey(hookContext.ProviderSessionID, stderr)
	}
	if !strictMode && !primeHookSessionStart(hookContext) {
		runHookSideEffects()
	}

	cityPath, err := resolveCity()
	if err != nil {
		if strictMode {
			fmt.Fprintf(stderr, "gc prime: no city config found: %v\n", err) //nolint:errcheck
			return 1
		}
		if hookMode && primeHookSessionStart(hookContext) {
			writePrimePromptWithFormat(stdout, "", "", "", hookMode, hookFormat, false, "")
			return 0
		}
		var stepReminder string
		if hookMode {
			stepReminder = wispStepInjectionContent("")
		}
		writePrimePromptWithFormat(stdout, "", "", defaultPrimePrompt, hookMode, hookFormat, suppressHookPrompt, stepReminder)
		return 0
	}
	if hookMode && primeHookSessionStart(hookContext) && !primeHookHasLiveManagedSession(cityPath) {
		writePrimePromptWithFormat(stdout, "", "", "", hookMode, hookFormat, false, "")
		return 0
	}
	if !strictMode && primeHookSessionStart(hookContext) {
		runHookSideEffects()
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		if strictMode {
			fmt.Fprintf(stderr, "gc prime: loading city config: %v\n", err) //nolint:errcheck
			return 1
		}
		var stepReminder string
		if hookMode {
			stepReminder = wispStepInjectionContent(cityPath)
		}
		writePrimePromptWithFormat(stdout, "", "", defaultPrimePrompt, hookMode, hookFormat, suppressHookPrompt, stepReminder)
		return 0
	}
	resolveRigPaths(cityPath, cfg.Rigs)

	if citySuspended(cfg) {
		// Suspended is a legitimate quiet state, not a strict failure —
		// keep hook behavior consistent with non-strict (which already
		// ran side effects eagerly above).
		if strictMode {
			runHookSideEffects()
		}
		return 0
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
			return 1
		case len(resolvedAgents) == 0:
			fmt.Fprintf(stderr, "gc prime: agent %q not found in city config\n", agentName) //nolint:errcheck
			return 1
		}
		// renderPrompt returns "" both when the template file cannot be read
		// and when a valid template legitimately renders empty. Readability is
		// the strict precondition, so check it before hook side effects.
		for _, a := range resolvedAgents {
			if isAgentEffectivelySuspended(cfg, &a) {
				continue
			}
			if a.PromptTemplate == "" {
				continue
			}
			if _, fErr := os.ReadFile(promptTemplateSourcePath(cityPath, a.PromptTemplate)); fErr != nil {
				fmt.Fprintf(stderr, "gc prime: prompt_template %q for agent %q: %v\n", a.PromptTemplate, agentName, fErr) //nolint:errcheck
				return 1
			}
		}
		// Strict preconditions passed; now it's safe to update provider resume metadata.
		runHookSideEffects()
	}

	for _, a := range resolvedAgents {
		if isAgentEffectivelySuspended(cfg, &a) {
			return 0
		}
		if resolved, rErr := config.ResolveProvider(&a, &cfg.Workspace, cfg.Providers, exec.LookPath); rErr == nil && hookMode {
			sessionName := os.Getenv("GC_SESSION_NAME")
			if sessionName == "" {
				sessionName = cliSessionName(cityPath, cityName, a.QualifiedName(), cfg.Workspace.SessionTemplate)
			}
			maybeStartNudgePoller(withNudgeTargetFence(openNudgeBeadStore(cityPath).Store, nudgeTarget{
				cityPath:          cityPath,
				cityName:          cityName,
				cfg:               cfg,
				agent:             a,
				resolved:          resolved,
				sessionID:         os.Getenv("GC_SESSION_ID"),
				continuationEpoch: os.Getenv("GC_CONTINUATION_EPOCH"),
				sessionName:       sessionName,
			}))
		}
		var ctx PromptContext
		if a.PromptTemplate != "" || hookMode || sessionTemplateContext {
			ctx = buildPrimeContextForBeads(cityPath, cityName, &a, cfg.Rigs, cfg.Beads, stderr)
			ctx.ProviderKey, ctx.ProviderDisplayName = providerInfoForAgent(&a, &cfg.Workspace, cfg.Providers)
			ctx.InstructionsFile = instructionsFileForAgent(&a, &cfg.Workspace, cfg.Providers)
		}
		if a.PromptTemplate != "" {
			fragments := effectivePromptFragments(
				cfg.Workspace.GlobalFragments,
				a.InjectFragments,
				a.AppendFragments,
				a.InheritedAppendFragments,
				cfg.AgentDefaults.AppendFragments,
			)
			packDirs := cfg.PackDirsForRig(ctx.RigName)
			prompt := renderPrompt(fsys.OSFS{}, cityPath, cityName, a.PromptTemplate, ctx, cfg.Workspace.SessionTemplate, stderr,
				packDirs, fragments, nil)
			if prompt != "" {
				var stepReminder string
				if hookMode {
					stepReminder = wispStepInjectionContent(cityPath)
				}
				writePrimePromptWithFormat(stdout, cityName, ctx.AgentName, prompt, hookMode, hookFormat, suppressHookPrompt, stepReminder)
				return 0
			}
			// File is present but rendered empty. Treat as a legitimate
			// (if unusual) minimal config — emit the default fallback.
		}
		// Agents without a prompt_template: read a builtin prompt shipped by
		// the core bootstrap pack, resolved from the composed pack dirs.
		// When formula_v2 is enabled, all agents use graph-worker.md.
		// Otherwise pool agents use pool-worker.md.
		// Pool instances have Pool=nil after resolution, so also check the
		// template agent via findAgentByName.
		if a.PromptTemplate == "" {
			promptFile := ""
			if coreDir := cfg.PackDirByName("core"); coreDir != "" {
				if cfg.Daemon.FormulaV2Enabled() {
					promptFile = filepath.Join(coreDir, "assets", "prompts", "graph-worker.md")
				} else if a.SupportsInstanceExpansion() || isPoolInstance(cfg, a) {
					promptFile = filepath.Join(coreDir, "assets", "prompts", "pool-worker.md")
				}
			}
			if promptFile != "" {
				if content, fErr := os.ReadFile(promptFile); fErr == nil {
					var stepReminder string
					if hookMode {
						stepReminder = wispStepInjectionContent(cityPath)
					}
					writePrimePromptWithFormat(stdout, cityName, ctx.AgentName, string(content), hookMode, hookFormat, suppressHookPrompt, stepReminder)
					return 0
				}
			}
		}
	}

	// Fallback: default run-once prompt. Under strict, this is only reached
	// when the agent has no prompt_template and doesn't match a builtin
	// worker prompt — a supported config shape, so the default prompt is
	// the correct output even under --strict.
	var stepReminder string
	if hookMode {
		stepReminder = wispStepInjectionContent(cityPath)
	}
	writePrimePromptWithFormat(stdout, cityName, agentName, defaultPrimePrompt, hookMode, hookFormat, suppressHookPrompt, stepReminder)
	return 0
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

func primeHookHasLiveManagedSession(cityPath string) bool {
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	if sessionID == "" {
		return false
	}
	sessionName := strings.TrimSpace(os.Getenv("GC_SESSION_NAME"))
	if sessionName == "" {
		return false
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return false
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
		return false
	}
	if info.Closed {
		return false
	}
	// Use the RAW session_name mirror (SessionNameMetadata), not SessionName which
	// falls back to sessionNameFor(ID) and would loosen the exact-match semantics.
	if strings.TrimSpace(info.SessionNameMetadata) != sessionName {
		return false
	}
	if template := strings.TrimSpace(os.Getenv("GC_TEMPLATE")); template != "" &&
		strings.TrimSpace(info.Template) != template {
		return false
	}
	// MetadataState is the RAW state metadata; Info.State is blanked on closed
	// beads, so the raw mirror preserves the original exact comparison.
	switch sessionpkg.State(strings.TrimSpace(info.MetadataState)) {
	case sessionpkg.StateActive, sessionpkg.StateAwake, sessionpkg.StateCreating, sessionpkg.StateStartPending:
		return true
	default:
		return false
	}
}

func writePrimePromptWithFormat(stdout io.Writer, cityName, agentName, prompt string, hookMode bool, hookFormat string, suppressPrompt bool, hookContextSuffix string) {
	if hookMode && suppressPrompt {
		// Managed sessions receive the rendered startup prompt through the
		// launch payload or nudge path. SessionStart hooks add context only.
		prompt = ""
	}
	if hookMode {
		prompt = prependHookBeacon(cityName, agentName, prompt)
		// The step reminder is hook-only context, not the startup prompt, so it
		// survives suppression — managed SessionStart hooks still carry it. Folded
		// into the single write below to keep exactly one provider hook context.
		prompt += hookContextSuffix
	}
	if hookMode && hookFormat != "" {
		_ = writeProviderHookContextForEvent(stdout, hookFormat, "SessionStart", prompt)
		return
	}
	fmt.Fprint(stdout, prompt) //nolint:errcheck // best-effort stdout
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
		if providerSessionID := strings.TrimSpace(input.SessionID); providerSessionID != "" {
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
		warn("GC_SESSION_ID is empty")
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
	cityPath, err := resolveCity()
	if err != nil {
		warn("resolving city for session %q: %v", gcSessionID, err)
		return
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
	if fromHookStdin && sessionProviderFamily(info) != "codex" {
		warn("hook stdin provider session id is only accepted for codex session %q", gcSessionID)
		return
	}
	if existing := strings.TrimSpace(info.SessionKey); existing != "" {
		return
	}
	if err := sessFront.SetMarker(gcSessionID, "session_key", providerSessionID); err != nil {
		warn("writing session_key for session %q: %v", gcSessionID, err)
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
	return buildPrimeContextForBeads(cityPath, cityName, a, rigs, config.BeadsConfig{}, stderr)
}

func buildPrimeContextForBeads(cityPath, cityName string, a *config.Agent, rigs []config.Rig, beadsCfg config.BeadsConfig, stderr io.Writer) PromptContext {
	ctx := PromptContext{
		CityRoot:      cityPath,
		TemplateName:  a.Name,
		BindingName:   a.BindingName,
		BindingPrefix: a.BindingPrefix(),
		Env:           a.Env,
	}

	// Agent identity: prefer GC_ALIAS, then GC_AGENT, else config.
	if gcAlias := os.Getenv("GC_ALIAS"); gcAlias != "" {
		ctx.AgentName = gcAlias
	} else if gcAgent := os.Getenv("GC_AGENT"); gcAgent != "" {
		ctx.AgentName = gcAgent
	} else {
		ctx.AgentName = a.QualifiedName()
	}

	// Working directory.
	if gcDir := os.Getenv("GC_DIR"); gcDir != "" {
		ctx.WorkDir = gcDir
	}

	// Rig context.
	if gcRig := os.Getenv("GC_RIG"); gcRig != "" {
		ctx.RigName = gcRig
		ctx.RigRoot = os.Getenv("GC_RIG_ROOT")
		if ctx.RigRoot == "" {
			ctx.RigRoot = rigRootForName(gcRig, rigs)
		}
		ctx.IssuePrefix = findRigPrefix(gcRig, rigs)
	} else if rigName := configuredRigName(cityPath, a, rigs); rigName != "" {
		ctx.RigName = rigName
		ctx.RigRoot = rigRootForName(rigName, rigs)
		ctx.IssuePrefix = findRigPrefix(rigName, rigs)
	}

	ctx.Branch = os.Getenv("GC_BRANCH")
	ctx.DefaultBranch = defaultBranchForRig(ctx.RigName, rigs, ctx.WorkDir)
	ctx.WorkQuery = expandAgentCommandTemplate(cityPath, cityName, a, rigs, "work_query", a.EffectiveWorkQueryForBeads(beadsCfg), stderr)
	ctx.AssignedInProgressQuery = expandAgentCommandTemplate(cityPath, cityName, a, rigs, "assigned_in_progress_query", a.EffectiveAssignedInProgressQueryForBeads(beadsCfg), stderr)
	ctx.AssignedReadyQuery = expandAgentCommandTemplate(cityPath, cityName, a, rigs, "assigned_ready_query", a.EffectiveAssignedReadyQueryForBeads(beadsCfg), stderr)
	ctx.RoutedPoolQuery = expandAgentCommandTemplate(cityPath, cityName, a, rigs, "routed_pool_query", a.EffectiveRoutedPoolQueryForBeads(beadsCfg), stderr)
	ctx.SlingQuery = expandAgentCommandTemplate(cityPath, cityName, a, rigs, "sling_query", a.EffectiveSlingQuery(), stderr)
	return ctx
}
