package herdr

import (
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

	"github.com/gastownhall/gascity/internal/runtime"
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
	mu           sync.Mutex    // serializes workspace/tab find-or-create across concurrent Starts
}

var (
	_ runtime.Provider                = (*Provider)(nil)
	_ runtime.ServerLifecycleProvider = (*Provider)(nil)
)

// defaultSetupTimeout is the per-command timeout for pre_start/session_setup
// commands when the caller passes 0 (tests, city-less construction). Mirrors
// the [session] setup_timeout config default and tmux's DefaultConfig().
const defaultSetupTimeout = 10 * time.Second

// New builds a herdr Provider. herdrSession is the shared per-city herdr session
// name; metaDir is a writable directory for sidecar session metadata (a temp
// fallback is used when empty, e.g. a city-less standalone construction); cityRoot
// is the city directory used as the shared server's launch cwd and as the
// effectiveWorkDir fallback for sessions with no WorkDir configured (empty in
// city-less construction); setupTimeout is the per-command timeout for
// pre_start/session_setup commands ([session] setup_timeout; <=0 uses the 10s
// default).
func New(herdrSession, metaDir, cityRoot string, setupTimeout time.Duration) *Provider {
	if metaDir == "" {
		metaDir = filepath.Join(os.TempDir(), "gc-herdr-meta", sanitize(herdrSession))
	}
	if setupTimeout <= 0 {
		setupTimeout = defaultSetupTimeout
	}
	return &Provider{c: newClient(herdrSession, cityRoot), metaDir: metaDir, setupTimeout: setupTimeout}
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
	// Prepare the working directory BEFORE anything launches, mirroring the
	// other host-side providers: stage overlays/CopyFiles (tmux stageStartFiles,
	// subprocess/acp StageSessionWorkDir), then run pre_start host-side (tmux
	// doStartSession Step 0, same staging-then-pre_start order). pre_start
	// failures are fatal: those commands do directory/worktree preparation
	// (e.g. pack scripts running `git worktree add` for a per-bead worktree),
	// and launching without them points the agent at the wrong repo. Before
	// this, herdr never ran pre_start — per-bead worktrees were never
	// materialized and effectiveWorkDir silently dropped agents in the city root.
	if err := runtime.StageSessionWorkDir(cfg); err != nil {
		return fmt.Errorf("herdr: staging workdir for %q: %w", name, err)
	}
	if err := p.runPreStart(ctx, cfg); err != nil {
		return fmt.Errorf("herdr: running pre_start: %w", err)
	}
	workDir, err := effectiveWorkDir(cfg, p.c.cityRoot)
	if err != nil {
		return fmt.Errorf("herdr: start %q: %w", name, err)
	}
	// Place the agent in its own tab under a per-rig (per-town) workspace, so
	// agents are separate switchable spaces rather than tiled panes. The
	// find-or-create is serialized so concurrent same-rig Starts share one
	// workspace instead of racing to create duplicates.
	wsLabel, tabLabel := placementFor(name, cfg.Env)
	p.mu.Lock()
	tabID, strayPane, err := p.c.ensurePlacement(ctx, wsLabel, tabLabel)
	p.mu.Unlock()
	if err != nil {
		return fmt.Errorf("herdr: place %q: %w", name, err)
	}
	info, err := p.c.startAgent(ctx, name, tabID, workDir, cfg.Env, shellArgv(cfg.Command))
	if err != nil {
		return fmt.Errorf("herdr: start %q: %w", name, err)
	}
	// herdr auto-spawns a stray shell pane when it creates a workspace/tab; close
	// it so the tab holds only the agent.
	if strayPane != "" && strayPane != info.PaneID {
		_ = p.c.closePane(ctx, strayPane)
	}
	// Post-launch steps mirror tmux's ordering: wait for readiness, run
	// session_setup (Step 5.5), then deliver the startup nudge (Step 6).
	//
	// The first turn has two mutually-exclusive sources: a pool/sling slot
	// carries its claim instruction in cfg.Nudge; a named always-awake Claude
	// session carries its behavioral prime in cfg.PromptSuffix (PromptMode=arg).
	// herdr launches via exec argv and — unlike tmux/acp/t3bridge — has no
	// shell-arg slot to ride PromptSuffix onto, so without this it would drop
	// the prime, boot a bare `claude` REPL, and (because the resolver already
	// set startupPromptDeliveredEnv, suppressing the SessionStart hook's copy of
	// the prime) leave the agent wholly unprimed and idle. Route both through
	// the one hardened post-idle paste+submit path; cfg.Nudge takes precedence
	// so the working pool path is byte-for-byte unchanged. See startupDeliveryText.
	startupText := startupDeliveryText(cfg)
	if info.PaneID != "" && (startupText != "" || hasSessionSetup(cfg)) {
		// A freshly-spawned agent boots through a shell→TUI handoff before its
		// input prompt is listening; a paste or submit delivered in that window is
		// silently swallowed, leaving the agent idle forever instead of running its
		// first turn. Wait for herdr to report the agent idle (its prompt rendered)
		// before delivering, mirroring how tmux's doStartSession waits for readiness
		// before its Step-5.5 session_setup and Step-6 startup nudge. Idle is
		// necessary but not sufficient — input-readiness lags it — so deliverNudge
		// additionally verifies the paste visibly lands (re-pasting until it does)
		// rather than trusting `pane run`, which reports success even on a swallowed
		// paste.
		// Bounded and best-effort: on a boot that never idles we deliver anyway (no
		// worse than the prior unconditional send), and the reconciler tolerates a
		// slow Start (pendingCreateNeverStartedTimeout = 10m).
		_ = p.WaitForIdle(ctx, name, startupNudgeIdleTimeout)
	}
	// session_setup runs host-side ("in gc's process via sh -c", per the Config
	// contract), so herdr can honor it the same way tmux does. Non-fatal.
	p.runSessionSetup(ctx, name, cfg, os.Stderr)
	if startupText != "" && info.PaneID != "" {
		if err := p.c.deliverNudge(ctx, info.PaneID, name, startupText); err != nil {
			// Best-effort: the submit didn't confirm (TUI race under boot load).
			// Surface it rather than silently leaving a stranded startup turn; the
			// warm-bind claim nudge (startPreparedStartCandidate's warm-reuse branch)
			// re-delivers on the next reconcile tick — by then the slot is running with
			// its trigger still unclaimed, which is precisely that hook's condition.
			fmt.Fprintf(os.Stderr, "herdr: startup delivery for %q not confirmed: %v\n", name, err) //nolint:errcheck // best-effort diagnostic
		}
	}
	return nil
}

// runPreStart runs cfg.PreStart commands host-side before the agent is
// spawned, mirroring the tmux adapter's runPreStart: same command source and
// ordering, same fatal error semantics (an unprepared workDir must abort the
// launch, per the Config.PreStart contract), same per-command timeout and
// GC_DIR/env handling via the shared runtime.RunSetupCommand.
func (p *Provider) runPreStart(ctx context.Context, cfg runtime.Config) error {
	if len(cfg.PreStart) == 0 {
		return nil
	}
	setupEnv := make(map[string]string, len(cfg.Env))
	for k, v := range cfg.Env {
		setupEnv[k] = v
	}
	for i, cmd := range cfg.PreStart {
		if err := runtime.RunSetupCommand(ctx, cmd, setupEnv, p.setupTimeout); err != nil {
			return fmt.Errorf("pre_start[%d]: %w", i, err)
		}
	}
	return nil
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
		if err := runtime.RunSetupCommand(ctx, cmd, setupEnv, p.setupTimeout); err != nil {
			fmt.Fprintf(stderr, "gc: session_setup[%d] warning: %v\n", i, err) //nolint:errcheck // best-effort warning
		}
	}
	if cfg.SessionSetupScript != "" {
		if err := runtime.RunSetupCommand(ctx, cfg.SessionSetupScript, setupEnv, p.setupTimeout); err != nil {
			fmt.Fprintf(stderr, "gc: session_setup_script warning: %v\n", err) //nolint:errcheck // best-effort warning
		}
	}
}

// hasSessionSetup reports whether cfg carries any session_setup work.
func hasSessionSetup(cfg runtime.Config) bool {
	return len(cfg.SessionSetup) > 0 || cfg.SessionSetupScript != ""
}

// startupDeliveryText resolves the first-turn text Start delivers to a freshly
// spawned agent. A pool/sling slot carries its claim instruction in cfg.Nudge and
// is delivered unchanged (it takes precedence, so the working pool path is
// untouched). A named always-awake Claude session instead carries its behavioral
// prime in cfg.PromptSuffix (PromptMode=arg, shell-quoted for argv use that herdr's
// exec launch has no slot for); unquote it — mirroring the parts[0] round-trip used
// on the resume path in session_lifecycle_parallel.go — and deliver it through the
// same post-idle paste+submit path. Returns "" when there is nothing to deliver
// (deterministic workers, suppressed startup prompt). Falls back to the raw string
// if PromptSuffix somehow fails to unquote: delivering something beats stranding the
// agent idle.
func startupDeliveryText(cfg runtime.Config) string {
	if cfg.Nudge != "" {
		return cfg.Nudge
	}
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
// only bites on a boot that never idles, after which the nudge is sent
// best-effort. Sized generously to cover cold, concurrent boots during a
// town-wide restart.
const startupNudgeIdleTimeout = 60 * time.Second

// Stop closes the agent's pane and clears its metadata sidecar. Idempotent.
func (p *Provider) Stop(name string) error {
	ctx := context.Background()
	pid, err := p.paneID(ctx, name)
	if err != nil || pid == "" {
		return nil // idempotent
	}
	_ = p.c.closePane(ctx, pid)
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

// IsRunning reports whether an agent with this name exists in the session.
func (p *Provider) IsRunning(name string) bool {
	agents, err := p.c.listAgents(context.Background())
	if err != nil {
		return false
	}
	for _, a := range agents {
		if a.Name == name {
			return true
		}
	}
	return false
}

// IsAttached reports false: herdr 0.7.1 exposes no clean attach-state query.
func (p *Provider) IsAttached(_ string) bool { return false }

// Attach runs `herdr agent attach`, blocking until the user detaches.
func (p *Provider) Attach(name string) error {
	cmd := exec.Command(p.c.bin, "--session", p.c.session, "agent", "attach", name)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run() // blocks until the user detaches
}

// ProcessAlive reports whether the agent's pane has a live foreground process,
// optionally requiring one of processNames to be present.
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	ctx := context.Background()
	pid, err := p.paneID(ctx, name)
	if err != nil || pid == "" {
		return false
	}
	shellPID, fg, err := p.c.processInfo(ctx, pid)
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
	return false
}

// Nudge injects and submits text into a running agent's input.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	ctx := context.Background()
	pid, err := p.paneID(ctx, name)
	if err != nil || pid == "" {
		return runtime.ErrSessionNotFound
	}
	return p.c.deliverNudge(ctx, pid, name, runtime.FlattenText(content))
}

// Peek reads the current rendered screen ("visible") — the liveness/fingerprint
// snapshot. recent*/scrollback is empty until lines scroll off.
func (p *Provider) Peek(name string, lines int) (string, error) {
	return p.c.read(context.Background(), name, "visible", lines)
}

// ListRunning returns the names of running agents whose names start with prefix.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	agents, err := p.c.listAgents(context.Background())
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range agents {
		if strings.HasPrefix(a.Name, prefix) {
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
		CanReportActivity:   false, // no GetLastActivity
		CanStream:           true,  // push session-event stream via SubscribeSessionEvents (events.subscribe socket API)
		CanAttachTTY:        true,  // agent attach
	}
}

// ── best-effort / unsupported (the contract permits these) ───────────────────

// GetLastActivity is unsupported (herdr exposes no activity timestamp); it
// returns the zero time.
func (p *Provider) GetLastActivity(_ string) (time.Time, error) { return time.Time{}, nil }

// ClearScrollback is a no-op: herdr exposes no scrollback-clear op.
func (p *Provider) ClearScrollback(_ string) error { return nil }

// RunLive is a no-op: herdr agents are launched at Start.
func (p *Provider) RunLive(_ string, _ runtime.Config) error { return nil }

// CopyTo copies a local path into the agent's working directory (best-effort).
func (p *Provider) CopyTo(name, src, relDst string) error {
	if _, err := os.Stat(src); err != nil {
		return nil // best-effort: missing src
	}
	a, ok, err := p.c.getAgent(context.Background(), name)
	if err != nil || !ok || a.Cwd == "" {
		return nil
	}
	// An empty relDst means "into the workdir under the source's own name".
	// Joining "" targets the directory itself, which copyPath cannot write a
	// file to — preserve the basename, as the other providers do.
	if relDst == "" {
		relDst = filepath.Base(src)
	}
	return copyPath(src, filepath.Join(a.Cwd, relDst))
}

// ── metadata sidecar (herdr has no per-session KV) ───────────────────────────

// SetMeta writes a per-session metadata value to the sidecar store (herdr has
// no per-session KV).
func (p *Provider) SetMeta(name, key, value string) error {
	dir := filepath.Join(p.metaDir, sanitize(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sanitize(key)), []byte(value), 0o644)
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

// paneID resolves a gascity session name to its herdr pane id (or "" if absent).
func (p *Provider) paneID(ctx context.Context, name string) (string, error) {
	a, ok, err := p.c.getAgent(ctx, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	return a.PaneID, nil
}

// shellArgv wraps a shell command string as argv for `herdr agent start -- …`.
func shellArgv(command string) []string {
	if strings.TrimSpace(command) == "" {
		return []string{"/bin/sh"}
	}
	return []string{"/bin/sh", "-c", command}
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
// (e.g. "gastown__polecat-gc-wisp-3nvj3yx"), so workspaceTabFor alone drops them
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
	// "polecat-furiosa" rather than "polecat-gc-wisp-3nvj3yx". The role prefix
	// (everything before the wisp id) is preserved. Falls through unchanged when
	// no alias is available yet, or when the alias is itself the wisp identity.
	if i := strings.Index(tab, "gc-wisp-"); i >= 0 {
		alias := strings.TrimSpace(env["GC_ALIAS"])
		if alias == "" {
			alias = strings.TrimSpace(env["GC_AGENT"])
		}
		if leaf := lastSegment(alias); leaf != "" && !strings.Contains(leaf, "gc-wisp-") {
			tab = tab[:i] + leaf
		}
	}
	return workspace, tab
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
		if _, err := os.Stat(cfg.WorkDir); err != nil {
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

// copyPath copies a file or directory tree from src to dst.
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, info.Mode().Perm())
}
