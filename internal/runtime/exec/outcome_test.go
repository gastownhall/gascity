package exec

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/runtime"
)

// outcomeCaps declares carrier.ops, so the four driving ops run as the
// pack's own ops, with input-outcome.
const outcomeCaps = `"carrier.ops","input-outcome"`

// drivingCalls are the four driving ops through the public Provider methods.
var drivingCalls = []struct {
	op   string
	call func(runtime.Provider) error
}{
	{"nudge", func(p runtime.Provider) error { return p.Nudge("s", runtime.TextContent("hello")) }},
	{"send-keys", func(p runtime.Provider) error { return p.SendKeys("s", "Enter") }},
	{"interrupt", func(p runtime.Provider) error { return p.Interrupt("s") }},
	{"clear-scrollback", func(p runtime.Provider) error { return p.ClearScrollback("s") }},
}

// answer makes every driving op of pk print stdout and exit code, after
// reading its stdin.
func (pk opsPack) answer(t *testing.T, stdout string, code int) {
	t.Helper()
	body := fmt.Sprintf("cat >/dev/null; printf '%%s' %s; echo 'pack stderr' >&2; exit %d\n", shellQuote([]string{stdout}), code)
	for _, d := range drivingCalls {
		pk.put(t, d.op+".body", body)
	}
}

// outcomeSentinels are every sentinel a word names, so a test can assert an
// error wraps none of them.
func outcomeSentinels() []error {
	var out []error
	for _, w := range runtime.InputOutcomes() {
		if s := w.Sentinel(); s != nil {
			out = append(out, s)
		}
	}
	return out
}

func wrapsAnySentinel(err error) error {
	for _, s := range outcomeSentinels() {
		if errors.Is(err, s) {
			return s
		}
	}
	return nil
}

// TestInputOutcomeExit80LineReachesTheOpHelpers pins the path from the
// pack's exit 80 to the four op helpers: runCapped keeps the outcome line on
// opExitError only for an exit 80 of a driving op, and each helper turns a
// valid line into an error wrapping its word's sentinel, naming the op, the
// session and the detail. Every other failure discards stdout as before.
func TestInputOutcomeExit80LineReachesTheOpHelpers(t *testing.T) {
	allowed := map[string][]runtime.InputOutcome{
		"nudge":            {runtime.InputOutcomeUnconfirmed, runtime.InputOutcomeNotSent, runtime.InputOutcomeRejected, runtime.InputOutcomeBusy, runtime.InputOutcomePendingInteraction, runtime.InputOutcomeUnavailable, runtime.InputOutcomeGone},
		"send-keys":        {runtime.InputOutcomeNotSent, runtime.InputOutcomeRejected, runtime.InputOutcomeUnavailable, runtime.InputOutcomeGone},
		"interrupt":        {runtime.InputOutcomeNotSent, runtime.InputOutcomeRejected, runtime.InputOutcomeUnavailable, runtime.InputOutcomeGone},
		"clear-scrollback": {runtime.InputOutcomeNotSent, runtime.InputOutcomeRejected, runtime.InputOutcomeUnavailable, runtime.InputOutcomeGone},
	}
	for _, d := range drivingCalls {
		for _, word := range allowed[d.op] {
			t.Run(d.op+"/"+string(word), func(t *testing.T) {
				pk := newOpsPack(t, outcomeCaps)
				pk.answer(t, `{"outcome":"`+string(word)+`","detail":"the pack's reason"}`+"\n", runtime.ExitCodeInputOutcome)
				err := d.call(NewProvider(pk.script))
				if !errors.Is(err, word.Sentinel()) {
					t.Fatalf("%s answering %s = %v, want an error wrapping %v", d.op, word, err, word.Sentinel())
				}
				for _, part := range []string{d.op, `"s"`, string(word), "the pack's reason"} {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("error %q does not name %q", err, part)
					}
				}
			})
		}
	}

	t.Run("stdout kept only on exit 80 of a driving op", func(t *testing.T) {
		pk := newOpsPack(t, outcomeCaps)
		line := `{"outcome":"gone"}`
		pk.put(t, "nudge.body", "cat >/dev/null; echo '"+line+"'; exit 80\n")
		pk.put(t, "get-meta.body", "echo '"+line+"'; exit 80\n")
		pk.put(t, "send-keys.body", "echo '"+line+"'; exit 1\n")
		p := NewProvider(pk.script)
		cases := []struct {
			args []string
			want string
		}{
			{[]string{"nudge", "s"}, line + "\n"},
			{[]string{"get-meta", "s", "k"}, ""},
			{[]string{"send-keys", "s", "Enter"}, ""},
		}
		for _, tc := range cases {
			_, err := p.runCapped(context.Background(), p.timeout, InputOutcomeStdoutCap, []byte("x"), tc.args...)
			var e *opExitError
			if !errors.As(err, &e) {
				t.Fatalf("%v: err = %v, want an opExitError", tc.args, err)
			}
			if e.stdout != tc.want {
				t.Errorf("%v exit %d kept stdout %q, want %q", tc.args, e.code, e.stdout, tc.want)
			}
		}
	})
}

// TestInputOutcomeUndeclaredKeepsTodaysSemantics pins compatibility: for a
// pack that does not declare input-outcome, stdout on exit 0 is ignored and
// exit 80 is the same untyped error an exit 1 gives, stderr included.
func TestInputOutcomeUndeclaredKeepsTodaysSemantics(t *testing.T) {
	for _, d := range drivingCalls {
		t.Run(d.op, func(t *testing.T) {
			pk := newOpsPack(t, `"carrier.ops"`)
			p := NewProvider(pk.script)
			for _, stdout := range []string{`{"outcome":"not_sent"}`, `{"outcome":"delivered_unobserved"}`, "anything at all", strings.Repeat("x", 2*InputOutcomeStdoutCap)} {
				pk.answer(t, stdout, 0)
				if err := d.call(p); err != nil {
					t.Errorf("exit 0 printing %.40q = %v, want nil (stdout ignored)", stdout, err)
				}
			}
			pk.answer(t, `{"outcome":"busy","detail":"d"}`, runtime.ExitCodeInputOutcome)
			err80 := d.call(p)
			pk.answer(t, `{"outcome":"busy","detail":"d"}`, 1)
			err1 := d.call(p)
			if err80 == nil || err1 == nil {
				t.Fatalf("exit 80 = %v, exit 1 = %v; want both failures", err80, err1)
			}
			if err80.Error() != err1.Error() {
				t.Errorf("exit 80 = %q, exit 1 = %q; want the same untyped error", err80, err1)
			}
			if !strings.Contains(err80.Error(), "pack stderr") {
				t.Errorf("exit 80 = %q, want the pack's stderr", err80)
			}
			if s := wrapsAnySentinel(err80); s != nil {
				t.Errorf("undeclared exit 80 = %v wraps %v, want untyped", err80, s)
			}
		})
	}
}

// TestInputOutcomeExitZeroIsAlwaysSuccess pins that exit 0 means delivered
// whatever it prints: no line or delivered is nil, delivered_unobserved on a
// nudge is runtime.ErrNudgeDeliveredUnobserved, and anything else (a
// non-success word, a word the op may not use, garbage, two lines, output
// past the cap) is a contract violation gc ignores.
func TestInputOutcomeExitZeroIsAlwaysSuccess(t *testing.T) {
	ignored := []string{
		"",
		`{"outcome":"delivered"}`,
		`{"outcome":"delivered","detail":"typed"}` + "\n",
		`{"outcome":"not_sent"}`,
		`{"outcome":"gone"}`,
		"not json",
		`{"outcome":"delivered"}` + "\n" + `{"outcome":"delivered"}`,
		strings.Repeat("x", 2*InputOutcomeStdoutCap),
	}
	for _, d := range drivingCalls {
		t.Run(d.op, func(t *testing.T) {
			pk := newOpsPack(t, outcomeCaps)
			p := NewProvider(pk.script)
			for _, stdout := range ignored {
				pk.answer(t, stdout, 0)
				if err := d.call(p); err != nil {
					t.Errorf("exit 0 printing %.40q = %v, want nil", stdout, err)
				}
			}
			pk.answer(t, `{"outcome":"delivered_unobserved","detail":"no turn start seen"}`, 0)
			err := d.call(p)
			if d.op == "nudge" {
				if !errors.Is(err, runtime.ErrNudgeDeliveredUnobserved) || !runtime.NudgeDelivered(err) {
					t.Errorf("nudge exit 0 delivered_unobserved = %v, want ErrNudgeDeliveredUnobserved", err)
				}
				return
			}
			if err != nil {
				t.Errorf("%s exit 0 delivered_unobserved = %v, want nil (the word is nudge's only)", d.op, err)
			}
		})
	}
}

// TestReadExit80OutcomeAppliesTheStdoutCap pins that the exported reader,
// which the conformance runner and rppcheck read exit 80 through, applies
// gc's stdout cap: a valid line padded past InputOutcomeStdoutCap with
// trailing newlines is malformed, as gc never reads stdout past the cap,
// while the same line within the cap is read.
func TestReadExit80OutcomeAppliesTheStdoutCap(t *testing.T) {
	line := `{"outcome":"gone"}`
	if _, _, err := ReadExit80Outcome("nudge", line+strings.Repeat("\n", InputOutcomeStdoutCap-len(line))); err != nil {
		t.Errorf("a gone line at the cap = %v, want gone", err)
	}
	_, _, err := ReadExit80Outcome("nudge", line+strings.Repeat("\n", InputOutcomeStdoutCap))
	if !errors.Is(err, errMalformedOutcome) {
		t.Errorf("a gone line padded past the cap = %v, want errMalformedOutcome", err)
	}
}

// TestInputOutcomeMalformedExit80IsUntyped pins that an exit 80 gc cannot
// read as one valid non-success word allowed on the op is an untyped
// failure: it wraps no sentinel, so a queue charges it, and it never reads
// as delivered.
func TestInputOutcomeMalformedExit80IsUntyped(t *testing.T) {
	malformed := map[string]string{
		"no line":           "",
		"two lines":         `{"outcome":"gone"}` + "\n" + `{"outcome":"gone"}`,
		"a success word":    `{"outcome":"delivered"}`,
		"unobserved":        `{"outcome":"delivered_unobserved"}`,
		"an unknown word":   `{"outcome":"ok"}`,
		"not json":          "gone",
		"trailing bytes":    `{"outcome":"gone"} {}`,
		"stdout over a cap": `{"outcome":"gone","detail":"` + strings.Repeat("x", InputOutcomeStdoutCap) + `"}`,
		"newlines past cap": `{"outcome":"gone"}` + strings.Repeat("\n", InputOutcomeStdoutCap),
	}
	for _, d := range drivingCalls {
		for name, stdout := range malformed {
			t.Run(d.op+"/"+name, func(t *testing.T) {
				pk := newOpsPack(t, outcomeCaps)
				pk.answer(t, stdout, runtime.ExitCodeInputOutcome)
				err := d.call(NewProvider(pk.script))
				if err == nil {
					t.Fatal("malformed exit 80 = nil, want a failure")
				}
				if s := wrapsAnySentinel(err); s != nil {
					t.Errorf("malformed exit 80 = %v wraps %v, want untyped", err, s)
				}
				if !errors.Is(err, errMalformedOutcome) {
					t.Errorf("malformed exit 80 = %v, want errMalformedOutcome", err)
				}
			})
		}
	}
	for _, op := range []string{"send-keys", "interrupt", "clear-scrollback"} {
		for _, word := range []runtime.InputOutcome{runtime.InputOutcomeBusy, runtime.InputOutcomePendingInteraction, runtime.InputOutcomeUnconfirmed} {
			t.Run(op+"/disallowed "+string(word), func(t *testing.T) {
				pk := newOpsPack(t, outcomeCaps)
				pk.answer(t, `{"outcome":"`+string(word)+`"}`, runtime.ExitCodeInputOutcome)
				var call func(runtime.Provider) error
				for _, d := range drivingCalls {
					if d.op == op {
						call = d.call
					}
				}
				err := call(NewProvider(pk.script))
				if err == nil || wrapsAnySentinel(err) != nil || !errors.Is(err, errMalformedOutcome) {
					t.Errorf("%s answering %s = %v, want an untyped malformed outcome", op, word, err)
				}
			})
		}
	}
}

// TestInputOutcomeCanceledNudgeIsUnconfirmed pins that a nudge canceled at
// gc's deadline under input-outcome may have been typed, so it is
// runtime.ErrNudgeDeliveryUnconfirmed; other ops, and an undeclared pack,
// keep the plain cancellation error.
func TestInputOutcomeCanceledNudgeIsUnconfirmed(t *testing.T) {
	run := func(t *testing.T, caps, op string) error {
		pk := newOpsPack(t, caps)
		pk.put(t, op+".body", "cat >/dev/null; sleep 5\n")
		p := NewProvider(pk.script)
		if _, err := p.Protocol(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		c := opsCarrier{p}
		if op == "nudge" {
			return c.Nudge(ctx, "s", runtime.TextContent("hello"))
		}
		return c.SendKeys(ctx, "s", "Enter")
	}
	err := run(t, outcomeCaps, "nudge")
	if !errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("declared nudge past its deadline = %v, want ErrNudgeDeliveryUnconfirmed wrapping the deadline", err)
	}
	if err := run(t, `"carrier.ops"`, "nudge"); err == nil || errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed) {
		t.Errorf("undeclared nudge past its deadline = %v, want the plain cancellation error", err)
	}
	if err := run(t, outcomeCaps, "send-keys"); err == nil || errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed) {
		t.Errorf("declared send-keys past its deadline = %v, want the plain cancellation error", err)
	}
}

// TestInputOutcomeSkipsPresenceCheckWhenRead pins that the seam-backed
// driving verbs skip the presence pre-check when the pack's outcomes are
// read (input-outcome on the dedicated ops), so a driving call costs one
// spawn and the pack answers a vanished session itself; without the token
// the pre-check stays.
func TestInputOutcomeSkipsPresenceCheckWhenRead(t *testing.T) {
	ops := func(log string) []string {
		var out []string
		for _, line := range strings.Split(log, "\n") {
			if f := strings.Fields(strings.TrimPrefix(line, "OP ")); len(f) > 0 && strings.HasPrefix(line, "OP ") {
				out = append(out, f[0])
			}
		}
		return out
	}
	for _, d := range drivingCalls {
		t.Run(d.op, func(t *testing.T) {
			pk := newOpsPack(t, outcomeCaps)
			sp := NewSeamBacked(pk.script)
			if _, err := sp.(*seamBackedProvider).raw.Protocol(); err != nil {
				t.Fatal(err)
			}
			pk.put(t, "log", "")
			if err := d.call(sp); err != nil {
				t.Fatalf("%s = %v", d.op, err)
			}
			if got := ops(pk.log(t)); !slices.Equal(got, []string{d.op}) {
				t.Errorf("%s spawned %v, want only the op", d.op, got)
			}
			pk.answer(t, `{"outcome":"gone"}`, runtime.ExitCodeInputOutcome)
			if err := d.call(sp); !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Errorf("%s answered gone = %v, want ErrSessionNotFound", d.op, err)
			}
		})
	}

	t.Run("undeclared keeps the pre-check", func(t *testing.T) {
		pk := newOpsPack(t, `"carrier.ops"`)
		sp := NewSeamBacked(pk.script)
		if _, err := sp.(*seamBackedProvider).raw.Protocol(); err != nil {
			t.Fatal(err)
		}
		pk.put(t, "log", "")
		if err := sp.Nudge("s", runtime.TextContent("hello")); err != nil {
			t.Fatal(err)
		}
		if got := ops(pk.log(t)); !slices.Equal(got, []string{"is-running", "nudge"}) {
			t.Errorf("Nudge spawned %v, want the is-running pre-check then nudge", got)
		}
	})

	t.Run("fallback ops are read once exec is known unsupported", func(t *testing.T) {
		pk := newOpsPack(t, `"input-outcome"`)
		pk.put(t, "exec.code", "2")
		sp := NewSeamBacked(pk.script)
		if _, err := sp.(*seamBackedProvider).raw.Protocol(); err != nil {
			t.Fatal(err)
		}
		if err := sp.Nudge("s", runtime.TextContent("warm")); err != nil {
			t.Fatal(err)
		}
		pk.put(t, "log", "")
		if err := sp.Nudge("s", runtime.TextContent("hello")); err != nil {
			t.Fatal(err)
		}
		if got := ops(pk.log(t)); !slices.Equal(got, []string{"nudge"}) {
			t.Errorf("Nudge spawned %v, want only nudge once exec is known unsupported", got)
		}
	})
}

// TestInputOutcomeTrailingNewlinesAreOneRule pins one reading of trailing
// newlines for both exit codes, the same one conformance and gc runtime
// check apply: exit 0 already drops every trailing newline, so an exit 80
// outcome line followed by blank lines is that line, not "more than one
// line" (which would charge a gone, busy or unavailable as untyped).
func TestInputOutcomeTrailingNewlinesAreOneRule(t *testing.T) {
	for _, d := range drivingCalls {
		t.Run(d.op, func(t *testing.T) {
			pk := newOpsPack(t, outcomeCaps)
			pk.answer(t, `{"outcome":"gone"}`+"\n\n\n", runtime.ExitCodeInputOutcome)
			if err := d.call(NewProvider(pk.script)); !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("%s exit 80 gone with trailing blank lines = %v, want runtime.ErrSessionNotFound", d.op, err)
			}
		})
	}
	t.Run("nudge exit 0", func(t *testing.T) {
		pk := newOpsPack(t, outcomeCaps)
		pk.answer(t, `{"outcome":"delivered_unobserved"}`+"\n\n\n", 0)
		if err := NewProvider(pk.script).Nudge("s", runtime.TextContent("hello")); !errors.Is(err, runtime.ErrNudgeDeliveredUnobserved) {
			t.Fatalf("nudge exit 0 delivered_unobserved with trailing blank lines = %v, want runtime.ErrNudgeDeliveredUnobserved", err)
		}
	})
}

// TestInputOutcomeDetailIsSanitized pins the pack-controlled detail an
// outcome's error carries: cut at the cap on a character boundary, never
// mid-rune, and stripped of control characters (newlines included), so it
// cannot forge log lines or split a UTF-8 character.
func TestInputOutcomeDetailIsSanitized(t *testing.T) {
	long := strings.Repeat("a", InputOutcomeDetailCap-1) + "é and more"
	for name, detail := range map[string]string{
		"control characters": "line one\nforged: line\r\x1b[31mred\x00",
		"cut mid-rune":       long,
	} {
		t.Run(name, func(t *testing.T) {
			err := outcomeError("nudge", "s", runtime.InputOutcomeGone, detail)
			msg := err.Error()
			if !utf8.ValidString(msg) {
				t.Errorf("error %q is not valid UTF-8", msg)
			}
			for _, r := range msg {
				if unicode.IsControl(r) {
					t.Errorf("error %q carries control character %U", msg, r)
				}
			}
			if !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Errorf("error %v does not wrap the gone sentinel", err)
			}
		})
	}
}
