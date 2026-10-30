//go:build integration

package conversationlane_test

// W5's end-to-end evidence: the work ladder's reminder to a quiet pool seat
// is a lane notice the controller's lane host types into the seat. The city
// runs real tmux (the v2 reconciler, which runs the ladder, refuses the
// exec runtime the other cities here run), its server under a socket root
// of the city's own, and a pool of one seat whose agent is `cat` into a
// file, so what the lane typed is read back from the file.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/workladder"
)

const (
	// poolName is a pool city's one pool.
	poolName = "crew"
	// ladderPrincipal is the work ladder's notice principal.
	ladderPrincipal = "gc:planner/work-ladder"
	// ladderClaimedText is the ladder's reminder for an unstarted claim
	// (cmd/gc's ladderClaimedNudge).
	ladderClaimedText = "Run gc hook --claim --json now; it returns the work assigned to you. Execute it."
	// ladderStartedText is its reminder for a started claim (cmd/gc's
	// ladderStartedNudge), naming the claim.
	ladderStartedText = "You hold %s and its step has started. Continue it now (run gc hook --claim --json to re-read your assignment). If you are waiting on something, say what, then stop."
	// tmuxCommandTimeout bounds each tmux call the cleanup makes.
	tmuxCommandTimeout = 5 * time.Second
)

// preparePool gives a pool city its tmux socket root and the pool seat's
// agent script. The root is under /tmp, not the test's TMPDIR: a socket
// path past 104 bytes does not bind.
func (c *laneCity) preparePool() {
	c.t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		c.t.Fatalf("a pool city runs real tmux: %v", err)
	}
	root, err := os.MkdirTemp("/tmp", "gclt")
	if err != nil {
		c.t.Fatalf("creating the tmux socket root: %v", err)
	}
	c.tmuxRoot = root
	c.writeFile(c.poolAgent(), "#!/bin/sh\nexec cat >> "+shellQuote(c.typedPath())+"\n", 0o755)
}

// poolAgent is the pool seat's start command: it appends every line typed
// into its pane to typedPath.
func (c *laneCity) poolAgent() string { return filepath.Join(c.dir, ".gc", "typed-agent.sh") }

// typedPath is the file the pool seat's agent writes what it was typed.
func (c *laneCity) typedPath() string { return filepath.Join(c.state, "typed."+poolName) }

// poolTOML is the pool: one seat, always up.
func (c *laneCity) poolTOML() string {
	return "\n[[agent]]\nname = " + `"` + poolName + `"` + "\nstart_command = " + `"` + c.poolAgent() + `"` +
		"\nmin_active_sessions = 1\nmax_active_sessions = 1\nscale_check = \"echo 1\"\n"
}

// killTmuxServers kills every tmux server whose socket is under the city's
// own root, and removes the root. It never addresses any other server.
func (c *laneCity) killTmuxServers() {
	if c.tmuxRoot == "" {
		return
	}
	sockets, _ := filepath.Glob(filepath.Join(c.tmuxRoot, "tmux-*", "*"))
	for _, sock := range sockets {
		ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
		_ = exec.CommandContext(ctx, "tmux", "-S", sock, "kill-server").Run()
		cancel()
	}
	_ = os.RemoveAll(c.tmuxRoot)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// poolSeat waits for the pool's seat to be active and returns its row.
func (c *laneCity) poolSeat(timeout time.Duration) beads.Bead {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var seat beads.Bead
		c.mustStore(func(raw beads.Store, _ *opjournal.Store) error {
			rows, err := raw.List(beads.ListQuery{Label: "gc:session"})
			if err != nil {
				return err
			}
			for _, b := range rows {
				state := b.Metadata["state"]
				if b.Status != "closed" && b.Metadata["pool_managed"] == "true" && (state == "active" || state == "awake") {
					seat = b
				}
			}
			return nil
		})
		if seat.ID != "" {
			return seat
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("no active pool seat within %s", timeout)
		}
		time.Sleep(time.Second)
	}
}

// claimFor writes a bead to the city's work store held in progress by seat.
// The agent never starts it, but the reconciler records the work a live seat
// is awake for (current_bead_id), which the ladder may seed as started, so
// the ladder reminds it at its claimed rung or its started one.
func (c *laneCity) claimFor(seat beads.Bead) string {
	c.t.Helper()
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(c.dir, ".gc", "beads.json"))
	if err != nil {
		c.t.Fatalf("opening the work store: %v", err)
	}
	b, err := store.Create(beads.Bead{Title: "the ladder's claim", Type: "task", Metadata: map[string]string{"gc.routed_to": poolName}})
	if err != nil {
		c.t.Fatalf("creating the claim: %v", err)
	}
	status, assignee := "in_progress", seat.ID
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		c.t.Fatalf("claiming %s for %s: %v", b.ID, seat.ID, err)
	}
	return b.ID
}

// seatOps are every message op of conversation conv, closed ones included.
func (c *laneCity) seatOps(conv string) []opjournal.Op {
	c.t.Helper()
	saved := c.conv
	c.conv = conv
	defer func() { c.conv = saved }()
	return c.ops()
}

// TestWorkLadderLaneNoticeEndToEnd (W5 §12.3): with message on the lane, a
// quiet pool seat holding a claim gets the ladder's reminder as a lane
// notice. The controller's lane host types it into the
// seat as one reminder block carrying no identifier, exactly once, closes it
// succeeded, and the ladder settles its mark from that close: one delivered
// attempt, reported as one work.nudged.
func TestWorkLadderLaneNoticeEndToEnd(t *testing.T) {
	parallelCity(t)
	c := newLaneCityWith(t, armSQLite, activityNone, cityOptions{pool: true})
	c.startController("")
	seat := c.poolSeat(2 * time.Minute)
	claim := c.claimFor(seat)

	// Within the ladder's grace (90 s of quiet) plus a pass, its notice is
	// proposed; the lane host types it into the idle seat within seconds.
	var notice opjournal.Op
	deadline := time.Now().Add(5 * time.Minute)
	for notice.ID == "" || notice.Status != opjournal.StatusClosed {
		for _, op := range c.seatOps(seat.ID) {
			if op.Producer.Principal == ladderPrincipal {
				if notice.ID != "" && op.ID != notice.ID {
					t.Fatalf("a second ladder notice %s while %s was pending", op.ID, notice.ID)
				}
				notice = op
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ladder notice closed within 5m (last %+v)", notice)
		}
		time.Sleep(time.Second)
	}
	switch arm, reason := notice.Outcome.Effective(); {
	case arm != opjournal.ArmSucceeded:
		t.Fatalf("ladder notice outcome %s{%s}, want succeeded", arm, reason)
	case notice.Executor != opjournal.ExecutorLane || notice.Causation != claim:
		t.Fatalf("ladder notice executor %s causation %q, want lane, %s", notice.Executor, notice.Causation, claim)
	case notice.Payload != ladderClaimedText && notice.Payload != fmt.Sprintf(ladderStartedText, claim):
		t.Fatalf("ladder notice payload %q, want the claimed or the started rung's", notice.Payload)
	}

	// The seat's agent read the reminder block from its pane.
	want := "- [planner] " + notice.Payload
	var typed string
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(typed, want); time.Sleep(time.Second) {
		typed = readFile(c.typedPath())
		if time.Now().After(deadline) {
			t.Fatalf("the seat was never typed %q; it read:\n%s", want, typed)
		}
	}
	if !strings.Contains(typed, "<system-reminder>") {
		t.Fatalf("the seat read no reminder block:\n%s", typed)
	}
	// I14: no identifier but the claim the started rung's text names.
	for _, id := range []string{notice.ID, notice.ClientKey, seat.ID, ladderPrincipal, seat.Metadata["instance_token"]} {
		if id != "" && strings.Contains(typed, id) {
			t.Fatalf("the seat was typed identifier %q:\n%s", id, typed)
		}
	}

	// The ladder settles the mark from the close on its next pass: one
	// delivered attempt, no refusal, no mark left, reported as work.nudged
	// about the claim (the fold's event follows its write).
	var nudged []events.Event
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		var row beads.Bead
		c.mustStore(func(raw beads.Store, _ *opjournal.Store) error {
			var err error
			row, err = raw.Get(seat.ID)
			return err
		})
		cell, err := workladder.DecodeLadder(row.Metadata["work_ladder"])
		if err != nil {
			t.Fatalf("decoding the seat's ladder cell: %v", err)
		}
		var entry *workladder.Entry
		for i := range cell.Entries {
			if cell.Entries[i].Key.Bead == claim {
				entry = &cell.Entries[i]
			}
		}
		nudged, err = events.ReadFiltered(filepath.Join(c.dir, ".gc", "events.jsonl"), events.Filter{Type: events.WorkNudged, Subject: claim})
		if err != nil {
			t.Fatalf("reading the city's events: %v", err)
		}
		if entry != nil && entry.Notice.IsZero() {
			if entry.Attempts != 1 || entry.Refused != 0 || entry.Paused != 0 {
				t.Fatalf("ladder entry %+v, want one delivered attempt", *entry)
			}
			if len(nudged) > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ladder never settled its notice, or never reported it: cell %+v, work.nudged %+v", cell, nudged)
		}
	}
	if len(nudged) != 1 || nudged[0].SessionID != seat.ID {
		t.Fatalf("work.nudged about %s = %+v, want one for seat %s", claim, nudged, seat.ID)
	}

	// Exactly once: the lane typed the block once, and nothing typed the
	// reminder's text beside it (a direct keystroke under the lane's
	// authority would).
	typed = readFile(c.typedPath())
	if n := strings.Count(typed, want); n != 1 {
		t.Fatalf("the seat was typed the reminder block %d times, want once:\n%s", n, typed)
	}
	if n := strings.Count(typed, notice.Payload); n != 1 {
		t.Fatalf("the seat was typed the reminder's text %d times, want once:\n%s", n, typed)
	}
}
