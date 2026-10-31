// Command osc7501emitter is a scripted fake agent program for OSC 7501
// conformance tests: it writes Program Status Protocol sequences to its
// terminal as a line script directs (see package emit for the steps).
//
// Usage:
//
//	osc7501emitter <script-file>
//	osc7501emitter -e <step> [-e <step>...]
//
// It exits with the script's exit status, or ends by the script's signal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/gastownhall/gascity/internal/runtime/runtimetest/osc7501emitter/emit"
)

// splitPause separates the chunks of a split write so the terminal's reader
// sees separate reads.
const splitPause = 5 * time.Millisecond

// steps collects repeated -e flags.
type steps []string

func (s *steps) String() string     { return strings.Join(*s, "; ") }
func (s *steps) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	code, sig, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "osc7501emitter:", err)
		os.Exit(2)
	}
	if sig != 0 {
		signal.Reset(sig)
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			fmt.Fprintln(os.Stderr, "osc7501emitter: raising", sig, err)
			os.Exit(2)
		}
		time.Sleep(time.Second) // the signal's default action ends the process
		fmt.Fprintln(os.Stderr, "osc7501emitter: survived", sig)
		os.Exit(2)
	}
	os.Exit(code)
}

// run parses args, runs the script on the process's terminal, and returns
// its exit status or signal.
func run(args []string) (int, syscall.Signal, error) {
	fs := flag.NewFlagSet("osc7501emitter", flag.ContinueOnError)
	var inline steps
	fs.Var(&inline, "e", "a script step (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 0, 0, err
	}
	prog, err := load(fs.Args(), inline)
	if err != nil {
		return 0, 0, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	out, err := emit.Run(ctx, prog, emit.Terminal{
		In:         os.Stdin,
		Out:        os.Stdout,
		Getenv:     os.Getenv,
		RawInput:   rawInput(),
		SplitPause: splitPause,
	})
	if errors.Is(err, context.Canceled) {
		return 0, syscall.SIGTERM, nil
	}
	return out.Exit, out.Signal, err
}

// load parses the script file named by args, or the inline steps.
func load(args []string, inline steps) (emit.Program, error) {
	switch {
	case len(args) == 1 && len(inline) == 0:
		f, err := os.Open(args[0])
		if err != nil {
			return emit.Program{}, fmt.Errorf("opening script: %w", err)
		}
		defer f.Close() //nolint:errcheck // read-only
		return emit.Parse(f)
	case len(args) == 0 && len(inline) > 0:
		return emit.ParseLines(inline...)
	default:
		return emit.Program{}, errors.New("usage: osc7501emitter <script-file> | -e <step> [-e <step>]")
	}
}

// rawInput puts stdin in raw mode for probe replies when it is a terminal.
func rawInput() func() (func(), error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil
	}
	return func() (func(), error) {
		old, err := term.MakeRaw(fd)
		if err != nil {
			return nil, err
		}
		return func() { _ = term.Restore(fd, old) }, nil
	}
}
