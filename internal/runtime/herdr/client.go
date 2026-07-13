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
	session  string // herdr named session (shared per city)
	bin      string // herdr binary (default "herdr")
	cityRoot string // city root: the shared server's launch cwd, and the effectiveWorkDir fallback when a session's WorkDir doesn't exist yet (empty in city-less/standalone construction)
	sockPath string // test override for socketPath (unit tests point it at a fake server)
}

func newClient(session, cityRoot string) *client {
	return &client{session: session, bin: "herdr", cityRoot: cityRoot}
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
	// Revision is the pane's output revision counter. The activity tracker
	// diffs it for sessions herdr cannot classify (agent_status "unknown").
	// Verified live on 0.7.3: it moves only while a client renders the pane;
	// a headless server holds it at 0.
	Revision uint64 `json:"revision"`
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
//
//nolint:unparam // source documents herdr's read API; every current caller snapshots the visible screen
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

// deliverNudge types a nudge into the agent's input and submits it, closing the
// loop on *both* the paste and the submit before it trusts either landed.
//
// Injection is `pane run` (paste semantics: multi-line content is preserved and
// the paste's own trailing newline is swallowed by the TUI, so the text never
// submits on its own). Submission is a real Enter key event (`pane send-keys`,
// which commits where a pasted `\r` does not).
//
// Both steps are fragile against a freshly-spawned pane, learned empirically
// against herdr 0.7.1 + the Claude Code TUI. Start waits for the agent to report
// idle before delivering (see startupNudgeIdleTimeout), but idle (prompt process
// up) precedes input-readiness (the shell→TUI handoff is still settling): a paste
// or submit delivered in that window is silently swallowed. Critically, `pane run`
// reports success even when the paste never lands (empty output → nil), so a
// swallowed paste leaves an *empty* box that no Enter can ever submit — the missed
// startup-nudge stall. (Observed directly: the box shows nothing at all, so this
// is not "typed-but-unsubmitted" — there is nothing typed.)
//
// Delivery runs as two separately-verified phases whose retry actions differ,
// because their failure costs differ:
//
//   - Paste phase: snapshot the visible screen, paste, re-read. A paste that
//     landed changes the screen (its text, or a "[Pasted text]" pill); a
//     swallowed one leaves it identical, so re-paste next attempt. Verifying
//     the paste landed doubles as the input-readiness gate the bare idle check
//     lacked — we never Enter into a dead box.
//
//   - Submit phase: spend an Enter, then confirm via `agent get` that the
//     agent left idle. If it still reads idle, retry the Enter ONLY — never
//     the paste. "Still idle" does not mean the submit failed: a landed submit
//     keeps the agent reporting idle through hook and first-token latency
//     (seconds under town-restart load), and a re-paste in that window queues
//     a duplicate turn. (Observed live 2026-07-06: a named session received
//     its startup prime 4× because each still-idle poll re-pasted and
//     re-submitted.) An extra Enter on an already-empty input box is a no-op,
//     so over-Entering is safe where over-pasting is not. Tie-breaker while
//     idle persists: re-read the screen — if it moved on from the pasted
//     state, the box consumed the text, i.e. the submit landed and the agent
//     just hasn't visibly started; return success rather than an error that
//     would invite a caller-level re-nudge of a message that was delivered.
//
// Bounded so a nudge that legitimately produces no work cannot spin; returns an
// error if delivery never confirms, so the caller can surface a stranded agent
// instead of hiding it.
//
// Contract: inject + submit by pane id, observe (read/confirm) by agent name.
func (c *client) deliverNudge(ctx context.Context, paneID, name, text string) error {
	var lastErr error
	// Paste phase: re-paste only while the screen provably didn't take it. A
	// failed verification read is not proof — re-verify on the next attempt
	// rather than re-pasting into a box that may already hold the text.
	pasted := false
	var pasteScreen string // pre-submit baseline: the screen with the paste in the box
	needPaste := true
	var before string
	for attempt := 0; attempt < submitMaxAttempts && !pasted; attempt++ {
		if needPaste {
			b, rerr := c.read(ctx, name, "visible", 0)
			if rerr != nil {
				lastErr = rerr // transient read failure; retry within the bound
			}
			before = strings.TrimSpace(b)
			if err := c.paneRun(ctx, paneID, text); err != nil {
				return err
			}
			needPaste = false
		}
		time.Sleep(submitSettleDelay) // let the paste commit before we check it
		after, rerr := c.read(ctx, name, "visible", 0)
		if rerr != nil {
			lastErr = rerr // can't verify this paste; re-verify next attempt
			continue
		}
		if strings.TrimSpace(after) != before {
			pasted = true
			pasteScreen = strings.TrimSpace(after)
		} else {
			needPaste = true // provably swallowed — pane not input-ready yet; re-paste
		}
	}
	if !pasted {
		if lastErr != nil {
			return fmt.Errorf("herdr deliverNudge: %q paste never landed after %d attempts: %w", name, submitMaxAttempts, lastErr)
		}
		return fmt.Errorf("herdr deliverNudge: %q paste never landed after %d attempts", name, submitMaxAttempts)
	}
	// Submit phase: retry the Enter only — a re-paste here duplicates the turn.
	for attempt := 0; attempt < submitMaxAttempts; attempt++ {
		if err := c.sendKeys(ctx, paneID, "Enter"); err != nil {
			lastErr = err // transient send failure; verify + retry within the bound
		}
		time.Sleep(submitSettleDelay)
		info, ok, err := c.getAgent(ctx, name)
		switch {
		case err != nil:
			lastErr = err // transient read failure; retry within the bound
		case !ok:
			return fmt.Errorf("herdr deliverNudge: agent %q vanished before submit confirmed", name)
		case !strings.EqualFold(strings.TrimSpace(info.AgentStatus), "idle"):
			return nil // left the idle prompt → submit landed, agent is running
		}
		cur, rerr := c.read(ctx, name, "visible", 0)
		if rerr != nil {
			lastErr = rerr
			continue // can't tell whether the box consumed it; another Enter is safe
		}
		if strings.TrimSpace(cur) != pasteScreen {
			return nil // box consumed the paste → submit landed; agent hasn't visibly started yet (hook/first-token latency)
		}
	}
	if lastErr != nil {
		return fmt.Errorf("herdr deliverNudge: %q submit not confirmed after %d attempts: %w", name, submitMaxAttempts, lastErr)
	}
	return fmt.Errorf("herdr deliverNudge: %q still idle after %d attempts (submit unconfirmed)", name, submitMaxAttempts)
}

// submitSettleDelay is how long deliverNudge waits for a `pane run` paste to
// commit in the TUI before each submit Enter and before re-reading agent status.
// A submit that races the paste is swallowed; ~1s clears it with margin even
// under the concurrent boot load of a town-wide restart.
// Var (not const) so tests can shrink it; production code never writes it.
var submitSettleDelay = 1 * time.Second

// submitMaxAttempts bounds each of deliverNudge's two phases independently
// (paste-until-landed, then Enter-until-confirmed): with one settle wait per
// attempt, ~2·submitMaxAttempts·settle is the worst-case latency before
// deliverNudge gives up and returns an error. Sized to cover a slow shell→TUI
// handoff under restart-time load without spinning on a nudge that
// legitimately leaves the agent idle.
// Var (not const) so tests can shrink it; production code never writes it.
var submitMaxAttempts = 5

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
	if c.sockPath != "" {
		return c.sockPath
	}
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
