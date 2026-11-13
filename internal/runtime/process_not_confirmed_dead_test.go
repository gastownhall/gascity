package runtime

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// Kills: IsSessionGone certifying a still-live process as gone. The sentinel
// wins over every session-gone marker, however it is combined with one.
func TestIsSessionGoneRefusesProcessNotConfirmedDead(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"bare sentinel", ErrProcessNotConfirmedDead, false},
		{"wrapped sentinel", fmt.Errorf("stop %q: %w", "w", ErrProcessNotConfirmedDead), false},
		{"joined with a not-found string", errors.Join(ErrProcessNotConfirmedDead, errors.New("sh: foo: not found")), false},
		{"sentinel inside a not-found message", fmt.Errorf("sh: foo: not found: %w", ErrProcessNotConfirmedDead), false},
		{"sentinel inside a session-not-found message", fmt.Errorf("session not found while reaping: %w", ErrProcessNotConfirmedDead), false},
		{"wrapping ErrSessionNotFound and the sentinel", fmt.Errorf("%w: %w", ErrSessionNotFound, ErrProcessNotConfirmedDead), false},
		{"unrelated session-not-found error", errors.New("tmux: session not found: w"), true},
		{"ErrSessionNotFound", ErrSessionNotFound, true},
	}
	for _, tt := range tests {
		if got := IsSessionGone(tt.err); got != tt.want {
			t.Errorf("%s: IsSessionGone(%v) = %v, want %v", tt.name, tt.err, got, tt.want)
		}
	}
}

// Kills: a stop-reply token that collides with the success reply, or that a
// line scanner would split.
func TestStopReplyNotConfirmedDeadIsADistinctSingleToken(t *testing.T) {
	if StopReplyNotConfirmedDead == "" || StopReplyNotConfirmedDead == "ok" {
		t.Fatalf("StopReplyNotConfirmedDead = %q, want a non-empty token distinct from ok", StopReplyNotConfirmedDead)
	}
	if strings.ContainsAny(StopReplyNotConfirmedDead, " \t\r\n") {
		t.Fatalf("StopReplyNotConfirmedDead = %q, want a single line token without whitespace", StopReplyNotConfirmedDead)
	}
}
