package runtime

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestLatestTmuxWindowActivity(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{name: "single timestamp", input: "123", want: 123},
		{name: "multiple timestamps", input: "123\n456\n234", want: 456},
		{name: "blank lines ignored", input: "\n123\n\n456\n", want: 456},
		{name: "invalid timestamp", input: "123\nnope", wantErr: true},
		{name: "no timestamps", input: "\n\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LatestTmuxWindowActivity(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("LatestTmuxWindowActivity(%q) error = nil, want error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("LatestTmuxWindowActivity(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("LatestTmuxWindowActivity(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// activityConn is an ExecProvider that answers every command with one result
// and records the argv it ran.
type activityConn struct {
	argv [][]string
	out  string
	code int
	err  error
}

func (c *activityConn) Exec(_ context.Context, _ string, argv []string) ([]byte, int, error) {
	c.argv = append(c.argv, argv)
	return []byte(c.out), c.code, c.err
}

// TestReadTmuxWindowActivity pins the read: one list-windows of the target,
// the newest window wins, and every failure is reported, never a zero time
// passed off as an answer.
func TestReadTmuxWindowActivity(t *testing.T) {
	conn := &activityConn{out: "1700000000\n1700000090\n1700000045\n"}
	got, err := ReadTmuxWindowActivity(context.Background(), conn, "worker-1", "=worker-1")
	if err != nil {
		t.Fatalf("ReadTmuxWindowActivity: %v", err)
	}
	if want := time.Unix(1700000090, 0); !got.Equal(want) {
		t.Errorf("activity = %v, want the newest window %v", got, want)
	}
	wantArgv := [][]string{{"tmux", "list-windows", "-t", "=worker-1", "-F", "#{window_activity}"}}
	if !slices.EqualFunc(conn.argv, wantArgv, slices.Equal[[]string]) {
		t.Errorf("argv = %q, want %q", conn.argv, wantArgv)
	}

	transport := errors.New("connection reset")
	for _, tc := range []struct {
		name string
		conn *activityConn
		is   error
	}{
		{name: "transport failure", conn: &activityConn{err: transport}, is: transport},
		{name: "no such session", conn: &activityConn{code: 1}},
		{name: "unparseable output", conn: &activityConn{out: "soon\n"}},
		{name: "empty output", conn: &activityConn{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadTmuxWindowActivity(context.Background(), tc.conn, "worker-1", "=worker-1")
			if err == nil {
				t.Fatalf("ReadTmuxWindowActivity = %v, nil; want an error", got)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.is)
			}
			if !got.IsZero() {
				t.Errorf("activity = %v, want zero alongside the error", got)
			}
		})
	}
}

// TestPokeLedgerDiscountsUnansweredPokeEcho walks one poke through the ledger:
// gc's own keystroke echo stands while the agent may still answer, is
// discounted to the genuine pre-poke activity once the grace elapses with no
// answer, and real output after the echo window is never discounted.
func TestPokeLedgerDiscountsUnansweredPokeEcho(t *testing.T) {
	genuine := time.Unix(1700000000, 0) // the agent's last real output
	poke := genuine.Add(5 * time.Hour)  // gc nudges 5h later
	var l PokeLedger

	if got := l.Discount("s", poke, poke.Add(time.Hour)); !got.Equal(poke) {
		t.Fatalf("with no poke recorded, Discount = %v, want raw %v", got, poke)
	}

	l.Begin("s", genuine)(poke, poke)

	if got := l.Discount("s", poke, poke.Add(PokeGrace/2)); !got.Equal(poke) {
		t.Errorf("echo inside the grace window = %v, want the echo %v (the agent may still answer)", got, poke)
	}
	if got := l.Discount("s", poke, poke.Add(PokeGrace+time.Second)); !got.Equal(genuine) {
		t.Errorf("unanswered echo after the grace = %v, want the genuine %v", got, genuine)
	}
	answer := poke.Add(30 * time.Second)
	if got := l.Discount("s", answer, answer.Add(time.Minute)); !got.Equal(answer) {
		t.Errorf("agent output past the echo window = %v, want it kept: %v", got, answer)
	}
	if got := l.Discount("other", poke, poke.Add(time.Minute)); !got.Equal(poke) {
		t.Errorf("a poke to s discounted session other: %v, want raw %v", got, poke)
	}
	if got := l.Discount("s", time.Time{}, poke.Add(time.Minute)); !got.IsZero() {
		t.Errorf("unknown raw activity = %v, want zero", got)
	}
}

// TestPokeLedgerUndeliveredPokeRecordsNothing: a Begin whose input never
// lands is never committed, so it leaves no poke to discount.
func TestPokeLedgerUndeliveredPokeRecordsNothing(t *testing.T) {
	genuine := time.Unix(1700000000, 0)
	wa := genuine.Add(time.Hour)
	var l PokeLedger

	_ = l.Begin("s", genuine) // the send failed; commit is never called

	if got := l.Discount("s", wa, wa.Add(PokeGrace+time.Second)); !got.Equal(wa) {
		t.Errorf("Discount = %v, want raw %v: an undelivered poke must record nothing", got, wa)
	}
}

// TestPokeLedgerUnreadablePriorLeavesPokeUndiscounted: when the activity
// before the poke cannot be read there is no genuine baseline to fall back to,
// so the raw activity stands rather than becoming a zero or a guess.
func TestPokeLedgerUnreadablePriorLeavesPokeUndiscounted(t *testing.T) {
	poke := time.Unix(1700000000, 0)
	var l PokeLedger

	l.Begin("s", time.Time{})(poke, poke)

	if got := l.Discount("s", poke, poke.Add(PokeGrace+time.Second)); !got.Equal(poke) {
		t.Errorf("Discount = %v, want raw %v", got, poke)
	}
}

// boxSkews are box clocks running with, behind and ahead of the provider's.
var boxSkews = []time.Duration{0, -10 * time.Second, 10 * time.Second}

// TestPokeLedgerChainedUnansweredPokesKeepGenuineBaseline: a second poke
// inside the first one's grace window, with no answer in between, reads raw
// activity that is only the first poke's echo. Its baseline must stay the
// genuine activity, or last_active would ratchet up to gc's own nudge,
// whichever way the box's clock is off the provider's.
func TestPokeLedgerChainedUnansweredPokesKeepGenuineBaseline(t *testing.T) {
	for _, skew := range boxSkews {
		t.Run("box clock "+skew.String(), func(t *testing.T) {
			genuine := time.Unix(1700000000, 0)    // on the box's clock
			first := genuine.Add(30 * time.Minute) // on the provider's clock
			second := first.Add(PokeGrace / 3)
			var l PokeLedger

			l.Begin("s", genuine)(first, first.Add(skew))
			l.Begin("s", first.Add(skew))(second, second.Add(skew)) // raw is the first poke's echo

			if got := l.Discount("s", second.Add(skew), second.Add(PokeGrace+time.Second)); !got.Equal(genuine) {
				t.Errorf("after two unanswered pokes Discount = %v, want the genuine %v, not gc's echo %v", got, genuine, first.Add(skew))
			}
		})
	}
}

// TestPokeLedgerMatchesEchoOnTheBoxClock: the box's tmux stamps activity on
// the box's clock, which is not the provider's, so the echo is matched against
// the activity read on the box just after the poke. With the box clock ahead
// or behind, an unanswered echo is still discounted once the provider's grace
// elapses, and a reply the agent prints 10s after the poke is still its own,
// not rolled back to the activity before the poke.
func TestPokeLedgerMatchesEchoOnTheBoxClock(t *testing.T) {
	for _, skew := range boxSkews {
		t.Run("box clock "+skew.String(), func(t *testing.T) {
			at := time.Unix(1700000000, 0)      // the poke, on the provider's clock
			echo := at.Add(skew)                // the poke's echo, on the box's clock
			genuine := echo.Add(-5 * time.Hour) // the agent's last real output
			var l PokeLedger

			l.Begin("s", genuine)(at, echo)

			if got := l.Discount("s", echo, at.Add(PokeGrace/2)); !got.Equal(echo) {
				t.Errorf("echo inside the grace window = %v, want the echo %v (the agent may still answer)", got, echo)
			}
			if got := l.Discount("s", echo, at.Add(PokeGrace+time.Second)); !got.Equal(genuine) {
				t.Errorf("unanswered echo after the grace = %v, want the genuine %v", got, genuine)
			}
			reply := echo.Add(10 * time.Second)
			if got := l.Discount("s", reply, at.Add(time.Minute)); !got.Equal(reply) {
				t.Errorf("reply 10s after the poke = %v, want it kept: %v", got, reply)
			}
		})
	}
}

// TestPokeLedgerUnreadableEchoRecordsNothing: when the activity read just
// after the poke fails there is no time on the box's clock to match its echo
// against, so the poke records nothing, as an undelivered one does, rather
// than matching the echo against the provider's own clock.
func TestPokeLedgerUnreadableEchoRecordsNothing(t *testing.T) {
	genuine := time.Unix(1700000000, 0)
	at := genuine.Add(time.Hour)
	var l PokeLedger

	l.Begin("s", genuine)(at, time.Time{})

	if got := l.Discount("s", at, at.Add(PokeGrace+time.Second)); !got.Equal(at) {
		t.Errorf("Discount = %v, want raw %v: a poke whose echo was not read records nothing", got, at)
	}
}

// TestPokeLedgerPokeThatTypedNothingKeepsBaseline: a delivered poke that typed
// nothing (empty input, or a send-keys tmux refused with a non-zero exit)
// leaves the box's clock at the earlier poke's echo, so the read just after
// it finds that echo again. The earlier poke's genuine baseline carries
// however much later it comes, and the earlier echo is still discounted.
func TestPokeLedgerPokeThatTypedNothingKeepsBaseline(t *testing.T) {
	genuine := time.Unix(1700000000, 0)
	first := genuine.Add(time.Hour)
	later := first.Add(time.Minute) // far past the first poke's echo window
	var l PokeLedger

	l.Begin("s", genuine)(first, first)
	l.Begin("s", first)(later, first) // nothing typed: the box still reads the first echo

	if got := l.Discount("s", first, later.Add(PokeGrace+time.Second)); !got.Equal(genuine) {
		t.Errorf("Discount = %v, want the genuine %v, not gc's echo %v", got, genuine, first)
	}
}
