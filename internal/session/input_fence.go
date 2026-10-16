package session

import (
	"context"
	"fmt"
	"strconv"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// InputFenceFor is the input fence of the incarnation info's row names: its
// instance token verbatim (false when the row has none), and its generation
// and continuation epoch read with strconv.Atoi, a missing, malformed or
// non-positive value reading as DefaultGeneration or
// DefaultContinuationEpoch, exactly as ensureRunning and the controller's
// prepared start publish them in the runtime environment (RuntimeEnv). It
// is the fence of the runtime that row's start launched only while the row
// still names that start.
func InputFenceFor(info Info) (runtime.InputFence, bool) {
	if info.InstanceToken == "" {
		return runtime.InputFence{}, false
	}
	return runtime.InputFence{
		Generation: int64(positiveOr(info.Generation, DefaultGeneration)),
		Epoch:      int64(positiveOr(info.ContinuationEpoch, DefaultContinuationEpoch)),
		Token:      info.InstanceToken,
	}, true
}

func positiveOr(raw string, fallback int) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// InputFencing reports how the session runtime fences session id's input
// (runtime.InputFencingFor its runtime name).
func (m *Manager) InputFencing(id string) (runtime.InputFencing, error) {
	_, sessName, err := m.sessionBead(id)
	if err != nil {
		return "", err
	}
	return runtime.InputFencingFor(m.sp, sessName), nil
}

// StopTurnFenced is StopTurn with every key and interrupt it sends fenced
// to f's incarnation. A runtime that cannot fence the session is refused
// with runtime.ErrInputFenceUnsupported before anything is sent, and a miss
// is a *runtime.FenceMismatchError. Unlike StopTurn, which goes by the
// runtime's IsRunning, it reads the session fresh before the stop and again
// before its first input, and maps what it finds as
// runtime.LivenessFenceMiss does: a session that is not running, or stops
// before the first input, is a miss finding it absent
// (runtime.FenceFoundAbsent), one whose agent exited a miss finding it dead,
// and a read that cannot answer is runtime.ErrRuntimeUnavailable, never a
// miss. A suspended row is a miss finding it absent, even while its runtime
// is still stopping. A fenced call never reports success into an
// incarnation that is not there.
func (m *Manager) StopTurnFenced(ctx context.Context, id string, f runtime.InputFence) error {
	if err := f.Validate(); err != nil {
		return fmt.Errorf("stopping the turn of %s: %w", id, err)
	}
	return m.stopTurn(runtime.WithInputFence(ctx, f), id)
}

// stopFindsNoTurn reports whether a stop of sessName finds no turn to
// interrupt and, if so, what the stop returns. A suspended row has no turn:
// the stop returns nil, or under an input fence in ctx a miss finding it
// absent. Unfenced, a runtime the provider's IsRunning reads not running has
// no turn either, and the stop returns nil. Under a fence a fresh liveness
// read decides instead, because the tmux state cache can read a live session
// not running: a read that does not find the agent alive is no turn, and the
// stop returns its runtime.LivenessFenceMiss.
func (m *Manager) stopFindsNoTurn(ctx context.Context, b beads.Bead, sessName string) (bool, error) {
	_, fenced := runtime.InputFenceFrom(ctx)
	switch {
	case State(b.Metadata["state"]) == StateSuspended:
		return true, runtime.LivenessFenceMiss(ctx, sessName, runtime.Liveness{}, nil)
	case !fenced:
		return !m.sp.IsRunning(sessName), nil
	}
	live, err := m.freshLiveness(sessName, nil)
	miss := runtime.LivenessFenceMiss(ctx, sessName, live, err)
	return miss != nil, miss
}
