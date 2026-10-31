// Command gc-osc7501 is gc's OSC 7501 (Program Status Protocol) companion. It
// ships next to gc and is pure Go, so one is small enough to run per agent
// pane.
//
// Usage:
//
//	gc-osc7501 tap -x <tmux-bin> -S <socket-path> -t <pane-id> -n <rand> [-a <app>]
//	gc-osc7501 version
//
// tap runs under tmux pipe-pane -O, which gc arms on each declared agent
// pane. It reads the pane's output on stdin and publishes the program's
// root record to the pane options @gc_ps and @gc_ps_text, every write
// guarded by the pane's nonce "<rand>.<pane-id>" (@gc_ps_tap). It exits 0 on
// EOF and 1 when writes keep failing.
//
// version prints "gc-osc7501 <version> codec=N relay=N"; gc arms a pane only
// when the codec matches its own.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"

	"github.com/gastownhall/gascity/internal/runtime/programstatus"
	"github.com/gastownhall/gascity/internal/runtime/programstatus/tap"
)

// version is the build's version label, set at link time
// (-X main.version=...).
var version = "dev"

// relayVersion is the relay protocol this build implements; 0: no relay.
const relayVersion = 0

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

const usage = `usage:
  gc-osc7501 tap -x <tmux-bin> -S <socket-path> -t <pane-id> -n <rand> [-a <app>]
  gc-osc7501 version
`

// The tap runs once per armed pane, so its footprint is budgeted (8 MB
// RSS): two goroutines need at most two Ps, and every extra P costs threads
// and per-P caches; a low GC target keeps the heap near its small live set.
const (
	tapMaxProcs  = 2
	tapGCPercent = 25
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "tap" {
		runtime.GOMAXPROCS(tapMaxProcs)
		debug.SetGCPercent(tapGCPercent)
	}
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Environ()))
}

// run executes one subcommand and returns the process exit code. Nothing but
// the version line is ever written to stdout.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, environ []string) int {
	if len(args) == 0 {
		diag(stderr, "%s", usage)
		return exitUsage
	}
	switch args[0] {
	case "tap":
		return runTap(args[1:], stdin, stderr, environ)
	case "version":
		if len(args) != 1 {
			diag(stderr, "%s", usage)
			return exitUsage
		}
		v := programstatus.CompanionVersion{Version: version, Codec: programstatus.CodecVersion, Relay: relayVersion}
		if _, err := fmt.Fprintln(stdout, v.String()); err != nil {
			diag(stderr, "gc-osc7501 version: %v\n", err)
			return exitFailure
		}
		return exitOK
	default:
		diag(stderr, "gc-osc7501: unknown subcommand %q\n%s", args[0], usage)
		return exitUsage
	}
}

// tapFlags are the tap's arguments, as gc's pipe-pane command passes them.
type tapFlags struct {
	tmux, socket, pane, rand, app string
}

func parseTapFlags(args []string, stderr io.Writer) (tapFlags, error) {
	var f tapFlags
	fs := flag.NewFlagSet("gc-osc7501 tap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.tmux, "x", "", "tmux binary")
	fs.StringVar(&f.socket, "S", "", "tmux server socket path (#{socket_path})")
	fs.StringVar(&f.pane, "t", "", "pane id (#{pane_id})")
	fs.StringVar(&f.rand, "n", "", "nonce random part: 1-64 of [A-Za-z0-9_-]")
	fs.StringVar(&f.app, "a", "", "keep only reports from this app")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	switch {
	case fs.NArg() != 0:
		return f, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case f.tmux == "":
		return f, errors.New("-x <tmux-bin> is required")
	case f.socket == "":
		return f, errors.New("-S <socket-path> is required")
	case f.pane == "":
		return f, errors.New("-t <pane-id> is required")
	case f.rand == "":
		return f, errors.New("-n <rand> is required")
	case !programstatus.ValidNonce(f.nonce()):
		return f, fmt.Errorf("-n %q -t %q do not form a nonce <rand>.%%<n>", f.rand, f.pane)
	case f.app != "" && !programstatus.ValidApp(f.app):
		return f, fmt.Errorf("-a %q is not an app name", f.app)
	}
	return f, nil
}

func (f tapFlags) nonce() string { return f.rand + "." + f.pane }

func runTap(args []string, stdin io.Reader, stderr io.Writer, environ []string) int {
	f, err := parseTapFlags(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			diag(stderr, "gc-osc7501 tap: %v\n%s", err, usage)
		}
		return exitUsage
	}
	// The publisher drops TMUX and TMUX_PANE from environ: the pipe child
	// inherits the tmux server's global environment, which can name another
	// server, so every write addresses the server by -S alone.
	pub, err := tap.NewTmuxPublisher(f.tmux, f.socket, f.pane, f.nonce(), environ)
	if err != nil {
		diag(stderr, "gc-osc7501 tap: %v\n", err)
		return exitUsage
	}
	logf := func(format string, a ...any) { diag(stderr, "gc-osc7501 %s\n", fmt.Sprintf(format, a...)) }
	opts := tap.Options{Nonce: f.nonce(), App: f.app, Logf: logf}
	if err := tap.Run(context.Background(), stdin, pub, opts); err != nil {
		diag(stderr, "gc-osc7501 tap: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// diag writes a diagnostic to stderr. A failed stderr write has nowhere left
// to be reported, so its error is dropped deliberately.
func diag(stderr io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(stderr, format, a...)
}
