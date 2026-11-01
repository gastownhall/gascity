package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workladder"
)

// A reading reduces the seat's turns to what the ladder judges progress and
// rate limits by: whether the newest ended turn failed (whatever the cause,
// with no turn open), when the newest turn the provider completed ended, and
// how many of the newest ended turns in a row the provider refused with a
// rate limit (none while a turn is open), the newest's end and whether its
// source classed it a spent quota. The refusal's failure rides beside it for
// events, never into the reading.
func TestLadderTurnReading(t *testing.T) {
	at := stepsTestNow
	completed := runtime.EndedTurn{TurnID: "ok", EndedAt: at.Add(-time.Hour), Status: runtime.TurnStatusCompleted}
	limited := runtime.EndedTurn{TurnID: "429", EndedAt: at, Status: runtime.TurnStatusFailed, Failure: runtime.TurnFailure{Code: "rate_limit", HTTPStatus: 429, RateLimited: true}}
	gateway := runtime.EndedTurn{TurnID: "524", EndedAt: at, Status: runtime.TurnStatusFailed, Failure: runtime.TurnFailure{HTTPStatus: 524}}
	noStatus := runtime.EndedTurn{TurnID: "gemini", EndedAt: at}
	canceled := runtime.EndedTurn{TurnID: "esc", EndedAt: at, Status: runtime.TurnStatusCancelled}
	inferred := runtime.EndedTurn{TurnID: "idle", EndedAt: at, Inferred: true}
	open := runtime.TurnState{TurnID: "open"}
	earlier := limited
	earlier.TurnID, earlier.EndedAt = "429-earlier", at.Add(-time.Minute)
	spent := runtime.EndedTurn{TurnID: "quota", EndedAt: at, Status: runtime.TurnStatusFailed, Failure: runtime.TurnFailure{Code: "usage_limit_exceeded", RateLimited: true, Quota: true, Message: "You've hit your usage limit."}}
	for _, tc := range []struct {
		name    string
		hist    runtime.TurnHistory
		want    workladder.TurnReading
		refusal runtime.TurnFailure
	}{
		{"newest completed", runtime.TurnHistory{Recent: []runtime.EndedTurn{completed}}, workladder.TurnReading{Read: true, Conv: 3, LastCompleted: true, ServedAt: completed.EndedAt}, runtime.TurnFailure{}},
		{"newest completed after a refusal", runtime.TurnHistory{Recent: []runtime.EndedTurn{completed, limited}}, workladder.TurnReading{Read: true, Conv: 3, LastCompleted: true, ServedAt: completed.EndedAt}, runtime.TurnFailure{}},
		{"newest refused (429), served behind it", runtime.TurnHistory{Recent: []runtime.EndedTurn{limited, completed}}, workladder.TurnReading{Read: true, Conv: 3, LastFailed: true, ServedAt: completed.EndedAt, Refused: 1, RefusedAt: at}, limited.Failure},
		{"two refused in a row", runtime.TurnHistory{Recent: []runtime.EndedTurn{limited, earlier, completed}}, workladder.TurnReading{Read: true, Conv: 3, LastFailed: true, ServedAt: completed.EndedAt, Refused: 2, RefusedAt: at}, limited.Failure},
		{"a 524 breaks the run", runtime.TurnHistory{Recent: []runtime.EndedTurn{limited, gateway, earlier}}, workladder.TurnReading{Read: true, Conv: 3, LastFailed: true, Refused: 1, RefusedAt: at}, limited.Failure},
		{"a spent quota", runtime.TurnHistory{Recent: []runtime.EndedTurn{spent, limited}}, workladder.TurnReading{Read: true, Conv: 3, LastFailed: true, Refused: 2, RefusedAt: at, Quota: true}, spent.Failure},
		{"newest failed (524), never served", runtime.TurnHistory{Recent: []runtime.EndedTurn{gateway, limited}}, workladder.TurnReading{Read: true, Conv: 3, LastFailed: true}, runtime.TurnFailure{}},
		{"no status (Gemini) is no failure, and no completion", runtime.TurnHistory{Recent: []runtime.EndedTurn{noStatus, limited}}, workladder.TurnReading{Read: true, Conv: 3}, runtime.TurnFailure{}},
		{"an inferred end with no status after a completed turn is not completed", runtime.TurnHistory{Recent: []runtime.EndedTurn{inferred, completed}}, workladder.TurnReading{Read: true, Conv: 3, ServedAt: completed.EndedAt}, runtime.TurnFailure{}},
		{"canceled is neither", runtime.TurnHistory{Recent: []runtime.EndedTurn{canceled}}, workladder.TurnReading{Read: true, Conv: 3}, runtime.TurnFailure{}},
		{"a turn open has no failure yet", runtime.TurnHistory{Current: open, Recent: []runtime.EndedTurn{limited, completed}}, workladder.TurnReading{Read: true, Conv: 3, Open: true, ServedAt: completed.EndedAt}, runtime.TurnFailure{}},
		{"a turn open after a completed one is open, not completed", runtime.TurnHistory{Current: open, Recent: []runtime.EndedTurn{completed}}, workladder.TurnReading{Read: true, Conv: 3, Open: true, ServedAt: completed.EndedAt}, runtime.TurnFailure{}},
		{"nothing yet", runtime.TurnHistory{}, workladder.TurnReading{Read: true, Conv: 3}, runtime.TurnFailure{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ladderTurnReading(tc.hist, 3); got != tc.want {
				t.Fatalf("reading = %+v, want %+v", got, tc.want)
			}
			if got := ladderRefusal(tc.hist); got != tc.refusal {
				t.Fatalf("refusal = %+v, want %+v", got, tc.refusal)
			}
		})
	}
}

// fakeLadderTurns is a turn source over a seat's history, counting reads.
type fakeLadderTurns struct {
	hist  runtime.TurnHistory
	err   error
	reads int
}

func (f *fakeLadderTurns) install(c *ladderCity) {
	c.lane.turnReaderFor = func(externalReadsEnv) ladderTurnReader {
		return func(context.Context, beads.Bead) (runtime.TurnHistory, error) {
			f.reads++
			return f.hist, f.err
		}
	}
}

// answerDelay is how long after a nudge answeredSeat's seat ends the turn
// that answers it: a slow turn, ending past ProgressReset.
const answerDelay = 12 * time.Minute

// answeredSeat is a started seat whose every nudge is answered, answerDelay
// later, by a turn that ends with status (and failure), its end the seat's
// newest activity; its turns are read when read is set. It runs until the
// ladder recycles the seat or restarts it fresh, or for eight hours.
func answeredSeat(t *testing.T, status runtime.TurnStatus, failure runtime.TurnFailure, read bool) *ladderCity {
	t.Helper()
	c := newLadderCity(t)
	id := c.claim("the step")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
	turns := &fakeLadderTurns{}
	if read {
		turns.install(c)
	}
	for end := c.now.Add(8 * time.Hour); c.now.Before(end) && len(c.posted) == 0 && c.row().Metadata["restart_requested"] == ""; c.now = c.now.Add(30 * time.Second) {
		turns.hist = runtime.TurnHistory{}
		for _, n := range c.nudgedAt {
			at := c.start.Add(n + answerDelay)
			if at.After(c.now) {
				continue
			}
			turn := runtime.EndedTurn{TurnID: fmt.Sprint(n), EndedAt: at, Status: status}
			if status == runtime.TurnStatusFailed {
				turn.Failure = failure
			}
			turns.hist.Recent = append([]runtime.EndedTurn{turn}, turns.hist.Recent...)
			c.sp.SetActivity(ladderSeatName, at)
		}
		c.pass()
	}
	return c
}

// C1. A seat whose every answer is a turn its provider failed, ending 12
// minutes after the nudge it answers (a slow 429, a slow Cloudflare 524), is
// not making progress. A slow 524 climbs the ladder to its recycle; a slow
// 429 is a rate-limit episode (C3), restarted fresh once confirmed, never
// recycled. Rolled back (no turn reads), each failure is activity past the
// reset and the seat is nudged forever, never recycled or escalated.
func TestWorkLadderFailedTurnIsNotProgress(t *testing.T) {
	for name, failure := range map[string]runtime.TurnFailure{
		"429": {Code: "rate_limit", HTTPStatus: 429, RateLimited: true, Message: "API Error: Request rejected (429)"},
		"524": {HTTPStatus: 524, Message: "API Error: 524"},
	} {
		t.Run(name, func(t *testing.T) {
			c := answeredSeat(t, runtime.TurnStatusFailed, failure, true)
			if failure.RateLimited {
				if len(c.posted) != 0 || c.row().Metadata["restart_requested"] != "true" || c.cell().RateLimit.Phase != workladder.RateLimitRestarted {
					t.Fatalf("nudged at %v, recycles posted %+v, cell %s: want the seat restarted fresh, never recycled", c.nudgedAt, c.posted, c.row().Metadata[session.WorkLadderKey])
				}
			} else {
				if len(c.posted) != 1 {
					t.Fatalf("nudged at %v, recycles posted %+v: want the seat recycled, its failed turns no progress", c.nudgedAt, c.posted)
				}
				if got := escalations(c); len(got) != 1 || got[0] != workladder.AlertStartedRecycled {
					t.Fatalf("work.escalated reasons = %v, want the recycle's alert", got)
				}
			}
			old := answeredSeat(t, runtime.TurnStatusFailed, failure, false)
			if len(old.posted) != 0 || old.row().Metadata["restart_requested"] != "" {
				t.Fatalf("with no turn reads: recycles posted %+v, restart %q; the replay assumes each failure resets the count", old.posted, old.row().Metadata["restart_requested"])
			}
		})
	}
}

// A seat whose turns the provider completes, as slowly, is making progress:
// its count restarts after each answer, and it is never recycled.
func TestWorkLadderCompletedTurnIsProgress(t *testing.T) {
	c := answeredSeat(t, runtime.TurnStatusCompleted, runtime.TurnFailure{}, true)
	if len(c.posted) != 0 || len(escalations(c)) != 0 {
		t.Fatalf("recycles %+v, alerts %v: a served seat is making progress", c.posted, escalations(c))
	}
}

// A family whose turns carry no outcome (Gemini records no turn end), and a
// read that fails, leave progress to the activity clock: a working seat is
// never recycled for want of a completed turn.
func TestWorkLadderNoStatusFamilyKeepsActivityProgress(t *testing.T) {
	t.Run("no status", func(t *testing.T) {
		c := answeredSeat(t, "", runtime.TurnFailure{}, true)
		if len(c.posted) != 0 || len(escalations(c)) != 0 {
			t.Fatalf("recycles %+v, alerts %v: turns with no status are judged on the activity clock", c.posted, escalations(c))
		}
	})
	t.Run("the read fails", func(t *testing.T) {
		c := newLadderCity(t)
		id := c.claim("the step")
		c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
		turns := &fakeLadderTurns{err: errors.New("transcript not found")}
		turns.install(c)
		for end := c.now.Add(8 * time.Hour); c.now.Before(end); c.now = c.now.Add(30 * time.Second) {
			for _, n := range c.nudgedAt {
				if at := c.start.Add(n + answerDelay); !at.After(c.now) {
					c.sp.SetActivity(ladderSeatName, at)
				}
			}
			c.pass()
		}
		if turns.reads == 0 || len(c.posted) != 0 || len(escalations(c)) != 0 {
			t.Fatalf("reads %d, recycles %+v, alerts %v: an unreadable seat is judged on its activity", turns.reads, c.posted, escalations(c))
		}
	})
}

// Progress is judged only on a pass that may act on the seat. A busy pass,
// or one behind a gate, writes nothing, though the seat worked long past its
// last nudge; the first pass that may act restarts the count.
func TestWorkLadderJudgesProgressOnlyWhenActing(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("the step")
	c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
	c.run(5*time.Minute, 30*time.Second, nil) // nudged at 1m30s and 4m30s
	e := c.entry(id)
	if e.Attempts != 2 {
		t.Fatalf("entry %+v, want two counted nudges", e)
	}
	c.now = e.Last.Add(30 * time.Minute)
	c.sp.SetActivity(ladderSeatName, c.now.Add(-10*time.Second)) // busy: inside the quiet grace
	before := c.row().Metadata[session.WorkLadderKey]
	c.pass()
	if got := c.row().Metadata[session.WorkLadderKey]; got != before {
		t.Fatalf("busy pass wrote the cell:\n%s\nwant\n%s", got, before)
	}

	c.now = c.now.Add(5 * time.Minute) // quiet again, behind an agent's wait
	c.setRow(map[string]string{"wait_hold": "true"})
	c.pass()
	if got := c.row().Metadata[session.WorkLadderKey]; got != before {
		t.Fatalf("gated pass wrote the cell:\n%s\nwant\n%s", got, before)
	}

	c.setRow(map[string]string{"wait_hold": ""})
	c.pass()
	if got := c.entry(id); got.Since.Equal(e.Since) || got.Attempts > 1 {
		t.Fatalf("acting pass: entry %+v, want the count restarted by the seat's work", got)
	}
}

// The ladder reads a seat's turns only on a pass that may act on it (a
// running, quiet seat, its gate open, with work outstanding), and reads it
// again only when the seat moves (its generation, conversation, activity or
// the last nudge it took) or the reading is ladderTurnsMaxAge old.
func TestWorkLadderReadsTurnsOnlyWhenActing(t *testing.T) {
	c := newLadderCity(t)
	turns := &fakeLadderTurns{}
	turns.install(c)
	c.pass()
	if turns.reads != 0 {
		t.Fatalf("read %d times with no work outstanding", turns.reads)
	}
	c.claim("the step")
	c.sp.SetActivity(ladderSeatName, c.now.Add(-10*time.Second))
	c.pass()
	if turns.reads != 0 {
		t.Fatalf("read %d times on a busy seat", turns.reads)
	}
	c.sp.SetActivity(ladderSeatName, c.now.Add(-time.Hour))
	c.setRow(map[string]string{"wait_hold": "true"})
	c.pass()
	if turns.reads != 0 {
		t.Fatalf("read %d times behind a gate", turns.reads)
	}
	c.setRow(map[string]string{"wait_hold": ""})
	if err := c.sp.Stop(ladderSeatName); err != nil {
		t.Fatal(err)
	}
	c.pass()
	if turns.reads != 0 {
		t.Fatalf("read %d times on a stopped runtime", turns.reads)
	}
	if err := c.sp.Start(context.Background(), ladderSeatName, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	c.sp.SetActivity(ladderSeatName, c.now.Add(-time.Hour))

	c.pass()
	if turns.reads != 1 {
		t.Fatalf("reads = %d on an acting pass, want 1", turns.reads)
	}
	c.now = c.now.Add(30 * time.Second)
	c.pass()
	if turns.reads != 1 {
		t.Fatalf("reads = %d, want the reading cached while the seat has not moved", turns.reads)
	}
	c.sp.SetActivity(ladderSeatName, c.now.Add(-5*time.Minute))
	c.pass()
	if turns.reads != 2 {
		t.Fatalf("reads = %d, want a re-read when the seat's activity moved", turns.reads)
	}
	c.setRow(map[string]string{"continuation_epoch": "2"})
	c.pass()
	if turns.reads != 3 {
		t.Fatalf("reads = %d, want a re-read in a new conversation", turns.reads)
	}
	c.now = c.now.Add(ladderTurnsMaxAge)
	c.pass()
	if turns.reads != 4 {
		t.Fatalf("reads = %d, want a re-read once the reading is %v old", turns.reads, ladderTurnsMaxAge)
	}
}

// A seat whose turns cannot be read is reported once per conversation and
// error, not every pass; a family that records no turns is not reported.
func TestWorkLadderReportsAnUnreadableSeatOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"a read that fails", errors.New("transcript not found"), 2},
		{"no turn identity", fmt.Errorf("claude: %w", runtime.ErrTurnIdentityUnsupported), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newLadderCity(t)
			var stderr bytes.Buffer
			c.lane.stderr = &stderr
			id := c.claim("the step")
			c.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
			turns := &fakeLadderTurns{err: tc.err}
			turns.install(c)
			c.run(30*time.Minute, time.Minute, nil)
			before := turns.reads
			c.setRow(map[string]string{"continuation_epoch": "2"})
			c.run(30*time.Minute, time.Minute, nil)
			if before < 2 || turns.reads-before < 2 {
				t.Fatalf("reads = %d, want the seat read again and again", turns.reads)
			}
			if got := strings.Count(stderr.String(), "reading its turns"); got != tc.want {
				t.Fatalf("reported %d times, want %d:\n%s", got, tc.want, stderr.String())
			}
		})
	}
}

// workerLadderTurns is the turn source rule 1 reads its evidence through
// (plannerRuntime.start installs it). It reads a pool seat's turns through
// the seat's worker handle: the refusals its runtime records reach the
// ladder's reading, and a read that fails, or a row the store does not
// hold, is an error, never a reading.
func TestWorkerLadderTurnsReadsAPoolSeat(t *testing.T) {
	c := newLadderCity(t)
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), ladderSeatName, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	env := c.env
	env.SP = runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: fake})
	refused := func(id string, ago time.Duration) runtime.EndedTurn {
		return runtime.EndedTurn{TurnID: id, StartedAt: c.now.Add(-ago - time.Second), EndedAt: c.now.Add(-ago), Status: runtime.TurnStatusFailed, Failure: coolingRefusal}
	}
	fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{ladderSeatName: {{
		Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact, Lossless: true,
	}}}
	fake.TurnHistoryScripts = map[string][]runtime.TurnHistory{ladderSeatName: {{
		ObservedAt: c.now, Recent: []runtime.EndedTurn{refused("t2", time.Minute), refused("t1", 3*time.Minute)},
	}}}
	read := workerLadderTurns(env)
	row := c.row()
	hist, err := read(context.Background(), row)
	if err != nil {
		t.Fatalf("reading a pool seat's turns: %v", err)
	}
	if r := ladderTurnReading(hist, rowConversation(row.Metadata)); r.Refused != 2 || !r.RefusedAt.Equal(c.now.Add(-time.Minute)) || ladderRefusal(hist) != coolingRefusal {
		t.Fatalf("reading %+v (refusal %+v) of %+v: want the two refusals the runtime recorded", r, ladderRefusal(hist), hist)
	}
	fake.TurnHistoryErrors = map[string][]error{ladderSeatName: {runtime.ErrRuntimeUnavailable}}
	if _, err := read(context.Background(), row); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("a read that failed: %v, want ErrRuntimeUnavailable", err)
	}
	if _, err := read(context.Background(), beads.Bead{ID: "gc-not-a-session"}); err == nil {
		t.Fatal("a row the store does not hold read as turns")
	}
}

// The planner's lane reads turns through the worker boundary: a started
// lane's turn source is workerLadderTurns.
func TestV2LaneReadsTurnsThroughTheWorkerBoundary(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	rt := attachTestV2(t, cr)
	rt.publishEnv()
	rt.start(context.Background())
	lane := rt.lane.Load()
	if lane == nil || lane.turnReaderFor == nil || reflect.ValueOf(lane.turnReaderFor).Pointer() != reflect.ValueOf(workerLadderTurns).Pointer() {
		t.Fatal("the started lane does not read turns through workerLadderTurns")
	}
}
