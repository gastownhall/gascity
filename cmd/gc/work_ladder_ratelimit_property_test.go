package main

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/workladder"
)

// The rate-limit episode end to end through the lane, over random worlds
// (#8132 C3; the core's own sims are internal/workladder's
// ratelimit_property_test.go): two to four seats on one or two providers
// whose pool serves, refuses only the conversation it bound to (a fresh one
// is served), or refuses everything, flipping at random; answers ending a
// second to twenty minutes after the nudge they answer; reads that fail;
// restarts the reconciler loses; and controller restarts. On every pass:
//
//   - RL3: a seat gets at most one fresh restart and one rate-limit alert per
//     episode (between two openings), and every rate-limit alert rides on its
//     episode's held or exhausted move.
//   - RL10: no two fresh restarts on one provider within ProviderFreshGap,
//     across controller restarts.
//   - RL4: no drain recycle is posted for a seat whose episode stands open
//     after the pass.
//   - The restart request lands only with its restarted move, and a seat is
//     never restarted while its provider is held unhealthy.
func TestWorkLadderRateLimitLaneProperty(t *testing.T) {
	seeds := 40
	if testing.Short() {
		seeds = 10
	}
	seen := map[string]int{} // every transition(cause) the worlds reached
	for seed := range seeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(uint64(seed), 8132))
			providers := []string{"glm", "opus"}[:1+rng.IntN(2)]
			names := make([]string, 2+rng.IntN(3))
			for i := range names {
				names[i] = providers[rng.IntN(len(providers))]
			}
			w := newRLWorld(t, false, names...)
			const (
				serving = iota
				sticky
				pool
			)
			state := map[string]int{}
			stickyConv := map[string]map[string]int64{} // provider -> seat -> the conversation it bound to
			flipAt := map[string]time.Time{}
			held := map[string]bool{}
			unreadable := map[*rlSeat]time.Time{}
			seatProvider := map[*rlSeat]string{}
			for i, s := range w.seats {
				seatProvider[s] = names[i]
				s.delay = time.Second + time.Duration(rng.Int64N(int64(20*time.Minute)))
				s.loseRestart = rng.IntN(10) == 0
				s.restartAfter = 1 + rng.IntN(3)
				w.claimFor(s, "step of "+s.name)
				if rng.IntN(3) == 0 {
					w.claimFor(s, "second step of "+s.name)
				}
				prov, seat := names[i], s
				s.refuse = func(conv int64) bool {
					switch state[prov] {
					case pool:
						return true
					case sticky:
						bound, ok := stickyConv[prov][seat.id]
						if !ok {
							if stickyConv[prov] == nil {
								stickyConv[prov] = map[string]int64{}
							}
							stickyConv[prov][seat.id], bound = conv, conv
						}
						return conv <= bound
					}
					return false
				}
			}
			for _, p := range providers {
				state[p], flipAt[p] = pool, w.now.Add(time.Duration(rng.Int64N(int64(3*time.Hour))))
			}
			healthWrite := func() {
				var hp []string
				for p, h := range held {
					if h {
						hp = append(hp, p)
					}
				}
				w.unhealthy(hp...)
			}

			type seatTrack struct{ restarts, alerts, moves int }
			track := map[*rlSeat]*seatTrack{}
			for _, s := range w.seats {
				track[s] = &seatTrack{}
			}
			lastRestart := map[string]time.Time{}
			for end := w.now.Add(8 * time.Hour); w.now.Before(end); w.now = w.now.Add(30 * time.Second) {
				for _, p := range providers {
					if !w.now.Before(flipAt[p]) {
						state[p] = rng.IntN(3)
						delete(stickyConv, p)
						flipAt[p] = w.now.Add(20*time.Minute + time.Duration(rng.Int64N(int64(3*time.Hour))))
					}
				}
				if rng.IntN(200) == 0 {
					p := providers[rng.IntN(len(providers))]
					held[p] = !held[p]
				}
				healthWrite()
				for _, s := range w.seats {
					if rng.IntN(400) == 0 { // its transcript moves out of reach for a while
						unreadable[s] = w.now.Add(time.Duration(rng.Int64N(int64(3 * time.Hour))))
					}
					s.readErr = nil
					if rng.IntN(10) == 0 || w.now.Before(unreadable[s]) {
						s.readErr = fmt.Errorf("transcript not found")
					}
				}
				if rng.IntN(50) == 0 {
					w.restartController()
				}
				posted := len(w.posted)
				w.step()

				for _, s := range w.seats {
					tr := track[s]
					moves := w.moves(s)
					for _, m := range moves[tr.moves:] {
						switch m.Transition {
						case events.WorkRateLimitedOpened:
							tr.restarts, tr.alerts = 0, 0
						case events.WorkRateLimitedRestarted:
							if tr.restarts++; tr.restarts > 1 {
								t.Fatalf("at %v: %s restarted twice in one episode: %v", w.now.Sub(w.start), s.name, w.transitions(s))
							}
							prov := seatProvider[s]
							if held[prov] {
								t.Fatalf("at %v: %s restarted while %s is held", w.now.Sub(w.start), s.name, prov)
							}
							if last, ok := lastRestart[prov]; ok && w.now.Sub(last) < workladder.DefaultProviderFreshGap {
								t.Fatalf("at %v: %s restarted %v after another seat on %s", w.now.Sub(w.start), s.name, w.now.Sub(last), prov)
							}
							lastRestart[prov] = w.now
							if w.rowOf(s).Metadata["restart_requested"] != "true" && len(s.restarts) == 0 {
								t.Fatalf("at %v: %s moved restarted with no request on its row", w.now.Sub(w.start), s.name)
							}
						}
						if m.Alerted {
							if tr.alerts++; tr.alerts > 1 {
								t.Fatalf("at %v: %s alerted twice in one episode: %v", w.now.Sub(w.start), s.name, w.transitions(s))
							}
						}
					}
					tr.moves = len(moves)
					if len(w.posted) > posted && w.phaseOf(s) != workladder.RateLimitClear {
						for _, r := range w.posted[posted:] {
							if r.ID == s.id {
								t.Fatalf("at %v: %s drained while its episode is %v", w.now.Sub(w.start), s.name, w.phaseOf(s))
							}
						}
					}
				}
			}
			for _, s := range w.seats {
				reasons, _ := w.alertsOf(s)
				var rate, alerted int
				for _, r := range reasons {
					if isRateLimitAlert(r) {
						rate++
					}
				}
				for _, m := range w.moves(s) {
					if m.Alerted {
						alerted++
					}
				}
				for _, tr := range w.transitions(s) {
					seen[tr]++
				}
				if rate != alerted {
					t.Fatalf("%s: %d rate-limit alerts, %d moves carrying one: %v", s.name, rate, alerted, w.transitions(s))
				}
			}
		})
	}
	// The worlds must reach every way an episode moves, or they prove
	// nothing about it.
	for _, want := range []string{"opened", "restarted", "exhausted(fresh_refused)", "exhausted(restart_lost)", "closed(served)", "closed(stale)"} {
		if seen[want] == 0 {
			t.Errorf("no world reached %s: %v", want, seen)
		}
	}
}
