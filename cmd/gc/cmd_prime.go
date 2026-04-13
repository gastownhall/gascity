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

// newPrimeCmd creates the "gc prime [agent-name]" command.
func newPrimeCmd(stdout, stderr io.Writer) *cobra.Command {
	var hookMode bool
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
  - agent's prompt_template is configured but renders empty

Strict does NOT error on agents whose config intentionally lacks a
prompt_template: that is a supported config, and the default prompt is
the correct output. Suspended states (city or agent) also remain silent.`,
		Args: cobra.MaximumNArgs(1),
	}
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		if doPrimeWithMode(args, stdout, stderr, hookMode, strictMode) != 0 {
			return errExit
		}
		return nil
	}
	cmd.Flags().BoolVar(&hookMode, "hook", false, "compatibility mode for runtime hook invocations")
	cmd.Flags().BoolVar(&strictMode, "strict", false, "fail on missing city, missing agent, or empty-rendered template instead of falling back to the default prompt")
	return cmd
}

// doPrime exists as the public non-strict entry point so callers don't
// need to know about the strict flag; its return type stays int because
// the caller shape matches other cmd/gc entry points.
func doPrime(args []string, stdout, stderr io.Writer) int { //nolint:unparam // strictMode=false means always returns 0
	return doPrimeWithMode(args, stdout, stderr, false, false)
}

// doPrimeWithMode's strict-mode contract: only states that would indicate
// a user mistake (bad agent name, missing config, template that silently
// rendered empty) error out. Supported minimal configs (agent with no
// prompt_template) and intentional quiet states (suspended city/agent)
// remain silent even under --strict — strict is a debugging aid, not a
// stricter mode for the whole command.
//
// Hook-mode side effects are deferred under strict so a failing --strict
// invocation cannot leave session-id state or a running nudge poller
// behind for an agent that doesn't exist.
func doPrimeWithMode(args []string, stdout, stderr io.Writer, hookMode, strictMode bool) int {
	agentName := os.Getenv("GC_ALIAS")
	if agentName == "" {
		agentName = os.Getenv("GC_AGENT")
	}
	if len(args) > 0 {
		agentName = args[0]
	}

	// In non-strict mode, hook side effects fire eagerly (existing behavior).
	// In strict mode, we defer them until after strict checks pass so that a
	// failing --strict invocation does not persist a session-id for an agent
	// that doesn't exist.
	runHookSideEffects := func() {
		if !hookMode {
			return
		}
		if sessionID, _ := readPrimeHookContext(); sessionID != "" {
			persistPrimeHookSessionID(sessionID)
		}
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
		fmt.Fprint(stdout, defaultPrimePrompt) //nolint:errcheck // best-effort stdout
		return 0
	}
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		if strictMode {
			fmt.Fprintf(stderr, "gc prime: loading city config: %v\n", err) //nolint:errcheck
			return 1
		}
		fmt.Fprint(stdout, defaultPrimePrompt) //nolint:errcheck // best-effort stdout
		return 0
	}

	if citySuspended(cfg) {
		return 0 // empty output; hooks call this
	}

	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}

	// Look up agent in config. First try qualified identity resolution
	// (handles "rig/agent" and rig-context matching), then fall back to
	// bare template name lookup (handles "gc prime polecat" for pool agents
	// whose config name is "polecat" regardless of dir).
	var a config.Agent
	var agentFound bool
	if agentName != "" {
		a, agentFound = resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
		if !agentFound {
			a, agentFound = findAgentByName(cfg, agentName)
		}
		if agentFound && isAgentEffectivelySuspended(cfg, &a) {
			return 0 // suspended agent: silent even under strict (legitimate state)
		}
	}

	// Strict preconditions: fail now, before any hook side effects or the
	// nudge poller start, so a failing --strict leaves no partial state.
	if strictMode {
		switch {
		case agentName == "":
			fmt.Fprintf(stderr, "gc prime: --strict requires an agent name (from args, GC_ALIAS, or GC_AGENT)\n") //nolint:errcheck
			return 1
		case !agentFound:
			fmt.Fprintf(stderr, "gc prime: agent %q not found in city config\n", agentName) //nolint:errcheck
			return 1
		}
		// Strict preconditions passed; now it's safe to persist session-id.
		runHookSideEffects()
	}

	if agentFound {
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
		if a.PromptTemplate != "" || hookMode {
			ctx = buildPrimeContext(cityPath, &a, cfg.Rigs)
		}
		if a.PromptTemplate != "" {
			fragments := mergeFragmentLists(cfg.Workspace.GlobalFragments, a.InjectFragments)
			prompt := renderPrompt(fsys.OSFS{}, cityPath, cityName, a.PromptTemplate, ctx, cfg.Workspace.SessionTemplate, stderr,
				cfg.PackDirs, fragments, nil)
			if prompt != "" {
				writePrimePrompt(stdout, cityName, ctx.AgentName, prompt, hookMode)
				return 0
			}
			// Template is configured but rendered empty (missing file,
			// render error, empty fragments, etc.). Under strict, surface
			// this as a distinct failure; renderPrompt itself writes any
			// underlying error to stderr.
			if strictMode {
				fmt.Fprintf(stderr, "gc prime: prompt_template %q for agent %q rendered empty\n", a.PromptTemplate, agentName) //nolint:errcheck
				return 1
			}
		}
		// Agents without a prompt_template: read a materialized builtin prompt.
		// When formula_v2 is enabled, all agents use graph-worker.md.
		// Otherwise pool agents use pool-worker.md.
		// Pool instances have Pool=nil after resolution, so also check the
		// template agent via findAgentByName.
		if a.PromptTemplate == "" {
			promptFile := ""
			if cfg.Daemon.FormulaV2 {
				promptFile = "prompts/graph-worker.md"
			} else if isMultiSessionCfgAgent(&a) || isPoolInstance(cfg, a) {
				promptFile = "prompts/pool-worker.md"
			}
			if promptFile != "" {
				if content, fErr := os.ReadFile(filepath.Join(cityPath, promptFile)); fErr == nil {
					writePrimePrompt(stdout, cityName, ctx.AgentName, string(content), hookMode)
					return 0
				}
			}
		}
	}

	// Fallback: default run-once prompt. Under strict, this is only reached
	// when the agent has no prompt_template and doesn't match a builtin
	// worker prompt — a supported config shape, so the default prompt is
	// the correct output even under --strict.
	fmt.Fprint(stdout, defaultPrimePrompt) //nolint:errcheck // best-effort stdout
	return 0
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

func writePrimePrompt(stdout io.Writer, cityName, agentName, prompt string, hookMode bool) {
	if hookMode {
		prompt = prependHookBeacon(cityName, agentName, prompt)
	}
	fmt.Fprint(stdout, prompt) //nolint:errcheck // best-effort stdout
}

func readPrimeHookContext() (sessionID, source string) {
	source = os.Getenv("GC_HOOK_SOURCE")
	if id := os.Getenv("GC_SESSION_ID"); id != "" {
		return id, source
	}
	if id := os.Getenv("CLAUDE_SESSION_ID"); id != "" {
		return id, source
	}
	if input := readPrimeHookStdin(); input != nil {
		if input.Source != "" {
			source = input.Source
		}
		if input.SessionID != "" {
			return input.SessionID, source
		}
	}
	return "", source
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

// isPoolInstance reports whether a resolved agent (with Pool=nil) originated
// from a pool template. Checks if the agent's base name (without -N suffix)
// matches a configured pool agent in the same dir.
func isPoolInstance(cfg *config.City, a config.Agent) bool {
	for _, ca := range cfg.Agents {
		if !isMultiSessionCfgAgent(&ca) {
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
		if isMultiSessionCfgAgent(&a) {
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
func buildPrimeContext(cityPath string, a *config.Agent, rigs []config.Rig) PromptContext {
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
	ctx.WorkQuery = a.EffectiveWorkQuery()
	ctx.SlingQuery = a.EffectiveSlingQuery()
	return ctx
}
