package runtimecontract

import (
	"context"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

// This file is the input group: the input-outcome token's typed answers to
// the four driving ops (docs/reference/exec-session-provider.md, "Typed
// input outcomes"). Every probe SKIPs unless the handshake declares
// input-outcome, and the fault probes also SKIP without conformance-faults.
// The faults are GC_RPP_FAULT values: unavailable (the backend cannot be
// reached), blocked (the box's agent waits on a pending interaction),
// lose_reply (the input is typed, then the reply is lost), hold_send (a
// nudge stays unresolved until the fault is gone) and dead (the box holds the
// name but nothing runs in it).

// The fault values the input group injects through GC_RPP_FAULT.
const (
	inputFaultUnavailable = "unavailable"
	inputFaultBlocked     = "blocked"
	inputFaultLoseReply   = "lose_reply"
	inputFaultHoldSend    = "hold_send"
	inputFaultDead        = "dead"
)

// drivingOps are the four ops input-outcome types.
var drivingOps = []string{"nudge", "send-keys", "interrupt", "clear-scrollback"}

// inputAnswer is one driving op's answer: its exit code and stdout.
type inputAnswer struct {
	op          string
	code        int
	stdout      string
	unsupported bool
}

func (a inputAnswer) String() string {
	return fmt.Sprintf("%s exit %d stdout %q", a.op, a.code, a.stdout)
}

// word is the answer's outcome word, when its stdout is exactly one valid
// outcome line.
func (a inputAnswer) word() (runtime.InputOutcome, bool) {
	w, _, err := sessionexec.ParseInputOutcome(a.stdout)
	return w, err == nil
}

// drive runs op on name under fault ("" for none): a nudge sends text on
// stdin, send-keys sends Enter.
func drive(ctx context.Context, r *runner, fault, op, name, text string) inputAnswer {
	var env []string
	if fault != "" {
		env = []string{"GC_RPP_FAULT=" + fault}
	}
	args := []string{op, name}
	var stdin []byte
	switch op {
	case "nudge":
		stdin = []byte(text)
	case "send-keys":
		args = append(args, "Enter")
	}
	res := r.opEnv(ctx, r.opts.OpTimeout, env, stdin, args...)
	a := inputAnswer{op: op, unsupported: res.unsupported}
	if res.err != nil {
		a.code, a.stdout = res.exitCode, res.failedStdout
		if a.code == 0 {
			a.code = -1 // no exit status: the op could not run
		}
		return a
	}
	a.stdout = res.stdout
	return a
}

// inputDeclared reports whether hs declares input-outcome, and the skip
// detail when it does not; faults also requires conformance-faults.
func inputDeclared(hs runtime.ProtocolInfo, faults bool) (bool, string) {
	if !hs.Has(runtime.ProtocolCapabilityInputOutcome) {
		return false, runtime.ProtocolCapabilityInputOutcome + " not declared"
	}
	if faults && !hs.Has(observeFaultsToken) {
		return false, observeFaultsToken + " not declared"
	}
	return true, ""
}

// expectWord reports a non-pass status unless a answered exit 80 with
// exactly one line naming want.
func expectWord(a inputAnswer, want runtime.InputOutcome, when string) (Status, string) {
	if a.unsupported {
		return StatusFail, a.op + " " + when + " returned exit 2 although input-outcome is declared; a declared op cannot be unknown"
	}
	if w, ok := a.word(); a.code != runtime.ExitCodeInputOutcome || !ok || w != want {
		return StatusFail, fmt.Sprintf("%s %s answered %s, want exit %d with the outcome %q", a.op, when, a, runtime.ExitCodeInputOutcome, want)
	}
	return StatusPass, ""
}

// startInputBox starts a box for an input probe and returns its name and a
// cleanup, or a non-pass status.
func startInputBox(ctx context.Context, r *runner) (string, func(), Status, string) {
	name := r.nextName()
	if status, detail := requireStart(ctx, r, name); status != StatusPass {
		return "", func() {}, status, detail
	}
	return name, func() { r.stop(ctx, name) }, StatusPass, ""
}

// typedCount reads the box's screen with no fault and counts text in it,
// the screen being where typed input shows. It answers -1 and a non-pass
// status when the screen cannot be read.
func typedCount(ctx context.Context, r *runner, name, text string) (int, Status, string) {
	res := r.peek(ctx, name, 200)
	switch {
	case res.unsupported:
		return -1, StatusSkip, "peek not implemented (exit 2): the typed input cannot be read"
	case res.err != nil:
		return -1, StatusFail, "peek after the input failed: " + res.err.Error()
	}
	return strings.Count(res.stdout, text), StatusPass, ""
}

// inputText is a probe's nudge text, unique to the box.
func inputText(name string) string { return "gc-rpp-input " + name }

// probeInputNudgeDelivered checks that a nudge to a running box exits 0 with
// no line or one success line.
func probeInputNudgeDelivered(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := inputDeclared(hs, false); !ok {
		return StatusSkip, detail
	}
	name, cleanup, status, detail := startInputBox(ctx, r)
	defer cleanup()
	if status != StatusPass {
		return status, detail
	}
	a := drive(ctx, r, "", "nudge", name, inputText(name))
	if a.unsupported || a.code != 0 {
		return StatusFail, fmt.Sprintf("nudge to a running box answered %s, want exit 0", a)
	}
	if a.stdout == "" {
		return StatusPass, "nudge to a running box exits 0"
	}
	if w, ok := a.word(); !ok || !w.Success() || !sessionexec.InputOutcomeAllowed("nudge", w) {
		return StatusFail, fmt.Sprintf("nudge to a running box exited 0 with stdout %q, want nothing or one delivered or delivered_unobserved line", a.stdout)
	}
	return StatusPass, "nudge to a running box exits 0 with a success line"
}

// probeInputNudgeAbsentGone checks that a nudge to a name no box holds is
// gone, never delivered.
func probeInputNudgeAbsentGone(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := inputDeclared(hs, false); !ok {
		return StatusSkip, detail
	}
	name := r.nextName()
	if status, detail := expectWord(drive(ctx, r, "", "nudge", name, inputText(name)), runtime.InputOutcomeGone, "to a name no box holds"); status != StatusPass {
		return status, detail
	}
	return StatusPass, "nudge to a name no box holds exits 80 gone"
}

// probeInputKeysAbsentGone checks that the three other driving ops answer a
// name no box holds with gone.
func probeInputKeysAbsentGone(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := inputDeclared(hs, false); !ok {
		return StatusSkip, detail
	}
	name := r.nextName()
	for _, op := range drivingOps[1:] {
		if status, detail := expectWord(drive(ctx, r, "", op, name, ""), runtime.InputOutcomeGone, "to a name no box holds"); status != StatusPass {
			return status, detail
		}
	}
	return StatusPass, "send-keys, interrupt and clear-scrollback to a name no box holds exit 80 gone"
}

// inputScenarioAnswers runs every driving op on a running box and on a name
// no box holds, and, with conformance-faults declared, on the running box
// under the unavailable, blocked and lose_reply faults, and returns every
// answer.
func inputScenarioAnswers(ctx context.Context, r *runner, hs runtime.ProtocolInfo) ([]inputAnswer, Status, string) {
	name, cleanup, status, detail := startInputBox(ctx, r)
	defer cleanup()
	if status != StatusPass {
		return nil, status, detail
	}
	absent := r.nextName()
	faults := []string{""}
	if hs.Has(observeFaultsToken) {
		faults = append(faults, inputFaultUnavailable, inputFaultBlocked, inputFaultLoseReply)
	}
	var answers []inputAnswer
	for _, op := range drivingOps {
		answers = append(answers, drive(ctx, r, "", op, absent, inputText(absent)))
		for _, fault := range faults {
			answers = append(answers, drive(ctx, r, fault, op, name, inputText(name)))
		}
	}
	return answers, StatusPass, ""
}

// probeInputWordsAllowedOnOp checks that every outcome word an op answers on
// exit 0 or 80 is one that op may answer.
func probeInputWordsAllowedOnOp(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := inputDeclared(hs, false); !ok {
		return StatusSkip, detail
	}
	answers, status, detail := inputScenarioAnswers(ctx, r, hs)
	if status != StatusPass {
		return status, detail
	}
	for _, a := range answers {
		if a.code != 0 && a.code != runtime.ExitCodeInputOutcome {
			continue
		}
		if w, ok := a.word(); ok && !sessionexec.InputOutcomeAllowed(a.op, w) {
			return StatusFail, fmt.Sprintf("%s answered %q, a word only nudge may answer (%s)", a.op, w, a)
		}
	}
	return StatusPass, "every op answered only its own words"
}

// probeInputBlockedNotTyped checks that a nudge to a box whose agent waits on
// a pending interaction is refused and types nothing.
func probeInputBlockedNotTyped(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := inputDeclared(hs, true); !ok {
		return StatusSkip, detail
	}
	name, cleanup, status, detail := startInputBox(ctx, r)
	defer cleanup()
	if status != StatusPass {
		return status, detail
	}
	text := inputText(name)
	if status, detail := expectWord(drive(ctx, r, inputFaultBlocked, "nudge", name, text), runtime.InputOutcomePendingInteraction, "to a blocked box"); status != StatusPass {
		return status, detail
	}
	n, status, detail := typedCount(ctx, r, name, text)
	if status != StatusPass {
		return status, detail
	}
	if n != 0 {
		return StatusFail, "a nudge refused with pending_interaction was typed into the blocked box"
	}
	return StatusPass, "a nudge to a blocked box exits 80 pending_interaction and types nothing"
}

// probeInputUnavailable checks that every driving op answers an unreachable
// backend with unavailable and types nothing.
func probeInputUnavailable(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := inputDeclared(hs, true); !ok {
		return StatusSkip, detail
	}
	name, cleanup, status, detail := startInputBox(ctx, r)
	defer cleanup()
	if status != StatusPass {
		return status, detail
	}
	text := inputText(name)
	for _, op := range drivingOps {
		if status, detail := expectWord(drive(ctx, r, inputFaultUnavailable, op, name, text), runtime.InputOutcomeUnavailable, "with the backend unavailable"); status != StatusPass {
			return status, detail
		}
	}
	n, status, detail := typedCount(ctx, r, name, text)
	if status != StatusPass {
		return status, detail
	}
	if n != 0 {
		return StatusFail, "a nudge answered unavailable was typed into the box"
	}
	return StatusPass, "every driving op exits 80 unavailable with the backend unavailable, and nothing is typed"
}

// probeInputLostReplyUnknown checks that input written before its reply was
// lost is never answered not_sent: a nudge is unconfirmed and the other ops
// exit 1 (outcome unknown).
func probeInputLostReplyUnknown(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := inputDeclared(hs, true); !ok {
		return StatusSkip, detail
	}
	name, cleanup, status, detail := startInputBox(ctx, r)
	defer cleanup()
	if status != StatusPass {
		return status, detail
	}
	if status, detail := expectWord(drive(ctx, r, inputFaultLoseReply, "nudge", name, inputText(name)), runtime.InputOutcomeUnconfirmed, "with its reply lost"); status != StatusPass {
		return status, detail
	}
	for _, op := range drivingOps[1:] {
		if a := drive(ctx, r, inputFaultLoseReply, op, name, ""); a.code != 1 {
			return StatusFail, fmt.Sprintf("%s with its reply lost answered %s, want exit 1: the outcome is unknown", op, a)
		}
	}
	return StatusPass, "a lost reply reads unconfirmed for nudge and exit 1 for the other ops"
}

// probeInputExitZeroIsDelivered checks that no exit 0 carries a non-success
// word.
func probeInputExitZeroIsDelivered(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := inputDeclared(hs, false); !ok {
		return StatusSkip, detail
	}
	answers, status, detail := inputScenarioAnswers(ctx, r, hs)
	if status != StatusPass {
		return status, detail
	}
	for _, a := range answers {
		if a.code != 0 || a.unsupported {
			continue
		}
		if w, ok := a.word(); ok && !w.Success() {
			return StatusFail, fmt.Sprintf("%s exited 0 carrying the non-success word %q: exit 0 means delivered", a.op, w)
		}
	}
	return StatusPass, "no exit 0 carries a non-success word"
}

// probeInputHeldSendTypedOnce checks that a nudge redelivered while the first
// is unresolved is unconfirmed and typed once in all.
func probeInputHeldSendTypedOnce(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := inputDeclared(hs, true); !ok {
		return StatusSkip, detail
	}
	name, cleanup, status, detail := startInputBox(ctx, r)
	defer cleanup()
	if status != StatusPass {
		return status, detail
	}
	text := inputText(name)
	if first := drive(ctx, r, inputFaultHoldSend, "nudge", name, text); first.code != 0 && first.code != runtime.ExitCodeInputOutcome {
		return StatusFail, fmt.Sprintf("a held nudge answered %s, want exit 0 or 80", first)
	}
	if status, detail := expectWord(drive(ctx, r, inputFaultHoldSend, "nudge", name, text), runtime.InputOutcomeUnconfirmed, "again while the first is held"); status != StatusPass {
		return status, detail
	}
	n, status, detail := typedCount(ctx, r, name, text)
	if status != StatusPass {
		return status, detail
	}
	if n != 1 {
		return StatusFail, fmt.Sprintf("a nudge sent twice while the first was held was typed %d times after release, want once", n)
	}
	return StatusPass, "a redelivered nudge is unconfirmed while the first is held, and typed once"
}

// probeInputExit80OneLine checks that every exit 80 carries exactly one
// valid outcome line.
func probeInputExit80OneLine(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := inputDeclared(hs, false); !ok {
		return StatusSkip, detail
	}
	answers, status, detail := inputScenarioAnswers(ctx, r, hs)
	if status != StatusPass {
		return status, detail
	}
	seen := 0
	for _, a := range answers {
		if a.code != runtime.ExitCodeInputOutcome {
			continue
		}
		seen++
		if _, _, err := sessionexec.ParseInputOutcome(a.stdout); err != nil {
			return StatusFail, fmt.Sprintf("%s exited 80 without exactly one valid outcome line (%s): %v", a.op, a, err)
		}
	}
	if seen == 0 {
		return StatusFail, "no driving op exited 80, not even for a name no box holds"
	}
	return StatusPass, fmt.Sprintf("all %d exit-80 answers carried one valid outcome line", seen)
}

// probeInputDeadBoxGone checks that a box holding the name with nothing
// running in it answers every driving op gone and types nothing: gone is "no
// running box holds the name", absent or dead, because gc reads the pack's
// gone in place of its own presence check. With observe.v1 declared, observe
// under the fault must report the box dead, so the fault is the pack's real
// dead state and not a knob only the driving ops read.
func probeInputDeadBoxGone(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := inputDeclared(hs, true); !ok {
		return StatusSkip, detail
	}
	name, cleanup, status, detail := startInputBox(ctx, r)
	defer cleanup()
	if status != StatusPass {
		return status, detail
	}
	fault := []string{"GC_RPP_FAULT=" + inputFaultDead}
	if hs.Has(runtime.ProtocolCapabilityObserveV1) {
		o, res := r.observe(ctx, fault, name)
		switch {
		case res.err != nil:
			return StatusFail, "observe under GC_RPP_FAULT=dead failed: " + res.err.Error()
		case o.State != "dead":
			return StatusFail, fmt.Sprintf("observe under GC_RPP_FAULT=dead reported state %q, want dead", o.State)
		}
	}
	text := inputText(name)
	for _, op := range drivingOps {
		if status, detail := expectWord(drive(ctx, r, inputFaultDead, op, name, text), runtime.InputOutcomeGone, "to a dead box"); status != StatusPass {
			return status, detail
		}
	}
	n, status, detail := typedCount(ctx, r, name, text)
	if status != StatusPass {
		return status, detail
	}
	if n != 0 {
		return StatusFail, "a nudge to a dead box was typed into it"
	}
	return StatusPass, "every driving op to a dead box exits 80 gone, and nothing is typed"
}
