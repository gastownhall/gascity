package tmux

import (
	"os"
	"testing"
	"time"
)

// TestPokeActivityRealTmux dogfoods the #3049 fix against a REAL, isolated tmux
// server (its own -L socket, killed on cleanup — it never touches the gc tmux
// server or any live agent session). TestDiscountPokeActivity already covers the
// pure decision logic with synthetic times; this proves the real-tmux integration
// the unit test cannot:
//
//   - SendKeysDebounced's poke advances tmux #{window_activity} into the echo
//     window (within pokeEcho), which is the premise the whole discount rests on;
//   - recordPoke snapshots the genuine pre-poke activity; and
//   - genuine pane output AFTER the echo window is NOT discounted (a real turn).
func TestPokeActivityRealTmux(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server)")
	}
	tm := NewTmuxWithConfig(Config{SocketName: "gcdogfood3049"})
	const sess = "dogfood"
	_, _ = tm.run("kill-server") // clean slate on this isolated socket; ignore if none
	if _, err := tm.run("new-session", "-d", "-s", sess, "-x", "80", "-y", "24"); err != nil {
		t.Skipf("cannot create tmux session (tmux unavailable?): %v", err)
	}
	t.Cleanup(func() { _, _ = tm.run("kill-server") })

	// A genuine "turn": pane output. Let #{window_activity} settle.
	if _, err := tm.run("send-keys", "-t", sess, "-l", "echo genuine-turn"); err != nil {
		t.Fatalf("send-keys genuine: %v", err)
	}
	_, _ = tm.run("send-keys", "-t", sess, "Enter")
	time.Sleep(2 * time.Second)

	expectedPrior, err := tm.rawSessionActivity(sess)
	if err != nil {
		t.Fatalf("rawSessionActivity prior: %v", err)
	}

	// Space the poke clearly after the genuine turn (no pane activity in between).
	time.Sleep(2 * time.Second)

	// THE POKE — the gc wake/nudge path. "# ..." is a bash no-op (comment): it
	// advances #{window_activity} via the keystroke echo but produces no turn,
	// exactly like a wake landing on a parked agent that never replies.
	if err := tm.SendKeysDebounced(sess, "# gc-wake", 50); err != nil {
		t.Fatalf("SendKeysDebounced poke: %v", err)
	}

	wa, err := tm.rawSessionActivity(sess)
	if err != nil {
		t.Fatalf("rawSessionActivity post-poke: %v", err)
	}

	tm.pokeMu.Lock()
	pk, ok := tm.pokes[sess]
	tm.pokeMu.Unlock()
	if !ok {
		t.Fatal("SendKeysDebounced did not record a poke")
	}

	// 1. recordPoke snapshotted the genuine pre-poke activity.
	if !pk.prior.Equal(expectedPrior) {
		t.Errorf("pk.prior = %v, want pre-poke activity %v", pk.prior, expectedPrior)
	}
	// 2. REAL-DATA PREMISE: the poke advanced #{window_activity} into the echo
	//    window (allow tmux's 1s timestamp granularity around pokeEcho).
	if d := wa.Sub(pk.at); d > pokeEcho || d < -pokeEcho {
		t.Errorf("post-poke window_activity %v is %v from poke %v; want within pokeEcho=%v", wa, d, pk.at, pokeEcho)
	}
	// 3. Unanswered poke + grace elapsed -> genuine pre-poke time revealed.
	if got := discountPokeActivity(wa, pk, pk.at.Add(pokeGrace+time.Second)); !got.Equal(pk.prior) {
		t.Errorf("unanswered poke after grace = %v, want genuine prior %v (poke leaked through)", got, pk.prior)
	}
	// 4. Still in grace -> raw stands (don't prematurely flip a responsive agent).
	if got := discountPokeActivity(wa, pk, pk.at.Add(pokeGrace/2)); !got.Equal(wa) {
		t.Errorf("poke still in grace = %v, want raw %v", got, wa)
	}

	// 5. A genuine turn AFTER the echo window must NOT be discounted.
	time.Sleep(pokeEcho + 2*time.Second)
	if _, err := tm.run("send-keys", "-t", sess, "-l", "echo real-post-poke-turn"); err != nil {
		t.Fatalf("send-keys real turn: %v", err)
	}
	_, _ = tm.run("send-keys", "-t", sess, "Enter")
	time.Sleep(1 * time.Second)
	wa2, err := tm.rawSessionActivity(sess)
	if err != nil {
		t.Fatalf("rawSessionActivity post-turn: %v", err)
	}
	if wa2.Sub(pk.at) <= pokeEcho {
		t.Fatalf("real turn at %v not past poke echo %v (need > pokeEcho=%v)", wa2, pk.at, pokeEcho)
	}
	if got := discountPokeActivity(wa2, pk, pk.at.Add(pokeGrace+time.Second)); !got.Equal(wa2) {
		t.Errorf("genuine post-poke turn was discounted = %v, want raw %v", got, wa2)
	}

	t.Logf("real-tmux OK: prior=%d poke=%d echo_wa=%d (Δ%v) turn_wa=%d (Δ%v)",
		pk.prior.Unix(), pk.at.Unix(), wa.Unix(), wa.Sub(pk.at), wa2.Unix(), wa2.Sub(pk.at))
}
