package tmux

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ObserveLiveness on a session the state cache does not hold running answers
// from the snapshot alone: with no process names it must not fork tmux
// show-environment for GT_PROCESS_NAMES, because a session that is absent or
// a corpse has no live agent process whatever the names are. The API's rig
// list observes every configured agent row, and that one fork per stopped row
// was ~24 ms each on a 332-row city (ga-ylpbsy.93). A running session with no
// process names still reads GT_PROCESS_NAMES.
// Kills: the GT_PROCESS_NAMES read moved back ahead of the running check.
func TestObserveLivenessNotRunningForksNothing(t *testing.T) {
	p, fe := newListPanesProvider(
		"corpse\t1\tbash\t201\t0\t1000\t$7\t900",
		"live\t0\tclaude\t101\t1\t2000\t$3\t800",
	)
	if got := p.ObserveLiveness("live", []string{"claude"}); got != (runtime.Liveness{Running: true, Alive: true}) {
		t.Fatalf("ObserveLiveness(live) = %+v, want running and alive", got)
	}
	primed := len(fe.calls)
	for _, name := range []string{"missing", "corpse"} {
		if got := p.ObserveLiveness(name, nil); got != (runtime.Liveness{}) {
			t.Errorf("ObserveLiveness(%s, nil) = %+v, want zero", name, got)
		}
	}
	if extra := fe.calls[primed:]; len(extra) != 0 {
		t.Fatalf("ObserveLiveness of stopped sessions ran tmux %q, want no command", extra)
	}
	p.ObserveLiveness("live", nil)
	if got := len(fe.calls) - primed; got != 1 {
		t.Fatalf("ObserveLiveness(live, nil) ran %d tmux commands, want 1 (the GT_PROCESS_NAMES read): %q", got, fe.calls[primed:])
	}
}
