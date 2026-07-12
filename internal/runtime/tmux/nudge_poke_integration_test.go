package tmux

import (
	"os"
	"testing"
	"time"
)

// TestNudgePokeRealTmux dogfoods the nudge-path poke fix (residual of #3049) against a
// REAL, isolated tmux server (its own -L socket, killed on cleanup — it never
// touches the gc tmux server or any live agent session). It proves what the
// pure discountPokeActivity unit tests cannot: that NudgeSession/NudgePane
// themselves record a poke, that the stamped timestamp brackets the LAST
// keystroke (not the call's start) so a slow send span — including
// submitEnterAndConfirm burning its full confirm budget on a never-busy pane
// — doesn't defeat the echo-window discount, and that failed/aborted nudges
// record nothing.
//
// recordPoke, SendKeysDebounced, discountPokeActivity, pokeEcho and pokeGrace
// are unchanged by this fix — only the nudge path (NudgeSession/NudgePane) is
// touched. See recordPokeAt.
func TestNudgePokeRealTmux(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server)")
	}

	t.Run("never-busy claude nudge burns full submitEnterAndConfirm budget, still stamps at completion", func(t *testing.T) {
		tm := NewTmuxWithConfig(Config{SocketName: "gcdogfoodnudgea", NudgeReadyTimeout: 10 * time.Second, NudgeLockTimeout: 10 * time.Second})
		const sess = "dogfood-neverbusy"
		_, _ = tm.run("kill-server")
		t.Cleanup(func() { _, _ = tm.run("kill-server") })
		if _, err := tm.run("new-session", "-d", "-s", sess, "-x", "80", "-y", "24"); err != nil {
			t.Skipf("cannot create tmux session (tmux unavailable?): %v", err)
		}
		// GC_PROVIDER=claude routes through submitVerifyEligible ->
		// submitEnterAndConfirm. The plain shell pane never shows Claude's busy
		// indicator, so this burns the FULL confirm budget
		// (submitEnterMaxSends=3 sends x submitConfirmPollsPerSend=4 polls x
		// submitConfirmPollInterval=150ms, plus submitReEnterBackoff=200ms
		// between sends) — well over pokeEcho(3s). A naive "stamp at entry"
		// implementation would bracket the wrong window here.
		if err := tm.SetEnvironment(sess, "GC_PROVIDER", "claude"); err != nil {
			t.Fatalf("SetEnvironment: %v", err)
		}
		time.Sleep(300 * time.Millisecond)

		callStart := time.Now()
		if err := tm.NudgeSession(sess, "# gc-nudge-neverbusy"); err != nil {
			t.Fatalf("NudgeSession: %v", err)
		}
		callEnd := time.Now()
		if callEnd.Sub(callStart) <= pokeEcho {
			t.Fatalf("call span %v did not exceed pokeEcho=%v; the never-busy confirm budget did not burn as expected", callEnd.Sub(callStart), pokeEcho)
		}

		tm.pokeMu.Lock()
		pk, ok := tm.pokes[sess]
		tm.pokeMu.Unlock()
		if !ok {
			t.Fatal("NudgeSession did not record a poke")
		}

		// The stamped `at` must sit near the END of the call (after the last
		// Enter's confirm polling), not near the start.
		if pk.at.Before(callStart.Add(pokeEcho)) {
			t.Errorf("pk.at = %v is too close to callStart %v; want stamped near completion after the full confirm budget", pk.at, callStart)
		}
		if pk.at.After(callEnd.Add(time.Second)) {
			t.Errorf("pk.at = %v is after callEnd %v by more than slack; want stamped at/near completion", pk.at, callEnd)
		}

		wa, err := tm.rawSessionActivity(sess)
		if err != nil {
			t.Fatalf("rawSessionActivity: %v", err)
		}
		// Real-data premise: the poke's own keystroke echo lands within
		// pokeEcho of pk.at because pk.at brackets the final Enter, despite the
		// overall call spanning far longer than pokeEcho.
		if d := wa.Sub(pk.at); d > pokeEcho || d < -pokeEcho {
			t.Fatalf("post-nudge window_activity %v is %v from poke %v; want within pokeEcho=%v (proves late stamping survives the full confirm budget)", wa, d, pk.at, pokeEcho)
		}
		// Unanswered nudge + grace elapsed -> genuine pre-nudge activity
		// revealed (load robustness: this is the case a naive top-of-function
		// stamp fails).
		if got := discountPokeActivity(wa, pk, pk.at.Add(pokeGrace+time.Second)); !got.Equal(pk.prior) {
			t.Errorf("unanswered never-busy nudge after grace = %v, want genuine prior %v (poke leaked through despite full-budget delivery)", got, pk.prior)
		}
	})

	t.Run("non-claude fallback success path records a poke", func(t *testing.T) {
		tm := NewTmuxWithConfig(Config{SocketName: "gcdogfoodnudgeb", NudgeReadyTimeout: 10 * time.Second, NudgeLockTimeout: 10 * time.Second})
		const sess = "dogfood-plain"
		_, _ = tm.run("kill-server")
		t.Cleanup(func() { _, _ = tm.run("kill-server") })
		if _, err := tm.run("new-session", "-d", "-s", sess, "-x", "80", "-y", "24"); err != nil {
			t.Skipf("cannot create tmux session (tmux unavailable?): %v", err)
		}
		time.Sleep(200 * time.Millisecond)

		// No GC_PROVIDER set and a plain shell pane: submitVerifyEligible is
		// false, so this exercises the fallback best-effort delivery path
		// (the "forgot the second return" footgun the bead calls out).
		if err := tm.NudgeSession(sess, "# gc-nudge-plain"); err != nil {
			t.Fatalf("NudgeSession: %v", err)
		}

		tm.pokeMu.Lock()
		_, ok := tm.pokes[sess]
		tm.pokeMu.Unlock()
		if !ok {
			t.Fatal("NudgeSession fallback success path did not record a poke")
		}
	})

	t.Run("genuine post-nudge turn is not discounted", func(t *testing.T) {
		tm := NewTmuxWithConfig(Config{SocketName: "gcdogfoodnudgec", NudgeReadyTimeout: 10 * time.Second, NudgeLockTimeout: 10 * time.Second})
		const sess = "dogfood-turn"
		_, _ = tm.run("kill-server")
		t.Cleanup(func() { _, _ = tm.run("kill-server") })
		if _, err := tm.run("new-session", "-d", "-s", sess, "-x", "80", "-y", "24"); err != nil {
			t.Skipf("cannot create tmux session (tmux unavailable?): %v", err)
		}
		time.Sleep(200 * time.Millisecond)

		if err := tm.NudgeSession(sess, "# gc-nudge-turn"); err != nil {
			t.Fatalf("NudgeSession: %v", err)
		}
		tm.pokeMu.Lock()
		pk, ok := tm.pokes[sess]
		tm.pokeMu.Unlock()
		if !ok {
			t.Fatal("NudgeSession did not record a poke")
		}

		// Wait past the echo window, then produce genuine pane output — a
		// real agent turn responding to the nudge.
		time.Sleep(pokeEcho + 2*time.Second)
		if _, err := tm.run("send-keys", "-t", sess, "-l", "echo real-turn-after-nudge"); err != nil {
			t.Fatalf("send-keys real turn: %v", err)
		}
		_, _ = tm.run("send-keys", "-t", sess, "Enter")
		time.Sleep(1 * time.Second)

		wa2, err := tm.rawSessionActivity(sess)
		if err != nil {
			t.Fatalf("rawSessionActivity: %v", err)
		}
		if wa2.Sub(pk.at) <= pokeEcho {
			t.Fatalf("real turn at %v not past poke echo %v (need > pokeEcho=%v)", wa2, pk.at, pokeEcho)
		}
		if got := discountPokeActivity(wa2, pk, pk.at.Add(pokeGrace+time.Second)); !got.Equal(wa2) {
			t.Errorf("genuine post-nudge turn was discounted = %v, want raw %v", got, wa2)
		}
	})

	t.Run("error, lock-timeout, and failed-send paths record no poke", func(t *testing.T) {
		tm := NewTmuxWithConfig(Config{SocketName: "gcdogfoodnudged", NudgeReadyTimeout: 300 * time.Millisecond, NudgeLockTimeout: 300 * time.Millisecond})
		_, _ = tm.run("kill-server")
		t.Cleanup(func() { _, _ = tm.run("kill-server") })
		if _, err := tm.run("new-session", "-d", "-s", "dogfood-anchor", "-x", "80", "-y", "24"); err != nil {
			t.Skipf("cannot create tmux session (tmux unavailable?): %v", err)
		}

		// (a) session-not-found: sendKeysLiteralWithRetry fails non-transiently.
		const missing = "dogfood-does-not-exist"
		if err := tm.NudgeSession(missing, "# gc-nudge-missing"); err == nil {
			t.Fatal("NudgeSession on a nonexistent session succeeded, want error")
		}
		tm.pokeMu.Lock()
		_, ok := tm.pokes[missing]
		tm.pokeMu.Unlock()
		if ok {
			t.Error("NudgeSession recorded a poke despite session-not-found failure")
		}

		// (b) lock timeout: hold the nudge lock, then attempt to nudge.
		const locked = "dogfood-locked"
		if _, err := tm.run("new-session", "-d", "-s", locked, "-x", "80", "-y", "24"); err != nil {
			t.Fatalf("new-session locked: %v", err)
		}
		if !acquireNudgeLock(locked, time.Second) {
			t.Fatal("could not acquire nudge lock for setup")
		}
		defer releaseNudgeLock(locked)

		if err := tm.NudgeSession(locked, "# gc-nudge-locked"); err == nil {
			t.Fatal("NudgeSession succeeded despite held lock, want lock-timeout error")
		}
		tm.pokeMu.Lock()
		_, ok = tm.pokes[locked]
		tm.pokeMu.Unlock()
		if ok {
			t.Error("NudgeSession recorded a poke despite lock-timeout failure")
		}
	})

	t.Run("NudgePane success path records a poke", func(t *testing.T) {
		tm := NewTmuxWithConfig(Config{SocketName: "gcdogfoodnudgee", NudgeReadyTimeout: 10 * time.Second, NudgeLockTimeout: 10 * time.Second})
		const sess = "dogfood-pane"
		_, _ = tm.run("kill-server")
		t.Cleanup(func() { _, _ = tm.run("kill-server") })
		if _, err := tm.run("new-session", "-d", "-s", sess, "-x", "80", "-y", "24"); err != nil {
			t.Skipf("cannot create tmux session (tmux unavailable?): %v", err)
		}
		time.Sleep(200 * time.Millisecond)

		paneID, err := tm.run("display-message", "-t", sess, "-p", "#{pane_id}")
		if err != nil {
			t.Fatalf("display-message pane_id: %v", err)
		}
		if len(paneID) == 0 {
			t.Fatal("empty pane_id")
		}

		if err := tm.NudgePane(paneID, "# gc-nudge-pane"); err != nil {
			t.Fatalf("NudgePane: %v", err)
		}

		tm.pokeMu.Lock()
		_, ok := tm.pokes[paneID]
		tm.pokeMu.Unlock()
		if !ok {
			t.Fatal("NudgePane did not record a poke")
		}
	})
}
