package herdr

// The behavior of the Windows fake herdr CLIs (fakeherdr_windows_test.go),
// ported from the POSIX shell scripts each fakeHerdrSpec mode names. Only the
// Windows fakes run it.

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

// runFakeHerdr runs spec against argv, the fake's arguments, and returns its
// exit code.
func runFakeHerdr(spec fakeHerdrSpec, argv []string, stdout, stderr io.Writer) int {
	switch spec.Mode {
	case "reply":
		all := strings.Join(argv, " ")
		_, _ = io.WriteString(stdout, strings.ReplaceAll(spec.Stdout, "$*", all))
		_, _ = io.WriteString(stderr, strings.ReplaceAll(spec.Stderr, "$*", all))
		return spec.Exit
	case "start":
		return spec.start(argv, stdout)
	case "provider":
		return spec.provider(argv, stdout, stderr)
	case "registry":
		return spec.registry(argv, stdout, stderr)
	}
	_, _ = fmt.Fprintf(stderr, "fake herdr: unknown mode %q\n", spec.Mode)
	return 125
}

// shiftSession drops the leading "--session <name>", as the scripts' shift 2.
func shiftSession(argv []string) []string {
	if len(argv) < 2 {
		return nil
	}
	return argv[2:]
}

// promptLaunchPrefix is how a raw launch's statement opens on Windows: it
// clears $LASTEXITCODE, then starts the interpreter inside try.
const promptLaunchPrefix = "$global:LASTEXITCODE = $null; try { & '"

// decodePowerShellLaunch reads back a raw launch as Windows types it,
// `$global:LASTEXITCODE = $null; try { & '<interpreter>' ... -EncodedCommand
// <base64> } finally { ... }`: the interpreter, the script up to the exit
// epilogue the runtime appends (the command, after any prologue the runtime
// puts first), and the encoded argument itself. ok is false for any other
// line.
func decodePowerShellLaunch(line string) (interpreter, command, encoded string, ok bool) {
	rest, found := strings.CutPrefix(line, promptLaunchPrefix)
	if !found {
		return "", "", "", false
	}
	var path strings.Builder
	for i := 0; ; i++ {
		if i >= len(rest) {
			return "", "", "", false
		}
		if rest[i] == '\'' {
			if i+1 < len(rest) && rest[i+1] == '\'' {
				path.WriteByte('\'')
				i++
				continue
			}
			rest = rest[i+1:]
			break
		}
		path.WriteByte(rest[i])
	}
	_, encoded, found = strings.Cut(rest, " -EncodedCommand ")
	if !found {
		return "", "", "", false
	}
	if end := strings.IndexAny(encoded, "; "); end >= 0 {
		encoded = encoded[:end]
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw)%2 != 0 {
		return "", "", "", false
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	command, _, _ = strings.Cut(string(utf16.Decode(units)), "\n\n$gcShellOK = $?")
	return path.String(), command, encoded, true
}

// positional is the shell's "$n" for n = i+1: empty past the end.
func positional(argv []string, i int) string {
	if i < len(argv) {
		return argv[i]
	}
	return ""
}

func fakeFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// appendFakeFile appends text to path, as the scripts' >>.
func appendFakeFile(path, text string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(text)
	_ = f.Close()
}

// blockUntilKilled is the provider fake's hang flag: a herdr CLI that never
// answers, so its process runs until whoever started it kills it, as
// exec.CommandContext does when the ctx ends. It waits on a signal, not a
// timer: how long it runs is the caller's ctx, not the fake's.
func blockUntilKilled() {
	never := make(chan os.Signal, 1)
	signal.Notify(never, os.Interrupt)
	<-never
}

// fakeCallCount counts one more call against the flag file at path, whose
// length is the count so far, and returns the new count.
func fakeCallCount(path string) int {
	appendFakeFile(path, ".")
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return int(info.Size())
}

// takeFakeCountdown reports whether the decimal count in the file at path is
// above zero, and takes one from it when it is.
func takeFakeCountdown(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n <= 0 {
		return false
	}
	_ = os.WriteFile(path, []byte(strconv.Itoa(n-1)), 0o644)
	return true
}

// start is newFakeHerdr's script.
func (s fakeHerdrSpec) start(argv []string, stdout io.Writer) int {
	argv = shiftSession(argv)
	appendFakeFile(s.Log, strings.Join(argv, " ")+"\n")
	out := func(text string) { _, _ = io.WriteString(stdout, text) }
	switch positional(argv, 0) + " " + positional(argv, 1) {
	case "agent list":
		out(`{"result":{"agents":[]}}`)
	case "agent start":
		if fakeFileExists(s.Probe) {
			appendFakeFile(s.Log, "probe-at-agent-start: present\n")
		} else {
			appendFakeFile(s.Log, "probe-at-agent-start: absent\n")
		}
		out(`{"result":{"agent":{"name":"` + positional(argv, 2) + `","pane_id":"p1"}}}`)
	case "agent get":
		out(`{"error":{"code":"not_found","message":"no such agent"}}`)
	case "pane process-info":
		out(`{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[]}}}`)
	case "workspace list":
		out(`{"result":{"workspaces":[]}}`)
	case "workspace create":
		out(`{"result":{"workspace":{"workspace_id":"w1"},"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"stray"}}}`)
	case "tab list":
		out(`{"result":{"tabs":[]}}`)
	case "tab create":
		out(`{"result":{"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"stray"}}}`)
	default:
		out(`{"result":{}}`)
	}
	return 0
}

// provider is newFakeHerdrProviderForSession's script.
func (s fakeHerdrSpec) provider(argv []string, stdout, stderr io.Writer) int {
	argv = shiftSession(argv)
	appendFakeFile(filepath.Join(s.State, "calls.log"), strings.Join(argv, " ")+"\n")
	has := func(flag string) bool { return fakeFileExists(filepath.Join(s.State, flag)) }
	if has("hang") {
		blockUntilKilled()
	}
	touch := func(flag string) { _ = os.WriteFile(filepath.Join(s.State, flag), nil, 0o644) }
	out := func(text string) { _, _ = io.WriteString(stdout, text) }
	name := positional(argv, 2)
	agent := func(status string) string {
		return `{"result":{"agent":{"name":"` + name + `","pane_id":"%5","tab_id":"t1","workspace_id":"w1","agent_status":"` + status + `"}}}`
	}
	const (
		agentNotFound = `{"error":{"code":"agent_not_found","message":"agent target not found"}}`
		timedOut      = `{"error":{"code":"timeout","message":"timed out waiting for agent status"}}`
	)
	// fail answers the way herdr 0.9.1 reports a CLI error (measured on
	// Windows): a non-zero exit, nothing on stdout, and one error envelope as the
	// whole of stderr.
	fail := func(id, code, message string) int {
		_, _ = io.WriteString(stderr, `{"id":"`+id+`","error":{"code":"`+code+`","message":"`+message+`"}}`+"\n")
		return 1
	}
	switch positional(argv, 0) + "_" + positional(argv, 1) {
	case "agent_get":
		if data, err := os.ReadFile(filepath.Join(s.State, "rv10-get-"+name+".json")); err == nil {
			out(string(data))
			break
		}
		switch {
		case !has("registered"):
			out(agentNotFound)
		case has("agent_blocked"):
			out(agent("blocked"))
		case has("agent_working"):
			out(agent("working"))
		case has("agent_done"):
			out(agent("done"))
		case has("enter_late") && has("submitted"):
			out(agent("working"))
		default:
			out(agent("idle"))
		}
	case "agent_list":
		for _, file := range []string{"agent_list_json", "rv10-list.json"} {
			if data, err := os.ReadFile(filepath.Join(s.State, file)); err == nil {
				out(string(data))
				return 0
			}
		}
		if has("auto_copilot_agent") {
			out(`{"result":{"agents":[{"agent":"copilot","pane_id":"%5","tab_id":"t1","workspace_id":"w1","agent_status":"idle"}]}}`)
			break
		}
		out(`{"result":{"agents":[]}}`)
	case "agent_start":
		switch {
		case has("name_taken"):
			touch("registered")
			touch("busy")
			out(`{"error":{"code":"agent_name_taken","message":"agent name already registered"}}`)
		case has("name_taken_exit"):
			touch("registered")
			touch("busy")
			return fail("cli:agent:start:pane", "agent_name_taken", "agent name already registered")
		case has("pane_busy_once"):
			_ = os.Remove(filepath.Join(s.State, "pane_busy_once"))
			_, _ = io.WriteString(stderr, `{"error":{"code":"agent_pane_busy","message":"agent target pane %5 is not an available shell"}}`+"\n")
			return 1
		default:
			touch("agent_started")
			touch("registered")
			if fakeFileExists(filepath.Join(s.MetaDir, name, "GC_SESSION_ID")) {
				touch("meta_seeded_before_launch")
			}
			if fakeFileExists(filepath.Join(s.MetaDir, name, "GC_HERDR_PANE_ID")) {
				touch("bound_before_launch")
			}
			out(agent("idle"))
		}
	case "agent_wait":
		switch {
		case !has("registered"):
			out(agentNotFound)
		case has("wait_times_out"):
			out(timedOut)
		case has("enter_rejects_not_ready") && !strings.Contains(strings.Join(argv, " "), "--until idle"):
			// The agent is idle from launch on and a rejected submit leaves it
			// idle: only the readiness wait settles.
			out(timedOut)
		case has("marker_above_held_turn") && !strings.Contains(strings.Join(argv, " "), "--until idle"):
			// The turn never leaves the box: only the readiness wait settles.
			out(timedOut)
		case has("wait_times_out_exit"):
			return fail("cli:agent:wait", "timeout", "timed out waiting for agent status")
		case has("enter_late") && !has("submitted"):
			return fail("cli:agent:wait", "timeout", "timed out waiting for agent status")
		case has("enter_late"):
			out(agent("working"))
		default:
			out(`{"result":{"agent":{"name":"` + name + `","agent_status":"idle"}}}`)
		}
	case "agent_prompt":
		switch {
		case !has("registered"):
			out(agentNotFound)
		case has("enter_rejects_not_ready") && !has("rejected_on_enter"):
			// Typed before the agent's TUI was up: nothing on screen changes.
			out(`{"error":{"code":"agent_prompt_stalled","message":"no state change observed after submission"}}`)
		case has("marker_above_held_turn"):
			// The TUI comes up drawing a not-ready line from its resumed
			// history above a composer that still holds the typed turn, whose
			// CR and Enters it lost: no rejection, the turn is in the box. The
			// file says when: 0 at the first prompt's stall, N at the Nth Enter.
			_ = os.WriteFile(filepath.Join(s.State, "typed"), []byte(positional(argv, 3)), 0o644)
			if at, _ := os.ReadFile(filepath.Join(s.State, "marker_above_held_turn")); strings.TrimSpace(string(at)) == "0" && !has("held_turn_drawn") {
				appendFakeFile(filepath.Join(s.State, "screen"), fakeNotReadyLine+"\n"+fakeComposer+positional(argv, 3)+"\n")
				touch("held_turn_drawn")
			}
			out(`{"error":{"code":"agent_prompt_stalled","message":"no state change observed after submission"}}`)
		case has("prompt_not_ready_once"):
			_ = os.Remove(filepath.Join(s.State, "prompt_not_ready_once"))
			appendFakeFile(filepath.Join(s.State, "screen"), fakeRejectedScreen)
			out(`{"error":{"code":"agent_prompt_stalled","message":"no state change observed after submission"}}`)
		case has("prompt_not_ready_always"):
			appendFakeFile(filepath.Join(s.State, "screen"), fakeRejectedScreen)
			out(`{"error":{"code":"agent_prompt_stalled","message":"no state change observed after submission"}}`)
		case takeFakeCountdown(filepath.Join(s.State, "prompt_not_ready_times")):
			appendFakeFile(filepath.Join(s.State, "screen"), fakeRejectedScreen)
			out(`{"error":{"code":"agent_prompt_stalled","message":"no state change observed after submission"}}`)
		case has("prompt_stalled"):
			out(`{"error":{"code":"agent_prompt_stalled","message":"no state change observed after submission"}}`)
		case has("prompt_stalled_echo"):
			appendFakeFile(filepath.Join(s.State, "screen"), positional(argv, 3)+"\n")
			out(`{"error":{"code":"agent_prompt_stalled","message":"no state change observed after submission"}}`)
		case has("prompt_stalled_exit"):
			return fail("cli:agent:prompt", "agent_prompt_stalled", "agent prompt produced no observed working or blocked state within 5000 ms; current status is idle")
		case has("prompt_times_out"):
			out(timedOut)
		case has("prompt_blocked"):
			if strings.Contains(strings.Join(argv, " "), "--until blocked") {
				touch("prompted")
				out(`{"result":{"agent":{"name":"` + name + `","agent_status":"blocked"}}}`)
			} else {
				out(timedOut)
			}
		default:
			touch("prompted")
			out(`{"result":{"type":"agent_prompted"}}`)
		}
	case "pane_read":
		if data, err := os.ReadFile(filepath.Join(s.State, "screen")); err == nil {
			out(string(data))
			break
		}
		out(fakeIdleScreen)
	case "pane_send-keys":
		// enter_rejects_not_ready: the TUI came up holding the typed-ahead
		// turn, and the Enter numbered in the file submits it while the agent
		// is not ready, which consumes it and prints the not-ready line.
		if at, err := os.ReadFile(filepath.Join(s.State, "enter_rejects_not_ready")); err == nil &&
			strconv.Itoa(fakeCallCount(filepath.Join(s.State, "enters"))) == strings.TrimSpace(string(at)) {
			appendFakeFile(filepath.Join(s.State, "screen"), fakeRejectedScreen)
			touch("rejected_on_enter")
		}
		// marker_above_held_turn: see agent_prompt; the Enter is lost.
		if at, err := os.ReadFile(filepath.Join(s.State, "marker_above_held_turn")); err == nil &&
			strconv.Itoa(fakeCallCount(filepath.Join(s.State, "enters"))) == strings.TrimSpace(string(at)) {
			typed, _ := os.ReadFile(filepath.Join(s.State, "typed"))
			appendFakeFile(filepath.Join(s.State, "screen"), fakeNotReadyLine+"\n"+fakeComposer+string(typed)+"\n")
		}
		// enter_late: a TUI that loses keystrokes while it boots takes the
		// third Enter.
		if has("enter_late") && fakeCallCount(filepath.Join(s.State, "enter_late")) >= 3 {
			touch("submitted")
		}
	case "pane_run":
		touch("busy")
		if interpreter, command, encoded, ok := decodePowerShellLaunch(positional(argv, 3)); ok {
			_ = os.WriteFile(filepath.Join(s.State, "rawinterp"), []byte(interpreter), 0o644)
			_ = os.WriteFile(filepath.Join(s.State, "rawcmd"), []byte(command), 0o644)
			_ = os.WriteFile(filepath.Join(s.State, "rawtoken"), []byte(encoded), 0o644)
			break
		}
		// sed -e 's|^exec /bin/sh -c ||' -e "s/^'//" -e "s/'\$//", line by line.
		lines := strings.Split(positional(argv, 3), "\n")
		for i, line := range lines {
			line = strings.TrimPrefix(line, "exec /bin/sh -c ")
			line = strings.TrimPrefix(line, "'")
			lines[i] = strings.TrimSuffix(line, "'")
		}
		_ = os.WriteFile(filepath.Join(s.State, "rawcmd"), []byte(strings.Join(lines, "\n")), 0o644)
	case "pane_process-info":
		// herdr on Windows lists only the pane's shell here, whatever runs in
		// the pane (measured, 0.9.1); what runs is the shell's children, which
		// the fake reports through shellChildren.
		switch {
		case has("pane_gone"):
			out(`{"error":{"code":"pane_not_found","message":"pane not found"}}`)
		case has("processinfo_flaky") && fakeCallCount(filepath.Join(s.State, "processinfo_flaky")) <= 2:
			// A read that fails without saying anything about the pane.
			out(`{"error":{"code":"internal_error","message":"server busy, try again"}}`)
		case has("shell_cmd"):
			out(`{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4242,"name":"cmd.exe"}]}}}`)
		case has("shell_late") && fakeCallCount(filepath.Join(s.State, "shell_late")) <= 2:
			// A fresh pane whose shell is still starting: its PID, not yet listed.
			out(`{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[]}}}`)
		default:
			out(`{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4242,"name":"powershell.exe"}]}}}`)
		}
	case "workspace_list":
		if s.WorkspaceExists {
			out(`{"result":{"workspaces":[{"workspace_id":"w1","label":"gastown"}]}}`)
			break
		}
		touch("placement_attempted")
		out(`{"result":{"workspaces":[]}}`)
	case "workspace_create":
		out(`{"result":{"workspace":{"workspace_id":"w1"},"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"%5"}}}`)
	case "tab_list":
		if has("stale_tabs") {
			out(`{"result":{"tabs":[{"tab_id":"t-old1","label":"witness"},{"tab_id":"t-old2","label":"witness"},{"tab_id":"t-other","label":"deacon"}]}}`)
		} else {
			out(`{"result":{"tabs":[]}}`)
		}
	case "tab_create":
		out(`{"result":{"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"%5"}}}`)
	// The script answers neither of these verbs: only Windows code calls them.
	case "pane_get":
		if has("pane_no_tab") {
			out(`{"result":{"pane":{"pane_id":"` + name + `","workspace_id":"w1"}}}`)
			break
		}
		out(`{"result":{"pane":{"pane_id":"` + name + `","tab_id":"t1","workspace_id":"w1"}}}`)
	case "tab_focus":
		out(`{"result":{"tab":{"tab_id":"` + name + `","focused":true}}}`)
	}
	return 0
}

// registry is registryFake's script.
func (s fakeHerdrSpec) registry(argv []string, stdout, stderr io.Writer) int {
	var trace strings.Builder
	for _, arg := range argv {
		trace.WriteString(arg + "\n")
	}
	trace.WriteString("---\n")
	appendFakeFile(s.Trace, trace.String())

	all := strings.Join(argv, " ")
	switch {
	case strings.Contains(all, "agent list"):
		_, _ = io.WriteString(stdout, s.List+"\n")
	case strings.Contains(all, "agent prompt"), strings.Contains(all, "agent wait"):
		switch {
		case s.Rejection == "!!echo-argv":
			_, _ = io.WriteString(stderr, "unexpected arguments:\n")
			for _, arg := range argv {
				_, _ = io.WriteString(stderr, arg+"\n")
			}
			return 1
		case strings.HasPrefix(s.Rejection, "!"):
			_, _ = io.WriteString(stderr, s.Rejection[1:]+"\n")
			return 1
		default:
			_, _ = io.WriteString(stdout, s.Rejection+"\n")
		}
	}
	return 0
}
