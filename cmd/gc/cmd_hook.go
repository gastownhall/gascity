package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func newHookCmd(stdout, stderr io.Writer) *cobra.Command {
	var inject bool
	var hookFormat string
	var claim bool
	var drainAck bool
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "hook [agent]",
		Short: "Find routed work for an agent",
		Long: `Finds routed work using the agent's work_query config.

Without --inject: prints normalized ready-only output, exits 0 if work exists, 1 if empty.
With --inject: silent legacy Stop-hook compatibility; skips the work query and always exits 0.
With --claim: runs the standard startup claim protocol for one work item.

		The agent is determined from $GC_AGENT or a positional argument.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			opts := hookCommandOptions{
				Inject:     inject,
				HookFormat: hookFormat,
				Claim:      claim,
				DrainAck:   drainAck,
				JSON:       jsonOut,
			}
			if cmdHookWithOptions(args, opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&inject, "inject", false, "silent legacy Stop-hook compatibility; skip work query and exit 0")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	cmd.Flags().BoolVar(&claim, "claim", false, "atomically claim one routed work item for the current session")
	cmd.Flags().BoolVar(&drainAck, "drain-ack", false, "with --claim, acknowledge runtime drain when no work is available")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "with --claim, emit a JSON protocol result")
	if flag := cmd.Flags().Lookup("hook-format"); flag != nil {
		flag.Hidden = true
	}
	cmd.AddCommand(newHookRunCmd(stdout, stderr))
	return cmd
}

func newHookRunCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := hookRunOptions{
		Timeout:         defaultHookRunTimeout,
		TimeoutExitCode: 124,
	}
	cmd := &cobra.Command{
		Use:   "run -- <gc args...>",
		Short: "Run a managed hook command with a hard timeout",
		Long: `Runs a managed gc hook command in a child process with a hard timeout.

This protects provider hook callbacks from wedged data-plane commands. The
child process is the current gc executable, and <gc args...> are passed to it
verbatim.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc hook run: missing gc command arguments after --") //nolint:errcheck
				return errExit
			}
			return exitForCode(cmdHookRun(args, opts, c.InOrStdin(), stdout, stderr))
		},
	}
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", defaultHookRunTimeout, "hard timeout for the managed hook command")
	cmd.Flags().IntVar(&opts.TimeoutExitCode, "timeout-exit-code", 124, "exit code to return when the managed hook command times out")
	return cmd
}

const defaultHookRunTimeout = 15 * time.Second

type hookRunOptions struct {
	Timeout         time.Duration
	TimeoutExitCode int
}

var hookRunExecutable = os.Executable

func cmdHookRun(args []string, opts hookRunOptions, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "gc hook run: missing gc command arguments") //nolint:errcheck
		return 1
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultHookRunTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	exe, err := hookRunExecutable()
	if err != nil {
		fmt.Fprintf(stderr, "gc hook run: resolving gc executable: %v\n", err) //nolint:errcheck
		return 1
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	// Forward the provider hook stdin so wrapped commands like
	// `nudge drain --inject` still receive the UserPromptSubmit JSON
	// (carrying transcript_path) they need for context-pressure injection.
	// readHookStdin already bounds the read with an io.LimitReader and the
	// hard timeout below bounds any block, so forwarding is safe.
	cmd.Stdin = stdin
	// Buffer child stdout instead of streaming it straight to the provider so
	// a wedged command cannot leak partial injectable output before the
	// fail-open timeout path runs. The buffer is flushed only on a clean or
	// self-determined exit, and discarded on timeout.
	var childOut bytes.Buffer
	cmd.Stdout = &childOut
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	prepareProviderOpCommand(cmd)

	err = cmd.Run()
	// A clean exit wins even if the deadline fired in the same instant: the
	// child finished and produced complete output, so report success and flush.
	if err == nil {
		_, _ = stdout.Write(childOut.Bytes()) //nolint:errcheck
		return 0
	}
	if ctx.Err() == context.DeadlineExceeded {
		// Timed out: the child was killed mid-flight, so any buffered output is
		// partial. Discard it and return the configured fail-open code.
		fmt.Fprintf(stderr, "gc hook run: command timed out after %s\n", timeout) //nolint:errcheck
		return opts.TimeoutExitCode
	}
	// The child exited on its own with a non-zero status: its output is
	// complete, so preserve it and propagate the exit code.
	_, _ = stdout.Write(childOut.Bytes()) //nolint:errcheck
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	fmt.Fprintf(stderr, "gc hook run: %v\n", err) //nolint:errcheck
	return 1
}

type hookCommandOptions struct {
	Inject     bool
	HookFormat string
	Claim      bool
	DrainAck   bool
	JSON       bool
}

// cmdHook is the CLI entry point for gc hook. Resolves the agent from
// $GC_AGENT or a positional argument, loads the city config, and runs
// the agent's work query.
func cmdHook(args []string, stdout, stderr io.Writer) int {
	return cmdHookWithFormat(args, false, "", stdout, stderr)
}

func cmdHookWithFormat(args []string, inject bool, hookFormat string, stdout, stderr io.Writer) int {
	return cmdHookWithOptions(args, hookCommandOptions{Inject: inject, HookFormat: hookFormat}, stdout, stderr)
}

func cmdHookWithOptions(args []string, opts hookCommandOptions, stdout, stderr io.Writer) int {
	if opts.Inject {
		return 0
	}
	// Accepted for compatibility with installed hook commands; non-inject
	// gc hook output ignores provider-specific formatting.
	_ = opts.HookFormat
	if opts.DrainAck && !opts.Claim {
		fmt.Fprintln(stderr, "gc hook: --drain-ack requires --claim") //nolint:errcheck
		return 1
	}

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
	if agentName == "" {
		fmt.Fprintln(stderr, "gc hook: agent not specified (set $GC_AGENT or pass as argument)") //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc hook: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Normalize relative rig paths to absolute so downstream rig-matching
	// (agentCommandDir, bdRuntimeEnvForRig) compares apples to apples.
	// Other CLI entry points (cmd_sling, cmd_start, cmd_rig, cmd_supervisor)
	// do the same immediately after loadCityConfig.
	resolveRigPaths(cityPath, cfg.Rigs)

	st, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
	if citySuspendedWithState(cfg, st) {
		fmt.Fprintln(stderr, "gc hook: city is suspended") //nolint:errcheck // best-effort stderr
		return 1
	}

	a, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
	if !ok {
		// Pool instances run with GC_AGENT/GC_ALIAS set to their per-instance name
		// (e.g. "rig/polecat-adhoc-<hash>") which is not a config entry — only the
		// pool binding (GC_TEMPLATE, e.g. "rig/polecat") is. When a pack script
		// invokes "gc hook $GC_AGENT" the positional arg bypasses the no-args
		// sessionTemplateContext fallback. Retry with GC_TEMPLATE so pool agents
		// resolve correctly regardless of invocation style.
		//
		// Gate the retry to the runtime/session identity case: only fall back
		// when the unresolved arg is this instance's own runtime name
		// (GC_ALIAS/GC_AGENT/GC_SESSION_NAME). Otherwise an unrelated bad
		// explicit target in a pool session would silently reinterpret as the
		// template agent instead of erroring.
		isRuntimeIdentity := agentName == strings.TrimSpace(os.Getenv("GC_ALIAS")) ||
			agentName == strings.TrimSpace(os.Getenv("GC_AGENT")) ||
			agentName == strings.TrimSpace(os.Getenv("GC_SESSION_NAME"))
		if tpl := strings.TrimSpace(os.Getenv("GC_TEMPLATE")); tpl != "" && tpl != agentName && isRuntimeIdentity {
			if ta, tok := resolveAgentIdentity(cfg, tpl, currentRigContext(cfg)); tok {
				a, ok = ta, true
				agentName = tpl
				if !sessionTemplateContext {
					sessionTemplateContext = strings.TrimSpace(os.Getenv("GC_SESSION_NAME")) != "" ||
						strings.TrimSpace(os.Getenv("GC_SESSION_ID")) != ""
				}
			}
		}
	}
	if !ok {
		fmt.Fprintf(stderr, "gc hook: agent %q not found in config\n", agentName) //nolint:errcheck // best-effort stderr
		return 1
	}

	if isAgentEffectivelySuspendedWith(cfg, &a, st) {
		fmt.Fprintf(stderr, "gc hook: agent %q is suspended\n", agentName) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityName := loadedCityName(cfg, cityPath)
	workQuery := a.EffectiveWorkQueryForBeads(cfg.Beads)
	// Expand {{.Rig}}/{{.AgentBase}} in user-supplied work_query so agent-side
	// hook invocation sees the same rig substitution as the controller-side
	// probes in build_desired_state.go / session_reconcile.go. #793.
	workQuery = expandAgentCommandTemplate(cityPath, cityName, &a, cfg.Rigs, "work_query", workQuery, stderr)
	workDir := agentCommandDir(cityPath, &a, cfg.Rigs)

	// Build the work query subprocess environment. Rig-backed agents get
	// rig-scoped BEADS_DIR / GC_RIG_ROOT / Dolt coordinates so the query
	// reads the rig store rather than whatever BEADS_DIR the parent
	// process happens to inherit (issue #514). Many built-in work queries
	// also key off session identity. Explicit hook targets get resolved
	// names; named-session context preserves the runtime-supplied owner
	// env while selecting the backing config through GC_TEMPLATE.
	resolvedAgentName := a.QualifiedName()
	agentForQuery := resolvedAgentName
	sessionForQuery := ""
	if sessionTemplateContext {
		agentForQuery = os.Getenv("GC_ALIAS")
		if agentForQuery == "" {
			agentForQuery = os.Getenv("GC_SESSION_NAME")
		}
		if agentForQuery == "" {
			agentForQuery = os.Getenv("GC_AGENT")
		}
		sessionForQuery = os.Getenv("GC_SESSION_NAME")
	} else {
		sessionForQuery = cliSessionName(cityPath, cityName, resolvedAgentName, cfg.Workspace.SessionTemplate)
	}
	overrides, err := hookQueryEnv(cityPath, cfg, &a)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook: building work query env: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	overrides["GC_AGENT"] = agentForQuery
	overrides["GC_SESSION_NAME"] = sessionForQuery
	if sessionTemplateContext {
		overrides["GC_ALIAS"] = os.Getenv("GC_ALIAS")
		overrides["GC_SESSION_ID"] = os.Getenv("GC_SESSION_ID")
		overrides["GC_SESSION_ORIGIN"] = os.Getenv("GC_SESSION_ORIGIN")
		overrides["GC_TEMPLATE"] = os.Getenv("GC_TEMPLATE")
	} else {
		overrides["GC_ALIAS"] = resolvedAgentName
		overrides["GC_SESSION_ID"] = ""
		overrides["GC_SESSION_ORIGIN"] = ""
		overrides["GC_TEMPLATE"] = ""
	}
	queryEnv := mergeRuntimeEnv(os.Environ(), overrides)
	failureTemplate, emitFailureEvent := hookWorkQueryFailureTemplate(len(args) > 0, sessionTemplateContext, a.QualifiedName())

	// A cross-store-eligible (city-scoped) agent federates its work query across
	// all stores — its own first, then every rig store — matched on its own
	// identity (vp-kvp stage iii). A rig-scoped agent ("<rig>/<name>") instead
	// queries its own <rig> store FIRST: its routed work lives there, but its
	// city-scoped work-query env does not reach it, so without this the hook
	// returns empty and the spawned session exits with nothing to do. The rig
	// store goes first (as the primary entry, not a best-effort federated
	// extra) so a rig-store work-query timeout still surfaces to the reconciler
	// via firstStoreWithWork's emit-on-timeout contract — the agent's
	// (work-less) city-scoped env stays as a best-effort secondary. This
	// extends the #2877 city-scoped cross-store delivery to rig-scoped agents.
	stores := []hookStore{{dir: workDir, env: queryEnv}}
	if agentIsCrossStoreEligible(&a) {
		stores = appendRigHookStores(stores, cityPath, cfg, &a, overrides)
	} else if rig := rigScopedHookRig(cfg, agentForQuery); rig != "" {
		if rigStores := appendOneRigHookStore(nil, cityPath, cfg, &a, rig, overrides); len(rigStores) > 0 {
			stores = append(rigStores, stores...)
		}
		// A rig-backed agent's own env above is ALSO rig-scoped, so without
		// this no entry reaches the CITY store and root-only beads assigned
		// to the agent stay invisible. Best-effort tertiary; see
		// appendCityHookStore.
		stores = appendCityHookStore(stores, cityPath, cfg, &a, overrides)
	}

	runner := func(command, _ string) (string, error) {
		out, _, err := firstStoreWithWork(command, stores, shellWorkQueryWithEnv)
		if err != nil && emitFailureEvent {
			// A killed/timed-out work query strands the session with no
			// output and no cause on the event bus; emit one so the
			// reconciler can escalate instead of skipping it forever
			// (issues #1496/#1497). Ordinary command errors are ignored
			// by emitWorkQueryFailure and stay on the stderr path below.
			emitCityWorkQueryFailure(cityPath, stderr,
				os.Getenv("GC_SESSION_ID"), failureTemplate, command, err)
		}
		return out, err
	}
	if opts.Claim {
		sessionID := strings.TrimSpace(overrides["GC_SESSION_ID"])
		sessionName := strings.TrimSpace(sessionForQuery)
		alias := strings.TrimSpace(overrides["GC_ALIAS"])
		assignee := firstNonEmptyHookValue(sessionName, sessionID, alias, agentForQuery, resolvedAgentName)
		routeTarget := hookClaimPrimaryRouteTarget(&a)
		claimOpts := hookClaimOptions{
			Assignee: assignee,
			IdentityCandidates: hookClaimIdentityCandidates(
				assignee,
				sessionID,
				sessionName,
				alias,
				agentForQuery,
				resolvedAgentName,
			),
			RouteTargets: hookClaimRouteTargets(routeTarget, resolvedAgentName, strings.TrimSpace(overrides["GC_TEMPLATE"])),
			Env:          queryEnv,
			DrainAck:     opts.DrainAck,
			JSON:         opts.JSON,
		}
		_, selectedStore, err := firstStoreWithWork(workQuery, stores, shellWorkQueryWithEnv)
		if err != nil {
			if emitFailureEvent {
				emitCityWorkQueryFailure(cityPath, stderr,
					os.Getenv("GC_SESSION_ID"), failureTemplate, workQuery, err)
			}
			fmt.Fprintf(stderr, "gc hook --claim: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		storeDir := workDir
		storeEnv := queryEnv
		if strings.TrimSpace(selectedStore.dir) != "" {
			storeDir = selectedStore.dir
		}
		if len(selectedStore.env) > 0 {
			storeEnv = selectedStore.env
		}
		claimOpts.Env = storeEnv
		claimRunner := func(command, _ string) (string, error) {
			return shellWorkQueryWithEnv(command, storeDir, storeEnv)
		}
		return doHookClaim(workQuery, storeDir, claimOpts, hookClaimOps{Runner: claimRunner}, stdout, stderr)
	}
	return doHook(workQuery, workDir, false, runner, stdout, stderr)
}

func hookClaimPrimaryRouteTarget(a *config.Agent) string {
	if a == nil {
		return ""
	}
	if target := strings.TrimSpace(a.PoolName); target != "" {
		return target
	}
	return a.QualifiedName()
}

func firstNonEmptyHookValue(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func hookWorkQueryFailureTemplate(explicitTarget, sessionTemplateContext bool, resolvedAgentName string) (string, bool) {
	currentTemplate := strings.TrimSpace(os.Getenv("GC_TEMPLATE"))
	resolvedAgentName = strings.TrimSpace(resolvedAgentName)
	if explicitTarget {
		if currentTemplate == "" || currentTemplate != resolvedAgentName {
			return "", false
		}
		return currentTemplate, true
	}
	if currentTemplate != "" && (sessionTemplateContext || strings.TrimSpace(os.Getenv("GC_SESSION_ID")) != "") {
		return currentTemplate, true
	}
	return resolvedAgentName, true
}

// hookQueryEnv returns the full work-query environment for a hook subprocess.
// It includes scope metadata (store root/scope/prefix) plus any rig-scoped
// runtime overrides so hook queries observe the same routing contract as the
// controller probes.
func hookQueryEnv(cityPath string, cfg *config.City, a *config.Agent) (map[string]string, error) {
	env, err := controllerWorkQueryEnv(cityPath, cfg, a)
	if err != nil {
		return nil, err
	}
	if env == nil {
		env = map[string]string{}
	}
	return env, nil
}

// WorkQueryRunner runs a work query command and returns its stdout.
// dir sets the command's working directory.
type WorkQueryRunner func(command, dir string) (string, error)

// hookWorkQueryTimeout caps the work-query subprocess that `gc hook` and the
// workflow serve loop run via shellWorkQueryWithEnv. The default work-probe
// issues ~6 sequential bd/store round-trips before the pool-demand tier that
// finds routed work; on a multi-rig dolt city under concurrent load the probe
// intermittently exceeded the prior 30s cap, so shellWorkQueryWithEnv killed it
// and pool operators were starved of routed work. Raised to 60s to cover the
// realistic loaded cost. This is independent of defaultHookRunTimeout, which
// bounds the `gc hook run` managed-hook wrapper (around nudge drain / mail
// check) and does not enclose this work query. The package-level var lets us
// lower it again once the probe's round-trip count is reduced and the slow
// per-rig `bd ready`/`gc ready` paths are optimized.
var hookWorkQueryTimeout = 60 * time.Second

// shellWorkQueryWithEnv runs a work query command via sh -c and returns
// stdout. If env is non-nil it is used as the subprocess environment
// (including any rig-scoped BEADS_DIR / GC_RIG_ROOT overrides); otherwise
// the child inherits the parent process environment. Times out after a
// short bounded interval so startup hooks cannot strand sessions behind a
// wedged data-plane command.
func shellWorkQueryWithEnv(command, dir string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hookWorkQueryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.WaitDelay = 2 * time.Second
	prepareProviderOpCommand(cmd)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = workQueryEnvForDir(env, dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		// Wrap context.DeadlineExceeded so callers can classify the timeout as
		// transient (dispatch.IsTransientControllerError / errors.Is). Without
		// this, a work-query timeout reads as an opaque fatal error and kills
		// long-running consumers like the control-dispatcher --follow loop even
		// though the timeout is just transient bead-store load. The human-facing
		// "timed out after" text is preserved.
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return string(out), fmt.Errorf("running work query %q: timed out after %s with partial stdout %q: %w", command, hookWorkQueryTimeout, msg, context.DeadlineExceeded)
		}
		return "", fmt.Errorf("running work query %q: timed out after %s: %w", command, hookWorkQueryTimeout, context.DeadlineExceeded)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("running work query %q: %w: %s", command, err, msg)
		}
		return "", fmt.Errorf("running work query %q: %w", command, err)
	}
	return string(out), nil
}

// workQueryEnvForDir ensures the subprocess environment does not carry a
// stale inherited PWD when exec.Cmd.Dir points somewhere else. Some shells
// (notably macOS /bin/sh) preserve the inherited PWD instead of recomputing
// it from the real working directory, which breaks hook work_query commands
// that inspect $PWD.
func workQueryEnvForDir(env []string, dir string) []string {
	if env == nil {
		env = mergeRuntimeEnv(os.Environ(), nil)
	}
	if dir == "" {
		return env
	}
	out := removeEnvKey(append([]string(nil), env...), "PWD")
	return append(out, "PWD="+dir)
}

// doHook is the pure logic for gc hook. Runs the work query and outputs
// results based on mode. Without inject: prints normalized ready-only output,
// returns 0 if work exists, 1 if empty. With inject: skips the work query and
// returns 0.
func doHook(workQuery, dir string, inject bool, runner WorkQueryRunner, stdout, stderr io.Writer) int {
	if inject {
		return 0
	}

	output, err := runner(workQuery, dir)
	if err != nil {
		if normalized := normalizeWorkQueryOutput(strings.TrimSpace(output)); normalized != "" {
			fmt.Fprint(stdout, normalized) //nolint:errcheck // best-effort stdout
		}
		fmt.Fprintf(stderr, "gc hook: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	trimmed := strings.TrimSpace(output)
	normalized := normalizeWorkQueryOutput(trimmed)
	normalized = filterUnreadyHookCandidates(normalized, time.Now())
	hasWork := workQueryHasReadyWork(normalized)

	// Non-inject mode: print normalized, ready-only output. Return 0 only when work exists.
	if !hasWork {
		if normalized != "" {
			fmt.Fprint(stdout, normalized) //nolint:errcheck // best-effort stdout
		}
		return 1
	}
	fmt.Fprint(stdout, normalized) //nolint:errcheck // best-effort stdout
	return 0
}

func workQueryHasReadyWork(output string) bool {
	if output == "" {
		return false
	}
	// Newer bd versions print a human-readable no-work line to stdout instead
	// of staying silent. Treat that as "no work" for hooks and WakeWork.
	if strings.Contains(output, "No ready work found") {
		return false
	}
	var decoded any
	if err := json.Unmarshal([]byte(output), &decoded); err == nil {
		switch v := decoded.(type) {
		case []any:
			return len(v) > 0
		case map[string]any:
			return len(v) > 0
		case nil:
			return false
		}
	}
	return true
}

// filterUnreadyHookCandidates strips beads from work_query output that fail
// bd ready semantics: future defer_until, or any open blocking dep in the
// row's blocked_by array. The work_query is expected to gate these, but
// defensive filtering here prevents a single broken query from cascading
// into agent action on a bead it cannot progress.
// Pure function over JSON; takes time.Time so tests stay deterministic.
func filterUnreadyHookCandidates(output string, now time.Time) string {
	if output == "" {
		return output
	}
	var decoded any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		return output
	}
	arr, ok := decoded.([]any)
	if !ok {
		return output
	}
	filtered := make([]any, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		if isFutureDeferredHookCandidate(obj, now) {
			continue
		}
		if isDepBlockedHookCandidate(obj) {
			continue
		}
		filtered = append(filtered, obj)
	}
	reencoded, err := json.Marshal(filtered)
	if err != nil {
		return output
	}
	return string(reencoded)
}

func isFutureDeferredHookCandidate(item map[string]any, now time.Time) bool {
	raw, ok := item["defer_until"].(string)
	if !ok {
		return false
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	deferAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	return deferAt.After(now)
}

func isDepBlockedHookCandidate(item map[string]any) bool {
	blockedBy, ok := item["blocked_by"].([]any)
	if !ok || len(blockedBy) == 0 {
		return false
	}
	for _, b := range blockedBy {
		dep, ok := b.(map[string]any)
		if !ok {
			continue
		}
		status, ok := dep["status"].(string)
		if !ok {
			continue
		}
		status = strings.TrimSpace(status)
		if status != "" && !strings.EqualFold(status, "closed") {
			return true
		}
	}
	return false
}

func normalizeWorkQueryOutput(output string) string {
	if output == "" {
		return output
	}
	var decoded any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		return output
	}
	if _, ok := decoded.(map[string]any); !ok {
		return output
	}
	normalized, err := json.Marshal([]any{decoded})
	if err != nil {
		return output
	}
	return string(normalized)
}
