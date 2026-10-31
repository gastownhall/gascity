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

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
)

// client runs `herdr` CLI verbs against a named herdr session and decodes the
// response envelope ({"id":…,"result":…} | {"id":…,"error":{code,message}}).
type client struct {
	session        string                                                   // herdr named session (shared per city)
	bin            string                                                   // herdr binary (default "herdr")
	cityRoot       string                                                   // city root: the shared server's launch cwd, and the effectiveWorkDir fallback when a session's WorkDir doesn't exist yet (empty in city-less/standalone construction)
	settleDelay    time.Duration                                            // paste-fallback settle before the submit Enter (submitSettleDelay; shortened by tests against a fake herdr)
	rejectDelay    time.Duration                                            // pause before re-prompting a startup turn the agent rejected as not ready (startupNotReadyRetryDelay; shortened by tests against a fake herdr)
	notReadyBudget time.Duration                                            // longest re-prompting of a turn rejected as not ready runs from the first submit (startupNotReadyBudget; shortened by tests against a fake herdr)
	serverMu       sync.Mutex                                               // serializes startServer: serverAlive → removeStaleSocket → launch → readiness
	sockPath       string                                                   // test override for socketPath (unit tests point it at a fake server)
	dialUnix       func(ctx context.Context, path string) (net.Conn, error) // test override for the socket connect (unit tests observe the context the dial runs under)
}

func newClient(session, cityRoot string) *client {
	return &client{session: session, bin: "herdr", cityRoot: cityRoot, settleDelay: submitSettleDelay, rejectDelay: startupNotReadyRetryDelay, notReadyBudget: startupNotReadyBudget, dialUnix: dialUnix}
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

// herdrCodeAnyShape returns the herdr-reported error code from err
// regardless of which shape runWithSecrets wrapped it in: the typed
// envelope herdrErrorCode already recovers (a zero-exit CLI invocation), or
// the *herdrStderr shape a non-zero exit wraps instead. herdrErrorCode alone
// cannot see through the second shape, which is what left the pane-busy
// retry guard at provider.go unable to recognize agent_pane_busy
// (ga-iwanrj): herdr reports that rejection via a non-zero exit.
func herdrCodeAnyShape(err error) string {
	if code := herdrErrorCode(err); code != "" {
		return code
	}
	var hs *herdrStderr
	if !errors.As(err, &hs) {
		return ""
	}
	codes := herdrErrorCodeFromStderr(hs.Text)
	if len(codes) == 0 {
		return ""
	}
	return codes[0]
}

// herdrErrorCodeFromStderr scans stderr text from a non-zero-exit herdr
// invocation for every JSON error envelope it contains and returns their
// codes in the order found. Applies no filtering of its own -- callers
// decide which code, if any, matters; disqualifyingCode's agentlessCapableCode
// filter in particular stays there, not here.
func herdrErrorCodeFromStderr(text string) []string {
	var codes []string
	for i := strings.Index(text, "{"); i >= 0; {
		var env envelope
		// A decoder rather than Unmarshal: it stops at the end of the first
		// complete value, so an envelope with text after it still parses.
		if derr := json.NewDecoder(strings.NewReader(text[i:])).Decode(&env); derr == nil && env.Error != nil {
			if code := env.Error.Code; code != "" {
				codes = append(codes, code)
			}
		}
		next := strings.Index(text[i+1:], "{")
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return codes
}

// disqualifyingCode returns a herdr error code that rules the paste fallback out,
// or "" if none does. It is the only use this change makes of a failure's text, and
// the direction is the whole point: see targetHasNoNamedAgent for why text cannot
// be trusted to GRANT anything.
//
// Two sources, both herdr's own. The typed envelope is herdr's structured answer to
// an invocation that reached the server. The other is herdr's stderr from an
// invocation that exited non-zero, which is a type here (herdrStderr) precisely so
// this scan cannot reach the argv rendered beside it: the argv holds the nudge, and
// a nudge that could put a code in front of herdr's would be able to DISPLACE
// herdr's real rejection rather than merely add to it.
//
// Within herdr's own stderr, every envelope is considered and any refusing code
// wins. That keeps the one property this rests on: caller text can only ever ADD a
// refusal, never remove one. A refusal is a visible error the caller retries on, so
// the worst a nudge can do to itself is decline its own fallback; the reverse,
// granting a fallback, would paste into a pane whose real failure was something
// else. Do not introduce a bound on this scan, or a long echo becomes a way to
// push herdr's rejection out of view.
func disqualifyingCode(err error) string {
	if err == nil {
		return ""
	}
	if code := herdrErrorCode(err); code != "" && !agentlessCapableCode(code) {
		return code
	}
	var hs *herdrStderr
	if !errors.As(err, &hs) {
		return ""
	}
	for _, code := range herdrErrorCodeFromStderr(hs.Text) {
		if !agentlessCapableCode(code) {
			return code
		}
	}
	return ""
}

// agentlessCapableCode reports whether a herdr error code leaves open that the
// target carries no named agent. agent_not_found says so outright; 0.8.0's
// agent_not_ready covers both that and an agent still booting, which is why the
// code cannot settle the question on its own. Every other code is herdr naming a
// different problem (a busy pane, a bad flag), and naming one rules the fallback
// out: pasting then would type into whatever the real failure was about and
// swallow it.
func agentlessCapableCode(code string) bool {
	return code == "" || code == "agent_not_found" || code == "agent_not_ready"
}

// agentStateRejection reports whether herdr declined THIS invocation over the
// target's agent, rather than failing for some unrelated reason. It is the
// positive half of the gate: disqualifyingCode rules the paste fallback out when
// herdr names a different problem, and this rules it out when herdr named no
// problem at all. The registry can establish that a pane carries no named agent;
// only herdr can establish that this is why the call failed, and a failure herdr
// never coded (a malformed argv of ours, a binary that did not run) means it never
// reached the question. Without this, such a failure plus an empty registry pasted
// into a pane herdr had not checked for a foreground process.
//
// The two channels are asymmetric on purpose. A refusal may be found anywhere in
// herdr's stderr, because adding one only declines a fallback. A GRANT has to come
// from a channel a caller cannot write: either the typed envelope parsed off
// stdout, or a stderr stream that is NOTHING BUT one error envelope. That second
// shape is herdr 0.8.0's rejection as verified against the installed binary (the
// bare envelope, no prefix and nothing after it), and an echo cannot reach it,
// because when herdr rejects our argv it prints its own words alongside whatever it
// quotes back. Loosen it to "an envelope somewhere in the stream" and a nudge
// carrying one grants itself a paste.
//
// The cost is the direction this fails in. herdr respelling its agent-state code a
// third time would leave the fallback refused rather than wrongly granted: the
// caller sees herdr's error instead of a paste into a pane nothing was checked on.
func agentStateRejection(err error) bool {
	code := herdrAnswerCode(err)
	return code != "" && agentlessCapableCode(code)
}

// herdrAnswerCode returns the code herdr answered THIS invocation with, read only
// from the two channels a caller's text cannot write. The first is the envelope this
// client parsed off stdout. The second is a stderr stream that is nothing but one
// error envelope, which is how the CLI reports a rejection when it exits non-zero
// (verified against the installed herdr 0.8.0: the bare envelope, no prefix and
// nothing after it) and is the shape most rejections actually arrive in, since
// nothing reaches the stdout decode on a non-zero exit.
//
// An echo cannot reach either. When herdr rejects our argv it prints its own words
// alongside whatever it quotes back, so a forged envelope inside that text is never
// the whole stream. That is what makes this answer usable to GRANT the paste
// fallback, where disqualifyingCode's permissive scan of the same text is only ever
// usable to refuse it.
func herdrAnswerCode(err error) string {
	if code := herdrErrorCode(err); code != "" {
		return code
	}
	var hs *herdrStderr
	if !errors.As(err, &hs) {
		return ""
	}
	var env envelope
	if derr := json.Unmarshal([]byte(strings.TrimSpace(hs.Text)), &env); derr != nil || env.Error == nil {
		return ""
	}
	return env.Error.Code
}

// herdrStderr carries what herdr wrote to stderr on an invocation that exited
// non-zero without a parseable envelope on stdout. It is a type rather than text
// spliced into the error message so that a reader can reach herdr's half of the
// failure without reaching the argv this client also renders there: the argv holds
// the nudge body, and the nudge is free text. See disqualifyingCode.
type herdrStderr struct{ Text string }

func (e *herdrStderr) Error() string { return e.Text }

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
	cmd := exec.CommandContext(ctx, c.bin, full...)
	if err := execenv.CheckBatchFileArgs(cmd); err != nil {
		safe, _ := redactedArgv(args, declared)
		return nil, fmt.Errorf("herdr %v: %w", safe, err)
	}
	out, err := cmd.Output()
	if err != nil {
		safe, secrets := redactedArgv(args, declared)
		if cerr := ctx.Err(); cerr != nil {
			// exec killed herdr when ctx ended, and a killed process's exit
			// status says nothing about why.
			return nil, fmt.Errorf("herdr %v: %w", safe, cerr)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("herdr %v: %w", safe, &herdrStderr{Text: redactText(string(ee.Stderr), secrets)})
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

// agentInfo mirrors herdr's agent object. herdr 0.7.3 emits the name under
// the JSON key "agent"; 0.9.1 moved it to "name" and reuses "agent" for the
// agent kind ("claude", "codex"), which UnmarshalJSON resolves.
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

// UnmarshalJSON prefers "name" over "agent": on herdr 0.9.1 decoding "agent"
// keyed every pane by its kind, so all Claude sessions collapsed into one
// activity entry and none could ever read as idle for nudge delivery.
func (a *agentInfo) UnmarshalJSON(b []byte) error {
	type plain agentInfo
	var v struct {
		plain
		Label string `json:"name"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*a = agentInfo(v.plain)
	if v.Label != "" {
		a.Name = v.Label
	}
	return nil
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
	return filterAgentInfos(wrap.Agents), nil
}

// paneRead → `herdr pane read <paneID> --source <source> [--lines n]`
// (herdr ≥0.7.5). Reads any pane's screen without needing a registered agent
// (raw shell sessions never register one). Use "visible" for the current
// screen (the liveness/fingerprint snapshot). On 0.7.5 the CLI prints the
// text raw rather than in the JSON envelope, so this parses failures out of
// an envelope only when one is present.
func (c *client) paneRead(ctx context.Context, paneID, source string, lines int) (string, error) { //nolint:unparam // source mirrors the herdr CLI flag; callers read "visible" today
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
	cmd := exec.CommandContext(ctx, c.bin, full...)
	if err := execenv.CheckBatchFileArgs(cmd); err != nil {
		safe, _ := redactedArgv(args, nil)
		return "", fmt.Errorf("herdr %v: %w", safe, err)
	}
	out, err := cmd.Output()
	if err != nil {
		safe, secrets := redactedArgv(args, nil)
		if cerr := ctx.Err(); cerr != nil {
			// See run: a ctx that ended killed herdr.
			return "", fmt.Errorf("herdr %v: %w", safe, cerr)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("herdr %v: %w", safe, &herdrStderr{Text: redactText(string(ee.Stderr), secrets)})
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
//
// herdr refuses the prompt with agent_blocked while the agent waits on an
// interactive prompt of its own (a question to the user, a permission dialog).
// That comes back as [runtime.ErrAgentBlocked], with herdr's error still in the
// chain, so a caller holding the nudge can keep it until the prompt is answered.
// The code is read with herdrAnswerCode on every platform: herdr reports it
// either in the stdout envelope or, on a non-zero exit, as a stderr that is
// nothing but the envelope, and the nudge text can write neither.
func (c *client) deliverNudge(ctx context.Context, paneID, text string) error {
	err := c.agentPrompt(ctx, paneID, text)
	if err == nil {
		return nil
	}
	if herdrAnswerCode(err) == "agent_blocked" {
		return fmt.Errorf("%w: %w", runtime.ErrAgentBlocked, err)
	}
	if !c.targetHasNoNamedAgent(ctx, paneID, err) {
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

// normalizeAgentState folds one reported agent_status to the spelling the
// package's classifying readers compare against. It is deliberately shared:
// two readers normalizing the same field differently classify the same payload
// differently, and the disagreement is invisible at both sites. Note the
// keystroke guard below does NOT use it, on purpose — it withholds a real
// keystroke unless herdr reported exactly the idle state, and widening that is
// a behavior change, not a cleanup.
func normalizeAgentState(status string) string {
	return strings.ToLower(strings.TrimSpace(status))
}

// startupConfirmStates are the post-submission states that prove the submit
// CR took: the turn is running (working), already finished (done — an
// ultra-short turn can settle between herdr's observations), or hit a dialog
// (blocked). "idle" is deliberately absent — it is the pre-submit state, and
// matching it would confirm a swallowed CR. herdr matches only the states
// asked for, so dropping "blocked" would leave a dialog-opening first turn to
// time out as if it had never submitted.
var startupConfirmStates = []string{"working", "done", "blocked"}

// Bounds on re-prompting a startup turn the agent rejected as not ready (see
// runtime.Config.NotReadyMarkers). How long an agent stays not ready is the
// provider's business (Copilot loading its managed-account policy has taken
// 30-60s and more), so re-prompting is bounded by time, not a count: see
// notReadyGiveUp. startupNotReadyReserve is what it leaves of Start's ctx. It
// covers the last attempt (herdr's fixed 5000ms observed-state-change window
// plus the pane reads) and lets Start return before its ctx expires: the
// reconciler records a Start that returns after its deadline as a failed
// start, even though the session is live. A last attempt that stalls into the
// Enter recovery instead is bounded by startupDeadlineMargin, as every
// confirming wait is. startupNotReadyBudget bounds a ctx without a deadline.
const (
	startupNotReadyRetryDelay = 5 * time.Second
	startupNotReadyReserve    = 20 * time.Second
	startupNotReadyBudget     = 150 * time.Second
)

// startupDeadlineMargin is what the startup delivery leaves of Start's ctx for
// the work after its last herdr wait: the screen and state reads that act on
// it, and recording an unconfirmed turn on the sidecar. Every confirming wait
// the delivery starts is capped to end this long before the deadline
// (startupTimeoutMS), because the reconciler records a Start that returns
// after its deadline as a failed start, even though the session is live.
const startupDeadlineMargin = 2 * time.Second

// herdrStateChangeWindowMS is herdr's fixed window for a submitted prompt to
// show a working or blocked state (`agent prompt --wait`; otherwise
// agent_prompt_stalled). It is the shortest confirming wait the startup
// delivery runs: a prompt bound at or under it would report a stall as a
// plain timeout, and a shorter recovery wait could miss a submit that took.
const herdrStateChangeWindowMS = 5000

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
//     Recovery is then an explicit Enter plus a confirming wait, retried
//     while herdr still reports idle where the platform allows
//     (startupEnterRecoveryAttempts); never a re-prompt, which would double
//     the text. The one exception is a turn the agent rejected as not ready
//     (notReadyMarkers, see rejectedAsNotReady): it consumed the text and
//     left the box empty, so an Enter submits nothing and the turn is
//     prompted again instead, until the agent takes it or notReadyGiveUp
//     passes. The rejection is looked for at the stall and again after each
//     recovery Enter that does not confirm: a turn typed before the agent's
//     TUI was up is only submitted, and rejected, by one of those Enters.
//     A marker the text itself quotes is ignored (markersNotQuotedBy), and so
//     is any marker while the agent's input box, read off its composer line
//     (readyPromptPrefix), still holds text.
//   - timeout — reachable only after that state-change gate passed, because
//     the prompt's bound always exceeds herdrStateChangeWindowMS (a shorter
//     bound would surface the stall as a timeout and collapse the
//     distinction). So the submit demonstrably landed and only the settle
//     into a confirming state ran past the bound. The box is not idle: the
//     agent may be mid-turn, or `blocked` on a confirmation dialog, where an
//     Enter would answer the dialog rather than do nothing. No keystroke —
//     report it unconfirmed and let the caller record the strand.
//
// Every confirming wait is bounded by Start's ctx as well (startupTimeoutMS),
// so the delivery ends startupDeadlineMargin before the deadline. When that
// leaves less than herdr's state-change window, there is no time to confirm a
// submit: the turn is sent without --wait and reported unconfirmed, and the
// Enter recovery withholds its Enter.
//
// Unregistered panes (raw shells) keep the best-effort paste+settle+Enter
// path. A non-nil error means the turn could not be confirmed submitted.
func (c *client) deliverStartupTurn(ctx context.Context, paneID, text string, notReadyMarkers []string, readyPromptPrefix string) error {
	notReadyMarkers = markersNotQuotedBy(notReadyMarkers, text)
	first := time.Now()
	giveUp := c.notReadyGiveUp(ctx, first)
	for attempt := 1; ; attempt++ {
		timeoutMS := startupTimeoutMS(ctx, startupPromptConfirmTimeoutMS)
		if timeoutMS <= herdrStateChangeWindowMS {
			if err := c.deliverNudge(ctx, paneID, text); err != nil {
				return fmt.Errorf("startup turn not sent (%w): %w", startupTimeShort(timeoutMS), err)
			}
			return fmt.Errorf("startup turn sent without confirmation: %w", startupTimeShort(timeoutMS))
		}
		args := []string{"agent", "prompt", paneID, text, "--wait"}
		for _, s := range startupConfirmStates {
			args = append(args, "--until", s)
		}
		args = append(args, "--timeout", strconv.Itoa(timeoutMS))
		before, berr := c.countNotReadyMarkers(ctx, paneID, notReadyMarkers)
		_, err := c.run(ctx, args...)
		if err == nil {
			if attempt > 1 {
				fmt.Fprintf(os.Stderr, "herdr: startup turn on pane %s taken on attempt %d, %s after the first submit\n", //nolint:errcheck // best-effort diagnostic
					paneID, attempt, time.Since(first).Round(time.Second))
			}
			return nil
		}
		if c.targetHasNoNamedAgent(ctx, paneID, err) {
			return c.pasteAndSubmit(ctx, paneID, text)
		}
		// herdr reports both verdicts through a non-zero exit, which herdrErrorCode
		// cannot see; reading only that shape left every stall unrecovered.
		switch herdrCodeAnyShape(err) {
		case "timeout":
			return fmt.Errorf("startup submit landed but never reached %v within %dms: %w",
				startupConfirmStates, timeoutMS, err)
		case "agent_prompt_stalled":
			marker, rejected, nerr := c.rejectedAsNotReady(ctx, paneID, notReadyMarkers, readyPromptPrefix, before, berr)
			if nerr != nil {
				err = fmt.Errorf("%w (checking for a not-ready rejection: %w)", err, nerr)
			}
			if !rejected {
				recovered, rerr := c.recoverStalledStartup(ctx, paneID, err, func() (string, error) {
					m, ok, cerr := c.rejectedAsNotReady(ctx, paneID, notReadyMarkers, readyPromptPrefix, before, berr)
					if !ok {
						return "", cerr
					}
					return m, nil
				})
				if recovered == "" {
					return rerr
				}
				marker = recovered
			}
			if !time.Now().Add(c.rejectDelay).Before(giveUp) {
				return fmt.Errorf("startup turn rejected as not ready (%q on screen) on all %d attempts over %s: %w",
					marker, attempt, time.Since(first).Round(time.Second), err)
			}
			fmt.Fprintf(os.Stderr, "herdr: startup turn on pane %s rejected as not ready (%q on screen) on attempt %d, %s after the first submit; prompting again\n", //nolint:errcheck // best-effort diagnostic
				paneID, marker, attempt, time.Since(first).Round(time.Second))
			select {
			case <-ctx.Done():
				return fmt.Errorf("startup turn rejected as not ready (%q on screen), retry abandoned: %w", marker, ctx.Err())
			case <-time.After(c.rejectDelay):
			}
			continue
		}
		return err
	}
}

// notReadyGiveUp returns when deliverStartupTurn stops re-prompting a turn
// rejected as not ready: notReadyBudget after first, the first submit, or
// startupNotReadyReserve before ctx's deadline when that is sooner.
func (c *client) notReadyGiveUp(ctx context.Context, first time.Time) time.Time {
	giveUp := first.Add(c.notReadyBudget)
	if deadline, ok := ctx.Deadline(); ok {
		if reserved := deadline.Add(-startupNotReadyReserve); reserved.Before(giveUp) {
			giveUp = reserved
		}
	}
	return giveUp
}

// startupTimeoutMS bounds a confirming herdr wait of ms by Start's ctx: ms, or
// the milliseconds left until startupDeadlineMargin before ctx's deadline when
// that is sooner. It is not positive once that point has passed.
func startupTimeoutMS(ctx context.Context, ms int) int {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ms
	}
	return min(ms, int(time.Until(deadline.Add(-startupDeadlineMargin)).Milliseconds()))
}

// startupTimeShort says why the startup delivery cannot confirm a step:
// leftMS, what startupTimeoutMS leaves, is under herdr's state-change window.
func startupTimeShort(leftMS int) error {
	return fmt.Errorf("only %s of Start's ctx is left before its %s deadline margin, under herdr's %dms confirmation window",
		time.Duration(max(leftMS, 0))*time.Millisecond, startupDeadlineMargin, herdrStateChangeWindowMS)
}

// foldSpace folds every whitespace run in s to one space, so text the TUI
// wrapped across lines compares equal to its source.
func foldSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// markersNotQuotedBy returns the markers that text does not itself contain. A
// marker the submitted turn quotes cannot tell a rejection from a swallowed
// CR: when the CR is swallowed the typed text stays on screen in the input box
// and raises that marker's count on its own. Dropping such a marker leaves
// that stall to the Enter recovery, which is right for a swallowed CR.
func markersNotQuotedBy(markers []string, text string) []string {
	if len(markers) == 0 {
		return nil
	}
	folded := foldSpace(text)
	var usable []string
	for _, m := range markers {
		if !strings.Contains(folded, foldSpace(m)) {
			usable = append(usable, m)
		}
	}
	return usable
}

// countNotReadyMarkers counts each of markers on the pane's visible screen,
// with whitespace runs folded to one space on both sides so a marker the TUI
// wrapped across lines still counts. It reads nothing when there are no
// markers.
func (c *client) countNotReadyMarkers(ctx context.Context, paneID string, markers []string) ([]int, error) {
	if len(markers) == 0 {
		return nil, nil
	}
	screen, err := c.paneRead(ctx, paneID, "visible", 0)
	if err != nil {
		return nil, err
	}
	return countMarkers(screen, markers), nil
}

// countMarkers is countNotReadyMarkers on a screen already read.
func countMarkers(screen string, markers []string) []int {
	screen = foldSpace(screen)
	counts := make([]int, len(markers))
	for i, m := range markers {
		if m = foldSpace(m); m != "" {
			counts[i] = strings.Count(screen, m)
		}
	}
	return counts
}

// composerHoldsText reports whether the agent's input box holds text, as screen
// shows it. The box is the last line that starts with readyPromptPrefix (the
// live composer, drawn below the transcript, whose echoes of past turns start
// with the prompt too), and it holds text when anything follows the prompt.
// observed is false when no line starts with the prompt, or there is no
// prompt. NBSP is read as a space, as the tmux provider's composer reader does.
func composerHoldsText(screen, readyPromptPrefix string) (holds, observed bool) {
	prompt := strings.ReplaceAll(readyPromptPrefix, "\u00a0", " ")
	bare := strings.TrimSpace(prompt)
	if bare == "" {
		return false, false
	}
	for _, line := range strings.Split(screen, "\n") {
		line = strings.TrimSpace(strings.ReplaceAll(line, "\u00a0", " "))
		if rest, ok := strings.CutPrefix(line, prompt); ok {
			holds, observed = strings.TrimSpace(rest) != "", true
		} else if line == bare {
			holds, observed = false, true
		}
	}
	return holds, observed
}

// rejectedAsNotReady reports whether the agent discarded the startup turn it
// was just sent because it was not ready for input, returning the marker that
// says so. An agent that does this consumes the text, prints a marker and
// stays idle, which herdr reports as agent_prompt_stalled just like a
// swallowed CR; the difference is that its input box is empty. Three things
// establish that: a marker that is on screen more times than it was before
// the submit (a marker already there says nothing about this submit, whose
// text may still be in the box), an input box that does not hold text
// (composerHoldsText; a marker the TUI drew for another reason, such as a
// resumed conversation's history, can rise while the turn still sits in the
// box, and a re-prompt would type it twice), and herdr reporting the agent
// idle (a re-prompt into any other state would be typed into whatever the
// agent is showing). A box the screen does not show is not held against the
// marker: the rejection it would miss strands the agent with no instructions.
// before and berr are countNotReadyMarkers' result from before the submit;
// without a baseline nothing is established, and berr is returned.
func (c *client) rejectedAsNotReady(ctx context.Context, paneID string, markers []string, readyPromptPrefix string, before []int, berr error) (string, bool, error) {
	if len(markers) == 0 {
		return "", false, nil
	}
	if berr != nil {
		return "", false, fmt.Errorf("reading the screen before the submit: %w", berr)
	}
	screen, err := c.paneRead(ctx, paneID, "visible", 0)
	if err != nil {
		return "", false, fmt.Errorf("reading the screen after the submit: %w", err)
	}
	after := countMarkers(screen, markers)
	marker := ""
	for i := range markers {
		if after[i] > before[i] {
			marker = markers[i]
			break
		}
	}
	if marker == "" {
		return "", false, nil
	}
	if holds, _ := composerHoldsText(screen, readyPromptPrefix); holds {
		return "", false, nil
	}
	cur, ok, err := c.getAgent(ctx, paneID)
	if err != nil {
		return marker, false, fmt.Errorf("reading the agent state to qualify a re-prompt: %w", err)
	}
	return marker, ok && cur.AgentStatus == agentStateIdle, nil
}

// recoverStalledStartup recovers a first turn herdr reported stalled: an
// explicit Enter on an agent herdr reports idle, then a confirming wait of
// startupEnterRecoveryWaitMS. When that wait runs out, the recovery is tried
// again, up to startupEnterRecoveryAttempts in all. Each retry is qualified
// like the first Enter, by the state herdr reports, and a state that confirms
// the submit ends the recovery without another keystroke. At the first check,
// before any recovery Enter, only startupFirstCheckConfirmStates end it.
//
// After each wait that runs out, rejected reports a not-ready marker the agent
// has printed since the first submit (see rejectedAsNotReady). A turn typed
// before the agent's TUI was up stalls with nothing on screen yet; the TUI
// reads it when it comes up, and a recovery Enter can submit it while the agent
// is still not ready, which consumes it. The box is empty from then on, so the
// recovery stops pressing Enter and returns that marker for the caller to
// prompt the turn again. Otherwise the marker is "" and the error is nil once
// the submit is confirmed.
//
// Each Enter is sent only when its confirming wait can run herdr's state-change
// window and still end startupDeadlineMargin before Start's deadline, and the
// wait is capped to that (startupTimeoutMS). Otherwise the recovery stops there,
// unconfirmed: a wait that outlives the ctx is killed, and Start returns too late.
func (c *client) recoverStalledStartup(ctx context.Context, paneID string, stalled error, rejected func() (string, error)) (string, error) {
	var werr, rerr error
	for attempt := 0; attempt < startupEnterRecoveryAttempts; attempt++ {
		cur, ok, gerr := c.getAgent(ctx, paneID)
		switch {
		case gerr != nil:
			return "", fmt.Errorf("startup submit not confirmed (%w) and reading the agent state to qualify recovery failed: %w", stalled, gerr)
		case !ok:
			return "", fmt.Errorf("startup submit not confirmed (%w) and the agent went away before recovery could be qualified", stalled)
		case isStartupConfirmState(cur.AgentStatus) && (attempt > 0 || confirmsAtFirstRecoveryCheck(cur.AgentStatus)):
			return "", nil
		case cur.AgentStatus != agentStateIdle:
			return "", fmt.Errorf("startup submit not confirmed (%w) and recovery Enter withheld: agent is %q, not %q — the keystroke would land in that state, not on an empty input box",
				stalled, cur.AgentStatus, agentStateIdle)
		}
		waitMS := startupTimeoutMS(ctx, startupEnterRecoveryWaitMS)
		if waitMS < herdrStateChangeWindowMS {
			werr = fmt.Errorf("recovery Enter %d of %d withheld: %w", attempt+1, startupEnterRecoveryAttempts, startupTimeShort(waitMS))
			break
		}
		if kerr := c.sendKeys(ctx, paneID, "Enter"); kerr != nil {
			return "", fmt.Errorf("startup submit not confirmed (%w) and recovery Enter failed: %w", stalled, kerr)
		}
		wait := []string{"agent", "wait", paneID}
		for _, s := range startupConfirmStates {
			wait = append(wait, "--until", s)
		}
		wait = append(wait, "--timeout", strconv.Itoa(waitMS))
		if _, werr = c.run(ctx, wait...); werr == nil {
			return "", nil
		}
		if herdrCodeAnyShape(werr) != "timeout" {
			break
		}
		var marker string
		if marker, rerr = rejected(); marker != "" {
			return marker, nil
		}
	}
	if rerr != nil {
		return "", fmt.Errorf("startup submit not confirmed after Enter recovery (%w): %w (checking for a not-ready rejection: %w)", stalled, werr, rerr)
	}
	return "", fmt.Errorf("startup submit not confirmed after Enter recovery (%w): %w", stalled, werr)
}

// confirmsAtFirstRecoveryCheck reports whether status is one of
// startupFirstCheckConfirmStates.
func confirmsAtFirstRecoveryCheck(status string) bool {
	status = normalizeAgentState(status)
	for _, s := range startupFirstCheckConfirmStates {
		if status == s {
			return true
		}
	}
	return false
}

// isStartupConfirmState reports whether status is one of startupConfirmStates.
func isStartupConfirmState(status string) bool {
	status = normalizeAgentState(status)
	for _, s := range startupConfirmStates {
		if status == s {
			return true
		}
	}
	return false
}

// pasteAndSubmit is the unregistered-pane delivery: paste, settle, submit.
func (c *client) pasteAndSubmit(ctx context.Context, paneID, text string) error {
	if err := c.paneRun(ctx, paneID, text); err != nil {
		return err
	}
	time.Sleep(c.settleDelay)
	return c.sendKeys(ctx, paneID, "Enter")
}

// targetHasNoNamedAgent reports whether a failed herdr verb failed because the
// target carries no named agent to act on: a raw `exec /bin/sh -c` pane, a bare
// shell, a pane holding only a reported agent state. Those have no prompt
// machinery, so callers degrade to typing into the pane directly, or treat an
// idle wait as nothing to wait on.
//
// The question is answered by asking herdr's agent registry, not by reading the
// failure's text. Text cannot answer it. The error this client returns renders the
// argv next to herdr's complaint, and for `agent prompt` that argv is the nudge
// body, so matching the rendered error asks what the nudge said as much as what
// herdr said: a nudge quoting herdr's own wording made deliverNudge treat an
// unrelated busy-pane rejection as agentless, swallow it, and paste into a pane
// with a foreground process. Narrowing to herdr's own stderr does not fix it,
// because herdr quotes the offending operand back on its ordinary failure paths
// (see redaction.go), and subtracting our operands from its text deletes a phrase
// herdr wrote whenever a nudge repeats it. One text stream carrying both voices
// cannot be attributed. The registry can: nothing a caller passes in changes what
// `agent list` reports.
//
// It is also the distinction itself, not a proxy for it. A kind-launched agent is
// registered from the moment it starts, so an agent that is merely still booting
// is present and this returns false, which is what keeps a startup turn from
// being typed into a TUI that is not accepting input yet. 0.7.x and 0.8.0 spell
// the rejection differently (agent_not_found became agent_not_ready plus "is not
// an active named agent"), and that respelling is exactly what silently killed
// this fallback before; the registry answer does not move when herdr renames a
// code.
//
// The registry answers what the pane carries, though, never why this call failed,
// so herdr still has to have declined over the agent: see agentStateRejection for
// why an uncoded failure is not evidence, and why a grant is read from a narrower
// channel than a refusal. When herdr names a different problem that rules the
// fallback out; see disqualifyingCode. And when herdr answers agent_not_found, it
// has answered THIS question about THIS invocation, so that is taken directly and
// no query is made. A registry lookup that itself fails rules the fallback out too,
// so the caller sees the original failure rather than a guess.
//
// The query is a present-time snapshot, which leaves a window: between the
// rejection and the lookup, a booting agent can exit (the lookup then reports an
// agentless pane and the turn is pasted into whatever is left of it) or a new one
// can register (the lookup reports an agent and a raw pane loses its fallback,
// which surfaces as an error the caller retries). The window is not closable from
// here. Answering from the failure itself would mean reading its text, and the
// text is not attributable; herdr offers no way to ask "was there an agent when
// that call failed". agent_not_found above is the one case where herdr does answer
// exactly that, which is why it bypasses the query rather than confirming it.
func (c *client) targetHasNoNamedAgent(ctx context.Context, target string, err error) bool {
	if err == nil {
		return false
	}
	if disqualifyingCode(err) != "" {
		return false
	}
	// The registry answers what the pane carries, never why this call failed.
	// Without herdr having declined over the agent, an empty registry would turn
	// any failure at all into a paste. See agentStateRejection.
	if !agentStateRejection(err) {
		return false
	}
	// agent_not_found is herdr answering the question itself, about the
	// invocation that just failed, so take it and skip the query: that answer
	// cannot be stale, and the query's answer can. It counts from either channel
	// herdr answers in, and on a non-zero exit that is the stderr one.
	if herdrAnswerCode(err) == "agent_not_found" {
		return true
	}
	agents, lerr := c.listAgents(ctx)
	if lerr != nil {
		return false
	}
	for _, a := range agents {
		if a.PaneID == target || a.Name == target {
			return false
		}
	}
	return true
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
// wherever the herdr binary itself binds the session server (herdr's
// api_socket_path_for): <config>/herdr/herdr.sock for the unnamed/default
// session, <config>/herdr/sessions/<name>/herdr.sock otherwise, with <config>
// resolved by herdrConfigDir. Any divergence makes this client dial a socket no herdr
// process ever binds, so serverAlive() reads false against a perfectly
// healthy server ("did not become ready"), and every retry launches a
// redundant herdr server contending for the same pane ("agent_pane_busy") —
// ga-nqlb8q, and on macOS #5770, where the former os.UserConfigDir()
// resolution named $HOME/Library/Application Support, a directory herdr
// never uses.
//
// herdrConfigDir supplies <config> by herdr's rule for the platform. herdr on
// Windows resolves it by other rules, which the Windows herdrConfigDir
// reproduces, and serves the path as a named pipe rather than a socket
// (herdrPipeName); herdrJoin builds the path as herdr does on each platform,
// which on Windows means without cleaning it.
func (c *client) socketPath() string {
	if c.sockPath != "" {
		return c.sockPath
	}
	configDir := herdrConfigDir()
	if c.session == "" || c.session == "default" {
		return herdrJoin(configDir, "herdr", "herdr.sock")
	}
	return herdrJoin(configDir, "herdr", "sessions", c.session, "herdr.sock")
}

// serverAlive reports whether the session-server is actually accepting
// connections on its socket. A bare os.Stat is insufficient: a herdr server
// that exits uncleanly leaves its socket inode behind — and herdr's own
// `session stop` can't remove it, since that too needs a live server to reach —
// so the stale socket answers connects with ECONNREFUSED. Presence != liveness;
// dial to find out for real. The dial is c.dialUnix, the connect the event
// stream uses, so alive means reachable by that same connect.
func (c *client) serverAlive() bool {
	alive, _ := c.probeServer()
	return alive
}

// probeServer is serverAlive that also reports a connect which reached
// something at the socket path and refused it (errEndpointRefused): the
// endpoint is held by something that is not this user's herdr, which no server
// started now could take from it.
func (c *client) probeServer() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	conn, err := c.dialUnix(ctx, c.socketPath())
	if err != nil {
		if errors.Is(err, errEndpointRefused) {
			return false, err
		}
		return false, nil
	}
	_ = conn.Close()
	return true, nil
}

// errEndpointRefused marks a connect that reached something at herdr's socket
// path and refused it, because this user's herdr could not be what serves it.
var errEndpointRefused = errors.New("refused herdr's endpoint")

// endpointRefusal marks its error as errEndpointRefused, keeping its message.
type endpointRefusal struct{ error }

func (endpointRefusal) Is(target error) bool { return target == errEndpointRefused }

func (r endpointRefusal) Unwrap() error { return r.error }

// startServer launches the headless herdr server for this session (detached)
// and waits for it to accept connections. Idempotent — no-op if already live.
// An endpoint the connect refuses fails it at once, launched or not.
//
// It refuses a relative socket path before dialing, unlinking or launching
// anything. Each process resolves such a path against its own working
// directory: the server against the city root, gc and every herdr CLI call
// against gc's ($HOME or / under the supervisor). The server would bind a
// socket gc never dials, and every retry would launch another server.
func (c *client) startServer() error {
	if sock := c.socketPath(); !filepath.IsAbs(sock) {
		return c.relativeSocketError(sock)
	}
	c.serverMu.Lock()
	defer c.serverMu.Unlock()
	alive, err := c.probeServer()
	if err != nil {
		return fmt.Errorf("herdr server for session %q: %w", c.session, err)
	}
	if alive {
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
		alive, err := c.probeServer()
		if err != nil {
			return fmt.Errorf("herdr server for session %q: %w", c.session, err)
		}
		if alive {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("herdr server for session %q did not become ready on %q", c.session, c.socketPath())
}

// relativeSocketError is startServer's refusal of the relative socket path
// sock. It names both working directories and the variables to fix.
func (c *client) relativeSocketError(sock string) error {
	gcDir, err := os.Getwd()
	if err != nil {
		gcDir = fmt.Sprintf("unknown (%v)", err)
	}
	serverDir := c.cityRoot
	if serverDir == "" {
		serverDir = gcDir // city-less construction: the server inherits gc's cwd
	}
	return fmt.Errorf("herdr server for session %q: socket path %q is relative because XDG_CONFIG_HOME, or HOME when XDG_CONFIG_HOME is unset, is set but not absolute; "+
		"herdr would resolve it against the server's working directory %q and gc against its own, %q: set XDG_CONFIG_HOME to an absolute directory, or unset it and set HOME to one",
		c.session, sock, serverDir, gcDir)
}

// stopServer stops this session's server (best-effort; tolerates not-running).
// `session stop` targets the session by name and must bypass run() (which
// prepends --session).
func (c *client) stopServer() error {
	_ = exec.Command(c.bin, "session", "stop", c.session).Run()
	return nil
}
