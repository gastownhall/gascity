package main

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// stdinIsRealTerminal reports whether stdin is an interactive terminal. It
// uses golang.org/x/term rather than isTerminalFunc's file-mode check, which
// returns true for /dev/null (see cmd_supervisor_city.go).
var stdinIsRealTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// resolveImplicitCWD resolves the implicit target directory used when a
// state-creating command is given no explicit path argument. It refuses when
// stdin is not an interactive terminal: an unattended invocation with no path
// and cwd inside an arbitrary directory (e.g. a checkout root) has no way to
// confirm that directory is the intended target, and silently bootstrapping
// there leaks state that's hard to notice and hard to clean up. Pass an
// explicit path ("." for the current directory) to confirm the target.
//
// Scope: this guards the gc init entry points, which create a city at the
// resolved path. Commands that merely operate on an already-bootstrapped city
// (gc start, gc restart) do not use it — they resolve through
// requireBootstrappedCity, which fails before any side effect when cwd is not
// inside a city, so there is no state to leak.
func resolveImplicitCWD() (string, error) {
	if !stdinIsRealTerminal() {
		return "", fmt.Errorf(`no path given and stdin is not an interactive terminal; pass an explicit path (use "." for the current directory) to confirm the target`)
	}
	return os.Getwd()
}
