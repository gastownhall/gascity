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
	session     string        // herdr named session (shared per city)
	bin         string        // herdr binary (default "herdr")
	cityRoot    string        // city root: the shared server's launch cwd, and the effectiveWorkDir fallback when a session's WorkDir doesn't exist yet (empty in city-less/standalone construction)
	settleDelay time.Duration // paste-fallback settle before the submit Enter (submitSettleDelay; shortened by tests against a fake herdr)
	serverMu    sync.Mutex    // serializes startServer: serverAlive → removeStaleSocket → launch → readiness
	sockPath    string        // test override for socketPath (unit tests point it at a fake server)
}

func newClient(session, cityRoot string) *client {
	return &client{session: session, bin: "herdr", cityRoot: cityRoot, settleDelay: submitSettleDelay}
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
//
// Its errors are scrubbed of anything this client's flag grammar can identify;
// see redaction.go. An argv carrying a credential anywhere else needs
// [client.runWithSecrets].
func (c *client) run(ctx context.Context, args ...string) (json.RawMessage, error) {
	return c.runWithSecrets(ctx, nil, args...)
}

// runWithSecrets is run for an argv carrying a credential the grammar cannot
// find. declared comes from whoever built the argv; see redaction.go.
func (c *client) runWithSecrets(ctx context.Context, declared []string, args ...string) (json.RawMessage, error) {
	full := append([]string{"--session", c.session}, args...)
	out, err := exec.CommandContext(ctx, c.bin, full...).Output()
	if err != nil {
		safe, secrets := redactedArgv(args, declared)
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("herdr %v: %s", safe, redactText(string(ee.Stderr), secrets))
		}
		// err here is exec's own (*exec.Error, *exec.ExitError): it carries the
		// binary name and a status, never anything from args.
		return nil, fmt.Errorf("herdr %v: %w", safe, err)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil // success with no payload (e.g. pane send-keys / pane run)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		// The secrets are dropped deliberately: a json.SyntaxError carries an
		// offset and at most one byte of herdr's stdout, and an
		// UnmarshalTypeError carries type names, so there is nothing here to
		// scrub. Do not "fix" this either direction without changing that.
		safe, _ := redactedArgv(args, declared)
		return nil, fmt.Errorf("herdr %v: decode response: %w", safe, err)
	}
	if env.Error != nil {
		safe, secrets := redactedArgv(args, declared)
		return nil, fmt.Errorf("herdr %v: %w", safe, env.Error.redacted(secrets))
	}
	return env.Result, nil
}

// agentInfo mirrors herdr's agent object. Verified live against herdr 0.7.3:
// the per-entry name field is emitted under the JSON key "agent", not "name"
// (`herdr agent list` → {"agents":[{"agent":"act-a","agent_status":"idle",...}]}).
type agentInfo struct {
	Name        string `json:"agent"`
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

// startupBootBudgetMS is the bound every wait that can land inside an agent's
// boot pays. It is deliberately one number: a cold, concurrent claude boot
// during a town-wide restart is slow in every phase, and the phases are
// sequential parts of the same event. Sizing any one of them off the 15s
// shell-prompt bounds elsewhere in this package (paneShellReadyWait, the
// launch probes) would make that phase the one that gives up early on the
// exact wave it exists to survive. Its Duration twin is
// startupNudgeIdleTimeout. The worst case is three of these end to end
// (readiness, prompt confirmation, Enter recovery), well inside the
// reconciler's pendingCreateNeverStartedTimeout of 10m.
const startupBootBudgetMS = 60000

// agentStartTimeoutMS bounds herdr's own wait for the launched agent TUI to
// be detected and interactive-ready (`agent start --timeout`). herdr requires
// >3000 and defaults to 30000; sized up to the boot budget.
const agentStartTimeoutMS = startupBootBudgetMS

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
//
// It takes no declared secrets: no raw verb carries one outside an `--env`
// pair, which redaction.go finds on its own. Give it one and this needs the
// [client.runWithSecrets] treatment.
func (c *client) runRaw(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--session", c.session}, args...)
	out, err := exec.CommandContext(ctx, c.bin, full...).Output()
	if err != nil {
		safe, secrets := redactedArgv(args, nil)
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("herdr %v: %s", safe, redactText(string(ee.Stderr), secrets))
		}
		// See run: exec's own error carries the binary name and a status only.
		return "", fmt.Errorf("herdr %v: %w", safe, err)
	}
	trimmed := strings.TrimSpace(string(out))
	if strings.HasPrefix(trimmed, "{") {
		var env envelope
		if jerr := json.Unmarshal([]byte(trimmed), &env); jerr == nil && env.Error != nil {
			safe, secrets := redactedArgv(args, nil)
			return "", fmt.Errorf("herdr %v: %w", safe, env.Error.redacted(secrets))
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
//
// The operand is not necessarily a command: pasteAndSubmit delivers an agent's
// prompt or nudge through this same verb. Text pasted here is treated as
// carrying no credentials, so it is rendered verbatim in errors — a raw launch
// uses [client.paneRunCommand] instead. The wider exposure of prompt text in
// argv is ga-mxj2a.
func (c *client) paneRun(ctx context.Context, paneID, command string) error {
	_, err := c.run(ctx, "pane", "run", paneID, command)
	return err
}

// paneRunCommand is paneRun for a session's configured launch command, which
// is opaque to us: it is a user-authored shell string, so any credential in it
// sits somewhere only a shell parser could find — after an `env` wrapper, past
// a `&&`, inside a nested `sh -c`, spelled with quotes that keep the value from
// matching its own rendering. Rather than guess at that structure, the whole
// operand is declared and so withheld from error text — down to the floor
// [runtime.RedactSecrets] substitutes above, below which a shell command is too
// short to hold a credential. The error still names the verb, the pane and
// herdr's own complaint; the command it was asked to run is in the session's
// config.
func (c *client) paneRunCommand(ctx context.Context, paneID, command string) error {
	_, err := c.runWithSecrets(ctx, []string{command}, "pane", "run", paneID, command)
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
//
// Note this path does NOT confirm the submit landed: `agent prompt` without
// --wait reports ok the moment the text is typed. Against a live mid-session
// TUI that is reliable; a FIRST turn into a freshly-booted TUI must go
// through deliverStartupTurn, which opts into herdr's submission
// confirmation.
func (c *client) deliverNudge(ctx context.Context, paneID, text string) error {
	err := c.agentPrompt(ctx, paneID, text)
	if err == nil {
		return nil
	}
	if !isAgentNotFound(err) {
		return err
	}
	return c.pasteAndSubmit(ctx, paneID, text)
}

// Startup-delivery confirmation bounds (herdr ≥0.7.5 `agent prompt --wait`).
// Both must exceed herdr's fixed 5000ms observed-state-change window, or a
// plain "timeout" masks the more precise "agent_prompt_stalled" and the two
// verdicts below stop being distinguishable. Both are the boot budget rather
// than merely >5000: they are waits on a booting agent, and a bound that
// expires early during a cold-boot wave turns a healthy slow session into a
// recorded strand.
const (
	startupPromptConfirmTimeoutMS = startupBootBudgetMS
	startupStallRecoveryTimeoutMS = startupBootBudgetMS
)

// agentStateIdle is herdr's pre-submit agent_status: the prompt is rendered
// and the input box is empty. It is the one state a bare Enter is a no-op in,
// which is why it qualifies the stall recovery below and is deliberately
// absent from startupConfirmStates.
const agentStateIdle = "idle"

// startupConfirmStates are the post-submission states that prove the submit
// CR took: the turn is running (working), already finished (done — an
// ultra-short turn can settle between herdr's observations), or hit a dialog
// (blocked). "idle" is deliberately absent — it is the pre-submit state, and
// matching it would confirm a swallowed CR. herdr matches only the states
// asked for, so dropping "blocked" would leave a dialog-opening first turn to
// time out as if it had never submitted.
var startupConfirmStates = []string{"working", "done", "blocked"}

// deliverStartupTurn delivers an agent's FIRST turn with submission
// confirmation, unlike deliverNudge's fire-and-forget prompt. A freshly
// spawned TUI boots through a shell→TUI handoff during which the submit CR
// can be swallowed: the text sits typed-but-unsubmitted in the input box and
// the agent idles forever (the stranded-startup outage, gas-90h). The
// un-waited `agent prompt` reports ok without verifying the CR took, so here
// the prompt runs with --wait: herdr requires an observed state change after
// submission and returns agent_prompt_stalled when the agent never leaves
// idle — the swallowed-CR detector.
//
// The two failure verdicts are NOT interchangeable, and only one of them
// earns a keystroke:
//
//   - agent_prompt_stalled — herdr saw no state change at all inside its
//     5000ms window. That is the swallowed CR *if the agent was idle
//     throughout*, and only then: an agent already sitting on a confirmation
//     dialog before its first turn is equally unchanging, and an Enter there
//     ANSWERS the dialog rather than no-op'ing on an empty box. The
//     readiness wait cannot rule that out — the live city measured 0 idle /
//     8 timeout / 2 error over 20h — so the state is read directly
//     (`agent get`) and the Enter is withheld unless herdr says idle.
//     Recovery is then a single explicit Enter plus a confirming wait; never
//     a re-prompt, which would double the text.
//   - timeout — reachable only after that state-change gate passed, because
//     startupPromptConfirmTimeoutMS exceeds the 5000ms window (a shorter
//     bound would surface the stall as a timeout and collapse the
//     distinction). So the submit demonstrably landed and only the settle
//     into a confirming state ran past the bound. The box is not idle: the
//     agent may be mid-turn, or `blocked` on a confirmation dialog, where an
//     Enter would answer the dialog rather than do nothing. No keystroke —
//     report it unconfirmed and let the caller record the strand.
//
// Unregistered panes (raw shells) keep the best-effort paste+settle+Enter
// path. A non-nil error means the turn could not be confirmed submitted.
func (c *client) deliverStartupTurn(ctx context.Context, paneID, text string) error {
	args := []string{"agent", "prompt", paneID, text, "--wait"}
	for _, s := range startupConfirmStates {
		args = append(args, "--until", s)
	}
	args = append(args, "--timeout", strconv.Itoa(startupPromptConfirmTimeoutMS))
	_, err := c.run(ctx, args...)
	if err == nil {
		return nil
	}
	if isAgentNotFound(err) {
		return c.pasteAndSubmit(ctx, paneID, text)
	}
	switch herdrErrorCode(err) {
	case "timeout":
		return fmt.Errorf("startup submit landed but never reached %v within %dms: %w",
			startupConfirmStates, startupPromptConfirmTimeoutMS, err)
	case "agent_prompt_stalled":
		cur, ok, gerr := c.getAgent(ctx, paneID)
		switch {
		case gerr != nil:
			return fmt.Errorf("startup submit not confirmed (%w) and reading the agent state to qualify recovery failed: %w", err, gerr)
		case !ok:
			return fmt.Errorf("startup submit not confirmed (%w) and the agent went away before recovery could be qualified", err)
		case cur.AgentStatus != agentStateIdle:
			return fmt.Errorf("startup submit not confirmed (%w) and recovery Enter withheld: agent is %q, not %q — the keystroke would land in that state, not on an empty input box",
				err, cur.AgentStatus, agentStateIdle)
		}
		if kerr := c.sendKeys(ctx, paneID, "Enter"); kerr != nil {
			return fmt.Errorf("startup submit not confirmed (%w) and recovery Enter failed: %w", err, kerr)
		}
		wait := []string{"agent", "wait", paneID}
		for _, s := range startupConfirmStates {
			wait = append(wait, "--until", s)
		}
		wait = append(wait, "--timeout", strconv.Itoa(startupStallRecoveryTimeoutMS))
		if _, werr := c.run(ctx, wait...); werr != nil {
			return fmt.Errorf("startup submit not confirmed after Enter recovery (%w): %w", err, werr)
		}
		return nil
	}
	return err
}

// pasteAndSubmit is the unregistered-pane delivery: paste, settle, submit.
func (c *client) pasteAndSubmit(ctx context.Context, paneID, text string) error {
	if err := c.paneRun(ctx, paneID, text); err != nil {
		return err
	}
	time.Sleep(c.settleDelay)
	return c.sendKeys(ctx, paneID, "Enter")
}

// isAgentNotFound reports whether err is herdr's missing-agent rejection
// (typed agent_not_found, or the message-text forms the pre-typed-error
// callers matched on).
func isAgentNotFound(err error) bool {
	if err == nil {
		return false
	}
	if herdrErrorCode(err) == "agent_not_found" {
		return true
	}
	return strings.Contains(err.Error(), "not_found") || strings.Contains(err.Error(), "not found")
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

// getAgent fetches one agent by target — an agent name or the pane id hosting
// it: (info, true, nil) if present, (zero, false, nil) if herdr reports it
// absent, (_, false, err) on failure. Verified live on herdr 0.7.3: `agent
// get <target>` only resolves a pane id — passing an agent name returns
// agent_not_found even though `agent list` lists that same agent under that
// name — so a name-shaped target that `agent get` rejects as not-found falls
// back to a listAgents scan, which does resolve by name.
func (c *client) getAgent(ctx context.Context, target string) (agentInfo, bool, error) {
	res, err := c.run(ctx, "agent", "get", target)
	if err == nil {
		var wrap struct {
			Agent agentInfo `json:"agent"`
		}
		if err := json.Unmarshal(res, &wrap); err != nil {
			return agentInfo{}, false, fmt.Errorf("herdr agent get: decode: %w", err)
		}
		return wrap.Agent, true, nil
	}
	if !strings.Contains(err.Error(), "not_found") && !strings.Contains(err.Error(), "not found") {
		return agentInfo{}, false, err
	}
	agents, lerr := c.listAgents(ctx)
	if lerr != nil {
		return agentInfo{}, false, fmt.Errorf("herdr agent get %q: list fallback: %w", target, lerr)
	}
	for _, a := range agents {
		if a.Name == target {
			return a, true, nil
		}
	}
	return agentInfo{}, false, nil
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
// created with the given cwd and env, and returns the default tab and root pane
// (the agent's pane) herdr auto-spawns inside it. The workspace id itself is not
// returned: callers address the agent by tab and pane, and resolveWorkspace
// looks the id up by label when it needs one.
func (c *client) workspaceCreate(ctx context.Context, label, cwd string, env map[string]string) (tabID, paneID string, err error) {
	args := []string{"workspace", "create", "--label", label, "--no-focus"}
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
		return "", "", fmt.Errorf("herdr workspace create: decode: %w", err)
	}
	return wrap.Tab.TabID, wrap.RootPane.PaneID, nil
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
		tabID, paneID, err = c.workspaceCreate(ctx, wsLabel, cwd, env)
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

// socketPath is the unix socket for this client's herdr session. Must match
// wherever the herdr binary itself resolves its config/state directory —
// confirmed empirically (`XDG_CONFIG_HOME=X herdr --help` prints
// "Config: X/herdr/config.toml" regardless of $HOME; with XDG_CONFIG_HOME
// unset it falls back to "$HOME/.config/herdr/…") to be standard XDG Base
// Directory precedence, i.e. exactly os.UserConfigDir() on this platform. A
// plain os.UserHomeDir()+".config" join (the prior implementation) silently
// ignores XDG_CONFIG_HOME, so it diverges from herdr's own resolution in any
// environment that sets XDG_CONFIG_HOME while pointing $HOME elsewhere —
// this fleet's agent sandboxes do exactly that. The result: this client
// dials a socket no herdr process ever binds, so serverAlive() reads false
// against a perfectly healthy server ("did not become ready"), and every
// retry launches a redundant herdr server contending for the same pane
// ("agent_pane_busy") — ga-nqlb8q.
func (c *client) socketPath() string {
	if c.sockPath != "" {
		return c.sockPath
	}
	configDir, _ := os.UserConfigDir()
	if c.session == "" || c.session == "default" {
		return filepath.Join(configDir, "herdr", "herdr.sock")
	}
	return filepath.Join(configDir, "herdr", "sessions", c.session, "herdr.sock")
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
