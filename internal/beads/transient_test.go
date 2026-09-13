package beads

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsTransientConnError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "unrelated", err: errors.New("permission denied"), want: false},
		{name: "io timeout", err: fmt.Errorf("wrapped: %w", errors.New("i/o timeout during write")), want: true},
		{name: "invalid connection", err: fmt.Errorf("wrapped: %w", errors.New("invalid connection state")), want: true},
		{name: "bad connection", err: fmt.Errorf("wrapped: %w", errors.New("bad connection from pool")), want: true},
		{name: "connection reset", err: fmt.Errorf("wrapped: %w", errors.New("connection reset by peer")), want: true},
		{name: "broken pipe", err: fmt.Errorf("wrapped: %w", errors.New("broken pipe while talking to server")), want: true},
		{name: "timed out after", err: fmt.Errorf("wrapped: %w", errors.New("timed out after 5s")), want: true},
		{name: "deadline exceeded", err: fmt.Errorf("wrapped: %w", errors.New("deadline exceeded waiting for reply")), want: true},
		// The matcher lowercases before comparing, so a marker that arrives
		// capitalised from an upstream driver still classifies.
		{name: "mixed case marker", err: errors.New("dial failed: Broken Pipe"), want: true},
		{name: "upper case marker", err: errors.New("EOF: I/O TIMEOUT"), want: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsTransientConnError(tt.err); got != tt.want {
				t.Fatalf("IsTransientConnError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
