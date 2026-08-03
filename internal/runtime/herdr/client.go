// Package herdr implements a gascity runtime.Provider backed by herdr
// (https://herdr.dev) — a terminal workspace manager for AI coding agents.
//
// It shells out to the `herdr` CLI (which wraps herdr's local JSON socket API),
// mirroring the tmux provider's executor pattern, and parses the JSON envelope
// each verb emits. herdr is opt-in via the "herdr" runtime selector; tmux stays
// the default. See herdr-provider-design.md for the full interface mapping and
// the 0.7.1 validation notes.
//
// Model: one shared herdr *session* per city (≈ the tmux `-L gc` server). Within
// that session agents are grouped one *workspace* per rig (or per town) and one
// *tab* per agent, so each gascity session is its own switchable space rather
// than a tiled pane. Agents are addressable by name, 1:1 with gascity session
// names.
package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// client runs `herdr` CLI verbs against a named herdr session and decodes the
// response envelope ({"id":…,"result":…} | {"id":…,"error":{code,message}}).
type client struct {
	session  string     // herdr named session (shared per city)
	bin      string     // herdr binary (default "herdr")
	cityRoot string     // city root: the shared server's launch cwd, and the effectiveWorkDir fallback when a session's WorkDir doesn't exist yet (empty in city-less/standalone construction)
	serverMu sync.Mutex // serializes startServer: serverAlive → removeStaleSocket → launch → readiness
}

func newClient(session, cityRoot string) *client {
	return &client{session: session, bin: "herdr", cityRoot: cityRoot}
}

type herdrError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error renders the herdr-reported failure as "<code>: <message>", matching the
// text run() previously formatted inline; wrapping it with %w additionally lets
// callers recover the typed error (and its Code) via errors.As.
func (e *herdrError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// herdrErrorCode returns the herdr-reported error code wrapped anywhere in err
// (via *herdrError), or "" if err carries no herdr error. Callers branch on
// specific herdr failures (e.g. "agent_name_taken") without matching message text.
func herdrErrorCode(err error) string {
	var he *herdrError
	if errors.As(err, &he) {
		return he.Code
	}
	return ""
}

type envelope struct {
	Result json.RawMessage `json:"result"`
	Error  *herdrError     `json:"error"`
}

// run executes `herdr --session <session> <args…>` and returns the result
// payload, or an error (transport failure or herdr-reported error).
func (c *client) run(ctx context.Context, args ...string) (json.RawMessage, error) {
	full := append([]string{"--session", c.session}, args...)
	out, err := exec.CommandContext(ctx, c.bin, full...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("herdr %v: %s", args, ee.Stderr)
		}
		return nil, fmt.Errorf("herdr %v: %w", args, err)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil // success with no payload (e.g. pane send-keys / pane run)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("herdr %v: decode response: %w", args, err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("herdr %v: %w", args, env.Error)
	}
	return env.Result, nil
}

// agentInfo mirrors herdr's agent object.
type agentInfo struct {
	Name        string `json:"name"`
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	TerminalID  string `json:"terminal_id"`
	AgentStatus string `json:"agent_status"`
	Cwd         string `json:"cwd"`
}

// agentStartTimeoutMS bounds herdr's own wait for the launched agent TUI to
// be detected and interactive-ready (`agent start --timeout`). herdr requires
// >3000 and defaults to 30000; sized up to cover cold, concurrent claude
// boots during a town-wide restart.
const agentStartTimeoutMS = 60000

// startAgentKind → `herdr agent start <name> --kind <kind> --pane <paneID>
// --timeout <ms> [-- <args…>]` (herdr ≥0.7.5). herdr launches the kind's
// canonical executable with args inside the existing shell pane and blocks
// until the agent TUI is detected and interactive-ready — its native
// claude-detection, which replaces the pre-0.7.5 exec-argv launch (whose
// shell→TUI occupant handoff is what cleared agent names mid-boot). cwd and
// env are properties of the pane (set at tab/workspace creation), not of the
// agent start.
func (c *client) startAgentKind(ctx context.Context, name, kind, paneID string, args []string) (agentInfo, error) {
	cli := []string{"agent", "start", name, "--kind", kind, "--pane", paneID, "--timeout", strconv.Itoa(agentStartTimeoutMS)}
	if len(args) > 0 {
		cli = append(cli, "--")
		cli = append(cli, args...)
	}
	res, err := c.run(ctx, cli...)
	if err != nil {
		return agentInfo{}, err
	}
	var wrap struct {
		Agent agentInfo `json:"agent"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return agentInfo{}, fmt.Errorf("herdr agent start: decode: %w", err)
	}
	return wrap.Agent, nil
}

// agentPrompt → `herdr agent prompt <target> <text>` (herdr ≥0.7.5): types
// text into a registered agent's input and submits it through herdr's own
// prompt machinery — the reliable replacement for the paste+Enter+confirm
// dance. target is an agent name or the pane id hosting it.
func (c *client) agentPrompt(ctx context.Context, target, text string) error {
	_, err := c.run(ctx, "agent", "prompt", target, text)
	return err
}

// listAgents → `herdr agent list`.
func (c *client) listAgents(ctx context.Context) ([]agentInfo, error) {
	res, err := c.run(ctx, "agent", "list")
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Agents []agentInfo `json:"agents"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return nil, fmt.Errorf("herdr agent list: decode: %w", err)
	}
	return wrap.Agents, nil
}

// paneRead → `herdr pane read <paneID> --source <source> [--lines n]`
// (herdr ≥0.7.5). Reads any pane's screen without needing a registered agent
// (raw shell sessions never register one). Use "visible" for the current
// screen (the liveness/fingerprint snapshot). On 0.7.5 the CLI prints the
// text raw rather than in the JSON envelope, so this parses failures out of
// an envelope only when one is present.
func (c *client) paneRead(ctx context.Context, paneID, source string, lines int) (string, error) {
	args := []string{"pane", "read", paneID, "--source", source}
	if lines > 0 {
		args = append(args, "--lines", strconv.Itoa(lines))
	}
	out, err := c.runRaw(ctx, args...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// runRaw executes a herdr verb whose success output is plain text, not the
// JSON envelope (0.7.5 `pane read`). Failures still arrive as an envelope on
// stdout or as stderr text, so an output that decodes to an envelope carrying
// an error is surfaced as that error; anything else is returned verbatim.
func (c *client) runRaw(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--session", c.session}, args...)
	out, err := exec.CommandContext(ctx, c.bin, full...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("herdr %v: %s", args, ee.Stderr)
		}
		return "", fmt.Errorf("herdr %v: %w", args, err)
	}
	trimmed := strings.TrimSpace(string(out))
	if strings.HasPrefix(trimmed, "{") {
		var env envelope
		if jerr := json.Unmarshal([]byte(trimmed), &env); jerr == nil && env.Error != nil {
			return "", fmt.Errorf("herdr %v: %w", args, env.Error)
		}
	}
	return string(out), nil
}

// proc is one process in a pane's foreground tree.
type proc struct {
	PID  int      `json:"pid"`
	Name string   `json:"name"`
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
}

// processInfo → `herdr pane process-info --pane <paneID>`: shell PID + the
// foreground process tree (powers ProcessAlive and the hard-kill path).
func (c *client) processInfo(ctx context.Context, paneID string) (shellPID int, fg []proc, err error) {
	res, e := c.run(ctx, "pane", "process-info", "--pane", paneID)
	if e != nil {
		return 0, nil, e
	}
	var wrap struct {
		ProcessInfo struct {
			ShellPID            int    `json:"shell_pid"`
			ForegroundProcesses []proc `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return 0, nil, fmt.Errorf("herdr pane process-info: decode: %w", err)
	}
	return wrap.ProcessInfo.ShellPID, wrap.ProcessInfo.ForegroundProcesses, nil
}

// sendKeys → `herdr pane send-keys <paneID> <key…>` (raw keys, e.g. ctrl+c, enter).
func (c *client) sendKeys(ctx context.Context, paneID string, keys ...string) error {
	args := append([]string{"pane", "send-keys", paneID}, keys...)
	_, err := c.run(ctx, args...)
	return err
}

// paneRun → `herdr pane run <paneID> <command>` (pastes text into the pane).
func (c *client) paneRun(ctx context.Context, paneID, command string) error {
	_, err := c.run(ctx, "pane", "run", paneID, command)
	return err
}

// deliverNudge types a nudge into the session and submits it. Registered
// agents (the kind-launch path) go through herdr ≥0.7.5's native
// `agent prompt`, which owns the type+submit handshake that the pre-0.7.5
// paste+Enter+confirm dance approximated — targeting the pane id, which agent
// verbs accept even after the registry name is unavailable to the caller.
// Panes with no registered agent (raw `exec /bin/sh -c` sessions, bare
// shells) fall back to paste + Enter: there is no TUI prompt machinery to
// confirm against, so delivery is best-effort by construction.
func (c *client) deliverNudge(ctx context.Context, paneID, text string) error {
	err := c.agentPrompt(ctx, paneID, text)
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "not_found") && !strings.Contains(err.Error(), "not found") {
		return err
	}
	// No registered agent on this pane: paste, settle, submit.
	if err := c.paneRun(ctx, paneID, text); err != nil {
		return err
	}
	time.Sleep(submitSettleDelay)
	return c.sendKeys(ctx, paneID, "Enter")
}

// submitSettleDelay is how long the unregistered-pane fallback waits for a
// `pane run` paste to commit before the submit Enter (a submit racing the
// paste is swallowed).
const submitSettleDelay = 1 * time.Second

// closePane → `herdr pane close <paneID>`.
func (c *client) closePane(ctx context.Context, paneID string) error {
	_, err := c.run(ctx, "pane", "close", paneID)
	return err
}

// getAgent fetches one agent by name: (info, true, nil) if present,
// (zero, false, nil) if herdr reports it absent, (_, false, err) on failure.
func (c *client) getAgent(ctx context.Context, name string) (agentInfo, bool, error) {
	res, err := c.run(ctx, "agent", "get", name)
	if err != nil {
		if strings.Contains(err.Error(), "not_found") || strings.Contains(err.Error(), "not found") {
			return agentInfo{}, false, nil
		}
		return agentInfo{}, false, err
	}
	var wrap struct {
		Agent agentInfo `json:"agent"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return agentInfo{}, false, fmt.Errorf("herdr agent get: decode: %w", err)
	}
	return wrap.Agent, true, nil
}

// ── workspace / tab placement ────────────────────────────────────────────────
//
// herdr's tree is workspace › tab › pane. To give each agent its own switchable
// space (vs tiling every agent as a pane in one tab), Start groups agents one
// workspace per rig/town and one tab per agent. Under herdr ≥0.7.5 the shell
// pane that `workspace create`/`tab create` auto-spawns IS the agent's pane —
// agents launch into an existing shell pane, and cwd/env are set here at pane
// creation (there is no longer a stray pane to close, which is what leaked one
// shell per wrongful Start in the spawn storm).

type workspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
}

type tabInfo struct {
	TabID string `json:"tab_id"`
	Label string `json:"label"`
}

// findWorkspace returns the id of the workspace whose label matches, or "".
func (c *client) findWorkspace(ctx context.Context, label string) (string, error) {
	res, err := c.run(ctx, "workspace", "list")
	if err != nil {
		return "", err
	}
	var wrap struct {
		Workspaces []workspaceInfo `json:"workspaces"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return "", fmt.Errorf("herdr workspace list: decode: %w", err)
	}
	for _, w := range wrap.Workspaces {
		if w.Label == label {
			return w.WorkspaceID, nil
		}
	}
	return "", nil
}

// workspaceCreate makes a workspace labeled label whose root shell pane is
// created with the given cwd and env, and returns the workspace id plus the
// default tab and root pane (the agent's pane) herdr auto-spawns inside it.
func (c *client) workspaceCreate(ctx context.Context, label, cwd string, env map[string]string) (wsID, tabID, paneID string, err error) {
	args := []string{"workspace", "create", "--label", label, "--no-focus"}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	for k, v := range env {
		args = append(args, "--env", k+"="+v)
	}
	res, err := c.run(ctx, args...)
	if err != nil {
		return "", "", "", err
	}
	var wrap struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		Tab struct {
			TabID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return "", "", "", fmt.Errorf("herdr workspace create: decode: %w", err)
	}
	return wrap.Workspace.WorkspaceID, wrap.Tab.TabID, wrap.RootPane.PaneID, nil
}

// listTabs returns the tabs in wsID.
func (c *client) listTabs(ctx context.Context, wsID string) ([]tabInfo, error) {
	res, err := c.run(ctx, "tab", "list", "--workspace", wsID)
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Tabs []tabInfo `json:"tabs"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return nil, fmt.Errorf("herdr tab list: decode: %w", err)
	}
	return wrap.Tabs, nil
}

// tabCreate makes a tab labeled label in wsID whose root shell pane is created
// with the given cwd and env, and returns the tab id plus that root pane (the
// agent's pane).
func (c *client) tabCreate(ctx context.Context, wsID, label, cwd string, env map[string]string) (tabID, paneID string, err error) {
	args := []string{"tab", "create", "--workspace", wsID, "--label", label}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	for k, v := range env {
		args = append(args, "--env", k+"="+v)
	}
	res, err := c.run(ctx, args...)
	if err != nil {
		return "", "", err
	}
	var wrap struct {
		Tab struct {
			TabID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return "", "", fmt.Errorf("herdr tab create: decode: %w", err)
	}
	return wrap.Tab.TabID, wrap.RootPane.PaneID, nil
}

// tabRename relabels a tab (cosmetic; best-effort at the call site).
func (c *client) tabRename(ctx context.Context, tabID, label string) error {
	_, err := c.run(ctx, "tab", "rename", tabID, label)
	return err
}

// tabClose closes a tab and its panes (used to recycle a stale tab left by a
// previous life of the same session before creating its replacement).
func (c *client) tabClose(ctx context.Context, tabID string) error {
	_, err := c.run(ctx, "tab", "close", tabID)
	return err
}

// ensurePlacement resolves where an agent should live and returns its tab id
// plus the fresh shell pane the agent will launch into: it finds or creates
// the per-rig/town workspace wsLabel, then creates the per-agent tab tabLabel
// inside it with the agent's cwd and env baked into the pane. A stale tab
// with the same label (left by a previous life of this session — e.g. an
// exited agent whose pane sits at a shell prompt) is closed first, so every
// Start gets a clean shell with the right cwd/env and dead panes never
// accumulate across restarts.
func (c *client) ensurePlacement(ctx context.Context, wsLabel, tabLabel, cwd string, env map[string]string) (tabID, paneID string, err error) {
	wsID, err := c.findWorkspace(ctx, wsLabel)
	if err != nil {
		return "", "", err
	}
	if wsID == "" {
		// New workspace: repurpose the default tab herdr spawns for this agent.
		_, tabID, paneID, err = c.workspaceCreate(ctx, wsLabel, cwd, env)
		if err != nil {
			return "", "", err
		}
		_ = c.tabRename(ctx, tabID, tabLabel) // cosmetic; ignore failure
		return tabID, paneID, nil
	}
	tabs, err := c.listTabs(ctx, wsID)
	if err != nil {
		return "", "", err
	}
	for _, tb := range tabs {
		if tb.Label == tabLabel {
			_ = c.tabClose(ctx, tb.TabID) // best-effort: replaced below either way
		}
	}
	return c.tabCreate(ctx, wsID, tabLabel, cwd, env)
}

// ── shared session-server lifecycle ──────────────────────────────────────────

// socketPath is the unix socket for this client's herdr session.
func (c *client) socketPath() string {
	home, _ := os.UserHomeDir()
	if c.session == "" || c.session == "default" {
		return filepath.Join(home, ".config", "herdr", "herdr.sock")
	}
	return filepath.Join(home, ".config", "herdr", "sessions", c.session, "herdr.sock")
}

// serverAlive reports whether the session-server is actually accepting
// connections on its socket. A bare os.Stat is insufficient: a herdr server
// that exits uncleanly leaves its socket inode behind — and herdr's own
// `session stop` can't remove it, since that too needs a live server to reach —
// so the stale socket answers connects with ECONNREFUSED. Presence != liveness;
// dial to find out for real.
func (c *client) serverAlive() bool {
	conn, err := net.DialTimeout("unix", c.socketPath(), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// removeStaleSocket unlinks the socket inode when it exists but nothing live is
// listening, so a freshly launched server can bind. Guard with serverAlive
// first — only call once liveness has already returned false.
func (c *client) removeStaleSocket() {
	if fi, err := os.Stat(c.socketPath()); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(c.socketPath())
	}
}

// startServer launches the headless herdr server for this session (detached)
// and waits for it to accept connections. Idempotent — no-op if already live.
func (c *client) startServer() error {
	c.serverMu.Lock()
	defer c.serverMu.Unlock()
	if c.serverAlive() {
		return nil
	}
	// A prior server may have died leaving a stale socket inode; serverAlive
	// just confirmed nothing live owns it, so clear it before launch or herdr
	// cannot bind — the exact failure that stranded provider swaps (agent list
	// → ECONNREFUSED → swap aborted → pool polecats stuck start-pending).
	c.removeStaleSocket()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("herdr server: open devnull: %w", err)
	}
	defer func() { _ = devnull.Close() }()
	cmd := exec.Command(c.bin, "--session", c.session, "server")
	cmd.Stdout, cmd.Stderr = devnull, devnull
	// Launch the shared daemon in the city root, not the inherited cwd (which is
	// often $HOME when gc is invoked from a login shell). Sessions whose --cwd is
	// empty/nonexistent fall back to this server cwd, so a $HOME-rooted server
	// stranded ephemeral pool spawns in $HOME (unprimed, re-prompted for trust).
	// Empty cityRoot (city-less construction) leaves cwd inherited, as before.
	cmd.Dir = c.cityRoot
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("herdr server start: %w", err)
	}
	_ = cmd.Process.Release() // detach; herdr owns the daemon lifetime
	for i := 0; i < 40; i++ {
		if c.serverAlive() {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("herdr server for session %q did not become ready", c.session)
}

// stopServer stops this session's server (best-effort; tolerates not-running).
// `session stop` targets the session by name and must bypass run() (which
// prepends --session).
func (c *client) stopServer() error {
	_ = exec.Command(c.bin, "session", "stop", c.session).Run()
	return nil
}
