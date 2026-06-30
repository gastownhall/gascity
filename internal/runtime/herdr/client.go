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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// client runs `herdr` CLI verbs against a named herdr session and decodes the
// response envelope ({"id":…,"result":…} | {"id":…,"error":{code,message}}).
type client struct {
	session string // herdr named session (shared per city)
	bin     string // herdr binary (default "herdr")
}

func newClient(session string) *client {
	return &client{session: session, bin: "herdr"}
}

type herdrError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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
		return nil, fmt.Errorf("herdr %v: %s: %s", args, env.Error.Code, env.Error.Message)
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

// startAgent → `herdr agent start <name> --no-focus [--tab <tabID>] [--cwd <cwd>]
// [--env k=v …] -- <argv…>`. A non-empty tabID places the agent in that tab;
// without it herdr splits the focused tab into a new pane.
func (c *client) startAgent(ctx context.Context, name, tabID, cwd string, env map[string]string, argv []string) (agentInfo, error) {
	args := []string{"agent", "start", name, "--no-focus"}
	if tabID != "" {
		args = append(args, "--tab", tabID)
	}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	for k, v := range env {
		args = append(args, "--env", k+"="+v)
	}
	args = append(args, "--")
	args = append(args, argv...)
	res, err := c.run(ctx, args...)
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

// read → `herdr agent read <name> --source <source> [--lines n]`. Use
// "visible" for the current screen (the liveness/fingerprint snapshot);
// "recent"/"recent-unwrapped" are scrollback only.
func (c *client) read(ctx context.Context, name, source string, lines int) (string, error) {
	args := []string{"agent", "read", name, "--source", source}
	if lines > 0 {
		args = append(args, "--lines", strconv.Itoa(lines))
	}
	res, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	var wrap struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return "", fmt.Errorf("herdr agent read: decode: %w", err)
	}
	return wrap.Read.Text, nil
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

// send → `herdr agent send <name> <text>` (literal text, no Enter).
func (c *client) send(ctx context.Context, name, text string) error {
	_, err := c.run(ctx, "agent", "send", name, text)
	return err
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

// deliverNudge types a nudge into the agent's input and submits it. The text is
// injected with `pane run` (paste semantics: multi-line content is preserved and
// the paste's own trailing newline is swallowed by the TUI, so the text never
// submits on its own). Submission is a separate `agent send` of a raw CR, with
// two timing requirements learned empirically against herdr 0.7.1 + the Claude
// Code TUI:
//
//   - The TUI must be at a ready input prompt: a CR delivered mid-boot is
//     swallowed. Callers deliver to a ready agent — Start waits for idle first
//     (see startupNudgeIdleTimeout); the Nudge path targets running agents.
//   - The CR must not race the paste: a CR sent immediately after `pane run` —
//     even on a fully idle agent — lands before the paste commits and is
//     swallowed, stranding the prompt typed-but-unsubmitted. So we settle before
//     submitting, then send a second CR after another settle as insurance (a
//     redundant CR on an already-submitted, empty prompt is a harmless no-op),
//     covering a slow paste-commit under restart-time load.
//
// (`pane run ""` as a submit is a no-op that never submits, so it is not used.)
// Contract: inject by pane id, submit by agent name.
func (c *client) deliverNudge(ctx context.Context, paneID, name, text string) error {
	if err := c.paneRun(ctx, paneID, text); err != nil {
		return err
	}
	time.Sleep(submitSettleDelay)
	if err := c.send(ctx, name, "\r"); err != nil {
		return err
	}
	time.Sleep(submitSettleDelay)
	return c.send(ctx, name, "\r")
}

// submitSettleDelay is how long deliverNudge waits for a `pane run` paste to
// commit in the TUI before sending the submit CR (and again before the insurance
// CR). A CR that races the paste is swallowed; ~1s clears it with margin even
// under the concurrent boot load of a town-wide restart.
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
// workspace per rig/town and one tab per agent. `workspace create` and `tab
// create` each auto-spawn a stray shell pane; the caller closes it so the tab
// holds only the agent.

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

// workspaceCreate makes a workspace labeled label and returns its id plus the
// default tab and stray shell pane herdr auto-spawns inside it (the caller
// repurposes the tab and closes the stray pane).
func (c *client) workspaceCreate(ctx context.Context, label string) (wsID, tabID, strayPane string, err error) {
	res, err := c.run(ctx, "workspace", "create", "--label", label, "--no-focus")
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

// findTab returns the id of the tab in wsID whose label matches, or "".
func (c *client) findTab(ctx context.Context, wsID, label string) (string, error) {
	res, err := c.run(ctx, "tab", "list", "--workspace", wsID)
	if err != nil {
		return "", err
	}
	var wrap struct {
		Tabs []tabInfo `json:"tabs"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return "", fmt.Errorf("herdr tab list: decode: %w", err)
	}
	for _, t := range wrap.Tabs {
		if t.Label == label {
			return t.TabID, nil
		}
	}
	return "", nil
}

// tabCreate makes a tab labeled label in wsID and returns its id plus the stray
// shell pane herdr auto-spawns (the caller closes it after the agent starts).
func (c *client) tabCreate(ctx context.Context, wsID, label string) (tabID, strayPane string, err error) {
	res, err := c.run(ctx, "tab", "create", "--workspace", wsID, "--label", label, "--no-focus")
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

// ensurePlacement resolves where an agent's pane should live: it finds or creates
// the per-rig/town workspace wsLabel, then finds or creates the per-agent tab
// tabLabel inside it. It returns the tab id and, when herdr auto-spawned a stray
// shell pane (new workspace or new tab), that pane's id so Start can close it —
// leaving the tab holding only the agent. A reused existing tab returns "".
func (c *client) ensurePlacement(ctx context.Context, wsLabel, tabLabel string) (tabID, strayPane string, err error) {
	wsID, err := c.findWorkspace(ctx, wsLabel)
	if err != nil {
		return "", "", err
	}
	if wsID == "" {
		// New workspace: repurpose the default tab herdr spawns for this agent.
		_, tabID, strayPane, err = c.workspaceCreate(ctx, wsLabel)
		if err != nil {
			return "", "", err
		}
		_ = c.tabRename(ctx, tabID, tabLabel) // cosmetic; ignore failure
		return tabID, strayPane, nil
	}
	if tabID, err = c.findTab(ctx, wsID, tabLabel); err != nil {
		return "", "", err
	}
	if tabID != "" {
		return tabID, "", nil // reuse existing tab; no stray pane to close
	}
	return c.tabCreate(ctx, wsID, tabLabel)
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

// serverRunning reports whether the session-server socket is present.
func (c *client) serverRunning() bool {
	fi, err := os.Stat(c.socketPath())
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// startServer launches the headless herdr server for this session (detached)
// and waits for its socket. Idempotent — no-op if already running.
func (c *client) startServer() error {
	if c.serverRunning() {
		return nil
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("herdr server: open devnull: %w", err)
	}
	defer func() { _ = devnull.Close() }()
	cmd := exec.Command(c.bin, "--session", c.session, "server")
	cmd.Stdout, cmd.Stderr = devnull, devnull
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("herdr server start: %w", err)
	}
	_ = cmd.Process.Release() // detach; herdr owns the daemon lifetime
	for i := 0; i < 40; i++ {
		if c.serverRunning() {
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
