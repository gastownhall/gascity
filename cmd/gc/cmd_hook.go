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
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
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

Without --inject: prints normalized ready-only output, exits 0 if work exists,
1 if the queue is empty, and 2 if the work query itself failed (for example bd
missing from PATH, a store bd could not read, or a store environment that could
not be built); the failure is named on stderr. A suspended city, rig or agent,
and an error before the work query is built (no agent named, a city or config
that cannot be loaded, an unknown agent), also exit 1.
With --inject: silent legacy Stop-hook compatibility; skips the work query and always exits 0.
With --claim: runs the standard startup claim protocol for one work item. A
failed work query is an error (nothing on stdout), never a drain result.

A work query that gets no answer from the bead store (a connection error, a
timeout, a killed query, a locked database, or bd's circuit breaker failing
fast) exits 2 and prints GC_HOOK_STORE_UNAVAILABLE on stderr: the answer is
unknown, not empty. This applies with and without --claim. The answers that
count as "no answer" are shared with the storage gate's transient faults and
include a hosted bd serve answering 502, 503 or 504, "cannot reach bd serve",
a killed bd ("signal: killed"), "too many open files", network timeouts and
unreachable hosts (EHOSTUNREACH), and EMFILE/EAGAIN; each of these exits 2
with the token (stdout keeps any partial output). Any other
work-query failure exits 2 without --claim and 1 with --claim. Neither writes a
drain result. When another store the hook searches serves work, a store that
failed is named on stderr and the work is served; when no store has work, a
failed read of any of them is a failed query, not an empty queue, so a rig
store that cannot be read keeps every city-scoped agent that searches it from
draining until it answers again. Two kinds of rig are exceptions: a suspended
rig is not searched at all (gc ready does not read it either), and a rig whose
managed Dolt server is not running is named on stderr but does not hold the
answer up. A failed query is also recorded as a session.work_query_failed event,
one per store that failed, naming it.

Before the work query, a session keyed by $GC_SESSION_ID is checked for a
pending drain (the drain fence), with and without --claim. A draining session
gets the drain result instead of work. If the session row cannot be read, the
fence depends on why. A store fault that may clear on its own (a timeout, a
locked database, a work store the storage gate could not read) fails open:
the hook proceeds, names the fault on stderr, and records a
session.drain_fence_unavailable event. A standing storage refusal (the city's
storage gate refuses to serve its relocated classes, for example a retained
work store holding infrastructure beads the binding cannot read) fails closed:
nothing is claimed or served, GC_HOOK_STORE_UNAVAILABLE is printed on stderr
with the refusal, the hook exits 2, and the event says the fence failed
closed.

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
			// exitForCode, not `!= 0 → errExit`: the store-unavailable
			// contract is exit 2 (reportWorkQueryFailure), and folding it
			// into errExit renders it as the no-work exit 1 at the process
			// boundary — the idle-agents-with-work-waiting dead-drop.
			return exitForCode(cmdHookWithOptions(args, opts, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&inject, "inject", false, "silent legacy Stop-hook compatibility; skip work query and exit 0")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	cmd.Flags().BoolVar(&claim, "claim", false, "atomically claim one routed work item for the current session")
	cmd.Flags().BoolVar(&drainAck, "drain-ack", false, "with --claim, acknowledge runtime drain when no work is available")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit a JSON protocol result (always with --claim; on the discovery door only for a drain refusal)")
	if flag := cmd.Flags().Lookup("hook-format"); flag != nil {
		flag.Hidden = true
	}
	cmd.AddCommand(newHookRunCmd(stdout, stderr))
	cmd.AddCommand(newHookCurrentCmd(stdout, stderr))
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
	// Mark the child as a provider CALLBACK lane. gc hook run is the managed
	// wrapper every rendered provider hook command flows through, and it runs
	// arbitrary gc argv verbatim — nothing stops `hook --claim` appearing there,
	// today by operator edit and tomorrow by a new overlay. A callback's stdout
	// goes to the hook runner, never to a model, so a claim minted in one is
	// parked the instant it is won; the claim path refuses on this marker (F-A,
	// hookClaimNonTurnMarker). Every other hook use of a callback lane —
	// --inject, nudge drain, mail check — is read-only and unaffected.
	//
	// The child also gets the deadline its output must beat (hookDeadlineEnv),
	// so it can skip a destructive step it would not finish in time
	// (childHookDeadline).
	cmd.Env = append(removeEnvKey(os.Environ(), hookDeadlineEnv),
		"GC_HOOK_CALLBACK_LANE=1", hookDeadlineEnv+"="+hookDeadlineEnvValue(childHookDeadline(ctx)))
	// Read the provider's hook stdin FULLY into a buffer before running the
	// wrapped command, then hand it that buffer. Forwarding the live stdin
	// (cmd.Stdin = stdin) let the wrapped command exit — on its fast path or on
	// the timeout — before consuming the payload, so gc hook run returned and
	// closed the pipe under the provider's in-flight write. Codex surfaced that
	// fleet-wide as "UserPromptSubmit hook (failed): failed to write hook stdin:
	// Broken pipe (os error 32)", silently killing nudge-drain and mail-check
	// injection on every prompt submit. Buffering up front guarantees the
	// provider's write always completes regardless of the wrapped command. The
	// 1<<20 bound matches readHookStdin, so `nudge drain --inject` still sees the
	// same UserPromptSubmit JSON (carrying transcript_path) for context
	// injection.
	//
	// drainHookStdin skips an interactive/inherited terminal (a char-device
	// stdin never EOFs, so a manual gc hook run must not drain it) and bounds the
	// pipe drain by ctx: os.Stdin carries no read deadline, so a provider pipe
	// that writes < 1 MiB and never closes (no EOF) would otherwise block this
	// read forever — before cmd.Run() and past the hard timeout — freezing the
	// prompt-submit hot path. Bounding it keeps the "gc hook run is always
	// bounded by the timeout" invariant enforced in code rather than assumed of
	// every present and future provider. On the deadline drainHookStdin returns
	// with ctx already expired, so cmd.Run() sees the canceled context and never
	// spawns the child: gc hook run fails open to the timeout exit code in the
	// timeout branch below instead of hanging before it spawns.
	cmd.Stdin = bytes.NewReader(drainHookStdin(ctx, stdin))
	// Buffer child stdout instead of streaming it straight to the provider so
	// a wedged command cannot leak partial injectable output before the
	// fail-open timeout path runs. The buffer is flushed only on a clean or
	// self-determined exit, and discarded on timeout.
	var childOut bytes.Buffer
	cmd.Stdout = &childOut
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	prepareProviderOpCommand(cmd)
	disableProductMetricsForChild(cmd)

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

// drainHookStdin reads up to 1 MiB of the provider's hook stdin, bounded by ctx.
// A normal provider write-then-close returns the full payload well before the
// timeout; a pipe that writes less than the limit and stays open (no EOF) is
// abandoned when ctx's hard timeout fires, so gc hook run cannot wedge before it
// even spawns the child. The read runs in a goroutine that can outlive a
// timed-out call: that is safe because gc hook run is a short-lived process
// which exits right after, and the buffered channel keeps the goroutine from
// blocking on send if it later unblocks. A partial buffer still lets the wrapped
// command run.
//
// An interactive or inherited terminal is skipped entirely, matching
// readHookStdin: a char-device stdin never reaches EOF on its own, so draining
// it would only unblock when ctx's hard timeout fired, handing the child an
// empty reader after gc hook run had already burned its whole budget — a manual
// `gc hook run -- <cmd>` would then time out without ever running <cmd>. Provider
// hooks always arrive on a pipe, which this guard leaves buffered and
// timeout-bounded.
func drainHookStdin(ctx context.Context, stdin io.Reader) []byte {
	if stdin == nil {
		return nil
	}
	if f, ok := stdin.(*os.File); ok {
		if st, err := f.Stat(); err != nil || st.Mode()&os.ModeCharDevice != 0 {
			return nil
		}
	}
	done := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(io.LimitReader(stdin, 1<<20)) //nolint:errcheck // best-effort; a partial read still lets the wrapped command run
		done <- data
	}()
	select {
	case data := <-done:
		return data
	case <-ctx.Done():
		return nil
	}
}

type hookCommandOptions struct {
	Inject     bool
	HookFormat string
	Claim      bool
	DrainAck   bool
	JSON       bool
	DrainAckFn hookDrainAckFunc
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
	// One composed city config for the whole invocation. The front door loads
	// it, and the claim's helpers and the drain-ack load it again on their own;
	// each composition walks city.toml and every pack include, ~0.15-0.2 s on
	// maintainer-city (see beginInvocationConfigMemo).
	defer beginInvocationConfigMemo()()
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

	// The rig context `gc hook` resolves agents against is its own
	// (currentRigContext), so the city resolution skips the full config load a
	// rig name costs.
	cityPath, err := resolveCityPathOnly()
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
	city := newHookCity(cityPath, cfg)

	// Fence a stale/superseded/unregistered runtime session BEFORE the
	// city-suspension, agent-resolution, and agent-suspension early returns
	// below. A stale incarnation in a suspended city, or one whose template was
	// removed from config (resolveAgentIdentity fails), or whose agent was
	// suspended, would otherwise hit one of those bare `return 1` paths, and its
	// startup wrapper would keep retrying the plain failure instead of seeing
	// the terminal drain result and exiting. The fence reads the runtime's own
	// identity from the environment; it is a no-op for a genuinely non-session
	// runtime (no GC_TEMPLATE and no GC_SESSION_ID) and fails open for an
	// eligible session, a session id present with no instance token (the
	// documented tokenless-runtime compatibility escape hatch), or a transient
	// session-store fault, so a healthy worker still falls through to the
	// suspension and config checks below.
	// fencedSession is the session bead the claim fence verified as this
	// runtime's current incarnation (nil when no fence ran or it failed open).
	// It is what lets the claim honor an identity the supervisor collapsed onto
	// the session after its runtime env was stamped (hookClaimCollapsedIdentity).
	var fencedSession *session.Info
	if opts.Claim {
		// F-A, at the earliest point that can answer it. tryHookClaim carries the
		// same fence over the same predicate — it is the seam every ops-level
		// caller funnels through — but by the time it runs, the federated store
		// selection has already spent a work query bounded by hookWorkQueryTimeout
		// (150s), and a provider callback's whole budget is 15s
		// (defaultHookRunTimeout). Refusing here keeps a callback lane cheap and
		// makes its refusal something the provider actually receives rather than
		// something its timeout truncates.
		if marker := hookClaimNonTurnMarker(os.Environ()); marker != "" {
			return writeHookClaimNonTurnDrain(marker, hookClaimOptions{JSON: opts.JSON}, stdout, stderr)
		}
		code, handled, info := fenceHookClaimSession(city, strings.TrimSpace(os.Getenv("GC_SESSION_ID")), opts, stdout, stderr)
		if handled {
			return code
		}
		fencedSession = info
	}

	st, err := loadSuspensionState(fsys.OSFS{}, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook: loading suspension state: %v\n", err) //nolint:errcheck
		return 1
	}
	if citySuspendedWithState(cfg, st) {
		fmt.Fprintln(stderr, "gc hook: city is suspended") //nolint:errcheck // best-effort stderr
		if opts.Claim {
			return writeHookClaimSuspensionDrain(hookClaimReasonCitySuspended, opts, stdout, stderr)
		}
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

	if scope, name, suspended := agentSuspensionCauseWith(cfg, cityPath, &a, st); suspended {
		reason := hookClaimReasonAgentSuspended
		if scope == "rig" {
			fmt.Fprintf(stderr, "gc hook: rig %q is suspended\n", name) //nolint:errcheck
			reason = hookClaimReasonRigSuspended
		} else {
			fmt.Fprintf(stderr, "gc hook: agent %q is suspended\n", agentName) //nolint:errcheck
		}
		if opts.Claim {
			return writeHookClaimSuspensionDrain(reason, opts, stdout, stderr)
		}
		return 1
	}

	cityName := loadedCityName(cfg, cityPath)
	topo, workQuery := hookWorkQuery(cityPath, cityName, cfg, &a, stderr)
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
		agentForQuery = hookSessionAgentForQuery()
		sessionForQuery = os.Getenv("GC_SESSION_NAME")
	} else {
		sessionForQuery = cliSessionName(cityPath, cityName, resolvedAgentName, cfg.Workspace.SessionTemplate)
	}
	overrides, err := hookStoreQueryEnv(cityPath, cfg, &a)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook: building work query env: %v\n", err) //nolint:errcheck // best-effort stderr
		// The query never ran: not the empty-queue exit 1 on the discovery door
		// (hookExitWorkQueryFailed), and no drain result on the claim door.
		if opts.Claim {
			return 1
		}
		return hookExitWorkQueryFailed
	}
	overrides["GC_AGENT"] = agentForQuery
	overrides["GC_SESSION_NAME"] = sessionForQuery
	collapsedIdentity := ""
	runtimeAlias := ""
	if sessionTemplateContext {
		overrides["GC_ALIAS"] = os.Getenv("GC_ALIAS")
		runtimeAlias = overrides["GC_ALIAS"]
		// The work query's assigned tiers walk $GC_SESSION_ID $GC_SESSION_NAME
		// $GC_ALIAS; a session serving a collapsed identity must look up that
		// identity's assignments too. The recorded claim assignee below still
		// comes from the runtime's own env (runtimeAlias).
		if collapsedIdentity = hookClaimCollapsedIdentity(&a, fencedSession, overrides["GC_ALIAS"]); collapsedIdentity != "" {
			fmt.Fprintf(stderr, "gc hook --claim: session %s serves collapsed identity %q\n", strings.TrimSpace(os.Getenv("GC_SESSION_ID")), collapsedIdentity) //nolint:errcheck
			overrides["GC_ALIAS"] = collapsedIdentity
		}
		overrides["GC_SESSION_ID"] = os.Getenv("GC_SESSION_ID")
		overrides["GC_SESSION_ORIGIN"] = os.Getenv("GC_SESSION_ORIGIN")
		overrides["GC_TEMPLATE"] = os.Getenv("GC_TEMPLATE")
	} else {
		overrides["GC_ALIAS"] = resolvedAgentName
		runtimeAlias = resolvedAgentName
		overrides["GC_SESSION_ID"] = ""
		overrides["GC_SESSION_ORIGIN"] = ""
		overrides["GC_TEMPLATE"] = ""
	}
	queryEnv := mergeRuntimeEnv(os.Environ(), overrides)
	failureTemplate, emitFailureEvent := hookWorkQueryFailureTemplate(len(args) > 0, sessionTemplateContext, a.QualifiedName())

	// On a split city the ready tiers of workQuery are already city-wide, so
	// running the whole query once per store re-asks the same question R+1 times
	// and re-opens every leg each time: the read is pinned to the primary entry
	// (scopeFederatedHookStores). Only that entry is built then, because every
	// other leg's environment is a managed-Dolt projection (~0.25 s each on
	// maintainer-city) the claim would discard. No-op on a single-store city and
	// for a custom work_query, where both forms are the same string.
	var stores []hookStore
	if singleStore := singleStoreHookWorkQuery(cityPath, cityName, cfg, &a, topo, stderr); hookStoresScopeToPrimary(workQuery, singleStore) {
		stores = []hookStore{hookWorkQueryPrimaryStore(cityPath, cfg, &a, agentForQuery, workDir, queryEnv, overrides)}
	} else {
		stores = hookWorkQueryStores(cityPath, cfg, &a, agentForQuery, workDir, queryEnv, overrides, buildEffectiveSuspendedRigNames(cfg, st))
	}

	// emitQueryFailure surfaces a killed/timed-out work query on the event bus
	// so the reconciler can escalate instead of silently treating the strand as
	// "no work" (issues #1496/#1497). Ordinary command errors are ignored by
	// emitCityWorkQueryFailure and stay on the caller's stderr path.
	emitQueryFailure := func(command string, err error) {
		if err == nil || !emitFailureEvent {
			return
		}
		emitCityWorkQueryFailure(cityPath, stderr,
			os.Getenv("GC_SESSION_ID"), failureTemplate, command, err)
	}
	runner := hookDiscoveryRunner(stores, hookRigLegRunner(cityPath, cfg, stores, shellWorkQueryWithEnv), emitQueryFailure, stderr)
	// The stale-session fence already ran before agent resolution above; this
	// sessionID feeds only the claim/visibility assignee identity.
	sessionID := strings.TrimSpace(overrides["GC_SESSION_ID"])
	sessionName := strings.TrimSpace(sessionForQuery)
	// The claim is RECORDED under the runtime's own identity (runtimeAlias, not
	// a collapsed identity the query env may carry), so liveness readers keep
	// resolving the assignee to this exact session.
	alias := strings.TrimSpace(runtimeAlias)
	assignee := hookClaimAssigneeIdentity(alias, sessionID, agentForQuery, resolvedAgentName, sessionName)
	// IdentityCandidates governs ADOPTION of already-owned in_progress/open
	// work (hookClaimExistingAssignment, claimFirstReadyHookAssignment, and
	// the display path's hookCandidateVisible own-work check); it must be
	// scoped to this session's OWN runtime identity, never the bare pool
	// template. A suffixed pool worker resolves config via the GC_TEMPLATE
	// fallback, so resolvedAgentName == a.QualifiedName() is the bare
	// template, which is ALSO the [[named_session]] holder's identity —
	// including it let a suffixed worker adopt the holder's in_progress bead
	// (ga-80pen8). The bare template stays in RouteTargets, which governs
	// FRESH claims of UNASSIGNED routed work. The canonical slot / named
	// holder keep it via `alias` (GC_ALIAS == qualified bare name); only
	// suffixed workers drop it. A single-slot pool session the supervisor
	// collapsed onto the canonical identity serves that identity, so it adopts
	// its assignments too (hookClaimCollapsedIdentity) — on the --claim door
	// only: fencedSession is loaded inside opts.Claim, so on the discovery door
	// collapsedIdentity is "" and neither the query env nor these candidates
	// carry the collapsed identity.
	identityCandidates := hookClaimIdentityCandidates(
		assignee,
		sessionID,
		sessionName,
		alias,
		agentForQuery,
		collapsedIdentity,
	)
	routeTargets := hookClaimRouteTargets(hookClaimPrimaryRouteTarget(&a), resolvedAgentName, strings.TrimSpace(overrides["GC_TEMPLATE"]))
	strictDrainAck := func() bool { return cityRunsV2(cityPath, cfg) }
	if opts.Claim {
		claimOpts := hookClaimOptions{
			Assignee:           assignee,
			SessionID:          sessionID,
			IdentityCandidates: identityCandidates,
			RouteTargets:       routeTargets,
			Env:                queryEnv,
			DrainAck:           opts.DrainAck,
			JSON:               opts.JSON,
			StrictDrainAck:     strictDrainAck,
			RuntimeActor:       strings.TrimSpace(os.Getenv("BEADS_ACTOR")),
			// The process env, as the pre-claim fence reads the token:
			// queryEnv has GC_INSTANCE_TOKEN filtered out.
			StartedFence: hookStartedFenceOf(os.Environ()),
		}
		return claimHookWork(city, workQuery, workDir, queryEnv, stores, claimOpts, emitQueryFailure, stdout, stderr)
	}
	// The discovery door is fenced too: a draining seat must not be handed its
	// preassigned continuation sibling by the packs' post-close `gc hook`.
	return hookDiscover(workQuery, workDir, false, hookDiscoveryOptions(assignee, identityCandidates, queryEnv, opts, strictDrainAck), city.claimOps(), runner, stdout, stderr, hookVisibility{
		Identities:   identityCandidates,
		RouteTargets: routeTargets,
	})
}

// hookClaimSessionVerdict classifies a runtime session's fitness to claim routed
// work, as decided by the pre-work-query identity fence in gc hook --claim.
type hookClaimSessionVerdict int

const (
	// hookClaimSessionEligible: the session bead is the current incarnation
	// (matching instance token) and in a state where a live worker legitimately
	// claims work — proceed to the work query.
	hookClaimSessionEligible hookClaimSessionVerdict = iota
	// hookClaimSessionStale: the session is closed, superseded (its instance token
	// was reminted onto a newer incarnation), or in a dormant/terminal state. The
	// incarnation must stop rather than claim, so the caller emits a structured
	// terminal drain result instead of a bare exit 1 the startup wrapper retries.
	hookClaimSessionStale
	// hookClaimSessionStoreUnavailable: the session store could not be opened, or
	// its read failed for a reason other than a confirmed-missing or non-session
	// bead. That is a transient infrastructure fault, not a definitive
	// ineligibility, so the caller fails open into the normal claim path (whose
	// runner surfaces and escalates its own store errors) rather than mislabeling
	// the fault as a stale session. A bead that is confirmed absent or resolves to
	// a non-session bead is NOT this verdict — it is a definitive identity failure
	// and classified stale.
	hookClaimSessionStoreUnavailable
)

// fenceHookClaimSession applies the runtime-identity fence that gates
// gc hook --claim before it runs the work query. It returns (code, handled):
// handled is true for a definitively stale session OR a managed pool runtime
// with no verifiable session-bead registration (GC_TEMPLATE set, GC_SESSION_ID
// empty), either of whose terminal drain result the caller must return as-is.
// A genuinely un-fenceable context (no GC_TEMPLATE and no session id, or a
// session id present but no instance token), an eligible session, or a
// transient session-store fault all return handled=false so the normal claim
// path runs — the fence never turns an infrastructure hiccup or an
// in-progress start into a false refusal.
func fenceHookClaimSession(city *hookCity, sessionID string, opts hookCommandOptions, stdout, stderr io.Writer) (int, bool, *session.Info) {
	if sessionID == "" {
		// GC_TEMPLATE is the pool-membership signal (set only alongside
		// GC_SESSION_ID by RuntimeEnvWithSessionContext, the single front door
		// that builds a live runtime's identity environment from its session
		// bead). A runtime carrying GC_TEMPLATE with no GC_SESSION_ID therefore
		// did not come through that front door with a durable session bead
		// intact: a bead-less legacy start (startPreparedStartCandidate's
		// empty-info.ID branch), a bead lost between mint and launch, or a
		// runtime that survived a restart without its registration. Refuse the
		// claim before any work query or mutation rather than let a slot with
		// no verifiable identity have assignee/routing metadata rewritten onto
		// it — the exact scenario this fence exists to prevent.
		//
		// A genuinely non-session caller (no GC_TEMPLATE either) never carried
		// pool-membership identity in the first place and keeps falling through
		// unfenced.
		if template := strings.TrimSpace(os.Getenv("GC_TEMPLATE")); template != "" {
			fmt.Fprintf(stderr, "gc hook --claim: refusing unregistered managed session for pool template %q: GC_TEMPLATE is set but GC_SESSION_ID is empty, so no durable session bead can be verified\n", template) //nolint:errcheck
			return writeHookClaimMissingSessionRegistrationDrain(opts, stdout, stderr), true, nil
		}
		return 0, false, nil
	}
	instanceToken := strings.TrimSpace(os.Getenv("GC_INSTANCE_TOKEN"))
	if instanceToken == "" {
		return 0, false, nil
	}
	verdict, reason, info := classifyHookClaimSessionVia(city.sessionFront, sessionID, instanceToken)
	switch verdict {
	case hookClaimSessionStale:
		fmt.Fprintf(stderr, "gc hook --claim: refusing stale session %s: %s\n", sessionID, reason) //nolint:errcheck
		return writeHookClaimStaleSessionDrain(opts, stdout, stderr), true, nil
	case hookClaimSessionStoreUnavailable:
		// Fail open: let the claim path run and surface/escalate its own store
		// error rather than reporting a false stale session. Name the fault
		// without the alarming "stale session" wording.
		fmt.Fprintf(stderr, "gc hook --claim: session fence unavailable for %s: %s; proceeding to claim\n", sessionID, reason) //nolint:errcheck
		return 0, false, nil
	default:
		return 0, false, info
	}
}

// classifyHookClaimSession loads the session bead named by sessionID and reports
// whether the runtime holding instanceToken may claim. A confirmed identity
// failure — the session bead is absent, or resolves to a non-session bead — is a
// stale verdict: the incarnation can no longer prove its identity and must drain
// rather than claim. Only a genuine store-open or read fault yields
// hookClaimSessionStoreUnavailable (transient, fails open), so an infrastructure
// hiccup is not mislabeled as staleness AND a vanished session is not laundered
// into an infrastructure hiccup that lets a stale runtime reach the claim path.
//
// It and classifyHookClaimSessionInfo are test-only: `gc hook`'s fence calls
// classifyHookClaimSessionVia with the invocation's session front door.
func classifyHookClaimSession(cityPath string, cfg *config.City, sessionID, instanceToken string) (hookClaimSessionVerdict, string) {
	verdict, reason, _ := classifyHookClaimSessionInfo(cityPath, cfg, sessionID, instanceToken)
	return verdict, reason
}

// classifyHookClaimSessionInfo is classifyHookClaimSession that also returns the
// session Info it read when the verdict is eligible (nil otherwise), so the
// claim can act on the session bead's persisted identity without a second read.
func classifyHookClaimSessionInfo(cityPath string, cfg *config.City, sessionID, instanceToken string) (hookClaimSessionVerdict, string, *session.Info) {
	// cfg is the caller's loaded city config; reuse it rather than reloading
	// the whole city config inside the open.
	return classifyHookClaimSessionVia(newHookCity(cityPath, cfg).sessionFront, sessionID, instanceToken)
}

// classifyHookClaimSessionVia is classifyHookClaimSessionInfo reading the
// session through front: in `gc hook`, the invocation's one session front door,
// which the claim's later session reads and writes reuse.
func classifyHookClaimSessionVia(front sessionFrontGetter, sessionID, instanceToken string) (hookClaimSessionVerdict, string, *session.Info) {
	sessFront, err := front()
	if err != nil {
		return hookClaimSessionStoreUnavailable, fmt.Sprintf("opening session store: %v", err), nil
	}
	info, err := sessFront.Get(sessionID)
	if err != nil {
		verdict, reason := classifyHookClaimSessionLookupError(err)
		return verdict, reason, nil
	}
	verdict, reason := hookClaimSessionEligibility(info, instanceToken)
	if verdict != hookClaimSessionEligible {
		return verdict, reason, nil
	}
	return verdict, reason, &info
}

// hookClaimCollapsedIdentity returns the canonical identity a pool session
// serves when the supervisor collapsed it onto that identity after its runtime
// env was stamped, or "" when the session serves only its own runtime identity.
//
// A single-slot pool (max_active_sessions = 1, no namepool) whose template is
// ALSO a [[named_session]] steps its runtime name aside to "<name>-pool"
// (poolRuntimeSessionName) and launches with a blank GC_ALIAS
// (clearPoolTemplateRuntimeIdentity). buildDesiredState then collapses the
// session bead onto the canonical identity ("collapsing phantom pool identity
// ... to <name>", normalizeNonExpandingPoolSessionInfo) and wakes it on
// namedWorkReady for work assigned to <name>. The runtime env never learns the
// collapse, so the claim looked only for <name>-pool / session-id work and
// drained no_work while the named identity's assignment waited — the olivia
// seat in maintainer-city, 2026-09-22/23. The persisted alias/agent_name on the
// fenced session bead is the authority for which identity this seat serves —
// specifically its exclusive alias, never the shared agent_name.
//
// The ga-80pen8 guard still holds: only a single-slot pool can collapse (there
// is never a second live member to double-adopt the holder's work), a
// configured named-session bead already carries its own alias, and a pool
// session whose bead does NOT carry the canonical identity gets nothing.
func hookClaimCollapsedIdentity(a *config.Agent, info *session.Info, runtimeAlias string) string {
	if a == nil || info == nil || info.Closed || !a.UsesCanonicalSingletonPoolIdentity() ||
		isNamedSessionInfo(*info) || isManualSessionInfoForAgent(*info, a) {
		return ""
	}
	canonical := strings.TrimSpace(a.QualifiedName())
	if canonical == "" || strings.TrimSpace(runtimeAlias) == canonical {
		return ""
	}
	// Only the alias is exclusive: it is written under the city session alias
	// lock after EnsureAliasAvailable, so at most one live session holds it.
	// agent_name is NOT exclusive — an alias-deferred pool session, a woken
	// named holder, and manual sessions (`gc session new olivia --alias x`) all
	// carry agent_name=<canonical> — so accepting it let two sessions adopt the
	// same in_progress assignment.
	if strings.TrimSpace(info.Alias) == canonical {
		return canonical
	}
	return ""
}

// classifyHookClaimSessionLookupError maps a session Store.Get error to a fence
// verdict. session.Store.Get reports a CONFIRMED-absent id as the store not-found
// error wrapped around beads.ErrNotFound, and a present-but-non-session id (the
// id resolves to a bead that is not a session) as session.ErrSessionNotFound.
// Both are definitive identity failures — the runtime's session no longer exists
// in the store — so the incarnation is stale and must drain. Any other error is a
// genuine store open/read fault the fence fails open on, letting the normal claim
// path surface and escalate its own store error rather than refusing a healthy
// worker over an infrastructure hiccup.
func classifyHookClaimSessionLookupError(err error) (hookClaimSessionVerdict, string) {
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return hookClaimSessionStale, fmt.Sprintf("session bead not found: %v", err)
	case errors.Is(err, session.ErrSessionNotFound):
		return hookClaimSessionStale, fmt.Sprintf("session id resolves to a non-session bead: %v", err)
	default:
		return hookClaimSessionStoreUnavailable, fmt.Sprintf("loading session bead: %v", err)
	}
}

// hookClaimSessionEligibility is the pure eligibility decision over a session Info
// snapshot. The instance-token arm proves whether this is the current
// incarnation; the state arm then admits only the states in which a live worker
// legitimately claims: active/awake plus the in-startup states creating/
// start-pending that the deferred-start path passes through before its async
// active commit lands (refusing those rejects a healthy first claim). An empty
// MetadataState (session.StateNone) is a pre-metadata legacy bead mid-upgrade,
// not a dormant state: the session lifecycle canonicalizes empty state to
// StateActive (canonicalLifecycleState in internal/session/manager.go), so once
// Closed is false and the instance token matches — proving this is the live
// current incarnation — it is admitted with the active states, or a healthy
// upgraded legacy runtime would be drained before claiming its routed work.
// Every other state — failed-create, draining, drained, asleep, suspended,
// archived, quarantined — is dormant or terminal and classified stale.
func hookClaimSessionEligibility(info session.Info, instanceToken string) (hookClaimSessionVerdict, string) {
	if info.Closed {
		return hookClaimSessionStale, "session bead is closed"
	}
	storedToken := strings.TrimSpace(info.InstanceToken)
	if storedToken == "" || storedToken != strings.TrimSpace(instanceToken) {
		return hookClaimSessionStale, "runtime instance token does not match the session bead"
	}
	switch state := session.State(strings.TrimSpace(info.MetadataState)); state {
	case session.StateNone, session.StateActive, session.StateAwake, session.StateCreating, session.StateStartPending:
		return hookClaimSessionEligible, ""
	default:
		return hookClaimSessionStale, fmt.Sprintf("session state %q is not claim-eligible", state)
	}
}

// claimHookWork claims routed work for gc hook --claim from the federated store
// set, binding the production shell work-query runner and real claim ops. See
// claimHookWorkWithRunner for the federation and lost-claim-race semantics.
//
// The claim ops carry the CLASS axis (claim_class_route.go): every store in the
// federated set is a bd WORKSPACE, and a relocated coordination class is not
// one, so the binding is reached through the ops rather than through a leg. On a
// city that relocates nothing the route is nil and the ops are the work-store
// ops alone.
func claimHookWork(city *hookCity, workQuery, workDir string, queryEnv []string, stores []hookStore, claimOpts hookClaimOptions, emitFailure func(command string, err error), stdout, stderr io.Writer) int {
	// The city relocates a class and its front door could not be projected.
	// Claiming through the work store anyway would write ownership into a
	// ledger that does not hold the bead, which is the wrong-answer lane this
	// routing exists to close — so every failure but one is fatal here. The
	// exception, a binding with no claim CAS, degrades to unrouted claiming;
	// see hookClaimRouteVerdict.
	opened, err := hookClaimClassRouteForCity(city.path)
	// Work-store writes announce themselves to the city's event log so the run
	// views see a claim within a poll (hook_claim_bead_events.go). They are
	// wrapped before the class route, whose binding writes announce themselves.
	ops, proceed := routedHookClaimOps(withHookClaimBeadEvents(city.claimOps(), city.path), opened, err, city.recorder, stderr)
	if !proceed {
		return 1
	}
	// A kept claim is announced before its result is written; this announces
	// whatever is left on the way out (a release, a reclaim, a claim that
	// stopped on an error).
	defer ops.announceBeadEvents()
	run := hookRigLegRunner(city.path, city.cfg, stores, shellWorkQueryWithEnv)
	return claimHookWorkWithRunner(workQuery, workDir, queryEnv, stores, claimOpts, ops, run, emitFailure, stdout, stderr)
}

// routedHookClaimOps is the claim ops one class-route resolution (opened, err)
// yields over base, and whether the claim may proceed (hookClaimRouteVerdict).
// A route that opened takes recorder and wraps the ops; a city that relocates
// its classes, even through a route that degraded to nil, is marked so
// (ClassesRelocated), so the started key never takes its legs for proven
// residences.
func routedHookClaimOps(base hookClaimOps, opened *hookClaimClassRoute, err error, recorder func() events.Recorder, stderr io.Writer) (hookClaimOps, bool) {
	route, proceed := hookClaimRouteVerdict(opened, err, stderr)
	if !proceed {
		return base, false
	}
	if route != nil {
		route.recorder = recorder
	}
	ops := classRoutedHookClaimOps(base, route)
	ops.ClassesRelocated = hookClaimClassesRelocated(opened, err)
	return ops, true
}

// claimHookWorkWithRunner is claimHookWork with the work-query runner and claim
// ops injected for tests. It selects the best-ranked store reporting ready work
// (see bestStoreWithWork), re-validates it for claim-time freshness and falls
// back to a later store if it emptied since discovery (claimStoreWithFallback),
// then attempts the claim against that store's captured rows, against that
// store's dir/env.
//
// When a selected store still reports ready work but every claimable row is lost
// to another claimant before the mutation, the single-store claim drains without
// work. That would strand routed work waiting in a LATER federated store behind
// the lost race, so this loop drops the exhausted store and reselects across the
// remaining stores. It writes the shared drain exactly once, after every store
// has been exhausted; the drain reason is claims_errored when any exhausted
// store's eligible claims errored rather than merely lost the race, else no_work.
// emitFailure surfaces a work-query timeout on the event bus when eligible.
//
// The store set is also what the claim-time class escalation is measured
// against: only the PRIMARY leg runs the city-wide reader, so it is the only leg
// whose query can serve a bead it does not itself hold, and a not-found there
// says nothing about the work legs behind it. observeWorkLegs hands the route
// the whole fan-out so it can prove "no WORK store holds this bead" before it
// writes ownership into the binding (claim_class_route.go). Nil on a city that
// relocates nothing.
func claimHookWorkWithRunner(workQuery, workDir string, queryEnv []string, stores []hookStore, claimOpts hookClaimOptions, ops hookClaimOps, run hookStoreRunner, emitFailure func(command string, err error), stdout, stderr io.Writer) int {
	ops.applyDefaults()
	ops.ClassRoute.observeWorkLegs(stores)
	// primary is the agent's own store (the first entry). It is captured once
	// here, before the loop shrinks remaining: only the primary's error is
	// surfaced as itself and emitted as this session's work-query failure. Once
	// the primary loses its claim race and is dropped, a later federated store
	// must never inherit that emit-on-timeout semantics, so it is matched by
	// identity, not slice index. A later store's failed read is still a failed
	// read, though: with no work anywhere it ends the claim below without a
	// drain (bestStoreWithWork's hookSecondaryStoreFailure), unless it is a rig
	// whose managed Dolt runtime is not running (errHookRigRuntimeUnavailable).
	// Each failed store is reported once per invocation however often the loop
	// reads it (hookStoreFailureReport).
	var primary hookStore
	if len(stores) > 0 {
		primary = stores[0]
	}
	report := newHookStoreFailureReport("gc hook --claim", stderr, emitFailure)
	noteStoreFailure := report.note
	remaining := stores
	// claimsErrored aggregates the per-store signal that a store reported ready
	// work but every eligible claim mutation errored, so the shared drain below can
	// report claims_errored instead of laundering a write failure into no_work.
	claimsErrored := false
	for len(remaining) > 0 {
		discovered, selected, err := selectStoreWithWorkRetrying(workQuery, remaining, primary, run, &ops, noteStoreFailure)
		if err != nil {
			report.failed(workQuery, err)
			return reportClaimWorkQueryFailure(err, stderr)
		}
		if isZeroHookStore(selected) {
			break // no remaining store has ready work
		}
		// On a multi-leg city this re-reads the selected store before the
		// mutation, and a primary-leg failure there is surfaced exactly like a
		// discovery failure: it is the same read of the same store, moments
		// later, so it gets the same classification.
		claimOutput, claimStore, err := claimStoreWithFallback(workQuery, remaining, selected, primary, discovered, run, noteStoreFailure)
		if err != nil {
			report.failed(workQuery, err)
			return reportClaimWorkQueryFailure(err, stderr)
		}
		if isZeroHookStore(claimStore) {
			break // selected store emptied and no later store has ready work
		}
		storeOpts := claimOpts
		storeOpts.Env = queryEnv
		if len(claimStore.env) > 0 {
			storeOpts.Env = claimStore.env
		}
		storeDir := workDir
		if dir := strings.TrimSpace(claimStore.dir); dir != "" {
			storeDir = dir
		}
		storeOps := ops
		storeOps.Runner = func(string, string) (string, error) { return claimOutput, nil }
		res := tryHookClaim(workQuery, storeDir, &storeOpts, &storeOps, stdout, stderr)
		if res.terminal {
			return res.code
		}
		if res.claimsErrored {
			claimsErrored = true
		}
		// This store reported ready work but the claim acquired nothing — every
		// claimable row was lost to another claimant, none matched this session, or
		// every claimable row's claim mutation errored and was skipped. Drop it and
		// reselect from the remaining stores so routed work in a later federated
		// store is not stranded behind it; claimsErrored carries any write-failure
		// signal to the shared drain.
		remaining = removeHookStore(remaining, claimStore)
	}
	return writeHookClaimNoWork(claimOpts, ops, claimsErrored, workDir, stdout, stderr)
}

// reportClaimWorkQueryFailure reports a failed claim-path work query on stderr
// and returns the exit code for it. Every claim read whose failure is surfaced
// funnels through here: discovery, the claim-time re-validation on a multi-leg
// city, and tryHookClaim's own read. So a timed-out or transport-class failure
// on any of them carries the token and exit 2, scoped as
// hookStoreUnavailableToken's doc states (the agent's own store only).
//
// This is the same store-unavailable classification the read path applies.
// --claim is the form agents actually run in the dispatch loop, so without the
// token here a transport-class failure is still indistinguishable from "no
// work" to every consumer that matches on it — the dead-drop the token exists
// to close. The error is classified ONCE and reused for both the stderr token
// and the exit code, so the published contract (token <=> exit 2) cannot be
// split by an edit reaching one and not the other.
//
// Deliberately NO drain result and NO drain-ack. A failed read is not an idle
// store: the controller counted demand for this seat, so draining here would
// convert a transport failure into a false idle, reap the seat, and leave the
// work for the next tick to rediscover. Keep the seat and let the event the
// caller emitted carry the cause; the idle-claim backstop re-drives the hook.
// That behavior is orthogonal to the exit CODE, which follows the token's
// published contract: hookStoreUnavailableToken documents exit 2, and a
// consumer gating on the code must be able to tell a dead store from no-work on
// this path too.
func reportClaimWorkQueryFailure(err error, stderr io.Writer) int {
	return reportWorkQueryFailure("gc hook --claim", err, stderr)
}

// workQueryStoreUnavailable reports whether err, after classification, means
// the store could not be read (an unknown answer, not an empty queue).
func workQueryStoreUnavailable(err error) bool {
	return errors.Is(classifyWorkQueryStoreUnavailable(err), beads.ErrStoreUnavailable)
}

// reportWorkQueryFailure is the one place the token <=> exit-2 contract is
// rendered: a store-unavailable failure (classifyWorkQueryStoreUnavailable)
// prints "<prefix>: <token>: <err>" and exits 2; anything else prints
// "<prefix>: <err>" and exits 1. The token and the exit code come from one
// classification, so they cannot disagree. Both gc hook doors report a failed
// work query through it (doHook directly, the claim reads via
// reportClaimWorkQueryFailure) so the published contract cannot drift between
// them. doHook returns hookExitWorkQueryFailed for every failure rather than
// this code: plain gc hook keeps exit 1 for an answered, empty queue only.
func reportWorkQueryFailure(prefix string, err error, stderr io.Writer) int {
	classified := classifyWorkQueryStoreUnavailable(err)
	if errors.Is(classified, beads.ErrStoreUnavailable) {
		fmt.Fprintf(stderr, "%s: %s: %v\n", prefix, hookStoreUnavailableToken, classified) //nolint:errcheck // best-effort stderr
		return 2
	}
	fmt.Fprintf(stderr, "%s: %v\n", prefix, err) //nolint:errcheck // best-effort stderr
	return 1
}

// Claim-read retry pacing. A work-query ERROR is a failed read, and the failures
// this bounds are transport-shaped: a contended SQLite leg, a store mid-write, a
// binding whose engine is briefly refusing. Those clear in seconds, and the
// alternative — failing the claim at once and parking a seat the controller
// minted demand for until the 90s backstop re-drives it — is strictly worse.
// Emptiness is NOT retried: an empty read is an answer, and a seat that lost
// the sibling race must drain promptly. Package vars follow
// hookWorkQueryTimeout's convention so tests drive the loop without sleeping.
var (
	hookClaimQueryRetryAttempts = 3
	hookClaimQueryRetryInterval = 5 * time.Second
)

// selectStoreWithWorkRetrying is bestStoreWithWork with a bounded retry around
// the ERROR case only. It returns the first successful selection, or the last
// error once the budget is spent.
//
// The retry budget is also bounded by the invocation's claim window (F-B). Three
// paced retries on top of a work query that may itself run to
// hookWorkQueryTimeout can carry a `gc hook --claim` process well past the turn
// that invoked it, and a read that lands past the window buys nothing: any claim
// it leads to is refused on arrival. So a retry runs only when it would start
// strictly inside the window, both before the pacing sleep (the sleep must not
// run past the window) and after it (the sleep may have overrun). When the window
// cuts the budget short, the last read error is returned, annotated, so the
// caller keeps its failed-read contract instead of treating a dead invocation
// as an idle store: no drain, and reportClaimWorkQueryFailure classifies the
// code (2 if the read failed transport-class or timed out, else 1).
//
// A failed secondary store with no work anywhere is an error here too
// (hookSecondaryStoreFailure), so it gets the same bounded retry. noted is
// forwarded to bestStoreWithWork.
func selectStoreWithWorkRetrying(workQuery string, stores []hookStore, primary hookStore, run hookStoreRunner, ops *hookClaimOps, noted ...hookStoreFailureNote) (string, hookStore, error) {
	out, selected, err := bestStoreWithWork(workQuery, stores, primary, run, noted...)
	for attempt := 0; err != nil && attempt < hookClaimQueryRetryAttempts; attempt++ {
		if ops.claimWindowSpentAfter(hookClaimQueryRetryInterval) {
			return out, selected, hookClaimRetryWindowClosed(err, ops)
		}
		ops.sleepOrWallClock(hookClaimQueryRetryInterval)
		if ops.claimWindowSpentAfter(0) {
			return out, selected, hookClaimRetryWindowClosed(err, ops)
		}
		out, selected, err = bestStoreWithWork(workQuery, stores, primary, run, noted...)
	}
	return out, selected, err
}

// hookClaimRetryWindowClosed annotates the last claim-read error with the reason
// the retry budget stopped early. It is a wrapped suffix, so errors.Is/As and the
// kill/timeout markers classifyWorkQueryFailure matches on stay intact.
func hookClaimRetryWindowClosed(err error, ops *hookClaimOps) error {
	return fmt.Errorf("%w (claim-read retries stopped: the %s claim window closes before the next retry could run)", err, ops.claimWindowOrDefault())
}

func hookClaimPrimaryRouteTarget(a *config.Agent) string {
	return agentutil.RoutedToIdentity(a)
}

// hookWorkQuery returns the topology `gc hook` reads with and the work query it
// runs for a, both built from the CITY topology, never from promptQueryTopology:
// the hook reaches the rig stores itself, by running the single-store query
// once per store (hookWorkQueryStores).
func hookWorkQuery(cityPath, cityName string, cfg *config.City, a *config.Agent, stderr io.Writer) (config.QueryTopology, string) {
	topo := cityQueryTopology(cityPath, cfg)
	warnFederationBlindOverrides(stderr, a, topo)
	// Expand {{.Rig}}/{{.AgentBase}} in user-supplied work_query so agent-side
	// hook invocation sees the same rig substitution as the controller-side
	// probes in build_desired_state.go / session_reconcile.go. #793.
	return topo, expandAgentCommandTemplate(cityPath, cityName, a, cfg.Rigs, "work_query", a.EffectiveWorkQueryFor(topo), stderr)
}

// singleStoreHookWorkQuery returns the agent's work query built for the SAME
// city with the federated reader turned off. Nothing runs it: the hook compares
// it with the federated form (hookStoresScopeToPrimary), and where the two
// differ the claim reads only the primary store, for the reasons
// scopeFederatedHookStores gives.
//
// It returns "" on a city that federates nothing, so the caller's scoping is a
// no-op there rather than a second build of the identical string.
func singleStoreHookWorkQuery(cityPath, cityName string, cfg *config.City, a *config.Agent, topo config.QueryTopology, stderr io.Writer) string {
	if !topo.FederatedReady || cfg == nil || a == nil {
		return ""
	}
	singleStore := topo
	singleStore.FederatedReady = false
	// A custom work_query is returned verbatim for both topologies; scoping then
	// no-ops on the equality check, and expanding it twice would repeat its
	// template diagnostic, so stop before that.
	command := a.EffectiveWorkQueryFor(singleStore)
	if command == a.EffectiveWorkQueryFor(topo) {
		return ""
	}
	return expandAgentCommandTemplate(cityPath, cityName, a, cfg.Rigs, "work_query", command, stderr)
}

func hookSessionAgentForQuery() string {
	return firstNonEmptyHookValue(
		os.Getenv("GC_ALIAS"),
		os.Getenv("BEADS_ACTOR"),
		os.Getenv("GC_AGENT"),
		os.Getenv("GC_SESSION_NAME"),
	)
}

// hookClaimAssigneeIdentity picks the identity a claim is RECORDED under. It is
// the writer half of the contract every liveness reader already implements, and
// the order is the whole of it.
//
// An unaliased pool spawn has no occupant name in the environment except its
// session bead id. clearPoolTemplateRuntimeIdentity blanks GC_ALIAS, and the
// runtime session name (GC_SESSION_NAME) is only a name for the runtime. Where
// that name is identity-derived — tmux_alias pools, and unaliased rows minted by
// pre-v1.5.0 builds (poolRuntimeSessionName) — it is a CHAIR: it is stable
// across every session that ever occupies the slot. Unaliased pools are
// bead-scoped again (PoolSessionName, <template>-<beadID>), but the bead id
// stays the canonical claim identity. Recording a claim under a chair name
// makes every "is the holder still alive?" consumer answer about the chair, so
// a dead occupant's in_progress
// bead reads as held by whoever sits there next and is never released, resumed,
// or replaced. On maintainer-city one such label was the session_name of 24
// distinct session beads, and the worst of them 66. The runtime projection
// (session.AssigneeIdentifier) exports the same session bead id as GC_AGENT and
// BEADS_ACTOR for an unaliased pool session, so the worker's later bd mutations
// are actored by exactly the string the claim is recorded under (#5716).
//
// So the session bead id goes ahead of every session/agent NAME form. Every
// reader already leads with it — sessionBeadAssigneeIdentities,
// currentSessionAssigneeIdentities and ComputeAwakeSet all list bead.ID first,
// directSessionBeadIDCandidates resolves it with a direct Get, and the default
// work query's own documented order is "$GC_SESSION_ID (bead ID) >
// $GC_SESSION_NAME > $GC_ALIAS" (config.EffectiveWorkQuery). The writer was the
// only side reading that list backwards; this changes which of several identities
// it picks, never what a reader has to understand.
//
// alias stays FIRST, and that is what scopes this to unaliased pool workers. A
// non-empty GC_ALIAS means clearPoolTemplateRuntimeIdentity did not run: the
// session is a named holder, a namepool member, or an explicit `gc hook <agent>`
// target, whose alias is a configured identity that a later invocation from a
// fresh shell — one with no GC_SESSION_ID at all — must still resolve to. Moving
// the session id ahead of it would strand exactly that adoption.
//
// Everything after sessionID is the pre-existing fallback chain, reached only
// when the environment carries no session bead id (a bare shell, an explicit
// target outside a session).
func hookClaimAssigneeIdentity(alias, sessionID, agentForQuery, resolvedAgentName, sessionName string) string {
	return firstNonEmptyHookValue(alias, sessionID, agentForQuery, resolvedAgentName, sessionName)
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
//
// 2026-08-14: raised 60s -> 150s. Measured on a loaded six-rig city: each
// `gc ready` leg of the default probe costs 10-14s (~4s process start + a
// 6-leg federated read), and the five sequential reads put the pool-demand
// payoff call at t=60s exactly — 850+ session.work_query_failed events with
// every hook starved while `gc ready` run standalone returned the routed
// rows. 150s covers the measured worst case (~70s) with margin for load;
// the real cure remains ga-4qdfn (fewer round-trips, faster reader).
var hookWorkQueryTimeout = 150 * time.Second

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
	disableProductMetricsForChild(cmd)
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

// hookVisibility scopes which already-returned work_query candidates doHook
// shows the caller. Identities match a candidate's OWN assignee
// (already-claimed work); RouteTargets match an UNASSIGNED candidate's
// gc.routed_to (freshly routed work). These are kept as two separate lists,
// not merged into one, because they are not interchangeable - see the
// IdentityCandidates/RouteTargets split in cmdHookWithOptions (ga-80pen8): a
// suffixed pool worker's own session identity must never act as a route
// target for fresh unassigned claims. The zero value disables filtering
// entirely, matching pre-ga-1xaqgo.2 behavior byte-for-byte.
type hookVisibility struct {
	Identities   []string
	RouteTargets []string
}

// hookStoreUnavailableToken is the stderr token gc hook prints, with exit code
// 2, when its work query got no answer from the store
// (classifyWorkQueryStoreUnavailable), with or without --claim. It is a
// published contract: a consumer of `gc hook` stderr matches it to tell "store
// down" from the exit-1 no-work answer, as agentScriptHookExitIsNoWork does.
// Rendering an unreachable store as no work is the chronic
// idle-agents-with-work-waiting dead-drop.
//
// It fires only when the failure reaches gc hook. The generated work query
// fails with the reader's own status and keeps bd's stderr when a store read
// fails (internal/config/workquery.go), so a store that fails fast reaches gc
// hook, not only one that overruns the deadline. Other stores gc hook queries
// beside the agent's own (bestStoreWithWork) reach it only when no store has
// work (hookSecondaryStoreFailure); a secondary store that failed while another
// served work, or a rig whose managed Dolt runtime is not running, is named on
// stderr, not reported as a failed query. A federated reader (`gc ready`) is
// not one of those: it runs as the primary store's query, so a leg it cannot
// read fails the primary read.
const hookStoreUnavailableToken = "GC_HOOK_STORE_UNAVAILABLE"

// bdConnectionErrorMarkers mirrors the messages bd's Dolt store counts as the
// server being down: the connection-error test its circuit breaker trips on
// (isConnectionError, beads v1.3.1 internal/storage/dolt/circuit.go). Several
// strike after a query is under way ("Error 2013: Lost connection to MySQL
// server during query"), past every connect-time marker in
// bdTransportRetryableMarkers, and they stay out of that table on purpose:
// the managed retry reads it and re-runs any bd command, writes included.
// The table is not exhaustive. No marker here, in bdTransportRetryableMarkers
// or in dispatch's availability needles matches a read- or write-phase "no
// route to host" or "connection timed out" (only a dial-phase one, through
// "dial tcp"), so those carry no token; such a phrase belongs here, with a row
// in TestClassifyWorkQueryStoreUnavailable.
var bdConnectionErrorMarkers = []string{
	"connection refused",
	"connection reset",
	"broken pipe",
	"i/o timeout",
	"bad connection",
	"invalid connection",
	"lost connection",
	"gone away",
}

// messageHasMarker reports whether a lowercased error message contains any of
// markers.
func messageHasMarker(lowerMsg string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(lowerMsg, marker) {
			return true
		}
	}
	return false
}

// workQueryErrorPrefix is the stamp shellWorkQueryWithEnv puts on every
// work-query failure, ahead of the command line it quotes with %q.
const workQueryErrorPrefix = "running work query"

// workQueryFailureText returns the lowercased text the work-query classifier
// scans: the error message with the command line quoted after
// workQueryErrorPrefix cut out. A work query is user-authored shell, so its
// command text is not the store's answer, and a marker or timeout needle that
// appears only there must not classify. The cut parses the span as the Go
// string literal %q wrote, on the original message because lowercasing can
// change a rune's byte length; a message without a parseable span is scanned
// whole.
func workQueryFailureText(msg string) string {
	i := strings.Index(msg, workQueryErrorPrefix+" ")
	if i < 0 {
		return strings.ToLower(msg)
	}
	head, rest := msg[:i+len(workQueryErrorPrefix)], msg[i+len(workQueryErrorPrefix)+1:]
	quoted, err := strconv.QuotedPrefix(rest)
	if err != nil {
		return strings.ToLower(msg)
	}
	return strings.ToLower(head + rest[len(quoted):])
}

// workQueryFailure presents a work-query error to the text classifiers as
// workQueryFailureText while keeping the original error chain, so typed
// signals such as context.DeadlineExceeded still classify through errors.Is.
type workQueryFailure struct {
	err  error
	text string
}

func (f workQueryFailure) Error() string { return f.text }

func (f workQueryFailure) Unwrap() error { return f.err }

// classifyWorkQueryStoreUnavailable wraps work-query failures that got no
// answer from the store as beads.ErrStoreUnavailable so both gc hook doors can report
// them as exit 2 rather than letting a dead store masquerade as a drained queue.
// Errors that already carry the sentinel pass through unchanged; anything else
// is returned as-is and stays an ordinary exit-1 error.
func classifyWorkQueryStoreUnavailable(err error) error {
	if err == nil || errors.Is(err, beads.ErrStoreUnavailable) {
		return err
	}
	// Secondary stores of the fan-out failed: unavailable when any of them got
	// no answer. Each is classified on its own whole failure, since the
	// aggregate's message is the bounded report (hookStoreReadError).
	var secondary *hookSecondaryStoreFailure
	if errors.As(err, &secondary) {
		for _, failure := range secondary.failures {
			if workQueryStoreUnavailable(failure.err) {
				return fmt.Errorf("%w: %w", beads.ErrStoreUnavailable, err)
			}
		}
		return err
	}
	// Every text scan skips the command line the runner quotes into its
	// error: only what the query reported can say the store was unreachable.
	failure := workQueryFailure{err: err, text: workQueryFailureText(err.Error())}
	// The control dispatcher's availability tier is the shared "the store did
	// not answer" vocabulary. It covers the typed deadline shellWorkQueryWithEnv
	// wraps for exactly this (a wedged-but-listening backend, with hung
	// connections and context-deadline floods, is the dominant outage shape,
	// not "connection refused"), bd's own timeouts (including a federated
	// reader's per-leg deadline, which reaches gc hook only as stderr text),
	// a killed work query, lock contention and bd's circuit breaker failing
	// fast. Its semantic tier is a refusal the store answered with, so it
	// carries no token.
	//
	// bdTransportRetryableMarkers is shared with the bd managed-retry path, so
	// a marker added there classifies here too. bdConnectionErrorMarkers
	// supplies bd's own connection-error vocabulary, including read-phase
	// disconnects ("lost connection") that neither of the others lists, and
	// storeNoAnswerMarkers the hosted bd serve answers that mean "try again".
	// All of it is storeGotNoAnswer, which the storage gate's transient
	// classification (isTransientStoreFault) reads too, so the two cannot
	// drift.
	if storeGotNoAnswer(failure, failure.text) {
		return fmt.Errorf("%w: %w", beads.ErrStoreUnavailable, err)
	}
	return err
}

// hookDiscoveryOptions are plain `gc hook`'s options for the drain fences it
// shares with --claim. The drain-ack fence matches a started claim's assignee
// against the seat's identities, on this door as on --claim, so they travel
// here too.
func hookDiscoveryOptions(assignee string, identities, env []string, opts hookCommandOptions, strict func() bool) hookClaimOptions {
	return hookClaimOptions{
		Assignee:           assignee,
		IdentityCandidates: identities,
		Env:                env,
		DrainAck:           opts.DrainAck,
		JSON:               opts.JSON,
		StrictDrainAck:     strict,
	}
}

// hookDiscover is doHookDiscovery, a seam for tests of what plain `gc hook`
// hands it. Tests that swap it MUST NOT call t.Parallel().
var hookDiscover = doHookDiscovery

// doHookDiscovery is the drain-fenced entry point for plain `gc hook`, the
// DISCOVERY door. doHook itself stays a pure query-and-print function; this
// wrapper is where the F-D fence lives for the non-claim path.
//
// It exists because F-D on --claim was only half the fence. Every workflows-pack
// prompt's post-close lifecycle tells an agent to run plain `gc hook` and
// continue any work sharing its root/continuation group — no --claim, because
// the continuation sibling was PREASSIGNED to this session at claim time and is
// already open under its assignee. Discovery listed that sibling for a draining
// seat exactly as for a healthy one, so the fleet's dominant workflow walked
// its seats back into multi-hour chains without ever crossing the fence.
//
// The refusal reuses the discovery no-work contract (nothing on stdout, exit 1)
// because the packs ALREADY route that answer to `gc runtime drain-ack` and
// exit. No prompt changes are needed to make the seat leave; the answer it
// already knows how to obey is simply now the true one.
func doHookDiscovery(workQuery, dir string, inject bool, opts hookClaimOptions, ops hookClaimOps, runner WorkQueryRunner, stdout, stderr io.Writer, visibility hookVisibility) int {
	// An inject invocation reads nothing and answers nothing, so there is no
	// work to withhold and no reason to pay for a probe.
	if inject {
		return doHook(workQuery, dir, inject, runner, stdout, stderr, visibility)
	}
	ops.applyDefaults()
	if sessionID := hookClaimSessionID(opts.Env); sessionID != "" {
		pending, err := ops.DrainPending(sessionID)
		switch {
		case hookDrainProbeFailsClosed(err):
			// A standing storage refusal fails CLOSED; see
			// hookDrainProbeFailsClosed for why it is not a transient.
			hookEmitDrainFenceUnavailable(stderr, sessionID, hookClaimEnvValue(opts.Env, "GC_TEMPLATE"), err)
			return reportHookDrainFenceClosed("gc hook", sessionID, err, stderr)
		case err != nil:
			// Fail open on a transient fault, and say so off-pane — same posture and same reasoning as
			// the claim door: a blind probe must not stop a healthy fleet finding
			// work, and must not go inert quietly.
			fmt.Fprintf(stderr, "gc hook: drain-pending probe unavailable for %s: %v; proceeding to discovery\n", sessionID, err) //nolint:errcheck
			hookEmitDrainFenceUnavailable(stderr, sessionID, hookClaimEnvValue(opts.Env, "GC_TEMPLATE"), err)
		case pending:
			return writeHookClaimDrainPending(hookDiscoveryLabel, sessionID, opts, ops, dir, stdout, stderr)
		}
	}
	return doHook(workQuery, dir, inject, runner, stdout, stderr, visibility)
}

// hookDiscoveryRunner is plain `gc hook`'s work-query runner: the best answer
// across the agent's stores (bestStoreWithWork). A failed query is returned as
// its error and recorded through emitFailure, and a store whose failed read the
// answer did not stop on is named on stderr (hookStoreFailureReport).
func hookDiscoveryRunner(stores []hookStore, run hookStoreRunner, emitFailure func(command string, err error), stderr io.Writer) WorkQueryRunner {
	report := newHookStoreFailureReport("gc hook", stderr, emitFailure)
	return func(command, _ string) (string, error) {
		out, _, err := bestStoreWithWork(command, stores, stores[0], run, report.note)
		report.failed(command, err)
		return out, err
	}
}

// hookExitWorkQueryFailed is plain `gc hook`'s exit code when the work query
// itself failed — bd missing from PATH, a store bd could not read, a timeout.
// It is deliberately not 1: exit 1 means "the query answered, and the queue is
// empty", and a caller that idles or drains on that answer must not do so
// because the query never ran.
const hookExitWorkQueryFailed = 2

// doHook is the pure logic for gc hook. Runs the work query and outputs
// results based on mode. Without inject: prints normalized ready-only output,
// returns 0 if work exists, 1 if empty. A work query that fails returns
// hookExitWorkQueryFailed (2) and names the failure on stderr
// (reportWorkQueryFailure). One that fails because the store could not be read
// (classifyWorkQueryStoreUnavailable) prints nothing on stdout and puts
// GC_HOOK_STORE_UNAVAILABLE on stderr; any other failure first surfaces
// whatever partial output the query produced. With inject: skips the work
// query and returns 0.
func doHook(workQuery, dir string, inject bool, runner WorkQueryRunner, stdout, stderr io.Writer, visibility hookVisibility) int {
	if inject {
		return 0
	}

	output, err := runner(workQuery, dir)
	if err != nil {
		// A store that could not be read is not an empty queue: no partial
		// stdout (the partial text stays in the stderr diagnostic), token +
		// exit 2 (reportWorkQueryFailure). Any other failure first surfaces
		// whatever partial output the query produced.
		if !workQueryStoreUnavailable(err) {
			if normalized := normalizeWorkQueryOutput(strings.TrimSpace(output)); normalized != "" {
				fmt.Fprint(stdout, normalized) //nolint:errcheck // best-effort stdout
			}
		}
		// Plain gc hook keeps exit 1 for an answered, empty queue, so every
		// failure exits 2, store-unavailable or not; reportWorkQueryFailure
		// names it on stderr, with the token when the store could not be read.
		reportWorkQueryFailure("gc hook", err, stderr)
		return hookExitWorkQueryFailed
	}

	trimmed := strings.TrimSpace(output)
	normalized := normalizeWorkQueryOutput(trimmed)
	normalized = filterUnreadyHookCandidates(normalized, time.Now())
	normalized = filterForeignHookCandidates(normalized, visibility)
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
// bd ready semantics: bd's deferred status (dated or not) or a future
// defer_until, any open blocking dep in the row's blocked_by array, the row's
// own is_blocked / status=="blocked" marker, or a canonical dispatch hold
// label. The work_query is expected to gate these, but defensive filtering
// here prevents a single broken query from cascading into agent action on a
// bead it cannot progress.
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
		if isClosedHookCandidate(obj) {
			continue
		}
		if isStoreDeferredHookCandidate(obj) || isFutureDeferredHookCandidate(obj, now) {
			continue
		}
		if isDepBlockedHookCandidate(obj) {
			continue
		}
		if isSelfBlockedHookCandidate(obj) {
			continue
		}
		if isHeldHookCandidate(obj) {
			continue
		}
		// Keep assigned workflow anchors available for continuation, but never
		// offer an unassigned structural latch as fresh worker work.
		assignee, _ := obj["assignee"].(string)
		metadata, _ := obj["metadata"].(map[string]any)
		kind, _ := metadata[beadmeta.KindMetadataKey].(string)
		expanded, _ := metadata[beadmeta.WorkflowExpandedMetadataKey].(string)
		if strings.TrimSpace(assignee) == "" && workflowTopologyIsNonExecutable(kind, expanded) {
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

// filterForeignHookCandidates drops work_query candidates that belong to a
// different agent or are routed to a different agent/rig, mirroring the
// assignee/route eligibility the claim path already enforces
// (hookCandidateVisible). Plain "gc hook" has no claim step to reject
// foreign work at, so under native-store schema skew (gc.routed_to's own
// store-side predicate silently no-ops - ga-lmy6yj) it would otherwise
// display another agent's in-flight or routed work as if it were this
// session's own (ga-1xaqgo.2). Fails open at every decode step -
// unparseable output, non-array output, non-object items, and items that
// fail to decode as a beads.Bead all pass through unchanged - and is a
// no-op entirely when visibility carries no identity/route context, since a
// filter that can silently empty an agent's queue is a worse outage than
// the over-serving it replaces.
func filterForeignHookCandidates(output string, visibility hookVisibility) string {
	if output == "" {
		return output
	}
	if len(visibility.Identities) == 0 && len(visibility.RouteTargets) == 0 {
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
		candidate, ok := decodeHookCandidateBead(obj)
		if !ok {
			filtered = append(filtered, obj)
			continue
		}
		if !hookCandidateVisible(candidate, visibility.Identities, visibility.RouteTargets) {
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

// decodeHookCandidateBead best-effort decodes a raw work_query candidate
// into a beads.Bead so filterForeignHookCandidates can reuse the same
// assignee/route predicates as gc hook --claim. Roundtrips through JSON
// rather than a field-by-field map read because beads.Bead.Metadata
// (StringMap) already carries the bd-CLI type-coercion tolerance that this
// decode should inherit unchanged.
func decodeHookCandidateBead(obj map[string]any) (beads.Bead, bool) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return beads.Bead{}, false
	}
	var candidate beads.Bead
	if err := json.Unmarshal(raw, &candidate); err != nil {
		return beads.Bead{}, false
	}
	return candidate, true
}

// isStoreDeferredHookCandidate reports whether item is a row bd records
// deferred. bd's ready query filters on the raw status, so such a row is not
// work whatever its defer_until says: an elapsed date only means bd's own
// sweep may lift the deferral, and the row is work once the store records
// that (#6207).
func isStoreDeferredHookCandidate(item map[string]any) bool {
	status, ok := item["status"].(string)
	return ok && strings.EqualFold(strings.TrimSpace(status), "deferred")
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

// isSelfBlockedHookCandidate reports whether a candidate carries bd's own
// is_blocked marker or an explicit status=="blocked", independent of the
// blocked_by dependency array checked by isDepBlockedHookCandidate. An
// absent is_blocked field is treated as NOT blocked - bd's denormalized
// projection is not always populated, and over-filtering here would strand
// otherwise-ready work.
func isSelfBlockedHookCandidate(item map[string]any) bool {
	if blocked, ok := item["is_blocked"].(bool); ok && blocked {
		return true
	}
	if status, ok := item["status"].(string); ok && strings.EqualFold(strings.TrimSpace(status), "blocked") {
		return true
	}
	return false
}

// isHeldHookCandidate reports whether item carries a canonical dispatch hold
// label (beadmeta.DispatchHoldLabels) — a bead deliberately parked because its
// required next actor or condition is, by construction, not this session.
//
// Serving one as work is never right for ANY hook path: the assignee cannot
// advance it, and because the assignment is never released the same bead comes
// back on every subsequent tick, so a worker that correctly parked its bead was
// permanently starved of ready work (gas-kg6). The status+assignee tests in
// hookClaimExistingAssignment / claimFirstReadyHookAssignment cannot catch this
// on their own — a held bead is validly in_progress and validly ours — so the
// hold dimension is filtered here, in the one seam every hook path already runs
// through (the claim, the cross-store federation, and plain `gc hook`).
//
// Only the two canonical values match: each label is trimmed and compared
// whole, case-insensitively, against the shared beadmeta constants, so
// unrelated labels that merely look hold-ish (`mpr-human-hold`, the routing
// label `needs-mayor`) are ordinary work and must not be stranded.
//
// An absent or null labels field means "no labels", never "unknown" — bd emits
// labels:null for an unlabeled bead — so this fails open exactly like the
// is_blocked projection above it.
//
// Scope (ga-5736js): this filters what the hook SERVES as work, on every hook
// path. The controller applies the same predicate (beadmeta.HasDispatchHold)
// to assigned OPEN work, so a held OPEN row carries no wake readiness, does not
// keep its assignee awake, and is not a drain-ack strand. The rest of the
// assignment's accounting stays hold-transparent: the assigned-work snapshot,
// orphan release, pool accounting and the in_progress tiers still count it, so
// a held assignment keeps its owner visible to the pool and to crash recovery.
func isHeldHookCandidate(item map[string]any) bool {
	raw, ok := item["labels"].([]any)
	if !ok {
		return false
	}
	labels := make([]string, 0, len(raw))
	for _, entry := range raw {
		if label, ok := entry.(string); ok {
			labels = append(labels, label)
		}
	}
	// One predicate for "held": the controller's wake-demand and drain-ack
	// claimability gates (assignedOpenWorkHeld, classifyDemandRowClaimability)
	// answer with the same function, so they cannot drift from what the hook
	// actually refuses to serve.
	return beadmeta.HasDispatchHold(labels)
}

// isClosedHookCandidate reports whether item is a closed bead. Defense-in-depth
// against upstream Dolt status-index drift that can cause bd list --status=open
// to return closed beads (gcy-1on).
func isClosedHookCandidate(item map[string]any) bool {
	status, ok := item["status"].(string)
	return ok && strings.EqualFold(strings.TrimSpace(status), "closed")
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
