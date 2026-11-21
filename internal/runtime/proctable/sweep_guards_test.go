package proctable

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/nudgepoller"
)

// Guards the process-table orphan sweep relies on (CONTRACT C4).

// Kills: an unfenced nudge poller, which inherits its spawner's session env
// and outlives it by design; and a fence so broad it spares any `gc nudge`.
func TestIsCityInfrastructureArgvFencesNudgePoller(t *testing.T) {
	poller := append([]string{"/usr/local/bin/gc"}, nudgepoller.CommandArgs("/city", "worker", "mayor")...)
	for _, tc := range []struct {
		name string
		argv []string
		want bool
	}{
		{name: "nudge poller", argv: poller, want: true},
		{name: "nudge send", argv: []string{"gc", "nudge", "send", "mayor"}, want: false},
		{name: "bare nudge", argv: []string{"gc", "nudge"}, want: false},
		{name: "poll elsewhere", argv: []string{"gc", "mail", "poll"}, want: false},
	} {
		if got := IsCityInfrastructureArgv(tc.argv); got != tc.want {
			t.Errorf("%s: IsCityInfrastructureArgv(%q) = %v, want %v", tc.name, tc.argv, got, tc.want)
		}
	}
}

// Kills: a whole-scan classifier that counts unreadable entries (which only
// drop those processes) as a failed tracking read, or misses a listing or
// tracking failure joined beside them or wrapped by a composite provider.
func TestHasWholeScanFailure(t *testing.T) {
	entry := &EntryError{PID: 7, Err: errors.New("permission denied")}
	listing := errors.New("tmux list running: timed out")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "clean", err: nil, want: false},
		{name: "entries only", err: errors.Join(entry, &EntryError{PID: 8, Err: errors.New("gone")}), want: false},
		{name: "wrapped entries only", err: fmt.Errorf("default backend: %w", errors.Join(entry)), want: false},
		{name: "listing", err: listing, want: true},
		{name: "entries and listing", err: errors.Join(entry, listing), want: true},
		{name: "wrapped entries and listing", err: fmt.Errorf("default backend: %w", errors.Join(entry, listing)), want: true},
	} {
		if got := HasWholeScanFailure(tc.err); got != tc.want {
			t.Errorf("%s: HasWholeScanFailure = %v, want %v", tc.name, got, tc.want)
		}
	}
}
