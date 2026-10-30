package acp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file implements [runtime.TurnFenceProvider] and
// [runtime.TurnObservationProvider] from the turn records a sessionConn keeps.
// The process that started a session answers from memory. Any other process
// asks it over the control socket, with one line per command:
//
//	turn                -> idle | running <id> | starting | gone | unavailable
//	interrupt-turn <id> -> ok | stale <id> | idle | starting | gone | unavailable
//
// An owner that predates these commands closes the connection without a
// reply; the caller then reports [runtime.ErrTurnFenceUnavailable] and never
// falls back to an untargeted interrupt.

var (
	_ runtime.TurnFenceProvider       = (*Provider)(nil)
	_ runtime.TurnObservationProvider = (*Provider)(nil)
)

// The control-socket turn commands and their replies.
const (
	controlTurn          = "turn"
	controlInterruptTurn = "interrupt-turn "
	replyIdle            = "idle"
	replyRunning         = "running "
	replyStarting        = "starting"
	replyGone            = "gone"
	replyUnavailable     = "unavailable"
	replyStale           = "stale "
	replyOK              = "ok"
)

// turnSocketTimeout bounds one turn command over the control socket when
// the caller's context sets no earlier deadline.
const turnSocketTimeout = 2 * time.Second

// The cross-process WaitTurnEnded poll starts at turnPollInitial and doubles
// to turnPollMax. Tests shorten both.
var (
	turnPollInitial = 250 * time.Millisecond
	turnPollMax     = time.Second
)

// ownerTurnCapabilities are an ACP session's turn capabilities in the
// process that owns it. Every turn starts from gc's session/prompt and ends
// with its response, the turn record's id names both, and the owner keeps
// the last turnHistoryLen finished turns.
var ownerTurnCapabilities = runtime.TurnCapabilities{
	Start:    runtime.TurnStartRecorded,
	End:      runtime.TurnEndRecorded,
	Fence:    runtime.TurnFenceAtomic,
	Link:     runtime.TurnLinkExact,
	Lossless: true,
}

// TurnCapabilities implements [runtime.TurnObservationProvider]. Only the
// owning process keeps the finished turns, so a session another process
// owns reads as not lossless.
func (p *Provider) TurnCapabilities(name string) runtime.TurnCapabilities {
	caps := ownerTurnCapabilities
	p.mu.Lock()
	_, owned := p.conns[name]
	p.mu.Unlock()
	caps.Lossless = owned
	return caps
}

// turnConn returns the in-process connection that answers name's turn
// reads. inProcess is false, with no error, when this provider does not track
// name, so another process may own it. A session still in its handshake is
// errSessionStarting; one whose agent has exited is
// [runtime.ErrSessionNotFound].
func (p *Provider) turnConn(name string) (sc *sessionConn, inProcess bool, err error) {
	p.mu.Lock()
	sc, ok := p.conns[name]
	p.mu.Unlock()
	if !ok {
		return nil, false, nil
	}
	if sc.cancel != nil {
		return nil, true, errSessionStarting(name)
	}
	if !sc.alive() {
		return nil, true, errACPConnClosed(name)
	}
	return sc, true, nil
}

// errSessionStarting reports a turn read of a session still in its handshake.
// It wraps both [runtime.ErrSessionInitializing] and
// [runtime.ErrRuntimeUnavailable], the error [runtime.TurnFenceProvider]
// documents for a read that cannot be made, so a caller that knows only the
// latter retries rather than treating a starting session as failed.
func errSessionStarting(name string) error {
	return fmt.Errorf("%w: %w: ACP session %q is still in its handshake", runtime.ErrRuntimeUnavailable, runtime.ErrSessionInitializing, name)
}

// turnStateLocked is the running turn as a TurnState. Caller must hold mu.
func (sc *sessionConn) turnStateLocked() runtime.TurnState {
	if sc.currentTurn == nil {
		return runtime.TurnState{}
	}
	return runtime.TurnState{TurnID: sc.currentTurn.ID}
}

// observeTurn reads the running turn. A drained connection can run no turn
// and finish none, so it is gone, never idle.
func (sc *sessionConn) observeTurn(name string) (runtime.TurnState, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.drained {
		return runtime.TurnState{}, errACPConnClosed(name)
	}
	return sc.turnStateLocked(), nil
}

// turnHistory reads the running turn and the finished ones, newest first,
// in one critical section.
func (sc *sessionConn) turnHistory(name string) (runtime.TurnHistory, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.drained {
		return runtime.TurnHistory{}, errACPConnClosed(name)
	}
	h := runtime.TurnHistory{Current: sc.turnStateLocked(), ObservedAt: time.Now()}
	if sc.currentTurn != nil {
		h.CurrentStart = sc.currentTurn.StartedAt
	}
	h.Recent = make([]runtime.EndedTurn, 0, len(sc.recentTurns))
	for i := len(sc.recentTurns) - 1; i >= 0; i-- {
		rec := sc.recentTurns[i]
		h.Recent = append(h.Recent, runtime.EndedTurn{
			TurnID:    rec.ID,
			StartedAt: rec.StartedAt,
			EndedAt:   rec.EndedAt,
			Status:    turnStatus(rec.State, rec.StopReason),
		})
	}
	return h, nil
}

// waitTurnEnded waits on the idle channel of turn turnID, taken in the same
// critical section that finds turnID running, so a later turn cannot hold
// the wait open. A connection that drains has ended its turn.
func (sc *sessionConn) waitTurnEnded(ctx context.Context, name, turnID string, timeout time.Duration) (runtime.TurnState, error) {
	sc.mu.Lock()
	if sc.drained {
		sc.mu.Unlock()
		return runtime.TurnState{}, nil
	}
	if sc.currentTurn == nil || sc.currentTurn.ID != turnID {
		st := sc.turnStateLocked()
		sc.mu.Unlock()
		return st, nil
	}
	sc.ensureIdleChannelLocked()
	idleCh := sc.idleCh
	sc.mu.Unlock()

	running := runtime.TurnState{TurnID: turnID}
	timer := time.NewTimer(max(timeout, 0))
	defer timer.Stop()
	select {
	case <-idleCh:
	case <-sc.done:
	case <-ctx.Done():
		return running, ctx.Err()
	case <-timer.C:
		select {
		case <-idleCh:
		case <-sc.done:
		default:
			return running, fmt.Errorf("%w: session %q turn %q after %s", runtime.ErrTurnNotEnded, name, turnID, timeout)
		}
	}
	st, err := sc.observeTurn(name)
	if err != nil {
		return runtime.TurnState{}, nil // the connection drained: no turn runs
	}
	return st, nil
}

// ObserveTurn implements [runtime.TurnFenceProvider]: the running turn of a
// session this provider owns, or the owner's answer over the control socket.
func (p *Provider) ObserveTurn(ctx context.Context, name string) (runtime.TurnState, error) {
	if err := ctx.Err(); err != nil {
		return runtime.TurnState{}, err
	}
	sc, inProcess, err := p.turnConn(name)
	if err != nil {
		return runtime.TurnState{}, err
	}
	if inProcess {
		return sc.observeTurn(name)
	}
	reply, err := p.askOwner(ctx, name, controlTurn)
	if err != nil {
		return runtime.TurnState{}, err
	}
	return parseTurnReply(name, reply)
}

// TurnHistory implements [runtime.TurnObservationProvider]. In the owning
// process it reports the running turn with its start and the last
// turnHistoryLen finished turns; another process learns only the running
// turn, from the owner.
func (p *Provider) TurnHistory(ctx context.Context, name string) (runtime.TurnHistory, error) {
	if err := ctx.Err(); err != nil {
		return runtime.TurnHistory{}, err
	}
	sc, inProcess, err := p.turnConn(name)
	if err != nil {
		return runtime.TurnHistory{}, err
	}
	if inProcess {
		return sc.turnHistory(name)
	}
	reply, err := p.askOwner(ctx, name, controlTurn)
	if err != nil {
		return runtime.TurnHistory{}, err
	}
	st, err := parseTurnReply(name, reply)
	if err != nil {
		return runtime.TurnHistory{}, err
	}
	return runtime.TurnHistory{Current: st, ObservedAt: time.Now()}, nil
}

// InterruptTurn implements [runtime.TurnFenceProvider] with an atomic fence:
// the owner checks that turnID is running and takes that turn's idle channel
// in one critical section, then cancels it as Interrupt does on a goroutine
// it owns. Another process asks the owner to do the same. An id that is no
// turn identity ([runtime.IsTurnIdentity]) names no turn the session can be
// running, so it reads as not current without the interrupt-turn command.
func (p *Provider) InterruptTurn(ctx context.Context, name, turnID string) (runtime.TurnInterrupt, error) {
	if turnID == "" {
		return runtime.TurnInterrupt{}, fmt.Errorf("%w: interrupting a turn of %q", runtime.ErrTurnUnnamed, name)
	}
	if err := ctx.Err(); err != nil {
		return runtime.TurnInterrupt{}, err
	}
	sc, inProcess, err := p.turnConn(name)
	if err != nil {
		return runtime.TurnInterrupt{}, err
	}
	if !runtime.IsTurnIdentity(turnID) || strings.Contains(turnID, " ") {
		// Every ACP turn id is a UUID, and the control socket's line
		// protocol cannot carry a space, a control character or an id past
		// the owner's line limit.
		st, err := p.ObserveTurn(ctx, name)
		if err != nil {
			return runtime.TurnInterrupt{}, err
		}
		return runtime.TurnInterrupt{}, &runtime.TurnNotCurrentError{Session: name, TurnID: turnID, Current: st}
	}
	if inProcess {
		if err := sc.interruptTurn(name, turnID, p.cfg.cancelTimeout()); err != nil {
			return runtime.TurnInterrupt{}, err
		}
		return runtime.TurnInterrupt{Fenced: true}, nil
	}
	reply, err := p.askOwner(ctx, name, controlInterruptTurn+turnID)
	if err != nil {
		return runtime.TurnInterrupt{}, err
	}
	switch {
	case reply == replyOK:
		return runtime.TurnInterrupt{Fenced: true}, nil
	case reply == replyIdle:
		return runtime.TurnInterrupt{}, &runtime.TurnNotCurrentError{Session: name, TurnID: turnID}
	case strings.HasPrefix(reply, replyStale):
		current := runtime.TurnState{TurnID: strings.TrimPrefix(reply, replyStale)}
		return runtime.TurnInterrupt{}, &runtime.TurnNotCurrentError{Session: name, TurnID: turnID, Current: current}
	}
	_, err = parseTurnReply(name, reply)
	if err == nil {
		err = fmt.Errorf("%w: ACP session %q owner answered interrupt-turn with %q", runtime.ErrRuntimeUnavailable, name, reply)
	}
	return runtime.TurnInterrupt{}, err
}

// WaitTurnEnded implements [runtime.TurnFenceProvider]. The owner waits on the
// turn's own idle channel. Another process polls the owner, first after
// turnPollInitial and then at doubling intervals up to turnPollMax; a failed
// read is remembered and the poll goes on, except that an owner that predates
// the turn commands ends the wait at once with
// [runtime.ErrTurnFenceUnavailable]. A session that is gone has ended its
// turn.
func (p *Provider) WaitTurnEnded(ctx context.Context, name, turnID string, timeout time.Duration) (runtime.TurnState, error) {
	if turnID == "" {
		return runtime.TurnState{}, fmt.Errorf("%w: waiting on a turn of %q", runtime.ErrTurnUnnamed, name)
	}
	if err := ctx.Err(); err != nil {
		return runtime.TurnState{}, err
	}
	sc, inProcess, err := p.turnConn(name)
	switch {
	case errors.Is(err, runtime.ErrSessionNotFound):
		return runtime.TurnState{}, nil
	case inProcess && err == nil:
		return sc.waitTurnEnded(ctx, name, turnID, timeout)
	}
	return p.pollTurnEnded(ctx, name, turnID, timeout, err)
}

// pollTurnEnded is WaitTurnEnded for a session this process cannot read
// from memory: one owned elsewhere, or one still in its handshake here
// (lastErr).
func (p *Provider) pollTurnEnded(ctx context.Context, name, turnID string, timeout time.Duration, lastErr error) (runtime.TurnState, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var (
		last runtime.TurnState
		read bool
	)
	for delay := turnPollInitial; ; delay = min(delay*2, turnPollMax) {
		poll := time.NewTimer(delay)
		select {
		case <-waitCtx.Done():
			poll.Stop()
			if err := ctx.Err(); err != nil {
				return last, err
			}
			if !read && lastErr != nil {
				return last, fmt.Errorf("%w: session %q turn %q after %s: last read: %w", runtime.ErrTurnNotEnded, name, turnID, timeout, lastErr)
			}
			return last, fmt.Errorf("%w: session %q turn %q after %s", runtime.ErrTurnNotEnded, name, turnID, timeout)
		case <-poll.C:
		}
		st, err := p.ObserveTurn(waitCtx, name)
		switch {
		case errors.Is(err, runtime.ErrSessionNotFound):
			return runtime.TurnState{}, nil
		case errors.Is(err, runtime.ErrTurnFenceUnavailable):
			// No later poll can do better against this owner.
			return last, err
		case err != nil:
			lastErr = err
			if waitCtx.Err() != nil && ctx.Err() == nil {
				// The wait's own bound cut this read; its deadline is not
				// the caller's, so only the read's text is kept.
				detail := strings.TrimPrefix(err.Error(), runtime.ErrRuntimeUnavailable.Error()+": ")
				lastErr = fmt.Errorf("%w: %s", runtime.ErrRuntimeUnavailable, detail)
			}
		case st.TurnID != turnID:
			return st, nil
		default:
			last, read = st, true
		}
	}
}

// parseTurnReply decodes the owner's answer to the turn command.
func parseTurnReply(name, reply string) (runtime.TurnState, error) {
	switch {
	case reply == replyIdle:
		return runtime.TurnState{}, nil
	case strings.HasPrefix(reply, replyRunning) && len(reply) > len(replyRunning):
		return runtime.TurnState{TurnID: strings.TrimPrefix(reply, replyRunning)}, nil
	case reply == replyStarting:
		return runtime.TurnState{}, errSessionStarting(name)
	case reply == replyGone:
		return runtime.TurnState{}, errACPConnClosed(name)
	case reply == replyUnavailable:
		return runtime.TurnState{}, fmt.Errorf("%w: the owner of ACP session %q could not read its turn", runtime.ErrRuntimeUnavailable, name)
	}
	return runtime.TurnState{}, fmt.Errorf("%w: ACP session %q owner answered %q", runtime.ErrRuntimeUnavailable, name, reply)
}

// askOwner sends command to the process owning name over its control socket
// and returns the reply line. A session with no socket accepting connections
// is [runtime.ErrSessionNotFound]; a socket that cannot be classified or an
// owner that does not answer in time wraps [runtime.ErrRuntimeUnavailable];
// an owner that closes without replying, as one that predates the command
// does, is [runtime.ErrTurnFenceUnavailable].
func (p *Provider) askOwner(ctx context.Context, name, command string) (string, error) {
	deadline := time.Now().Add(turnSocketTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	var unknown error
	for _, sp := range []string{p.sockPath(name), p.legacySockPath(name)} {
		if runtime.UnixSocketPathTooLong(sp) {
			continue
		}
		conn, err := p.dialSocket(sp, time.Until(deadline))
		switch runtime.ClassifyControlSocketDial(err) {
		case runtime.ControlSocketPresent:
			return exchangeLine(ctx, conn, name, command, deadline)
		case runtime.ControlSocketUnknown:
			if unknown == nil {
				unknown = err
			}
		}
	}
	if unknown != nil {
		return "", fmt.Errorf("%w: acp control socket for %q: %w", runtime.ErrRuntimeUnavailable, name, unknown)
	}
	return "", fmt.Errorf("%w: no ACP control socket for %q", runtime.ErrSessionNotFound, name)
}

// exchangeLine writes command on conn and reads one reply line, by deadline
// or until ctx ends, and closes conn.
func exchangeLine(ctx context.Context, conn net.Conn, name, command string, deadline time.Time) (string, error) {
	defer conn.Close() //nolint:errcheck
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	fail := func(err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: asking the owner of %q: %w: %w", runtime.ErrRuntimeUnavailable, name, ctxErr, err)
		}
		return fmt.Errorf("%w: asking the owner of %q: %w", runtime.ErrRuntimeUnavailable, name, err)
	}
	if _, err := fmt.Fprintf(conn, "%s\n", command); err != nil {
		return "", fail(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	switch {
	case err == nil:
		return strings.TrimSuffix(line, "\n"), nil
	case line == "" && (errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)):
		return "", fmt.Errorf("%w: the owner of ACP session %q did not answer %q", runtime.ErrTurnFenceUnavailable, name, strings.Fields(command)[0])
	}
	return "", fail(err)
}

// controlCommands are the control socket's commands that run on the owner's
// connection.
type controlCommands struct {
	interrupt     func() error
	turn          func() string
	interruptTurn func(turnID string) string
}

// ownedControlCommands returns name's owner-side control commands. The socket
// starts before the connection exists; they run on the connection Start
// later stores in owned, and report the session starting until Start commits
// it.
func (p *Provider) ownedControlCommands(name string, owned *atomic.Pointer[sessionConn]) controlCommands {
	conn := func() (*sessionConn, error) {
		sc := owned.Load()
		if sc == nil {
			return nil, errSessionStarting(name)
		}
		committed, inProcess, err := p.turnConn(name)
		switch {
		case err != nil:
			return nil, err
		case !inProcess || committed != sc:
			return nil, errACPConnClosed(name)
		}
		return sc, nil
	}
	return controlCommands{
		interrupt: p.ownedInterrupt(name, owned),
		turn: func() string {
			sc, err := conn()
			if err != nil {
				return turnErrorReply(err)
			}
			st, err := sc.observeTurn(name)
			switch {
			case err != nil:
				return turnErrorReply(err)
			case st.Active():
				return replyRunning + st.TurnID
			}
			return replyIdle
		},
		interruptTurn: func(turnID string) string {
			sc, err := conn()
			if err != nil {
				return turnErrorReply(err)
			}
			err = sc.interruptTurn(name, turnID, p.cfg.cancelTimeout())
			var nc *runtime.TurnNotCurrentError
			switch {
			case err == nil:
				return replyOK
			case errors.As(err, &nc) && nc.Current.Active():
				return replyStale + nc.Current.TurnID
			case errors.As(err, &nc):
				return replyIdle
			}
			return turnErrorReply(err)
		},
	}
}

// turnErrorReply is the reply for a turn read the owner could not make:
// starting during the handshake, gone once the session is, and unavailable
// for anything else. Only a session that is gone may read as gone: a caller
// takes that as the turn having ended.
func turnErrorReply(err error) string {
	switch {
	case errors.Is(err, runtime.ErrSessionInitializing):
		return replyStarting
	case errors.Is(err, runtime.ErrSessionNotFound):
		return replyGone
	}
	return replyUnavailable
}
