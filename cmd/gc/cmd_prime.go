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

// newPrimeCmd creates the "gc prime [agent-name]" command.
func newPrimeCmd(stdout, stderr io.Writer) *cobra.Command {
	var hookMode bool
	var hookFormat string
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
that template is output. Otherwise outputs a default worker prompt.`,
		Args: cobra.MaximumNArgs(1),
	}
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		if doPrimeWithHookFormat(args, stdout, stderr, hookMode, hookFormat) != 0 {
			return errExit
		}
		return nil
	}
	cmd.Flags().BoolVar(&hookMode, "hook", false, "compatibility mode for runtime hook invocations")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	return cmd
}

// doPrime is the pure logic for "gc prime". Looks up the agent name in
// city.toml and outputs the corresponding prompt template. Falls back to
// the default run-once prompt if no match is found or no city exists.
func doPrime(args []string, stdout, stderr io.Writer) int { //nolint:unparam // always returns 0 by design (graceful fallback)
	return doPrimeWithMode(args, stdout, stderr, false)
}

func doPrimeWithMode(args []string, stdout, stderr io.Writer, hookMode bool) int { //nolint:unparam // always returns 0 by design (graceful fallback)
	return doPrimeWithHookFormat(args, stdout, stderr, hookMode, "")
}

func doPrimeWithHookFormat(args []string, stdout, stderr io.Writer, hookMode bool, hookFormat string) int { //nolint:unparam // always returns 0 by design (graceful fallback)
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
	if hookMode {
		if sessionID, _ := readPrimeHookContext(); sessionID != "" {
			persistPrimeHookSessionID(sessionID)
		}
		persistPrimeHookProviderSessionKey()
	}

	// Try to find city and load config.
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprint(stdout, defaultPrimePrompt) //nolint:errcheck // best-effort stdout
		return 0
	}
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprint(stdout, defaultPrimePrompt) //nolint:errcheck // best-effort stdout
		return 0
	}
	resolveRigPaths(cityPath, cfg.Rigs)

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
	// whose config name is "polecat" regardless of dir). Hook-driven manual
	// sessions may have GC_ALIAS set to a user-facing alias that is not an
	// agent name, so also try GC_TEMPLATE before falling back to the generic
	// run-once prompt.
	agentCandidates := primeAgentCandidates(agentName, hookMode, cityPath)
	for _, candidate := range agentCandidates {
		a, ok := resolveAgentIdentity(cfg, candidate, currentRigContext(cfg))
		if !ok {
			a, ok = findAgentByName(cfg, candidate)
		}
		if ok && isAgentEffectivelySuspended(cfg, &a) {
			return 0 // suspended agent gets no prompt
		}
		if ok {
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
		}
		var ctx PromptContext
		if ok && (a.PromptTemplate != "" || hookMode || sessionTemplateContext) {
			ctx = buildPrimeContext(cityPath, &a, cfg.Rigs)
		}
		if ok && a.PromptTemplate != "" {
			fragments := mergeFragmentLists(cfg.Workspace.GlobalFragments, a.InjectFragments)
			prompt := renderPrompt(fsys.OSFS{}, cityPath, cityName, a.PromptTemplate, ctx, cfg.Workspace.SessionTemplate, stderr,
				cfg.PackDirs, fragments, nil)
			if prompt != "" {
				writePrimePromptWithFormat(stdout, cityName, ctx.AgentName, prompt, hookMode, hookFormat)
				return 0
			}
		}
		// Agents without a prompt_template: read a builtin prompt shipped by
		// the core bootstrap pack, materialized under .gc/system/packs/core/.
		// When formula_v2 is enabled, all agents use graph-worker.md.
		// Otherwise pool agents use pool-worker.md.
		// Pool instances have Pool=nil after resolution, so also check the
		// template agent via findAgentByName.
		if ok && a.PromptTemplate == "" {
			promptFile := ""
			if cfg.Daemon.FormulaV2 {
				promptFile = citylayout.SystemPacksRoot + "/core/assets/prompts/graph-worker.md"
			} else if a.SupportsInstanceExpansion() || isPoolInstance(cfg, a) {
				promptFile = citylayout.SystemPacksRoot + "/core/assets/prompts/pool-worker.md"
			}
			if promptFile != "" {
				if content, fErr := os.ReadFile(filepath.Join(cityPath, promptFile)); fErr == nil {
					writePrimePromptWithFormat(stdout, cityName, ctx.AgentName, string(content), hookMode, hookFormat)
					return 0
				}
			}
		}
	}

	// Fallback: default run-once prompt.
	fmt.Fprint(stdout, defaultPrimePrompt) //nolint:errcheck // best-effort stdout
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

func writePrimePromptWithFormat(stdout io.Writer, cityName, agentName, prompt string, hookMode bool, hookFormat string) {
	if hookMode {
		prompt = prependHookBeacon(cityName, agentName, prompt)
	}
	if hookMode && hookFormat != "" {
		_ = writeProviderHookContext(stdout, hookFormat, prompt)
		return
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
