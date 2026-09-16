//go:build darwin

package proctable

import (
	"errors"
	"os"
	"testing"
)

func TestIsInfrastructureKillTargetClassifiesArgv(t *testing.T) {
	prev := killTargetCmdline
	t.Cleanup(func() { killTargetCmdline = prev })

	cases := []struct {
		name string
		argv []string
		err  error
		want bool
	}{
		{name: "bare tmux", argv: []string{"tmux"}, want: true},
		{name: "tmux by path with the founding new-session argv", argv: []string{"/opt/homebrew/bin/tmux", "-u", "-L", "gc", "new-session", "-d"}, want: true},
		{name: "retitled server", argv: []string{"tmux: server"}, want: true},
		{name: "tmux-named wrapper stays reapable", argv: []string{"tmux-wrapper"}, want: false},
		{name: "agent", argv: []string{"/usr/local/bin/claude", "--resume"}, want: false},
		{name: "empty argv fails open", argv: nil, want: false},
		{name: "unreadable fails open", err: errors.New("procargs2: operation not permitted"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			killTargetCmdline = func(int) ([]string, error) { return tc.argv, tc.err }
			if got := isInfrastructureKillTarget(4242); got != tc.want {
				t.Fatalf("isInfrastructureKillTarget(%v, %v) = %v, want %v", tc.argv, tc.err, got, tc.want)
			}
		})
	}
}

// TestIsInfrastructureKillTargetHostSmoke reads the live host once: the test
// binary itself must not classify as tmux infrastructure.
func TestIsInfrastructureKillTargetHostSmoke(t *testing.T) {
	if isInfrastructureKillTarget(os.Getpid()) {
		t.Fatal("the test binary classified as tmux infrastructure")
	}
}
