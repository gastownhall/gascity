package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file reads the input-outcome token's answers
// (runtime.ProtocolCapabilityInputOutcome; docs/reference/exec-session-provider.md,
// "Typed input outcomes"): the four driving ops exit 0 when their input was
// delivered, a nudge optionally refining that to delivered_unobserved with
// one stdout line, and exit runtime.ExitCodeInputOutcome with exactly one
// outcome line for every other outcome. Each word is the projection of one
// runtime sentinel (runtime.InputOutcome.Sentinel), so gc adds no meaning of
// its own here.

// InputOutcomeStdoutCap bounds the stdout, in bytes, a driving op's outcome
// line is read from. An exit 80 printing more reads as a malformed outcome.
const InputOutcomeStdoutCap = 64 << 10

// InputOutcomeDetailCap is the documented bound, in bytes, on an outcome
// line's detail; gc cuts a longer one (sanitizeDetail).
const InputOutcomeDetailCap = 1 << 10

// inputOutcomeWire is one outcome line. Unknown fields are ignored.
type inputOutcomeWire struct {
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// errMalformedOutcome marks an exit 80 gc cannot read as one valid
// non-success word allowed on the op. It wraps no runtime sentinel, so a
// caller treats it as an untyped failure (a queue charges it).
var errMalformedOutcome = errors.New("malformed input outcome")

// inputOutcomeOp reports one of the four driving ops input-outcome types.
func inputOutcomeOp(op string) bool {
	switch op {
	case "nudge", "send-keys", "interrupt", "clear-scrollback":
		return true
	default:
		return false
	}
}

// InputOutcomeAllowed reports whether op may answer word: delivered,
// not_sent, rejected, unavailable and gone on all four driving ops;
// delivered_unobserved, unconfirmed, busy and pending_interaction on nudge
// only.
func InputOutcomeAllowed(op string, word runtime.InputOutcome) bool {
	if !inputOutcomeOp(op) || !word.Known() {
		return false
	}
	switch word {
	case runtime.InputOutcomeDeliveredUnobserved, runtime.InputOutcomeUnconfirmed,
		runtime.InputOutcomeBusy, runtime.InputOutcomePendingInteraction:
		return op == "nudge"
	default:
		return true
	}
}

// ParseInputOutcome parses a driving op's stdout as exactly one outcome
// line: one JSON object naming a known word, followed by nothing but
// newlines. Trailing newlines are dropped as on exit 0 (runCapped), one rule
// for both exit codes. It returns the word and its detail.
func ParseInputOutcome(stdout string) (runtime.InputOutcome, string, error) {
	line := strings.TrimRight(stdout, "\n")
	if line == "" {
		return "", "", fmt.Errorf("%w: no outcome line", errMalformedOutcome)
	}
	if strings.Contains(line, "\n") {
		return "", "", fmt.Errorf("%w: more than one line", errMalformedOutcome)
	}
	dec := json.NewDecoder(strings.NewReader(line))
	var w inputOutcomeWire
	if err := dec.Decode(&w); err != nil {
		return "", "", fmt.Errorf("%w: %w", errMalformedOutcome, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", "", fmt.Errorf("%w: bytes after the outcome object", errMalformedOutcome)
	}
	word := runtime.InputOutcome(w.Outcome)
	if !word.Known() {
		return "", "", fmt.Errorf("%w: unknown outcome %q", errMalformedOutcome, w.Outcome)
	}
	return word, w.Detail, nil
}

// ReadExit80Outcome reads op's raw exit-80 stdout exactly as gc does: one
// outcome line naming a non-success word op may answer, within
// InputOutcomeStdoutCap bytes. Anything else (stdout past the cap, no line,
// two lines, a success word, an unknown word, a word op may not answer) is an
// error wrapping the malformed-outcome failure, which gc reads as untyped. It
// returns the word and its detail.
func ReadExit80Outcome(op, stdout string) (runtime.InputOutcome, string, error) {
	if len(stdout) > InputOutcomeStdoutCap {
		return "", "", fmt.Errorf("%w: stdout past the %d-byte cap", errMalformedOutcome, InputOutcomeStdoutCap)
	}
	word, detail, err := ParseInputOutcome(stdout)
	switch {
	case err != nil:
		return "", "", err
	case word.Success():
		return "", "", fmt.Errorf("%w: success word %q on exit %d", errMalformedOutcome, word, runtime.ExitCodeInputOutcome)
	case !InputOutcomeAllowed(op, word):
		return "", "", fmt.Errorf("%w: %s may not answer %q", errMalformedOutcome, op, word)
	}
	return word, detail, nil
}

// InputOutcomeError is the error the exec provider returns when a pack
// declaring input-outcome answers op on session name with exit code and
// stdout: nil for a delivery, runtime.ErrNudgeDeliveredUnobserved for a nudge
// answering delivered_unobserved on exit 0, the sentinel of a valid
// non-success word on exit 80, and an untyped malformed-outcome error for an
// exit 80 it cannot read. Any other exit code is an untyped failure.
func InputOutcomeError(op, name string, code int, stdout string) error {
	return inputOutcomeAnswer(op, name, code, stdout, fmt.Errorf("exec provider %s %q: exit status %d", op, name, code))
}

// inputOutcomeAnswer is InputOutcomeError over base, the op's own failure,
// which an untyped answer wraps.
func inputOutcomeAnswer(op, name string, code int, stdout string, base error) error {
	switch code {
	case 0:
		// Exit 0 is always a delivery: only delivered_unobserved on a nudge
		// refines it, and anything else printed is a contract violation gc
		// ignores.
		word, detail, err := ParseInputOutcome(stdout)
		if err == nil && word == runtime.InputOutcomeDeliveredUnobserved && InputOutcomeAllowed(op, word) {
			return outcomeError(op, name, word, detail)
		}
		return nil
	case runtime.ExitCodeInputOutcome:
		word, detail, err := ReadExit80Outcome(op, stdout)
		if err != nil {
			return fmt.Errorf("%w: %w", err, base)
		}
		return outcomeError(op, name, word, detail)
	default:
		return base
	}
}

// outcomeError is a valid word's error: its sentinel, naming the op, the
// session and the pack's detail (sanitizeDetail).
func outcomeError(op, name string, word runtime.InputOutcome, detail string) error {
	what := string(word)
	if detail = sanitizeDetail(detail); detail != "" {
		what += ": " + detail
	}
	return fmt.Errorf("exec provider %s %q: %s: %w", op, name, what, word.Sentinel())
}

// sanitizeDetail makes a pack's detail safe for errors that reach logs and
// CLI output: control characters (newlines included) become spaces, invalid
// UTF-8 is dropped, and the result is cut at InputOutcomeDetailCap bytes on a
// character boundary, then trimmed.
func sanitizeDetail(detail string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(detail, "") {
		if unicode.IsControl(r) {
			r = ' '
		}
		if b.Len()+utf8.RuneLen(r) > InputOutcomeDetailCap {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// inputOutcomesDeclared reports whether the pack's successful handshake
// declares input-outcome.
func (p *Provider) inputOutcomesDeclared() bool {
	return p.handshakeCapability(runtime.ProtocolCapabilityInputOutcome)
}

// outcomesRead reports whether the driving verbs read the pack's typed
// outcomes: input-outcome is declared and the dedicated ops carry the input,
// because carrier.ops selects them or the pack is known not to implement
// exec. The pack then answers a vanished session (gone) itself.
func (p *Provider) outcomesRead() bool {
	return p.inputOutcomesDeclared() && (p.opsCarrierSelected() || p.execUnsupported.Load())
}

// runDrivingOp runs one of the four driving ops (op is args[0], the session
// args[1]) and reads its answer. Under input-outcome the exit code is the
// outcome's class and the line its detail (inputOutcomeAnswer), and a nudge
// canceled at gc's deadline may have been typed, so it is unconfirmed.
// Otherwise the answer is today's: stdout is ignored on exit 0 and any
// failure is the op's untyped error.
func (p *Provider) runDrivingOp(ctx context.Context, stdin []byte, args ...string) error {
	out, err := p.runCapped(ctx, p.timeout, InputOutcomeStdoutCap, stdin, args...)
	var over *stdoutOverCapError
	if errors.As(err, &over) {
		// Exit 0 past the cap: still a delivery, its stdout unread.
		out, err = "", nil
	}
	if !p.inputOutcomesDeclared() {
		return err
	}
	op, name := args[0], args[1]
	if err == nil {
		return inputOutcomeAnswer(op, name, 0, out, nil)
	}
	if op == "nudge" && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
		return fmt.Errorf("%w: %w", runtime.ErrNudgeDeliveryUnconfirmed, err)
	}
	var e *opExitError
	if errors.As(err, &e) && e.code == runtime.ExitCodeInputOutcome {
		return inputOutcomeAnswer(op, name, e.code, e.stdout, err)
	}
	return err
}

// keepsOutcomeStdout reports whether a failed op's stdout is kept on its
// opExitError: only an exit 80 of a driving op, whose stdout is its outcome
// line, within the cap.
func keepsOutcomeStdout(op string, code int, stdout *cappedBuffer) string {
	if code != runtime.ExitCodeInputOutcome || !inputOutcomeOp(op) || stdout == nil || stdout.over {
		return ""
	}
	return stdout.buf.String()
}
