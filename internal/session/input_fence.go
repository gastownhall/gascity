package session

import (
	"context"
	"fmt"
	"strconv"

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
// is a *runtime.FenceMismatchError. Unlike StopTurn, a session that is not
// running is a miss finding it absent (runtime.FenceFoundAbsent): a fenced
// call never reports success into an incarnation that is not there.
func (m *Manager) StopTurnFenced(ctx context.Context, id string, f runtime.InputFence) error {
	if err := f.Validate(); err != nil {
		return fmt.Errorf("stopping the turn of %s: %w", id, err)
	}
	return m.stopTurn(runtime.WithInputFence(ctx, f), id)
}
