//go:build !windows

package runtime

import (
	"testing"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// TestShellQuoteIsPOSIXQuoting pins Unix byte parity: every command string a
// caller builds with ShellQuote is the one it built with shellquote.Quote.
func TestShellQuoteIsPOSIXQuoting(t *testing.T) {
	for _, arg := range hostileShellArgs {
		if got, want := ShellQuote(arg), shellquote.Quote(arg); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", arg, got, want)
		}
	}
}
