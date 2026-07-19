package herdr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	setupTimeout time.Duration // per-command timeout for pre_start ([session] setup_timeout)
	mu           sync.Mutex    // serializes workspace/tab find-or-create across concurrent Starts
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
// city-less construction). setupTimeout bounds each pre_start command
// ([session] setup_timeout); non-positive values fall back to
// defaultSetupTimeout.
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

// Start ensures the shared server is up, spawns the agent into its placed
// workspace/tab, and delivers the startup nudge once the agent reaches idle.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.ConfigureServer(); err != nil {
		return fmt.Errorf("herdr: configure server: %w", err)
	}
	if p.IsRunning(name) {
		return runtime.ErrSessionExists
	}
	// Step 0: pre_start — workDir/worktree preparation, and the carrier for
	// stage-2 skill/MCP materialization. Mirrors tmux doStartSession's first
	// step; fatal on failure so an agent never launches into an unprepared
	// workDir. Runs only once we know we're actually creating the agent (the
	// ErrSessionExists check above), so an existing session never re-runs prep.
	if err := p.runPreStart(ctx, cfg); err != nil {
		return fmt.Errorf("herdr: running pre_start: %w", err)
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
	info, err := p.c.startAgent(ctx, name, tabID, effectiveWorkDir(cfg, p.c.cityRoot), cfg.Env, shellArgv(cfg.Command))
	if err != nil {
		return fmt.Errorf("herdr: start %q: %w", name, err)
	}
	// Seed the metadata sidecar from cfg.Env NOW, before the (long) startup
	// delivery below. tmux gets this for free — its GetMeta reads the tmux
	// session environment, which new-session initializes from cfg.Env — but
	// herdr's meta store is a sidecar populated only by SetMeta. The reconciler's
	// pending-create ownership check (runningSessionMatchesPendingCreateInfo)
	// reads GC_SESSION_ID / GC_INSTANCE_TOKEN via GetMeta on ticks that fire
	// while Start is still waiting for the agent to idle; with an unseeded
	// sidecar it misreads the fresh runtime as "live runtime belongs to another
	// session" and reaps it seconds after a successful start.
	//
	// Seeding the whole env also persists GC_SESSION_ID, which ProcessAlive's
	// session-scoped tree-walk widening reads (herdr does not capture the
	// creation environment the way tmux does): process env survives reparenting
	// (only ppid changes), so this is what lets the walk find the agent when it
	// is no longer a descendant of the pane's shell/foreground PIDs. Stop clears
	// the whole meta dir, so teardown is covered.
	if err := p.seedMetaFromEnv(name, cfg.Env); err != nil {
		return fmt.Errorf("herdr: seed session metadata for %q: %w", name, err)
	}
	// herdr auto-spawns a stray shell pane when it creates a workspace/tab; close
	// it so the tab holds only the agent.
	if strayPane != "" && strayPane != info.PaneID {
		_ = p.c.closePane(ctx, strayPane)
	}
	// Deliver the agent's first turn. Two mutually-exclusive sources: a pool/sling
	// slot carries its claim instruction in cfg.Nudge; a named always-awake Claude
	// session carries its behavioral prime in cfg.PromptSuffix (PromptMode=arg).
	// herdr launches via exec argv and — unlike tmux/acp/t3bridge — has no shell-arg
	// slot to ride PromptSuffix onto, so without this it would drop the prime, boot
	// a bare `claude` REPL, and (because the resolver already set
	// startupPromptDeliveredEnv, suppressing the SessionStart hook's copy of the
	// prime) leave the agent wholly unprimed and idle. Route both through the one
	// hardened post-idle paste+submit path; cfg.Nudge takes precedence so the
	// working pool path is byte-for-byte unchanged. See startupDeliveryText.
	if startupText := startupDeliveryText(cfg); startupText != "" && info.PaneID != "" {
		// A freshly-spawned agent boots through a shell→TUI handoff before its
		// input prompt is listening. The paste buffers and survives that window,
		// but the submit CR does not: delivered too early it is swallowed, leaving
		// the text typed-but-unsubmitted in the box — and the agent then idles
		// forever instead of running its first turn. Wait for herdr to report the
		// agent idle (its prompt rendered) before delivering, mirroring how tmux's
		// doStartSession waits for readiness before its Step-6 startup nudge.
		// Bounded and best-effort: on a boot that never idles we deliver anyway (no
		// worse than the prior unconditional send), and the reconciler tolerates a
		// slow Start (pendingCreateNeverStartedTimeout = 10m).
		_ = p.WaitForIdle(ctx, name, startupNudgeIdleTimeout)
		if err := p.c.deliverNudge(ctx, info.PaneID, name, startupText); err != nil {
			// Best-effort: the submit didn't confirm (TUI race under boot load).
			// Surface it rather than silently leaving a stranded startup turn;
			// nudgeStalledPoolClaims is the reconcile-tick backstop of last resort.
			fmt.Fprintf(os.Stderr, "herdr: startup delivery for %q not confirmed: %v\n", name, err) //nolint:errcheck // best-effort diagnostic
		}
	}
	return nil
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

const (
	// preStartOutputLimit bounds the captured output tail attached to a failed
	// pre_start error (mirrors tmux's setupCommandOutputLimit).
	preStartOutputLimit = 4096
	// preStartWaitDelay force-closes the capture pipes shortly after the command
	// exits, so a pre_start that daemonizes a child holding inherited stdio
	// cannot hang the start (mirrors tmux's setupCommandWaitDelay).
	preStartWaitDelay = 2 * time.Second
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
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
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
	c.Stdout, c.Stderr = &out, &out
	c.WaitDelay = preStartWaitDelay
	if err := c.Run(); err != nil {
		// ErrWaitDelay means the command itself exited successfully and only the
		// force-closed pipes ended the wait: a setup command that daemonizes a
		// child holding inherited stdio succeeded (mirrors tmux).
		if errors.Is(err, exec.ErrWaitDelay) {
			return nil
		}
		if tail := strings.TrimSpace(out.String()); tail != "" {
			if len(tail) > preStartOutputLimit {
				tail = tail[len(tail)-preStartOutputLimit:]
			}
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	return nil
}

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
	sessionID, _ := p.GetMeta(name, "GC_SESSION_ID")
	return processTreeAlive(shellPID, fg, processNames, strings.TrimSpace(sessionID))
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
		CanStream:           false, // socket-event streaming is a later optimization
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

// seedMetaFromEnv initializes the session's metadata sidecar from cfg.Env,
// mirroring tmux's contract where the session environment (seeded from cfg.Env
// at creation) doubles as the GetMeta store. Ownership/identity keys like
// GC_SESSION_ID and GC_INSTANCE_TOKEN must be readable via GetMeta from the
// moment the runtime is alive. Later SetMeta calls override individual keys,
// exactly as tmux setenv does.
func (p *Provider) seedMetaFromEnv(name string, env map[string]string) error {
	for k, v := range env {
		if err := p.SetMeta(name, k, v); err != nil {
			return fmt.Errorf("meta %q: %w", k, err)
		}
	}
	return nil
}

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

// effectiveWorkDir picks the directory the agent should launch in. herdr falls
// back to its server cwd when --cwd is empty and to $HOME when --cwd points at a
// path that does not exist, and Claude Code never persists trust acceptance from
// $HOME — so it re-prompts "trust this folder?" on every launch and (worse) an
// ephemeral pool spawn that lands in $HOME boots a different shell state that
// swallows the startup nudge, leaving it idle and unclaimed. Ephemeral pool wisps
// are started before their per-bead worktree is created, so cfg.WorkDir may not
// exist yet at launch; fall back to the city root (a stable project dir where
// trust is saved once) rather than let herdr land the session in $HOME.
//
// Resolution order: an existing cfg.WorkDir; else a non-empty GC_CITY_ROOT env
// (legacy/explicit override); else the provider's cityRoot. The final fallback is
// the fix for the pool-spawn-in-$HOME bug: GC_CITY_ROOT is not actually populated
// in cfg.Env today, so before this the result was "" and herdr used its server
// cwd — which is $HOME whenever the daemon was launched from a login shell. An
// empty cityRoot (city-less construction) returns "" and defers to the server cwd
// (now itself pinned to the city root in startServer).
func effectiveWorkDir(cfg runtime.Config, cityRoot string) string {
	if cfg.WorkDir != "" {
		if _, err := os.Stat(cfg.WorkDir); err == nil {
			return cfg.WorkDir
		}
	}
	if root := cfg.Env["GC_CITY_ROOT"]; root != "" {
		return root
	}
	return cityRoot
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
