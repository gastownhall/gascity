// Package emit is the scripted fake agent program behind the osc7501emitter
// test binary: it writes OSC 7501 Program Status Protocol sequences (spec
// rev 0.4) to its terminal as a line script directs, so conformance suites
// can drive a capture path with every shape a real agent emits (passive ST
// reports, probe-gated BEL reports, env-forced reports, child records, every
// blocked kind, invalid and oversized reports, split writes, clear on exit,
// stale working with no heartbeat) without running a real agent.
//
// A script is one step per line; blank lines and lines starting with # are
// skipped:
//
//	report <body>       write ESC ] 7501 ; <body> and the terminator
//	oversize <n>        write a well-formed working report n bytes long in all
//	clear               write the root clear report (state=clear)
//	raw <hex>           write the hex-decoded bytes verbatim, never gated
//	split <n>           write the next sequence in n chunks, one cut inside an ST
//	term bel|st         terminate later sequences with BEL or ESC \ (default st)
//	sleep <duration>    pause (time.ParseDuration syntax)
//	probe-gated [d]     send the 7501 probe then DA1; report only if a 7501
//	                    reply arrives before the DA1 reply (within d, default 1s)
//	force-env <NAME>    report without probing exactly when NAME is set
//	on-line <path>      run the script at path once per typed input line
//	interactive         run each typed input line as a step
//	exit <code>         end with exit status code
//	signal <name>       end by signal name (TERM, INT, HUP, KILL)
//	hang                stop reading and writing until canceled
//
// Reports are gated: probe-gated and force-env can silence the program, after
// which report, oversize and clear write nothing. raw is never gated.
//
// Terminal replies never reach on-line or interactive as typed text:
// probe-gated reads its handshake's replies off the input, keeping the bytes
// typed around them, and a reply that arrives later is dropped.
package emit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The terminal sequences the program writes and reads.
const (
	oscIntro      = "\x1b]7501;"
	terminatorBEL = "\x07"
	terminatorST  = "\x1b\\"
	probeBody     = "?"
	da1Request    = "\x1b[c"
	// defaultProbeWait bounds probe-gated's wait for a reply.
	defaultProbeWait = time.Second
	// maxScriptLine bounds one script line (an oversized report's body).
	maxScriptLine = 1 << 20
)

// ErrScript wraps every reason a script does not parse.
var ErrScript = errors.New("emit: invalid script")

// op names a step's action.
type op string

// The steps a script may hold.
const (
	opReport      op = "report"
	opOversize    op = "oversize"
	opClear       op = "clear"
	opRaw         op = "raw"
	opSplit       op = "split"
	opTerm        op = "term"
	opSleep       op = "sleep"
	opProbeGated  op = "probe-gated"
	opForceEnv    op = "force-env"
	opOnLine      op = "on-line"
	opInteractive op = "interactive"
	opExit        op = "exit"
	opSignal      op = "signal"
	opHang        op = "hang"
)

// signals are the signals a script may end by: those a Go process dies by
// once it resets their handling. Go dumps and exits 2 on QUIT and ignores
// USR1 and USR2, so a pane would never read them as its death signal.
var signals = map[string]syscall.Signal{
	"TERM": syscall.SIGTERM,
	"INT":  syscall.SIGINT,
	"HUP":  syscall.SIGHUP,
	"KILL": syscall.SIGKILL,
}

// step is one parsed script line.
type step struct {
	op   op
	text string        // report body, env name, sub-script path
	raw  []byte        // raw bytes
	n    int           // split chunks, oversize length, exit code
	d    time.Duration // sleep, probe wait
	st   bool          // term: ST rather than BEL
	sig  syscall.Signal
}

// Program is a parsed script.
type Program struct {
	steps []step
}

// Parse reads a script, one step per line.
func Parse(r io.Reader) (Program, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxScriptLine)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return Program{}, fmt.Errorf("%w: reading: %w", ErrScript, err)
	}
	return ParseLines(lines...)
}

// ParseLines parses script lines, one step each.
func ParseLines(lines ...string) (Program, error) {
	var p Program
	for i, line := range lines {
		s, ok, err := parseStep(line)
		if err != nil {
			return Program{}, fmt.Errorf("%w: line %d %q: %w", ErrScript, i+1, line, err)
		}
		if ok {
			p.steps = append(p.steps, s)
		}
	}
	return p, nil
}

// parseStep parses one line; ok is false for a blank or comment line.
func parseStep(line string) (s step, ok bool, err error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return step{}, false, nil
	}
	word, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	s.op = op(word)
	noArg := func() error {
		if arg != "" {
			return fmt.Errorf("%s takes no argument", word)
		}
		return nil
	}
	needArg := func() error {
		if arg == "" {
			return fmt.Errorf("%s needs an argument", word)
		}
		return nil
	}
	switch s.op {
	case opReport, opForceEnv, opOnLine:
		err = needArg()
		s.text = arg
	case opClear, opInteractive, opHang:
		err = noArg()
	case opRaw:
		if err = needArg(); err == nil {
			s.raw, err = hex.DecodeString(arg)
		}
	case opSplit, opOversize, opExit:
		if err = needArg(); err == nil {
			s.n, err = strconv.Atoi(arg)
		}
		switch {
		case err != nil:
		case s.op == opSplit && s.n < 1:
			err = fmt.Errorf("split needs at least 1 chunk, got %d", s.n)
		case s.op == opOversize && s.n < len(oscIntro)+len("state=working")+len(terminatorST):
			err = fmt.Errorf("oversize %d is shorter than a report", s.n)
		case s.op == opExit && (s.n < 0 || s.n > 255):
			err = fmt.Errorf("exit code %d outside 0-255", s.n)
		}
	case opTerm:
		switch arg {
		case "bel":
		case "st":
			s.st = true
		default:
			err = fmt.Errorf("term is bel or st, got %q", arg)
		}
	case opSleep:
		if err = needArg(); err == nil {
			s.d, err = time.ParseDuration(arg)
		}
	case opProbeGated:
		s.d = defaultProbeWait
		if arg != "" {
			s.d, err = time.ParseDuration(arg)
		}
	case opSignal:
		var known bool
		if s.sig, known = signals[strings.TrimPrefix(strings.ToUpper(arg), "SIG")]; !known {
			err = fmt.Errorf("unknown signal %q", arg)
		}
	default:
		err = fmt.Errorf("unknown step %q", word)
	}
	return s, err == nil, err
}

// Terminal is the program's terminal.
type Terminal struct {
	// In carries typed input and the terminal's replies.
	In io.Reader
	// Out receives everything the program writes.
	Out io.Writer
	// Getenv reads the environment; nil reads nothing.
	Getenv func(string) string
	// ReadFile reads on-line sub-scripts; nil uses os.ReadFile.
	ReadFile func(string) ([]byte, error)
	// RawInput puts In in raw mode while probe-gated waits for replies and
	// returns the restore; nil when In is not a terminal.
	RawInput func() (restore func(), err error)
	// SplitPause separates the chunks of a split write, so a reader on a
	// real terminal sees separate reads.
	SplitPause time.Duration
}

// Outcome is how a program ended.
type Outcome struct {
	Ended  bool           // an exit or signal step ended the program; false when the script ran out
	Exit   int            // the exit status, when Signal is zero
	Signal syscall.Signal // nonzero: the program ends by this signal
}

// Run runs p on term until p ends, an exit or signal step, or ctx is done.
// A program that reaches its end reports a zero Outcome, which a process
// exits 0 on. A hang step returns ctx's error once ctx is done.
func Run(ctx context.Context, p Program, term Terminal) (Outcome, error) {
	if term.Getenv == nil {
		term.Getenv = func(string) string { return "" }
	}
	if term.ReadFile == nil {
		term.ReadFile = os.ReadFile
	}
	r := &runner{ctx: ctx, term: term, enabled: true, st: true, input: newInput(term.In)}
	out, _, err := r.run(p.steps)
	return out, err
}

// runner is one program run's state.
type runner struct {
	ctx     context.Context
	term    Terminal
	input   *input
	enabled bool // reports are written
	forced  bool // force-env enabled reports; probe-gated is skipped
	st      bool // terminate with ST rather than BEL
	split   int  // chunks for the next sequence; 0 writes it whole
}

// run runs steps; done reports that an exit, signal or hang ended the program.
func (r *runner) run(steps []step) (out Outcome, done bool, err error) {
	for _, s := range steps {
		if err := r.ctx.Err(); err != nil {
			return Outcome{}, true, err
		}
		switch s.op {
		case opReport:
			err = r.report(s.text)
		case opClear:
			err = r.report("state=clear")
		case opOversize:
			err = r.report(oversizeBody(s.n, r.terminator()))
		case opRaw:
			err = r.write(s.raw)
		case opSplit:
			r.split = s.n
		case opTerm:
			r.st = s.st
		case opSleep:
			err = r.sleep(s.d)
		case opProbeGated:
			err = r.probeGated(s.d)
		case opForceEnv:
			r.forced = r.term.Getenv(s.text) != ""
			r.enabled = r.forced
		case opOnLine:
			out, done, err = r.onLine(s.text)
		case opInteractive:
			out, done, err = r.interactive()
		case opExit:
			return Outcome{Ended: true, Exit: s.n}, true, nil
		case opSignal:
			return Outcome{Ended: true, Signal: s.sig}, true, nil
		case opHang:
			<-r.ctx.Done()
			return Outcome{}, true, r.ctx.Err()
		}
		if err != nil || done {
			return out, true, err
		}
	}
	return Outcome{}, false, nil
}

// terminator is the current sequence terminator.
func (r *runner) terminator() string {
	if r.st {
		return terminatorST
	}
	return terminatorBEL
}

// report writes one OSC 7501 sequence with body, unless reports are gated.
func (r *runner) report(body string) error {
	if !r.enabled {
		r.split = 0
		return nil
	}
	return r.sequence(oscIntro + body + r.terminator())
}

// sequence writes seq whole, or in the chunks a split step asked for.
func (r *runner) sequence(seq string) error {
	n := r.split
	r.split = 0
	for _, chunk := range splitChunks(seq, n) {
		if err := r.write([]byte(chunk)); err != nil {
			return err
		}
		if err := r.sleep(r.term.SplitPause); err != nil {
			return err
		}
	}
	return nil
}

// splitChunks cuts seq into n chunks (fewer when seq is shorter). When seq
// ends in ST one cut falls between its ESC and its backslash, the split a
// reader most often gets wrong.
func splitChunks(seq string, n int) []string {
	if n <= 1 || len(seq) < 2 {
		return []string{seq}
	}
	n = min(n, len(seq))
	cuts := map[int]bool{}
	if strings.HasSuffix(seq, terminatorST) {
		cuts[len(seq)-1] = true
	}
	for i := 1; len(cuts) < n-1 && i < n; i++ {
		cuts[i*len(seq)/n] = true
	}
	for c := 1; len(cuts) < n-1; c++ {
		cuts[c] = true
	}
	var chunks []string
	start := 0
	for i := 1; i < len(seq); i++ {
		if cuts[i] {
			chunks = append(chunks, seq[start:i])
			start = i
		}
	}
	return append(chunks, seq[start:])
}

// oversizeBody is a working report body whose whole sequence, introducer
// and terminator included, is exactly n bytes. A key the parser ignores
// fills it, so only the sequence's length can break a limit: a report of
// exactly the sequence limit parses, and one byte more is discarded. Space
// the parser strips around a pair fills a room too small for the key.
func oversizeBody(n int, terminator string) string {
	const state, padKey = "state=working", ":zz="
	room := n - len(oscIntro) - len(terminator) - len(state)
	if room <= len(padKey) {
		return state + strings.Repeat(" ", max(room, 0))
	}
	return state + padKey + strings.Repeat("x", room-len(padKey))
}

// write writes b to the terminal.
func (r *runner) write(b []byte) error {
	if _, err := r.term.Out.Write(b); err != nil {
		return fmt.Errorf("emit: writing: %w", err)
	}
	return nil
}

// sleep pauses for d, or until the run is canceled.
func (r *runner) sleep(d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

// probeGated sends the probe and DA1 and gates reports on a 7501 reply
// arriving before the DA1 reply. No reply within wait, or input ending,
// gates them off. A forced program does not probe.
func (r *runner) probeGated(wait time.Duration) error {
	if r.forced {
		return nil
	}
	if r.term.RawInput != nil {
		restore, err := r.term.RawInput()
		if err != nil {
			return fmt.Errorf("emit: raw input for probe replies: %w", err)
		}
		defer restore()
	}
	if err := r.sequence(oscIntro + probeBody + r.terminator()); err != nil {
		return err
	}
	if err := r.write([]byte(da1Request)); err != nil {
		return err
	}
	answered, err := r.input.awaitReply(r.ctx, wait)
	if err != nil {
		return err
	}
	r.enabled = answered
	return nil
}

// onLine runs the script at path once per input line, until input ends.
func (r *runner) onLine(path string) (Outcome, bool, error) {
	data, err := r.term.ReadFile(path)
	if err != nil {
		return Outcome{}, true, fmt.Errorf("emit: reading on-line script: %w", err)
	}
	sub, err := Parse(bytes.NewReader(data))
	if err != nil {
		return Outcome{}, true, fmt.Errorf("on-line script %s: %w", path, err)
	}
	for {
		if _, ok, err := r.input.line(r.ctx); err != nil || !ok {
			return Outcome{}, err != nil, err
		}
		if out, done, err := r.run(sub.steps); done || err != nil {
			return out, true, err
		}
	}
}

// interactive runs each input line as a step, until input ends.
func (r *runner) interactive() (Outcome, bool, error) {
	for {
		text, ok, err := r.input.line(r.ctx)
		if err != nil || !ok {
			return Outcome{}, err != nil, err
		}
		s, ok, err := parseStep(text)
		if err != nil {
			return Outcome{}, true, fmt.Errorf("%w: typed %q: %w", ErrScript, text, err)
		}
		if !ok {
			continue
		}
		if out, done, err := r.run([]step{s}); done || err != nil {
			return out, true, err
		}
	}
}
