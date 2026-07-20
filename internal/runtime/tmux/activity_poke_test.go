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

// TestPokePriorBaseline covers the chained-nudge fix: when gc records a new poke
// while an earlier poke is still unanswered (raw window_activity is only that
// poke's own echo), the new poke's prior must carry the earlier genuine baseline
// forward instead of snapshotting gc's own echo.
func TestPokePriorBaseline(t *testing.T) {
	genuine := time.Date(2026, 6, 4, 1, 0, 0, 0, time.UTC) // last real agent turn
	poke := genuine.Add(time.Hour)                         // an earlier gc poke

	tests := []struct {
		name    string
		raw     time.Time
		pk      pokeInfo
		hasPoke bool
		want    time.Time
	}{
		{
			name:    "no prior poke -> raw stands",
			raw:     genuine,
			pk:      pokeInfo{},
			hasPoke: false,
			want:    genuine,
		},
		{
			name:    "chained unanswered poke, still echo-only -> carry prior forward",
			raw:     poke, // window_activity is only the earlier poke's echo
			pk:      pokeInfo{at: poke, prior: genuine},
			hasPoke: true,
			want:    genuine, // not the echo `poke`
		},
		{
			name:    "agent answered since the poke (raw past echo window) -> raw stands",
			raw:     poke.Add(30 * time.Second),
			pk:      pokeInfo{at: poke, prior: genuine},
			hasPoke: true,
			want:    poke.Add(30 * time.Second),
		},
		{
			name:    "earlier poke without a prior snapshot -> raw stands",
			raw:     poke,
			pk:      pokeInfo{at: poke},
			hasPoke: true,
			want:    poke,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pokePriorBaseline(tc.raw, tc.pk, tc.hasPoke)
			if !got.Equal(tc.want) {
				t.Errorf("pokePriorBaseline(raw=%v, pk=%+v, hasPoke=%v) = %v, want %v",
					tc.raw, tc.pk, tc.hasPoke, got, tc.want)
			}
		})
	}
}

// TestChainedUnansweredNudgesPreserveGenuinePrior walks the full chained-nudge
// sequence the codex review flagged: two unanswered nudges inside pokeGrace must
// not let last_active advance to gc's own earlier nudge echo. It composes the
// prior-selection (pokePriorBaseline) with the discount (discountPokeActivity)
// so the regression is covered without a real tmux server.
func TestChainedUnansweredNudgesPreserveGenuinePrior(t *testing.T) {
	genuine := time.Date(2026, 6, 4, 1, 0, 0, 0, time.UTC) // last real agent turn
	t0 := genuine.Add(30 * time.Minute)                    // first nudge
	t1 := t0.Add(pokeGrace / 3)                            // second nudge, still in grace, no answer

	// First nudge: nothing recorded yet; raw activity is the genuine turn.
	prior1 := pokePriorBaseline(genuine, pokeInfo{}, false)
	if !prior1.Equal(genuine) {
		t.Fatalf("first nudge prior = %v, want genuine %v", prior1, genuine)
	}
	poke1 := pokeInfo{at: t0, prior: prior1}

	// Second (chained, unanswered) nudge within pokeGrace: raw activity is still
	// only the first nudge's echo (~t0). The carry-forward must reject that echo
	// and keep the genuine baseline.
	raw2 := t0
	prior2 := pokePriorBaseline(raw2, poke1, true)
	if !prior2.Equal(genuine) {
		t.Fatalf("chained nudge prior = %v, want genuine %v (must not ratchet to gc's echo %v)", prior2, genuine, t0)
	}
	poke2 := pokeInfo{at: t1, prior: prior2}

	// After the second grace elapses with still no answer, last_active must
	// resolve to the genuine turn, not gc's earlier nudge echo.
	raw3 := t1 // still just the second nudge echo
	if got := discountPokeActivity(raw3, poke2, t1.Add(pokeGrace+time.Second)); !got.Equal(genuine) {
		t.Errorf("last_active after chained unanswered nudges = %v, want genuine %v", got, genuine)
	}

	// Regression guard: the pre-fix baseline (snapshot the in-grace echo) would
	// have surfaced gc's own nudge as last_active, so this test must fail against
	// the buggy behavior.
	buggyPoke2 := pokeInfo{at: t1, prior: raw2}
	if leaked := discountPokeActivity(raw3, buggyPoke2, t1.Add(pokeGrace+time.Second)); leaked.Equal(genuine) {
		t.Fatalf("regression guard ineffective: buggy baseline %v also yielded genuine %v", raw2, genuine)
	}
}
