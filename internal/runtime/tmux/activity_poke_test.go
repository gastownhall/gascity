package tmux

import (
	"testing"
	"time"
)

// TestDiscountPokeActivity covers the core fix: gc's own send-keys (wakes/nudges)
// must not make a woken-but-unresponsive agent look active. The agent only
// "did work" when it produces output AFTER the poke; an unanswered poke, once
// the grace elapses, must reveal the genuine pre-poke activity.
func TestDiscountPokeActivity(t *testing.T) {
	turn := time.Date(2026, 6, 4, 1, 0, 0, 0, time.UTC) // last real turn
	poke := turn.Add(5 * time.Hour)                     // gc poked 5h after that turn

	tests := []struct {
		name string
		wa   time.Time // raw tmux window_activity
		pk   pokeInfo
		now  time.Time
		want time.Time
	}{
		{
			name: "no poke recorded -> raw stands",
			wa:   poke,
			pk:   pokeInfo{},
			now:  poke.Add(time.Minute),
			want: poke,
		},
		{
			name: "poke without prior snapshot -> raw stands",
			wa:   poke,
			pk:   pokeInfo{at: poke},
			now:  poke.Add(pokeGrace + time.Second),
			want: poke,
		},
		{
			name: "echo only + grace elapsed -> prior (unresponsive agent looks idle)",
			wa:   poke, // window_activity is just the poke's echo
			pk:   pokeInfo{at: poke, prior: turn},
			now:  poke.Add(pokeGrace + time.Second),
			want: turn, // revealed: genuine activity is 5h old
		},
		{
			name: "echo only + still in grace -> raw (give the agent time to reply)",
			wa:   poke,
			pk:   pokeInfo{at: poke, prior: turn},
			now:  poke.Add(pokeGrace / 2),
			want: poke,
		},
		{
			name: "agent produced output after the poke -> raw (a real turn)",
			wa:   poke.Add(30 * time.Second), // output well past the echo window
			pk:   pokeInfo{at: poke, prior: turn},
			now:  poke.Add(2 * time.Minute),
			want: poke.Add(30 * time.Second),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := discountPokeActivity(tc.wa, tc.pk, tc.now)
			if !got.Equal(tc.want) {
				t.Errorf("discountPokeActivity(wa=%v, pk=%+v, now=%v) = %v, want %v",
					tc.wa, tc.pk, tc.now, got, tc.want)
			}
		})
	}
}
