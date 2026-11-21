package runtime

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A tmux session's activity clock is #{window_activity}, the newest over its
// windows: #{session_activity} does not advance on pane I/O while no client is
// attached, which is how gc runs its agent sessions. gc wakes and nudges an
// agent by typing into its pane, and tmux counts those keystrokes as window
// activity even when the agent never runs a turn. Without discounting them a
// woken but unresponsive agent looks perpetually active, misleading
// last_active and the idle, auto-suspend and reconciler logic keyed off it, so
// the local tmux provider, and the providers that drive tmux in a box through
// the carrier ([PokeLedger]), discount gc's own input ("pokes"). The discount
// is LLM-agnostic, purely gc input against pane output, so it holds for any
// agent CLI running in the pane.

const (
	// PokeEcho is the window within which raw window activity is treated as
	// a poke's own keystroke echo rather than agent output.
	PokeEcho = 3 * time.Second
	// PokeGrace is how long a just-poked agent still counts as active, so a
	// responsive agent about to reply is not flipped to idle. After it
	// elapses with no agent output, the poke is discounted.
	PokeGrace = 15 * time.Second
)

// Poke records a gc-initiated input ("poke", e.g. a wake or nudge) to a
// session: when it was sent, where its keystroke echo landed, and the genuine
// activity observed just before it.
type Poke struct {
	At    time.Time // when gc sent the keystrokes; the grace runs on this clock
	Echo  time.Time // raw activity read just after the poke (zero: At locates it)
	Prior time.Time // genuine activity immediately before the poke
}

// echo locates the poke's keystroke echo on the activity clock. A provider
// that reads another machine's tmux (a remote box) records it as Echo, read
// there just after the poke, so the echo is never matched against At, which
// is the provider's own clock. A zero Echo means the activity clock is At's
// (the local tmux provider), so At locates the echo.
func (pk Poke) echo() time.Time {
	if pk.Echo.IsZero() {
		return pk.At
	}
	return pk.Echo
}

// DiscountPokeActivity resolves the genuine activity time from the raw tmux
// window activity (wa), the session's last recorded poke (pk) and the current
// time (now, on At's clock).
//
// If wa is only the poke's own keystroke echo (within PokeEcho of the echo)
// AND the grace window has elapsed with no later agent output, it returns the
// activity seen before the poke — revealing that the agent never actually
// responded. Otherwise wa stands (a real post-poke turn, or a still-in-grace
// recent poke).
func DiscountPokeActivity(wa time.Time, pk Poke, now time.Time) time.Time {
	if pk.At.IsZero() || pk.Prior.IsZero() {
		return wa
	}
	echoOnly := wa.Sub(pk.echo()).Abs() <= PokeEcho
	graceElapsed := now.Sub(pk.At) >= PokeGrace
	if echoOnly && graceElapsed {
		return pk.Prior
	}
	return wa
}

// PokePriorBaseline selects the genuine activity to record as a new poke's
// prior. When an earlier poke is still on record and the current raw window
// activity is only that poke's own echo (raw within PokeEcho of the earlier
// poke's echo, i.e. no genuine agent output since), the last genuine activity
// is the earlier poke's prior, so it is carried forward. This stops chained
// unanswered nudges inside PokeGrace from recording gc's own earlier nudge
// echo as the new baseline — which DiscountPokeActivity would otherwise later
// surface as last_active, masking a stalled agent. Otherwise the freshly
// observed raw activity is genuine and becomes the new prior.
func PokePriorBaseline(raw time.Time, pk Poke, hasPoke bool) time.Time {
	if hasPoke && !pk.At.IsZero() && !pk.Prior.IsZero() && raw.Sub(pk.echo()).Abs() <= PokeEcho {
		return pk.Prior
	}
	return raw
}

// PokeLedger records the last poke to each session of a provider that reads
// raw tmux window activity in a box, so the provider reports activity less
// gc's own keystroke echo, as the local tmux provider does. The box's tmux
// stamps activity on the box's clock, not the provider's, so the echo is
// matched against activity read on the box and only the grace runs on the
// provider's clock: the two clocks are never compared. It takes every time as
// an argument, so the provider owns the clock. The zero value is ready to use
// and is safe for concurrent use.
type PokeLedger struct {
	mu    sync.Mutex
	pokes map[string]Poke
}

// Begin opens a poke to session name before its first keystroke. raw is the
// session's raw window activity read just then; zero, when it could not be
// read, leaves the poke undiscounted. An earlier poke the agent has not
// answered carries its baseline forward (PokePriorBaseline).
//
// Once the input is delivered, call the returned commit with the provider's
// time (at) and the session's raw window activity read just after it (echo).
// tmux has stamped the keystroke echo by then, on the box's clock; an echo the
// pane prints only later is not matched and counts as activity, the safe
// direction. A failed delivery calls nothing and records nothing, and so does
// a zero echo, when that read failed: there is then no time on the box's
// clock to match the echo against.
func (l *PokeLedger) Begin(name string, raw time.Time) (commit func(at, echo time.Time)) {
	pk, ok := l.last(name)
	prior := PokePriorBaseline(raw, pk, ok)
	return func(at, echo time.Time) {
		if echo.IsZero() {
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.pokes == nil {
			l.pokes = make(map[string]Poke)
		}
		l.pokes[name] = Poke{At: at, Echo: echo, Prior: prior}
	}
}

// Discount returns session name's genuine activity as of now: its raw window
// activity wa, less the echo of the last poke recorded for it
// (DiscountPokeActivity). A zero wa stays zero.
func (l *PokeLedger) Discount(name string, wa, now time.Time) time.Time {
	pk, ok := l.last(name)
	if !ok {
		return wa
	}
	return DiscountPokeActivity(wa, pk, now)
}

// last returns the poke recorded for session name, if any.
func (l *PokeLedger) last(name string) (Poke, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	pk, ok := l.pokes[name]
	return pk, ok
}

// LatestTmuxWindowActivity returns the newest of the epoch-seconds values that
// `tmux list-windows -F '#{window_activity}'` prints, one per window. Blank
// lines are skipped; a malformed value, or no value at all, is an error.
func LatestTmuxWindowActivity(out string) (int64, error) {
	var latest int64
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		timestamp, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing window activity %q: %w", line, err)
		}
		if timestamp > latest {
			latest = timestamp
		}
	}
	if latest == 0 {
		return 0, fmt.Errorf("parsing window activity: no timestamps found")
	}
	return latest, nil
}

// ReadTmuxWindowActivity reads the raw activity clock of the in-box tmux
// session target over conn, which reaches the box for session name: the
// newest #{window_activity} of the session's windows. A connection failure is
// returned verbatim, and a non-zero tmux exit or unparseable output is an
// error too; whether that is fatal is the caller's policy.
func ReadTmuxWindowActivity(ctx context.Context, conn ExecProvider, name, target string) (time.Time, error) {
	out, code, err := conn.Exec(ctx, name, []string{"tmux", "list-windows", "-t", target, "-F", "#{window_activity}"})
	if err != nil {
		return time.Time{}, err
	}
	if code != 0 {
		return time.Time{}, fmt.Errorf("tmux list-windows -t %s: exit status %d", target, code)
	}
	secs, err := LatestTmuxWindowActivity(string(out))
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(secs, 0), nil
}
