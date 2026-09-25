package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// turnState is the lifecycle state of one session/prompt turn.
type turnState string

const (
	// turnRunning means the session/prompt response is still outstanding.
	turnRunning turnState = "running"
	// turnCompleted means the agent answered the prompt with a result. The
	// ACP stop reason says why the turn ended (end_turn, max_tokens,
	// max_turn_requests, refusal, or the cancel reason); gc records it
	// verbatim.
	turnCompleted turnState = "completed"
	// turnFailed means the turn ended without a prompt result: the agent
	// answered with a JSON-RPC error, the result could not be decoded, the
	// prompt could not be sent, or the agent exited first.
	turnFailed turnState = "failed"
)

// turnFailureAgentExited is the failure message recorded when the agent's
// connection closes while a prompt response is outstanding.
const turnFailureAgentExited = "agent exited before the turn completed"

// promptResult is the session/prompt response result.
type promptResult struct {
	StopReason string     `json:"stopReason"`
	Usage      *turnUsage `json:"usage,omitempty"`
}

// turnUsage is the unstable ACP token-usage object an agent may attach to a
// session/prompt result. Agents that omit it leave the record's Usage nil.
type turnUsage struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	TotalTokens       int64 `json:"totalTokens"`
	ThoughtTokens     int64 `json:"thoughtTokens,omitempty"`
	CachedReadTokens  int64 `json:"cachedReadTokens,omitempty"`
	CachedWriteTokens int64 `json:"cachedWriteTokens,omitempty"`
}

// turnRecord describes one session/prompt turn. A sessionConn keeps only the
// running turn and the last finished one; it does not keep a history.
type turnRecord struct {
	ID         string // random UUIDv4, unique per turn
	PromptID   int64  // JSON-RPC id of the session/prompt request
	StartedAt  time.Time
	EndedAt    time.Time // zero while running
	State      turnState
	StopReason string     // set when State is turnCompleted
	Usage      *turnUsage // set when the agent reported usage
	Error      string     // set when State is turnFailed
}

// clone returns a copy that shares no memory with r.
func (r *turnRecord) clone() *turnRecord {
	if r == nil {
		return nil
	}
	out := *r
	if r.Usage != nil {
		usage := *r.Usage
		out.Usage = &usage
	}
	return &out
}

// turnOutcome is how a running turn ended.
type turnOutcome struct {
	state      turnState
	stopReason string
	usage      *turnUsage
	err        string
}

// promptOutcome decodes a session/prompt response into a turn outcome.
func promptOutcome(msg JSONRPCMessage) turnOutcome {
	if msg.Error != nil {
		return turnOutcome{state: turnFailed, err: msg.Error.Message}
	}
	var result promptResult
	if len(msg.Result) > 0 {
		if err := json.Unmarshal(msg.Result, &result); err != nil {
			return turnOutcome{state: turnFailed, err: fmt.Sprintf("decoding session/prompt result: %v", err)}
		}
	}
	return turnOutcome{state: turnCompleted, stopReason: result.StopReason, usage: result.Usage}
}

// newTurnID returns a random RFC 4122 version 4 UUID.
func newTurnID() string {
	var b [16]byte
	// crypto/rand.Read never returns an error; on failure it crashes the
	// program irrecoverably.
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:], b[10:])
	return string(out[:])
}

// startTurnLocked opens the turn record for prompt id. Caller must hold mu.
func (sc *sessionConn) startTurnLocked(id int64, now time.Time) {
	sc.currentTurn = &turnRecord{
		ID:        newTurnID(),
		PromptID:  id,
		StartedAt: now,
		State:     turnRunning,
	}
}

// endTurnLocked closes the running turn with outcome and makes it the last
// turn. Caller must hold mu.
func (sc *sessionConn) endTurnLocked(outcome turnOutcome, now time.Time) {
	turn := sc.currentTurn
	if turn == nil {
		return
	}
	sc.currentTurn = nil
	turn.EndedAt = now
	turn.State = outcome.state
	turn.StopReason = outcome.stopReason
	turn.Usage = outcome.usage
	turn.Error = outcome.err
	sc.lastTurn = turn
}

// turns returns copies of the running turn (nil when idle) and the last
// finished turn (nil before the first turn ends).
func (sc *sessionConn) turns() (current, last *turnRecord) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.currentTurn.clone(), sc.lastTurn.clone()
}

// errACPSessionStarting reports an idle query for a session whose handshake
// has not finished; it is neither idle nor gone.
var errACPSessionStarting = errors.New("ACP session is still starting")

// idleConn returns the in-process connection that can answer idle queries.
func (p *Provider) idleConn(name string) (*sessionConn, error) {
	p.mu.Lock()
	sc, ok := p.conns[name]
	p.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: ACP provider does not own session %q", runtime.ErrSessionNotFound, name)
	}
	if sc.cancel != nil {
		return nil, fmt.Errorf("%w: %q", errACPSessionStarting, name)
	}
	if !sc.usable() {
		return nil, fmt.Errorf("%w: ACP session %q has exited", runtime.ErrSessionNotFound, name)
	}
	return sc, nil
}

// usable reports whether the process is alive and its stdout is still being
// read. A drained connection can never finish another turn.
func (sc *sessionConn) usable() bool {
	if !sc.alive() {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return !sc.drained
}

// WaitForIdle blocks until the named session has no session/prompt response
// outstanding. It returns nil at once when the session is idle; otherwise it
// waits until the running turn ends, the timeout expires (an error wrapping
// [context.DeadlineExceeded]), ctx is done (ctx.Err()), or the agent exits
// (an error wrapping [runtime.ErrSessionNotFound]). A session this provider
// does not own in memory is reported as [runtime.ErrSessionNotFound].
// A turn waiting on a permission reply is busy.
func (p *Provider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	sc, err := p.idleConn(name)
	if err != nil {
		return err
	}
	sc.mu.Lock()
	sc.ensureIdleChannelLocked()
	busy := sc.activePromptID != 0
	idleCh := sc.idleCh
	sc.mu.Unlock()
	if !busy {
		return nil
	}
	if timeout <= 0 {
		return fmt.Errorf("ACP session %q is busy: %w", name, context.DeadlineExceeded)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idleCh:
		// drainPending also releases waiters when the agent exits; an exit
		// is not an idle boundary.
		if !sc.usable() {
			return fmt.Errorf("%w: ACP session %q exited while busy", runtime.ErrSessionNotFound, name)
		}
		return nil
	case <-sc.done:
		return fmt.Errorf("%w: ACP session %q exited while busy", runtime.ErrSessionNotFound, name)
	case <-timer.C:
		return fmt.Errorf("ACP session %q still busy after %s: %w", name, timeout, context.DeadlineExceeded)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SnapshotIdle reports whether the named session has no session/prompt
// response outstanding right now. A session this provider does not own, or
// whose agent has exited, is an error, never idle.
func (p *Provider) SnapshotIdle(name string) (bool, error) {
	sc, err := p.idleConn(name)
	if err != nil {
		return false, err
	}
	return !sc.isBusy(), nil
}
