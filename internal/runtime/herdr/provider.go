package herdr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/execgrace"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// Provider implements runtime.Provider (and ServerLifecycleProvider) backed by
// herdr. Model: one shared herdr session (server) per city; within it, one
// workspace per rig (or per town) and one tab per agent, so each gascity session
// is its own switchable "space" rather than a tiled pane. Agents are addressable
// by name, 1:1 with gascity session names. Opt-in via the "herdr" runtime
// selector; tmux default. See herdr-provider-design.md.
type Provider struct {
	c            *client
	metaDir      string        // sidecar KV root (herdr has no per-session metadata store)
	setupTimeout time.Duration // per-command timeout for pre_start/session_setup ([session] setup_timeout)
	// setupMaxTimeout enables the activity-aware pre_start budget
	// ([session] setup_max_timeout): when > 0, runSetupCommand replaces the
	// fixed wall-clock deadline with "no output for setupTimeout" (idle)
	// plus this absolute ceiling.
	setupMaxTimeout time.Duration
	mu              sync.Mutex // serializes workspace/tab find-or-create across concurrent Starts
	// act is the tracker-backed activity source behind GetLastActivity /
	// CanReportActivity (#4217); started lazily on first GetLastActivity.
	act activityTracker
}

// defaultSetupTimeout mirrors the tmux provider's [session] setup_timeout
// default for callers that don't supply one (city-less/standalone construction).
const defaultSetupTimeout = 10 * time.Second

var (
	_ runtime.Provider                = (*Provider)(nil)
	_ runtime.ServerLifecycleProvider = (*Provider)(nil)
)

// New builds a herdr Provider. herdrSession is the shared per-city herdr session
// name; metaDir is a writable directory for sidecar session metadata (a temp
// fallback is used when empty, e.g. a city-less standalone construction); cityRoot
// is the city directory used as the shared server's launch cwd and as the
// effectiveWorkDir fallback for sessions whose WorkDir doesn't exist yet (empty in
// city-less construction). setupTimeout bounds each pre_start/session_setup
// command ([session] setup_timeout); non-positive values fall back to
// defaultSetupTimeout. setupMaxTimeout enables the activity-aware pre_start
// budget ([session] setup_max_timeout).
func New(herdrSession, metaDir, cityRoot string, setupTimeout, setupMaxTimeout time.Duration) *Provider {
	if metaDir == "" {
		// Per-euid, because the fallback path is otherwise identical for every
		// user on the host and os.MkdirAll succeeds on a directory someone else
		// created first. The euid does not make the directory private on its
		// own — SetMeta validates ownership of this root and of the session
		// directory under it before writing — but it keeps two legitimate users
		// off one path so that validation is a real check rather than a
		// permanent outage for whoever logs in second.
		metaDir = filepath.Join(os.TempDir(), fmt.Sprintf("gc-herdr-meta-%d", os.Geteuid()), sanitize(herdrSession))
	}
	if setupTimeout <= 0 {
		setupTimeout = defaultSetupTimeout
	}
	return &Provider{c: newClient(herdrSession, cityRoot), metaDir: metaDir, setupTimeout: setupTimeout, setupMaxTimeout: setupMaxTimeout}
}

// ── ServerLifecycleProvider: own the shared herdr session-server ─────────────

// ConfigureServer ensures the shared herdr session-server is running. A named
// session's socket does not exist until its server starts, so this must run
// before any agent op. Idempotent.
func (p *Provider) ConfigureServer() error { return p.c.startServer() }

// TeardownServer stops the shared herdr session-server after sessions drain.
func (p *Provider) TeardownServer() error { return p.c.stopServer() }

// ── Provider core ────────────────────────────────────────────────────────────

// Start ensures the shared server is up, prepares the session's working
// directory (overlay/CopyFiles staging + pre_start), spawns the agent into its
// placed workspace/tab, runs session_setup, and delivers the startup nudge
// once the agent reaches idle.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	// Herdr starts a shared server before its per-session staging path. Do the
	// complete local, symlink-aware preflight first so malformed input cannot
	// mutate that server. A missing workdir is the one valid deferred case: run
	// its host-side pre_start before configuring the server, then validate the
	// newly materialized directory again.
	if err := runtime.ValidateStageSessionWorkDir(cfg); err != nil {
		if !errors.Is(err, runtime.ErrWorkDirPendingPreStart) {
			return fmt.Errorf("herdr: staging preflight: %w", err)
		}
		// Refuse an already-running session before the deferred pre_start path
		// can run. This preserves Start's idempotence even when the requested
		// workdir does not exist yet. Malformed input has already returned above,
		// so this lookup cannot weaken the no-infrastructure-before-validation
		// boundary.
		if p.IsRunning(name) {
			return runtime.ErrSessionExists
		}
		if err := p.runPreStart(ctx, cfg); err != nil {
			return fmt.Errorf("herdr: running pre_start: %w", err)
		}
		cfg.PreStart = nil
		if err := runtime.ValidateStageSessionWorkDir(cfg); err != nil {
			return fmt.Errorf("herdr: staging preflight after pre_start: %w", err)
		}
	}
	if err := p.ConfigureServer(); err != nil {
		return fmt.Errorf("herdr: configure server: %w", err)
	}
	return p.start(ctx, name, cfg)
}

// start is Start minus the shared-server ensure: the per-session orchestration,
// separated so tests can drive it against a fake herdr CLI without booting a
// real session-server socket (mirroring tmux's Start/doStartSession split).
func (p *Provider) start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.IsRunning(name) {
		return runtime.ErrSessionExists
	}
	// Step 0: stage the working directory, then run pre_start host-side,
	// mirroring the other host-side providers (tmux stageStartFiles +
	// doStartSession Step 0, subprocess/acp StageSessionWorkDir): stage
	// overlays/CopyFiles first, then pre_start (workDir/worktree preparation and
	// the carrier for stage-2 skill/MCP materialization). Both are fatal so an
	// agent never launches into an unprepared workDir — pre_start commands do
	// directory/worktree prep (e.g. pack scripts running `git worktree add` for a
	// per-bead worktree) and launching without them points the agent at the wrong
	// repo. Runs only once we know we're actually creating the agent (the
	// ErrSessionExists check above), so an existing session never re-runs prep.
	// Before this, herdr never staged or ran pre_start — per-bead worktrees were
	// never materialized and effectiveWorkDir silently dropped agents in the city
	// root.
	stagingDeferred := false
	if err := runtime.StageSessionWorkDir(cfg); err != nil {
		if !errors.Is(err, runtime.ErrWorkDirPendingPreStart) {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("herdr: workdir unavailable after staging/pre_start for %q: %w", name, err)
			}
			return fmt.Errorf("herdr: staging workdir for %q: %w", name, err)
		}
		stagingDeferred = true
	}
	if err := p.runPreStart(ctx, cfg); err != nil {
		return fmt.Errorf("herdr: running pre_start: %w", err)
	}
	if stagingDeferred {
		if err := runtime.StageSessionWorkDir(cfg); err != nil {
			return fmt.Errorf("herdr: staging workdir for %q after pre_start: %w", name, err)
		}
	}
	workDir, err := effectiveWorkDir(cfg, p.c.cityRoot)
	if err != nil {
		return fmt.Errorf("herdr: start %q: %w", name, err)
	}
	// Place the agent in its own tab under a per-rig (per-town) workspace, so
	// agents are separate switchable spaces rather than tiled panes. The
	// find-or-create is serialized so concurrent same-rig Starts share one
	// workspace instead of racing to create duplicates. Under herdr ≥0.7.5 the
	// tab's root shell pane — created here with the agent's cwd and env — IS
	// the agent's pane.
	wsLabel, tabLabel := placementFor(name, cfg.Env)
	p.mu.Lock()
	tabID, paneID, err := p.c.ensurePlacement(ctx, wsLabel, tabLabel, workDir, cfg.Env)
	p.mu.Unlock()
	if err != nil {
		return fmt.Errorf("herdr: place %q: %w", name, err)
	}
	spec := launchSpecFor(cfg.Command)
	info := agentInfo{PaneID: paneID, TabID: tabID}
	adopted := false
	mode := bindModeShell
	if spec.Kind != "" {
		mode = bindModeAgent
	}
	// Seed the metadata sidecar from cfg.Env and persist a provisional pane
	// binding BEFORE the launch. The launch below blocks for seconds (shell
	// readiness + herdr's TUI detection), and reconcile ticks that fire in
	// that window read both stores: the pending-create ownership check
	// (runningSessionMatchesPendingCreateInfo) reads GC_SESSION_ID /
	// GC_INSTANCE_TOKEN via GetMeta — with an unseeded sidecar it misreads
	// the fresh runtime as "live runtime belongs to another session" and
	// rolls it back mid-boot — and liveness reads the pane binding. tmux gets
	// the env half for free (its GetMeta reads the session environment, which
	// new-session initializes from cfg.Env); herdr's sidecar is populated
	// only by SetMeta. The seed also persists GC_SESSION_ID for ProcessAlive's
	// session-scoped tree-walk widening (process env survives reparenting).
	// Stop clears the whole meta dir, so teardown is covered, including a
	// launch that fails below.
	if err := p.seedMetaFromEnv(name, cfg.Env); err != nil {
		return fmt.Errorf("herdr: seed session metadata for %q: %w", name, err)
	}
	if err := p.bindPlacement(name, info, mode); err != nil {
		return fmt.Errorf("herdr: persist pane binding for %q: %w", name, err)
	}
	// Clear any unconfirmed-delivery marker a prior life left behind, here
	// rather than beside the delivery below: Stop wipes the whole sidecar but a
	// crash does not, and the delivery is guarded (skipped on adoption and on a
	// session with no startup text), so a clear hanging off it inherits that
	// guard and lets a dead life's marker outlive the life it described. This
	// Start is past the ErrSessionExists check, so a new life owns the sidecar
	// from here; the marker is re-set below only if THIS life's delivery fails.
	// Unconditional and idempotent — RemoveMeta tolerates a missing key.
	if err := p.RemoveMeta(name, metaStartupUnconfirmed); err != nil {
		fmt.Fprintf(os.Stderr, "herdr: clearing prior-life startup marker for %q failed: %v\n", name, err) //nolint:errcheck // best-effort diagnostic
	}
	// Launch. herdr ≥0.7.5's `agent start` launches a supported agent kind's
	// canonical executable into the shell pane and blocks until the TUI is
	// detected (native claude-detection); commands that aren't a clean kind
	// invocation are exec'd through the pane's shell instead, so the pane
	// still dies with the command. On agent_name_taken (a concurrent Start
	// won the name), adopt the live holder or reap a stale one and retry once
	// — never loop placement, which is the pane/PTY/process storm.
	switch {
	case spec.Kind != "":
		// herdr requires the target pane to be "an available shell" — a
		// fresh pane's shell spends its first moments sourcing rc files
		// (agent_pane_busy otherwise), so wait for the prompt, then retry a
		// residual busy rejection briefly.
		p.waitPaneShellReady(ctx, paneID)
		for attempt := 0; ; attempt++ {
			info, adopted, err = p.startAgentAdopting(ctx, name, spec.Kind, paneID, spec.Args)
			if err == nil || herdrErrorCode(err) != "agent_pane_busy" || attempt >= paneBusyRetries {
				break
			}
			// Back off before re-probing: herdr's own shell-prompt detection
			// lags the process-table probe on a fresh pane, so an immediate
			// retry burns the attempt against the same stale verdict.
			select {
			case <-ctx.Done():
				return fmt.Errorf("herdr: start %q: %w", name, ctx.Err())
			case <-time.After(time.Second << attempt):
			}
			p.waitPaneShellReady(ctx, paneID)
		}
		if err == nil && adopted && info.PaneID != "" && info.PaneID != paneID {
			// Adopted a live holder elsewhere: the fresh pane placed above is
			// surplus — close it (with its tab) or it leaks one shell per adopt.
			_ = p.c.tabClose(ctx, tabID)
		}
	case spec.Raw != "":
		// exec through the shell so the pane's root process becomes the
		// command: when it exits the pane (and tab) close, preserving the
		// tmux contract that a session ends with its command. The typed
		// command executes only after the fresh pane's shell finishes
		// initializing, so wait (bounded) for the launch to actually land —
		// otherwise callers probing right after Start see a bare shell.
		if err = p.c.paneRunCommand(ctx, paneID, "exec /bin/sh -c "+shellquote.Quote(spec.Raw)); err == nil {
			p.waitPaneLaunched(ctx, paneID, spec.Raw)
		}
	default:
		// Empty command: the pane's own shell is the session.
	}
	if err != nil {
		return fmt.Errorf("herdr: start %q: %w", name, err)
	}
	// Re-persist the binding with the launch's final placement: adoption may
	// have landed on the live holder's pane rather than the one placed above.
	// This binding is what keeps IsRunning/paneID resolving the session when
	// no registry name exists — herdr ≥0.7.4 clears names on occupant change,
	// and raw/bare-shell sessions never register one (see panebinding.go).
	if err := p.bindPlacement(name, info, mode); err != nil {
		return fmt.Errorf("herdr: persist pane binding for %q: %w", name, err)
	}
	// Post-launch steps mirror tmux's doStartSession ordering: wait for
	// readiness, run session_setup (Step 5.5), then deliver the startup nudge
	// (Step 6).
	//
	// The first turn has two independent sources, mirroring tmux: a named
	// always-awake Claude session carries its behavioral prime in
	// cfg.PromptSuffix (PromptMode=arg); a pool/sling slot carries its claim
	// instruction in cfg.Nudge; a named session may carry BOTH. herdr launches
	// via exec argv and — unlike tmux/acp/t3bridge — has no shell-arg slot to
	// ride PromptSuffix onto, so without this it would drop the prime, boot a
	// bare `claude` REPL, and (because the resolver already set
	// startupPromptDeliveredEnv, suppressing the SessionStart hook's copy of the
	// prime) leave the agent wholly unprimed and idle. startupDeliveryText
	// returns prime-then-nudge when both are set; a pool slot's claim nudge is
	// returned unchanged. Route it through the one hardened post-idle delivery
	// path. See startupDeliveryText.
	//
	// The identity keys (GC_SESSION_ID/GC_INSTANCE_TOKEN/…) a reconcile tick
	// probes via GetMeta during this window are already in the sidecar: Start
	// seeds the whole cfg.Env before the launch (seedMetaFromEnv above), a
	// superset of and earlier than a post-launch identity stamp.
	//
	// Skip the whole provisioning tail when we adopted an already-running
	// holder: it is a live, already-primed agent whose winning Start already ran
	// session_setup and delivered its prime; re-running here would double
	// session_setup and inject the startup prime into a working session.
	startupText := startupDeliveryText(cfg)
	if !adopted && info.PaneID != "" && (startupText != "" || hasSessionSetup(cfg)) {
		// A freshly-spawned agent boots through a shell→TUI handoff before its
		// input prompt is listening; a paste or submit delivered in that window is
		// silently swallowed, leaving the agent idle forever instead of running its
		// first turn. Wait for herdr to report the agent idle (its prompt rendered)
		// before delivering, mirroring how tmux's doStartSession waits for readiness
		// before its Step-5.5 session_setup and Step-6 startup nudge. Idle is
		// necessary but not sufficient — input-readiness lags it — but deliverNudge
		// delegates the type+submit handshake to herdr ≥0.7.5's native `agent
		// prompt`, which owns landing the input reliably rather than trusting a bare
		// `pane run` paste.
		// Bounded and best-effort: on a boot that never idles we deliver anyway (no
		// worse than the prior unconditional send), and the reconciler tolerates a
		// slow Start (pendingCreateNeverStartedTimeout = 10m).
		idleOutcome := p.waitForIdleOutcome(ctx, name, startupNudgeIdleTimeout)
		// session_setup runs host-side ("in gc's process via sh -c", per the Config
		// contract), so herdr can honor it the same way tmux does. Non-fatal, and
		// ordered before the first turn so the agent's workspace is prepared when
		// it starts working.
		p.runSessionSetup(ctx, name, cfg, os.Stderr)
		// Deliver with submission confirmation: the swallowed-CR strand is
		// detected (agent_prompt_stalled) and recovered in-band with an explicit
		// Enter (deliverStartupTurn). An error here means even recovery could not
		// confirm the first turn started — the session is live, so failing Start
		// would only trigger a respawn storm; record the strand durably instead
		// so it is machine-visible and countable. For a warm-bind slot, the
		// event-based claim nudge (startPreparedStartCandidate's warm-reuse
		// branch) also re-delivers on the next reconcile tick; for every other
		// path nudgeStalledPoolClaims remains the reconcile-tick backstop of
		// last resort.
		if startupText != "" {
			if err := p.c.deliverStartupTurn(ctx, info.PaneID, startupText); err != nil {
				p.recordStartupDeliveryUnconfirmed(name, info.PaneID, idleOutcome, err)
			}
		}
	}
	return nil
}

// metaStartupUnconfirmed is the sidecar key recording that this life's startup
// first-turn delivery was never confirmed submitted — either the swallowed-CR
// recovery did not confirm, or the submit landed but never settled into a
// confirming state. The value carries when, the pane, the readiness-guard
// verdict, and herdr's error, so a reader can tell those cases apart rather
// than treating every marker as a stranded turn.
//
// The key means "the CURRENT life", which takes two writes to hold: Stop's
// clearMeta wipes it with the session, and Start clears it unconditionally
// before launching, covering the crash that skips Stop. Start's clear is
// deliberately blind to adoption — an adopted holder is one Start has just
// declared live and already primed, so leaving a prior marker standing would
// hand the reader below a strand report about a session nobody is going to
// re-deliver to. herdr-server.log keeps the historical count (agent.prompt
// outcomes). Consumers: operators (`gc` sidecar inspection) and a future
// named-session delivery backstop (gas-90h fix 3).
const metaStartupUnconfirmed = "GC_HERDR_STARTUP_DELIVERY_UNCONFIRMED"

// recordStartupDeliveryUnconfirmed persists an unconfirmed startup delivery on
// the session's sidecar and mirrors it to stderr for interactive runs. stderr
// alone is not enough: daemonized controllers devnull it, which is how this
// failure stayed invisible for 20h of live operation (gas-90h).
func (p *Provider) recordStartupDeliveryUnconfirmed(name, paneID string, idleOutcome idleWaitOutcome, derr error) {
	detail := fmt.Sprintf("%s pane=%s idle_wait=%s: %v",
		time.Now().UTC().Format(time.RFC3339), paneID, idleOutcome, derr)
	if err := p.SetMeta(name, metaStartupUnconfirmed, detail); err != nil {
		fmt.Fprintf(os.Stderr, "herdr: recording unconfirmed startup delivery for %q failed: %v\n", name, err) //nolint:errcheck // best-effort diagnostic
	}
	fmt.Fprintf(os.Stderr, "herdr: startup delivery for %q not confirmed: %v\n", name, derr) //nolint:errcheck // best-effort diagnostic
}

// runSessionSetup runs cfg.SessionSetup commands then cfg.SessionSetupScript
// host-side after the agent is up, mirroring the tmux adapter's
// runSessionSetup (Step 5.5): commands execute in gc's process via sh -c with
// GC_SESSION added to the env, and failures are non-fatal warnings — the
// session still works. The tmux-specific GC_TMUX_SOCKET is not injected here;
// setup scripts that shell out to tmux are inapplicable under herdr.
//
// session_live is deliberately NOT wired: its documented use is tmux cosmetics
// (theming, keybindings, status bars), and herdr's RunLive stays a no-op like
// subprocess/acp.
func (p *Provider) runSessionSetup(ctx context.Context, name string, cfg runtime.Config, stderr io.Writer) {
	if !hasSessionSetup(cfg) {
		return
	}
	setupEnv := make(map[string]string, len(cfg.Env)+1)
	for k, v := range cfg.Env {
		setupEnv[k] = v
	}
	setupEnv["GC_SESSION"] = name
	for i, cmd := range cfg.SessionSetup {
		if err := p.runSetupCommand(ctx, cmd, setupEnv); err != nil {
			fmt.Fprintf(stderr, "gc: session_setup[%d] warning: %v\n", i, err) //nolint:errcheck // best-effort warning
		}
	}
	if cfg.SessionSetupScript != "" {
		if err := p.runSetupCommand(ctx, cfg.SessionSetupScript, setupEnv); err != nil {
			fmt.Fprintf(stderr, "gc: session_setup_script warning: %v\n", err) //nolint:errcheck // best-effort warning
		}
	}
}

// hasSessionSetup reports whether cfg carries any session_setup work.
func hasSessionSetup(cfg runtime.Config) bool {
	return len(cfg.SessionSetup) > 0 || cfg.SessionSetupScript != ""
}

// startupDeliveryText resolves the first-turn text Start delivers to a freshly
// spawned agent. Two independent sources, mirroring the tmux provider — which
// rides the behavioral prime on the launch arg (buildLaunchCommand) and sends the
// nudge as a separate Step-6 keystroke:
//
//   - a named always-awake Claude session carries its behavioral prime in
//     cfg.PromptSuffix (PromptMode=arg, shell-quoted for argv use that herdr's
//     exec launch has no slot for); startupPrimeText unquotes it.
//   - a pool/sling slot carries its claim instruction in cfg.Nudge.
//
// A session may carry BOTH — a named session whose pack also configures a startup
// nudge (e.g. an oversight tick). Deliver the prime first (the behavioral prompt)
// then the nudge (the first task): returning only the nudge left such sessions
// unprimed, because the prime was dropped and GC_STARTUP_PROMPT_DELIVERED=1 also
// suppresses the SessionStart hook's fallback copy of the prime. A pool slot has
// no prime, so its claim nudge is returned byte-for-byte unchanged. Returns ""
// when there is nothing to deliver (deterministic workers, suppressed prompt).
func startupDeliveryText(cfg runtime.Config) string {
	prime := startupPrimeText(cfg)
	if prime == "" {
		return cfg.Nudge
	}
	if cfg.Nudge == "" {
		return prime
	}
	return prime + "\n\n" + cfg.Nudge
}

// startupPrimeText recovers the behavioral prime from cfg.PromptSuffix, which is
// shell-quoted for the launch-arg slot that herdr's exec launch lacks — mirroring
// the parts[0] round-trip used on the resume path in session_lifecycle_parallel.go.
// Falls back to the raw string if it somehow fails to unquote: delivering
// something beats stranding the agent idle. Returns "" when no prime is set.
func startupPrimeText(cfg runtime.Config) string {
	if cfg.PromptSuffix == "" {
		return ""
	}
	if parts := shellquote.Split(cfg.PromptSuffix); len(parts) > 0 {
		return parts[0]
	}
	return cfg.PromptSuffix
}

// startupNudgeIdleTimeout bounds how long Start waits for a freshly-spawned
// agent to reach its idle input prompt before delivering the startup nudge. The
// wait returns as soon as the agent idles (typically a few seconds); the bound
// only bites on a boot that never idles, after which delivery proceeds anyway —
// deliverStartupTurn confirms (or recovers) the submit either way. Sized
// generously to cover cold, concurrent boots during a town-wide restart.
const startupNudgeIdleTimeout = 60 * time.Second

const (
	// preStartOutputLimit bounds the captured output tail attached to a failed
	// pre_start error (mirrors tmux's setupCommandOutputLimit).
	preStartOutputLimit = 4096
	// preStartWaitDelay force-closes the capture pipes shortly after the command
	// exits, so a pre_start that daemonizes a child holding inherited stdio
	// cannot hang the start (mirrors tmux's setupCommandWaitDelay).
	preStartWaitDelay = 2 * time.Second
	// preStartCancelGrace is the rollback-trap budget when the activity-aware
	// setup budget is enabled (mirrors tmux's setupCancelGrace).
	preStartCancelGrace = 10 * time.Second
)

// runPreStart runs cfg.PreStart shell commands on the host before the agent is
// created, mirroring the tmux provider (tmux/adapter.go runPreStart).
//
// This is load-bearing beyond directory/worktree prep: stage-2 skill/MCP
// materialization is delivered *as* a PreStart entry, so a runtime that skips
// PreStart silently drops materialization. That is precisely why herdr was held
// out of isStage2EligibleSession (see cmd/gc/skill_integration.go) — without
// this, an MCP-configured agent under herdr either hard-fails
// ("effective MCP cannot be delivered ... with session provider herdr") or, if
// naively allowlisted, starts with its MCP silently missing.
//
// Failures are fatal, as in tmux: an agent must never launch into an unprepared
// workDir.
func (p *Provider) runPreStart(ctx context.Context, cfg runtime.Config) error {
	if len(cfg.PreStart) == 0 {
		return nil
	}
	for i, cmd := range cfg.PreStart {
		if err := p.runSetupCommand(ctx, cmd, cfg.Env); err != nil {
			return fmt.Errorf("pre_start[%d]: %w", i, err)
		}
	}
	return nil
}

// runSetupCommand executes one setup command under the provider's setupTimeout,
// mirroring tmux's tmuxStartOps.runSetupCommand: `sh -c <cmd>`, cwd from GC_DIR
// (the workDir a pre_start may itself be creating — so it is intentionally read
// from env rather than cfg.WorkDir), process env plus cfg.Env.
func (p *Provider) runSetupCommand(ctx context.Context, cmd string, env map[string]string) error {
	timeout := p.setupTimeout
	if timeout <= 0 {
		timeout = defaultSetupTimeout
	}
	// Deadline shape (mirrors tmux's runSetupCommand): with setupMaxTimeout
	// unset the historical fixed wall-clock deadline applies; with it set the
	// budget is activity-aware — timeout bounds output silence,
	// setupMaxTimeout bounds total runtime.
	idle, grace := time.Duration(0), preStartWaitDelay
	if p.setupMaxTimeout > 0 {
		idle, grace = timeout, preStartCancelGrace
	}
	mon := execgrace.NewMonitor(ctx, idle, p.setupMaxTimeout)
	defer mon.Stop()
	runCtx := mon.Context()
	if !mon.Enabled() {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	c := exec.CommandContext(runCtx, "sh", "-c", cmd)
	// cwd from GC_DIR when it exists; otherwise fall back to the city root —
	// the same not-yet-created-workDir fallback effectiveWorkDir applies to the
	// agent itself. A pool session's worktree is often created concurrently with
	// (or by) pre_start, so chdir'ing into it unconditionally fails fast with
	// "chdir ... no such file" on resume-path starts that run before the
	// worktree lands. The injected pre_start commands carry their target as an
	// explicit --workdir flag and do not depend on cwd.
	if workDir := strings.TrimSpace(env["GC_DIR"]); workDir != "" {
		if _, err := os.Stat(workDir); err == nil {
			c.Dir = workDir
		} else if p.c.cityRoot != "" {
			c.Dir = p.c.cityRoot
		}
	}
	c.Env = os.Environ()
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	var out bytes.Buffer
	w := mon.Writer(&out)
	c.Stdout, c.Stderr = w, w
	// Cooperative cancellation (execgrace.Apply): deadline expiry interrupts
	// the command's process group first so shell rollback traps run before
	// the forced kill; the grace doubles as the pipe-closing WaitDelay
	// (mirrors tmux's runSetupCommand).
	execgrace.Apply(c, grace)
	if err := c.Run(); err != nil {
		// ErrWaitDelay means the command itself exited successfully and only the
		// force-closed pipes ended the wait: a setup command that daemonizes a
		// child holding inherited stdio succeeded (mirrors tmux).
		if errors.Is(err, exec.ErrWaitDelay) {
			return nil
		}
		// context.Cause surfaces which budget fired (execgrace.ErrIdle,
		// execgrace.ErrCeiling, or the fixed deadline's DeadlineExceeded).
		if ctxErr := context.Cause(runCtx); ctxErr != nil && runCtx.Err() != nil {
			err = fmt.Errorf("%w: %w", ctxErr, err)
		}
		// The command's own output can echo a credential back at us — `set -x`
		// traces every expansion, and a failing curl prints the header it sent.
		// This error is durable (logs, event bus, bead notes) for the same reason
		// the client's are.
		//
		// Both environments it was given, not just the session env: c.Env starts
		// from os.Environ() above, so the controller's own credentials are in
		// scope for that echo as much as the session's.
		//
		// [runtime.RedactSecretsTail] scrubs before it truncates, which is the
		// order that matters: redacting a cut tail would leave a straddling
		// credential decapitated and no longer matching itself. Unlike tmux's
		// bounded writer this holds the whole output already, so nothing has to
		// be retained to make that work.
		tail, _ := runtime.RedactSecretsTail(out.String(), preStartOutputLimit, runtime.SetupCommandSecrets(env))
		if tail = strings.TrimSpace(tail); tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	return nil
}

// Stop closes the agent's pane and clears its metadata sidecar. Idempotent.
// The pane resolves through the sidecar binding when the name is gone — the
// earlier "sleep leak" was exactly this gap: name lost ⇒ pane never found ⇒
// closePane never issued ⇒ panes piled up across witness sleep cycles.
func (p *Provider) Stop(name string) error {
	ctx := context.Background()
	pid, err := p.paneID(ctx, name)
	if err == nil && pid != "" {
		_ = p.c.closePane(ctx, pid)
	}
	_ = p.clearMeta(name)
	return nil
}

// Interrupt sends a soft ctrl+c to the agent (herdr exposes no signal API).
func (p *Provider) Interrupt(name string) error {
	ctx := context.Background()
	pid, err := p.paneID(ctx, name)
	if err != nil || pid == "" {
		return nil
	}
	return p.c.sendKeys(ctx, pid, "ctrl+c") // herdr has no signal API; ctrl+c is the soft interrupt
}

// IsRunning reports whether the agent's session is running: its name is live
// in herdr's registry OR its bound pane still runs its session (raw sessions
// never register a name; herdr ≥0.7.4 clears names on occupant change — a
// name-only check re-Starts live sessions every tick: the spawn storm). An
// exited agent whose pane idles at a shell prompt is NOT running, so
// restarts still happen.
func (p *Provider) IsRunning(name string) bool {
	_, running, err := resolveBinding(p.lookupOps(context.Background(), name))
	return err == nil && running
}

// IsAttached reports false: herdr 0.7.1 exposes no clean attach-state query.
func (p *Provider) IsAttached(_ string) bool { return false }

// Attach runs `herdr agent attach`, blocking until the user detaches.
func (p *Provider) Attach(name string) error {
	cmd := exec.Command(p.c.bin, "--session", p.c.session, "agent", "attach", herdrAgentName(name))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run() // blocks until the user detaches
}

// ProcessAlive reports whether the agent's pane has a live foreground process,
// optionally requiring one of processNames to be present.
//
// Foreground-process matching alone misses an agent that runs as a
// descendant of a wrapper process rather than as the pane's foreground itself
// — e.g. a mayor session launched under macOS `caffeinate` (a keep-awake
// wrapper): caffeinate stays the pane's reported foreground for the agent's
// entire lifetime, with the agent running underneath it as a child. That
// foreground-only check reports Alive=false for a session that is very much
// alive, which upstream (lifecycle_projection.go) reads as "runtime missing"
// and drives an endless respawn loop. So: check the cheap foreground list
// first, then fall back to a host process-table walk from the pane's shell
// and foreground PIDs to catch a wanted name living deeper in the tree.
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	ctx := context.Background()
	pid, err := p.paneID(ctx, name)
	if err != nil || pid == "" {
		return false
	}
	return p.processAliveByPane(ctx, name, pid, processNames)
}

// processAliveByPane reports whether the process tree rooted at paneID runs one
// of processNames. It is the shared core of ProcessAlive and the adopt decision
// in Start: ProcessAlive resolves the pane from the session name, while the
// adopt path already holds the contested holder's pane id. The session-scoped
// tree-walk widening (#4225) is still keyed by session name via GetMeta.
func (p *Provider) processAliveByPane(ctx context.Context, name, paneID string, processNames []string) bool {
	if paneID == "" {
		return false
	}
	shellPID, fg, err := p.c.processInfo(ctx, paneID)
	if err != nil || shellPID == 0 {
		return false
	}
	if len(processNames) == 0 {
		return true // per contract
	}
	for _, pr := range fg {
		for _, want := range processNames {
			if pr.Name == want {
				return true
			}
		}
	}
	sessionID, _ := p.GetMeta(name, "GC_SESSION_ID")
	return processTreeAlive(shellPID, fg, processNames, strings.TrimSpace(sessionID))
}

// startAgentAdopting issues the kind-launch agent start and, on herdr's
// agent_name_taken rejection (a concurrent Start won the name), adopts the
// live holder or reaps a stale one and retries once — breaking the recreate
// storm (see resolveAgentNameTaken). Holder liveness is the pane busy probe:
// a contested holder whose pane runs a foreground process is a live agent
// (version-robust, unlike matching claude ≥2.1.x's comm strings). adopted is
// true only when an already-running holder was adopted, so the caller can
// skip re-priming a live agent.
func (p *Provider) startAgentAdopting(ctx context.Context, name, kind, paneID string, args []string) (info agentInfo, adopted bool, err error) {
	hn := herdrAgentName(name) // herdr ≥0.7.5 rejects raw gc session names (invalid_agent_name)
	started, startErr := p.c.startAgentKind(ctx, hn, kind, paneID, args)
	return resolveAgentNameTaken(started, startErr, agentStartOps{
		getAgent: func() (agentInfo, bool, error) { return p.c.getAgent(ctx, herdrAgentName(name)) },
		paneAlive: func(holderPane string) bool {
			probe, perr := p.probePane(ctx, holderPane)
			return perr == nil && probe.Exists && probe.Busy
		},
		closePane:  func(holderPane string) error { return p.c.closePane(ctx, holderPane) },
		retryStart: func() (agentInfo, error) { return p.c.startAgentKind(ctx, hn, kind, paneID, args) },
	})
}

// paneBusyRetries bounds how many agent_pane_busy rejections the kind launch
// retries after re-waiting for the shell prompt (races between the readiness
// probe and herdr's own availability check).
const paneBusyRetries = 3

// paneShellReadyWait bounds the wait for a fresh pane's shell to reach its
// interactive prompt (rc files can run for seconds and spawn foreground
// children). Best-effort: on timeout the launch proceeds and surfaces
// herdr's own verdict.
const paneShellReadyWait = 15 * time.Second

// waitPaneShellReady polls the pane until it idles at a bare interactive
// shell prompt — what herdr's `agent start` requires of its target pane.
func (p *Provider) waitPaneShellReady(ctx context.Context, paneID string) {
	deadline := time.Now().Add(paneShellReadyWait)
	for time.Now().Before(deadline) {
		probe, err := p.probePane(ctx, paneID)
		if err == nil && probe.Exists && !probe.Busy {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// rawLaunchWait bounds how long Start's raw path waits for the typed
// `exec /bin/sh -c …` to actually execute in the fresh pane. The typed launch
// runs only after the pane's shell finishes initializing (rc files can take
// seconds and spawn their own foreground children, so pane busyness alone
// cannot confirm the launch). The bound only bites on a wedged shell, after
// which Start proceeds best-effort (the reconciler tolerates a slow launch).
const rawLaunchWait = 15 * time.Second

// waitPaneLaunched polls the pane until the launched `/bin/sh -c <raw>` shows
// up in its foreground (exec preserves argv), the pane is gone (the command
// already ran and exited), or the bound elapses. Best-effort by design.
func (p *Provider) waitPaneLaunched(ctx context.Context, paneID, raw string) {
	deadline := time.Now().Add(rawLaunchWait)
	for time.Now().Before(deadline) {
		shellPID, fg, err := p.c.processInfo(ctx, paneID)
		switch {
		case err != nil && (strings.Contains(err.Error(), "not_found") || strings.Contains(err.Error(), "not found")):
			return // pane already gone: the command ran and exited
		case err == nil && shellPID != 0 && (paneRunsCommand(fg, raw) || paneRootReplaced(shellPID, fg)):
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// processTreeAlive is the descendant-walk fallback for ProcessAlive: it takes
// a host-wide process snapshot and checks whether any process reachable from
// the pane's shell PID or foreground PIDs matches one of processNames. When
// sessionID is non-empty, every process in the snapshot carrying that
// GC_SESSION_ID is also treated as a root — this widens the walk to find the
// agent even when it has been reparented off the shell/foreground subtree,
// since process env (unlike ppid) survives reparenting. Purely additive: it
// never narrows the shell/foreground-rooted match, so a genuinely-dead agent
// still reports false.
var snapshotProcesses = proctable.SnapshotProcesses

func processTreeAlive(shellPID int, fg []proc, processNames []string, sessionID string) bool {
	records, err := snapshotProcesses()
	if err != nil || len(records) == 0 {
		return false
	}
	roots := make([]int, 0, len(fg)+1)
	if shellPID != 0 {
		roots = append(roots, shellPID)
	}
	for _, pr := range fg {
		roots = append(roots, pr.PID)
	}
	if sessionID != "" {
		for _, r := range records {
			if r.SessionID == sessionID {
				roots = append(roots, r.PID)
			}
		}
	}
	return proctable.DescendantAlive(records, roots, processNames)
}

// ObserveLiveness reports session presence (Running) and agent-process
// liveness (Alive) in one `agent get` pass, derived from herdr's own agent
// registry and status — the herdr analog of the tmux provider's
// ObserveLiveness. This is the LivenessObserver fast-path that
// runtime.ObserveLiveness prefers over the generic IsRunning + ProcessAlive
// fold, so it is what every liveness consumer (the API observer, the session
// manager, the worker handle, and city_runtime) actually reads.
//
// It deliberately does NOT consult the ProcessAlive host process-table walk.
// That walk locates the agent by matching a configured process name against
// the pane's process tree, widened by the GC_SESSION_ID carried in the
// process environment — and both signals are unreliable for claude >= 2.1.x:
// it runs as comm="<version>" (e.g. "2.1.216") rather than "claude", and it
// does not reliably export GC_SESSION_ID through herdr's `/bin/sh -c` launch
// wrapper (measured: most live claude procs carry no readable GC_SESSION_ID).
// The walk therefore false-negatives a live-but-idle singleton (mayor/adjunct),
// which upstream reads as "runtime missing" and drives an endless
// continuation-reset / quarantine loop. herdr tracks the pane's agent process
// directly, so its agent_status does not depend on either fragile signal and
// keeps a live orchestrator classified alive. ProcessAlive is retained
// unchanged for the non-observer call sites (doctor) and the caffeinate-wrapper
// case; processNames is unused here because herdr's status supersedes it.
func (p *Provider) ObserveLiveness(name string, _ []string) runtime.Liveness {
	if strings.TrimSpace(name) == "" {
		return runtime.Liveness{}
	}
	ctx := context.Background()
	info, present, err := p.c.getAgent(ctx, herdrAgentName(name))
	if err == nil && !present {
		// Name absent — fall back to the bound pane before declaring the
		// session gone: raw shell sessions never register a name at all, and
		// herdr ≥0.7.4 clears a registered name on occupant change. A binding
		// that resolves as running means the session is up even though no
		// agent_status is readable; report alive, matching
		// agentAliveFromStatus's fail-safe direction. A confirmed-gone pane
		// clears the stale binding; a transport failure clears nothing and
		// falls through to not-running (as a failed name query already does).
		if _, running, perr := resolveBinding(p.lookupOps(ctx, name)); perr == nil && running {
			return runtime.Liveness{Running: true, Alive: true}
		}
	}
	return livenessFromAgent(info, present, err)
}

// livenessFromAgent folds a herdr `agent get` result into a Liveness verdict.
// Split from ObserveLiveness so the decision is unit-testable without shelling
// out to herdr. A failed query or an absent agent is not running; a present
// agent is running, and its aliveness follows herdr's reported agent_status.
func livenessFromAgent(info agentInfo, present bool, err error) runtime.Liveness {
	if err != nil || !present {
		return runtime.Liveness{}
	}
	return runtime.Liveness{Running: true, Alive: agentAliveFromStatus(info.AgentStatus)}
}

// agentAliveFromStatus maps a herdr agent_status to agent-process liveness. An
// agent present in herdr's registry is alive unless herdr reports an explicit
// terminal status: any active status (idle, working, done, running, …) means
// the pane process is up, while a finished/gone marker means it has exited so a
// genuine crash still restarts. Unknown or empty statuses fail SAFE toward
// alive — the bug this fixes is a live singleton misread as dead (which drives
// a destructive reset loop), so a missed restart of a truly-dead agent (visible
// and non-destructive) is the acceptable direction to err. The terminal set is
// validated against live herdr output during rollout; extend it there.
func agentAliveFromStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "exited", "stopped", "dead", "gone", "terminated", "closed", "crashed":
		return false
	default:
		return true
	}
}

// Nudge injects and submits text into a running agent's input.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	ctx := context.Background()
	pid, err := p.paneID(ctx, name)
	if err != nil || pid == "" {
		return runtime.ErrSessionNotFound
	}
	return p.c.deliverNudge(ctx, pid, runtime.FlattenText(content))
}

// Peek reads the current rendered screen ("visible") — the liveness/fingerprint
// snapshot. It reads by pane (resolved through the binding when the registry
// name is gone), since raw shell sessions have no registered agent to read.
func (p *Provider) Peek(name string, lines int) (string, error) {
	ctx := context.Background()
	pid, err := p.paneID(ctx, name)
	if err != nil {
		return "", err
	}
	if pid == "" {
		return "", runtime.ErrSessionNotFound
	}
	return p.c.paneRead(ctx, pid, "visible", lines)
}

// ListRunning returns the names of running sessions whose names start with
// prefix. The sidecar bindings are the primary source (they hold the exact
// gc names — herdr's registry stores the mapped herdrAgentName forms, and
// never sees raw shell sessions at all); each bound candidate is verified
// running before it is listed. Registry agents that don't correspond to any
// bound gc session (foreign/manual agents) are appended under their own
// names.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	ctx := context.Background()
	agents, err := p.c.listAgents(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)   // gc names already listed
	mapped := make(map[string]bool) // herdr-side names owned by bound gc sessions
	var out []string
	for _, name := range p.boundSessionNames() {
		mapped[herdrAgentName(name)] = true
		if !strings.HasPrefix(name, prefix) || seen[name] {
			continue
		}
		if _, running, err := resolveBinding(p.lookupOps(ctx, name)); err == nil && running {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, a := range agents {
		if !mapped[a.Name] && strings.HasPrefix(a.Name, prefix) && !seen[a.Name] {
			seen[a.Name] = true
			out = append(out, a.Name)
		}
	}
	return out, nil
}

// SendKeys translates tmux-style key names and sends them to the agent's pane.
func (p *Provider) SendKeys(name string, keys ...string) error {
	ctx := context.Background()
	pid, err := p.paneID(ctx, name)
	if err != nil || pid == "" {
		return nil
	}
	hk := make([]string, len(keys))
	for i, k := range keys {
		hk[i] = translateKey(k)
	}
	return p.c.sendKeys(ctx, pid, hk...)
}

// Capabilities reports which optional provider features this backend supports.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{
		CanReportAttachment: false, // no clean IsAttached query
		CanReportActivity:   true,  // tracker-backed GetLastActivity (activity.go)
		CanStream:           true,  // push session-event stream via SubscribeSessionEvents (events.subscribe socket API)
		CanAttachTTY:        true,  // agent attach
		// Reporting activity must not turn off the stalled-claim nudge
		// backstop: a swallowed startup paste still has no relaunch/respawn
		// redelivery path here (Relaunch is deliberately unimplemented —
		// Stop+Start only), so the backstop stays the recovery of record.
		NeedsClaimBackstop: true,
	}
}

// GetLastActivity reports the session's last observed activity, maintained by
// the lazily started activity tracker (activity.go): now while the agent's
// status sits at working, the frozen stamp of its last observed change
// otherwise, and the zero time for sessions the tracker has not observed. The
// error is always nil — a tracker that cannot reach the server keeps its last
// known state, and never-observed sessions read as unknown (zero), which every
// consumer already treats as "no signal".
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	p.act.start(p, name)
	return p.act.lastActivity(name), nil
}

// ── best-effort / unsupported (the contract permits these) ───────────────────

// ClearScrollback is a no-op: herdr exposes no scrollback-clear op.
func (p *Provider) ClearScrollback(_ string) error { return nil }

// RunLive is a no-op: herdr agents are launched at Start.
func (p *Provider) RunLive(_ string, _ runtime.Config) error { return nil }

// CopyTo copies a local path into the agent's working directory (best-effort).
func (p *Provider) CopyTo(name, src, relDst string) error {
	if err := runtime.ValidateCopyRelDst(relDst); err != nil {
		return err
	}
	if _, err := os.Stat(src); err != nil {
		return nil // best-effort: missing src
	}
	a, ok, err := p.c.getAgent(context.Background(), herdrAgentName(name))
	if err != nil || !ok || a.Cwd == "" {
		return nil
	}
	// An empty relDst means "into the workdir under the source's own name".
	// Joining "" targets the directory itself, which copyPath cannot write a
	// file to — preserve the basename, as the other providers do.
	if relDst == "" {
		relDst = filepath.Base(src)
	}
	if err := runtime.StageWorkDir(a.Cwd, "", []runtime.CopyEntry{{Src: src, RelDst: relDst}}); err != nil {
		return fmt.Errorf("herdr: copying into session %q: %w", name, err)
	}
	return nil
}

// ── metadata sidecar (herdr has no per-session KV) ───────────────────────────

// seedMetaFromEnv initializes the session's metadata sidecar from cfg.Env,
// mirroring tmux's contract where the session environment (seeded from cfg.Env
// at creation) doubles as the GetMeta store. Ownership/identity keys like
// GC_SESSION_ID and GC_INSTANCE_TOKEN must be readable via GetMeta from the
// moment the runtime is alive. Later SetMeta calls override individual keys,
// exactly as tmux setenv does. Start calls this before the launch, so it is a
// superset of (and earlier than) the identity-key-only stampIdentityMeta below.
//
// It seeds the classified half of the environment rather than all of it. tmux's
// store is the session environment, which dies with the server; herdr's is a
// directory of files that outlives every session it describes, so seeding
// cfg.Env wholesale writes the agent's API keys to disk with no reader that
// ever wants them back. [runtime.SplitEnvForMetaSeed] keeps exactly the keys a
// GetMeta consumer reads and withholds the rest.
func (p *Provider) seedMetaFromEnv(name string, env map[string]string) error {
	seed, _ := runtime.SplitEnvForMetaSeed(env)
	for k, v := range seed {
		if err := p.SetMeta(name, k, v); err != nil {
			return fmt.Errorf("meta %q: %w", k, err)
		}
	}
	return nil
}

// identityMetaKeys are the session-identity keys the reconciler probes via
// GetMeta to bind a live runtime to its session bead (session id, instance
// token, runtime epoch).
var identityMetaKeys = []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH"}

// stampIdentityMeta copies just the identity keys present in env into the
// sidecar, best-effort. Start uses seedMetaFromEnv (the full env, pre-launch)
// instead; this narrower helper remains for callers that only need the identity
// subset stamped.
func (p *Provider) stampIdentityMeta(name string, env map[string]string) {
	for _, key := range identityMetaKeys {
		if v := env[key]; v != "" {
			if err := p.SetMeta(name, key, v); err != nil {
				fmt.Fprintf(os.Stderr, "herdr: stamping %s for %q: %v\n", key, name, err) //nolint:errcheck // best-effort diagnostic
			}
		}
	}
}

// SetMeta writes a per-session metadata value to the sidecar store (herdr has
// no per-session KV).
//
// The value is written owner-only under an owner-only directory: even filtered,
// the sidecar holds the incarnation fence token, and a forged or read fence is
// how a stale process talks its way past drain. Modes are applied explicitly
// rather than through MkdirAll/WriteFile's perm argument, which is consulted
// only when the path is created — a host that already ran an older binary keeps
// its 0755 directory and 0644 files otherwise, which is exactly the host that
// has credentials on disk already.
//
// The root is checked as well as the per-session leaf. EnsurePrivateDir only
// inspects the path it is handed, and MkdirAll walks through pre-existing
// ancestors without looking at them — so validating the leaf alone would leave
// an attacker-owned root holding the victim's session directory, and the owner
// of a directory can replace what is inside it between the check and the write.
func (p *Provider) SetMeta(name, key, value string) error {
	if err := runtime.EnsurePrivateDir(p.metaDir); err != nil {
		return err
	}
	dir := filepath.Join(p.metaDir, sanitize(name))
	if err := runtime.EnsurePrivateDir(dir); err != nil {
		return err
	}
	return runtime.WritePrivateFile(filepath.Join(dir, sanitize(key)), []byte(value))
}

// GetMeta reads a per-session metadata value from the sidecar store; a missing
// key returns an empty string.
func (p *Provider) GetMeta(name, key string) (string, error) {
	b, err := os.ReadFile(filepath.Join(p.metaDir, sanitize(name), sanitize(key)))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// RemoveMeta deletes a per-session metadata value from the sidecar store.
// Idempotent.
func (p *Provider) RemoveMeta(name, key string) error {
	err := os.Remove(filepath.Join(p.metaDir, sanitize(name), sanitize(key)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (p *Provider) clearMeta(name string) error {
	return os.RemoveAll(filepath.Join(p.metaDir, sanitize(name)))
}

// ── helpers ──────────────────────────────────────────────────────────────────

// paneID resolves a gascity session name to its herdr pane id (or "" if
// absent): registry name lookup first, then the sidecar pane binding Start
// persisted — the only handle for raw shell sessions and for agents whose
// registry name herdr cleared (see panebinding.go). The pane resolves
// whenever it still exists, even for an exited agent, so Stop/keys/read keep
// working on it.
func (p *Provider) paneID(ctx context.Context, name string) (string, error) {
	pane, _, err := resolveBinding(p.lookupOps(ctx, name))
	return pane, err
}

// workspaceTabFor maps a gascity runtime session name to its herdr placement: a
// per-rig (or per-town) workspace label and a per-agent tab label. Runtime names
// are "<rig>--<town>__<agent>" (rig-qualified; citylayout maps "/" → "--") or
// "<town>__<agent>" (town-level). Workspace = the rig when present, else the
// town; tab = the agent (the segment after the last "__"). Falls back to the
// whole name when those separators are absent (defensive for non-gc names).
func workspaceTabFor(name string) (workspace, tab string) {
	rest := name
	if i := strings.Index(name, "--"); i >= 0 {
		workspace, rest = name[:i], name[i+2:]
	} else if j := strings.Index(name, "__"); j >= 0 {
		workspace = name[:j]
	} else {
		workspace = name
	}
	if k := strings.LastIndex(rest, "__"); k >= 0 {
		tab = rest[k+2:]
	} else {
		tab = rest
	}
	if workspace == "" {
		workspace = name
	}
	if tab == "" {
		tab = name
	}
	return workspace, tab
}

// placementFor decides a session's herdr workspace and tab. It starts from the
// structural runtime name (workspaceTabFor) and then refines it with the richer
// identity the reconciler injects into the environment — the same GC_RIG /
// GC_ALIAS convention the t3bridge and k8s providers use (session/manager.go
// populates these via RuntimeEnvWithSessionContext).
//
// This matters for ephemeral pool wisps: their runtime name is town-qualified
// (e.g. "gastown__polecat-az-wisp-3nvj3yx", where "az" is the pouring scope's
// beads prefix and varies by city), so workspaceTabFor alone drops them
// in the town workspace under an opaque wisp-id tab. GC_RIG restores the
// originating rig workspace (webapp/mobile), and GC_ALIAS swaps the wisp id for
// the themed instance name, yielding e.g. workspace "webapp", tab
// "polecat-furiosa". Persistent and town-level sessions are unaffected: they
// either carry no GC_RIG (town agents) or already resolve to the same labels.
func placementFor(name string, env map[string]string) (workspace, tab string) {
	workspace, tab = workspaceTabFor(name)
	if len(env) == 0 {
		return workspace, tab
	}
	// Group under the originating rig when known. Town-level agents (mayor,
	// deacon, …) have no GC_RIG and keep their town workspace.
	if rig := strings.TrimSpace(env["GC_RIG"]); rig != "" {
		workspace = rig
	}
	// Replace a wisp id with the themed instance alias so tabs read e.g.
	// "polecat-furiosa" rather than "polecat-az-wisp-3nvj3yx". The role prefix
	// (everything before the wisp id) is preserved. Falls through unchanged when
	// no alias is available yet, or when the alias is itself the wisp identity.
	if i := wispIDStart(tab); i >= 0 {
		alias := strings.TrimSpace(env["GC_ALIAS"])
		if alias == "" {
			alias = strings.TrimSpace(env["GC_AGENT"])
		}
		if leaf := lastSegment(alias); leaf != "" && wispIDStart(leaf) < 0 {
			tab = tab[:i] + leaf
		}
	}
	return workspace, tab
}

// wispIDStart reports the index where a wisp id begins inside s, or -1 when s
// carries none. A wisp id is a bead id minted by PourWisp, so it reads
// "<prefix>-wisp-<suffix>" where <prefix> is the beads prefix of the scope that
// poured it — "gc" in a default city, but "az", "py", … anywhere else.
// Detection therefore keys on the "-wisp-" infix and walks back over the prefix
// token; keying on a literal "gc-wisp-" silently no-ops on every other city,
// and keying on "-wisp-" alone would cut mid-id ("polecat-az" + leaf).
func wispIDStart(s string) int {
	i := strings.Index(s, "-wisp-")
	if i < 0 {
		return -1
	}
	// i sits on the "-" closing the beads prefix, so the id starts just after
	// the preceding "-" — or at 0 when the wisp id has no role prefix, which
	// LastIndex's -1 gives for free.
	return strings.LastIndex(s[:i], "-") + 1
}

// lastSegment returns the trailing identity segment after the final "/" or ".",
// reducing a possibly-qualified alias ("webapp/gastown.furiosa") to its bare
// instance name ("furiosa").
func lastSegment(s string) string {
	if i := strings.LastIndexAny(s, "/."); i >= 0 {
		return s[i+1:]
	}
	return s
}

// effectiveWorkDir resolves the directory the agent should launch in. Start
// calls it AFTER staging and pre_start have run, so a configured cfg.WorkDir
// must exist on disk by now — pre_start is what creates per-bead worktrees
// (pack worktree-setup scripts) — and a missing directory means preparation
// failed or the config points somewhere wrong. That is a loud error: herdr
// itself falls back to $HOME when --cwd points at a nonexistent path, where
// Claude Code never persists trust acceptance (it re-prompts "trust this
// folder?" every launch) and the altered boot shell state swallows the startup
// nudge; and this provider's previous behavior — silently substituting the
// city root — masked the missing worktree entirely, leaving agents running in
// the wrong repo. (That substitution predates herdr executing pre_start, when
// a pool wisp's WorkDir could not exist yet at launch; with pre_start wired,
// set-but-absent means preparation genuinely failed. tmux, for its part, would
// silently land the pane in the server's cwd — verified: `new-session -c
// /nonexistent` exits 0 with the pane in $HOME — so failing loudly here is
// deliberate hardening over tmux, not parity with it.)
//
// An EMPTY cfg.WorkDir keeps the legitimate fallback chain: a non-empty
// GC_CITY_ROOT env (legacy/explicit override); else the provider's cityRoot (a
// stable project dir where trust is saved once, rather than herdr's server
// cwd — which is $HOME whenever the daemon was launched from a login shell).
// An empty cityRoot (city-less construction) returns "" and defers to the
// server cwd (itself pinned to the city root in startServer).
func effectiveWorkDir(cfg runtime.Config, cityRoot string) (string, error) {
	if cfg.WorkDir != "" {
		if err := runtime.ValidateLocalWorkDir(cfg.WorkDir); err != nil {
			return "", fmt.Errorf("workdir %q unavailable after staging/pre_start (refusing fallback launch dir): %w", cfg.WorkDir, err)
		}
		return cfg.WorkDir, nil
	}
	if root := cfg.Env["GC_CITY_ROOT"]; root != "" {
		return root, nil
	}
	return cityRoot, nil
}

// translateKey maps tmux-style key names (SendKeys uses "Enter"/"C-c"/"Down")
// to herdr key-combo strings ("enter"/"ctrl+c"/"down").
func translateKey(k string) string {
	switch k {
	case "Enter":
		return "enter"
	case "Escape", "Esc":
		return "esc"
	case "Tab":
		return "tab"
	case "Up":
		return "up"
	case "Down":
		return "down"
	case "Left":
		return "left"
	case "Right":
		return "right"
	case "Space":
		return "space"
	case "BSpace":
		return "backspace"
	}
	if len(k) > 2 && k[1] == '-' { // C-x / M-x / S-x
		switch k[0] {
		case 'C':
			return "ctrl+" + strings.ToLower(k[2:])
		case 'M':
			return "alt+" + strings.ToLower(k[2:])
		case 'S':
			return "shift+" + strings.ToLower(k[2:])
		}
	}
	return k
}

// sanitize makes a string safe as a single path segment.
func sanitize(s string) string {
	return strings.NewReplacer("/", "_", " ", "_", ":", "_", "..", "_").Replace(s)
}
