package tmux

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The pane a session was spawned into is its join key to the host's journal
// ("tmux child pane <PID> launched by process <PID>"), so the provider reports
// the pane id and pid of a session with exactly one pane.
func TestProviderSessionBindingReportsThePaneIdentity(t *testing.T) {
	fe := &fakeExecutor{out: "%7 48213\n"}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe
	p.tm.namedSocketLstat = stableNamedSocketLstat(t)

	got, err := p.SessionBinding("runner")
	if err != nil {
		t.Fatalf("SessionBinding: %v", err)
	}
	if want := (runtime.SessionBinding{PaneID: "%7", PanePID: "48213"}); got != want {
		t.Fatalf("SessionBinding = %+v, want %+v", got, want)
	}
	if len(fe.calls) == 0 || !strings.Contains(strings.Join(fe.calls[len(fe.calls)-1], " "), "list-panes") {
		t.Fatalf("calls = %v, want a list-panes read", fe.calls)
	}
}

// A session that is gone reports ErrSessionNotFound, so the record clears the
// previous incarnation's pane. A pane list that does not name exactly one
// well-formed pane is not a stamp: the record keeps what it had.
func TestProviderSessionBindingRefusesWhatIsNotOneWellFormedPane(t *testing.T) {
	gone := &fakeExecutor{err: ErrSessionNotFound}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = gone
	p.tm.namedSocketLstat = stableNamedSocketLstat(t)
	if _, err := p.SessionBinding("runner"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("SessionBinding(gone) error = %v, want runtime.ErrSessionNotFound", err)
	}

	for name, out := range map[string]string{
		"two panes":      "%1 10\n%2 11\n",
		"no panes":       "",
		"bad pane id":    "7 10\n",
		"bad pane pid":   "%1 abc\n",
		"missing pid":    "%1\n",
		"zero pid":       "%1 0\n",
		"trailing field": "%1 10 extra\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := NewProviderWithConfig(Config{SocketName: "x"})
			p.tm.exec = &fakeExecutor{out: out}
			p.tm.namedSocketLstat = stableNamedSocketLstat(t)
			got, err := p.SessionBinding("runner")
			if err == nil {
				t.Fatalf("SessionBinding(%q) = %+v, nil; want an error", out, got)
			}
			if errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("SessionBinding(%q) error = %v, must not claim the session is gone", out, err)
			}
		})
	}
}
