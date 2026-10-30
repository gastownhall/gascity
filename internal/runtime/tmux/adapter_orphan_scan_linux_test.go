package tmux

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// An absent tmux server tracks no sessions, so the orphan scan must succeed
// with an empty tracked set. Failing here refuses the first start of every
// city, because nothing would ever bring the server up.
func TestFindRuntimesBySessionIDTreatsAbsentServerAsNoTrackedSessions(t *testing.T) {
	t.Cleanup(proctable.SetScanRootForTesting(t.TempDir()))
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = &fakeExecutor{err: ErrNoServer}

	found, err := p.FindRuntimesBySessionID("gc-1")
	if err != nil {
		t.Fatalf("FindRuntimesBySessionID err = %v, want nil when the tmux server is absent", err)
	}
	if len(found) != 0 {
		t.Fatalf("FindRuntimesBySessionID found = %v, want none", found)
	}
}

func TestFindRuntimesBySessionIDFailsClosedOnTmuxListError(t *testing.T) {
	t.Cleanup(proctable.SetScanRootForTesting(t.TempDir()))
	sentinel := errors.New("tmux exploded")
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = &fakeExecutor{err: sentinel}

	if _, err := p.FindRuntimesBySessionID("gc-1"); !errors.Is(err, sentinel) {
		t.Fatalf("FindRuntimesBySessionID err = %v, want the tmux list error", err)
	}
}
