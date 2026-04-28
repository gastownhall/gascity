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

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/spf13/cobra"
)

// defaultPrimePrompt is the run-once worker prompt output when no agent name
// matches a configured agent. This is for users who start Claude Code manually
// inside a rig without being a managed agent.
const defaultPrimePrompt = `# Gas City Agent

You are an agent in a Gas City workspace. Check for available work
and execute it.

## Your tools

- ` + "`bd ready`" + ` — see available work items
- ` + "`bd show <id>`" + ` — see details of a work item
- ` + "`bd close <id>`" + ` — mark work as done

## How to work

1. Check for available work: ` + "`bd ready`" + `
2. Pick a bead and execute the work described in its title
3. When done, close it: ` + "`bd close <id>`" + `
4. Check for more work. Repeat until the queue is empty.
`

const primeHookReadTimeout = 500 * time.Millisecond

var primeStdin = func() *os.File { return os.Stdin }

type primeHookInput struct {
	SessionID string `json:"session_id"`
	Source    string `json:"source"`
}

type primeHookContext struct {
	SessionID     string
	Source        string
	HookEventName string
}

// newPrimeCmd creates the "gc prime [agent-name]" command.
func newPrimeCmd(stdout, stderr io.Writer) *cobra.Command {
	var hookMode bool
	var hookFormat string
	var strictMode bool
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
		if doPrimeWithHookFormat(args, stdout, stderr, hookMode, hookFormat, strictMode) != 0 {
			return errExit
		}
		return nil
	}
	cmd.Flags().BoolVar(&hookMode, "hook", false, "compatibility mode for runtime hook invocations")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	cmd.Flags().BoolVar(&strictMode, "strict", false, "fail on missing city, missing or unknown agent, or unreadable prompt_template instead of falling back to the default prompt")
	return cmd
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
// invocation is not a strict failure, so a failing --strict cannot leave
// session-id state behind for an agent that doesn't exist. Suspended
// paths still run side effects because suspension is a legitimate quiet
// state, not a failure.
func doPrimeWithMode(args []string, stdout, stderr io.Writer, hookMode, strictMode bool) int {
	return doPrimeWithHookFormat(args, stdout, stderr, hookMode, "", strictMode)
}

func doPrimeWithHookFormat(args []string, stdout, stderr io.Writer, hookMode bool, hookFormat string, strictMode bool) int {
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

	hookContext := primeHookContext{}
	suppressHookPrompt := false
	if hookMode {
		hookContext = readPrimeHookContext()
		suppressHookPrompt = managedSessionHookPromptAlreadyDelivered(hookContext)
	}
	// In non-strict mode, hook side effects fire eagerly (existing behavior).
	// In strict mode, we defer them until after strict checks pass so that a
	// failing --strict invocation does not persist a session-id for failed
	// agent resolution or template validation.
	runHookSideEffects := func() {
		if !hookMode {
			return
		}
		if sessionID := hookContext.SessionID; sessionID != "" {
			persistPrimeHookSessionID(sessionID)
		}
		persistPrimeHookProviderSessionKey()
	}
	if !strictMode {
		runHookSideEffects()
	}

	cityPath, err := resolveCity()
	if err != nil {
		if strictMode {
			fmt.Fprintf(stderr, "gc prime: no city config found: %v\n", err) //nolint:errcheck
			return 1
		}
		writePrimePromptWithFormat(stdout, "", "", defaultPrimePrompt, hookMode, hookFormat, suppressHookPrompt)
		return 0
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		if strictMode {
			fmt.Fprintf(stderr, "gc prime: loading city config: %v\n", err) //nolint:errcheck
			return 1
		}
		writePrimePromptWithFormat(stdout, "", "", defaultPrimePrompt, hookMode, hookFormat, suppressHookPrompt)
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
		// Strict preconditions passed; now it's safe to persist session-id.
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
			maybeStartNudgePoller(withNudgeTargetFence(openNudgeBeadStore(cityPath), nudgeTarget{
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
			ctx = buildPrimeContext(cityPath, cityName, &a, cfg.Rigs, stderr)
		}
		if a.PromptTemplate != "" {
			fragments := effectivePromptFragments(
				cfg.Workspace.GlobalFragments,
				a.InjectFragments,
				a.AppendFragments,
				a.InheritedAppendFragments,
				cfg.AgentDefaults.AppendFragments,
			)
			prompt := renderPrompt(fsys.OSFS{}, cityPath, cityName, a.PromptTemplate, ctx, cfg.Workspace.SessionTemplate, stderr,
				cfg.PackDirs, fragments, nil)
			if prompt != "" {
				writePrimePromptWithFormat(stdout, cityName, ctx.AgentName, prompt, hookMode, hookFormat, suppressHookPrompt)
				return 0
			}
			// File is present but rendered empty. Treat as a legitimate
			// (if unusual) minimal config — emit the default fallback.
		}
		// Agents without a prompt_template: read a builtin prompt shipped by
		// the core bootstrap pack, materialized under .gc/system/packs/core/.
		// When formula_v2 is enabled, all agents use graph-worker.md.
		// Otherwise pool agents use pool-worker.md.
		// Pool instances have Pool=nil after resolution, so also check the
		// template agent via findAgentByName.
		if a.PromptTemplate == "" {
			promptFile := ""
			if cfg.Daemon.FormulaV2 {
				promptFile = citylayout.SystemPacksRoot + "/core/assets/prompts/graph-worker.md"
			} else if a.SupportsInstanceExpansion() || isPoolInstance(cfg, a) {
				promptFile = citylayout.SystemPacksRoot + "/core/assets/prompts/pool-worker.md"
			}
			if promptFile != "" {
				if content, fErr := os.ReadFile(filepath.Join(cityPath, promptFile)); fErr == nil {
					writePrimePromptWithFormat(stdout, cityName, ctx.AgentName, string(content), hookMode, hookFormat, suppressHookPrompt)
					return 0
				}
			}
		}
	}

	// Fallback: default run-once prompt. Under strict, this is only reached
	// when the agent has no prompt_template and doesn't match a builtin
	// worker prompt — a supported config shape, so the default prompt is
	// the correct output even under --strict.
	writePrimePromptWithFormat(stdout, "", agentName, defaultPrimePrompt, hookMode, hookFormat, suppressHookPrompt)
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
	sessionBead, err := store.Get(sessionID)
	if err != nil {
		return ""
	}
	if template := strings.TrimSpace(sessionBead.Metadata["template"]); template != "" {
		return template
	}
	return strings.TrimSpace(sessionBead.Metadata["common_name"])
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

func writePrimePromptWithFormat(stdout io.Writer, cityName, agentName, prompt string, hookMode bool, hookFormat string, suppressPrompt bool) {
	if hookMode && suppressPrompt {
		// Managed sessions receive the rendered startup prompt through the
		// launch payload or nudge path. SessionStart hooks add context only.
		prompt = ""
	}
	if hookMode {
		prompt = prependHookBeacon(cityName, agentName, prompt)
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
	if shouldReadPrimeHookStdin() {
		if input := readPrimeHookStdin(); input != nil {
			if source := strings.TrimSpace(input.Source); source != "" {
				ctx.Source = source
			}
			ctx.SessionID = strings.TrimSpace(input.SessionID)
		}
	}
	if id := strings.TrimSpace(os.Getenv("GC_SESSION_ID")); id != "" {
		ctx.SessionID = id
	} else if id := strings.TrimSpace(os.Getenv("CLAUDE_SESSION_ID")); id != "" {
		ctx.SessionID = id
	}
	return ctx
}

func shouldReadPrimeHookStdin() bool {
	hasEnvSessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID")) != "" ||
		strings.TrimSpace(os.Getenv("CLAUDE_SESSION_ID")) != ""
	return !hasEnvSessionID
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

func persistPrimeHookSessionID(sessionID string) {
	if sessionID == "" {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	runtimeDir := filepath.Join(cwd, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(runtimeDir, "session_id"), []byte(sessionID+"\n"), 0o644)
}

func persistPrimeHookProviderSessionKey() {
	gcSessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	providerSessionID := strings.TrimSpace(os.Getenv("GEMINI_SESSION_ID"))
	if gcSessionID == "" || providerSessionID == "" || gcSessionID == providerSessionID {
		return
	}
	cityPath, err := resolveCity()
	if err != nil {
		return
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return
	}
	sessionBead, err := store.Get(gcSessionID)
	if err != nil {
		return
	}
	if existing := strings.TrimSpace(sessionBead.Metadata["session_key"]); existing != "" {
		return
	}
	_ = store.SetMetadata(gcSessionID, "session_key", providerSessionID)
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
		if a.SupportsInstanceExpansion() {
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
	ctx := PromptContext{
		CityRoot:     cityPath,
		TemplateName: a.Name,
		Env:          a.Env,
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
	ctx.DefaultBranch = defaultBranchFor(ctx.WorkDir)
	ctx.WorkQuery = expandAgentCommandTemplate(cityPath, cityName, a, rigs, "work_query", a.EffectiveWorkQuery(), stderr)
	ctx.SlingQuery = expandAgentCommandTemplate(cityPath, cityName, a, rigs, "sling_query", a.EffectiveSlingQuery(), stderr)
	return ctx
}
