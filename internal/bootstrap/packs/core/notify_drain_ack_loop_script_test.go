package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNotifyOnDrainAckLoopOrder pins the order's event contract: it wakes
// on the drain-ack-with-assigned-work anomaly and runs the one script.
func TestNotifyOnDrainAckLoopOrder(t *testing.T) {
	assertEventExecOrder(t, "notify-on-drain-ack-loop.toml", "session.drain_acked_with_assigned_work", "notify-drain-ack-loop.sh")
}

const notifyDrainAckLoopScriptPath = "assets/scripts/notify-drain-ack-loop.sh"

// drainAckLoopEvent is one session.drain_acked_with_assigned_work event at
// seq and ts (RFC3339, fractional seconds allowed) for bead gc-9.
func drainAckLoopEvent(seq int, ts, reason string) string {
	return fmt.Sprintf(`{"seq":%d,"type":"session.drain_acked_with_assigned_work","subject":"worker","message":"session drain-acked while still assigned to work bead","ts":%q,"payload":{"session_id":"gc-2","bead_id":"gc-9","template":"worker","bead_status":"in_progress","reason":%q}}`,
		seq, ts, reason)
}

// The window's qualifying cycles: three, at least a minute apart, all
// naming the bead the still-claiming session holds.
const (
	drainAckCycle1 = `{"seq":101,"type":"session.drain_acked_with_assigned_work","subject":"worker","message":"session drain-acked while still assigned to work bead","ts":"2026-11-23T07:00:00Z","payload":{"session_id":"gc-2","bead_id":"gc-9","template":"worker","bead_status":"in_progress","reason":"started_claim_kept"}}`
	drainAckCycle2 = `{"seq":102,"type":"session.drain_acked_with_assigned_work","subject":"worker","message":"session drain-acked while still assigned to work bead","ts":"2026-11-23T07:10:00Z","payload":{"session_id":"gc-2","bead_id":"gc-9","template":"worker","bead_status":"in_progress","reason":"started_claim_kept"}}`
	drainAckCycle3 = `{"seq":103,"type":"session.drain_acked_with_assigned_work","subject":"worker","message":"session drain-acked while still assigned to work bead","ts":"2026-11-23T07:20:00Z","payload":{"session_id":"gc-2","bead_id":"gc-9","template":"worker","bead_status":"in_progress","reason":"started_claim_kept"}}`
)

// drainAckLoopGC writes the fake gc the script runs: it answers the event
// read with events (JSON lines), the bead read with beads (id -> the JSON
// gc bd show prints), and mail for a mail send.
func drainAckLoopGC(t *testing.T, events []string, beads map[string]string) (work, binDir, logPath string) {
	t.Helper()
	work = t.TempDir()
	eventsFile := filepath.Join(work, "events.jsonl")
	if err := os.WriteFile(eventsFile, []byte(strings.Join(events, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beadsDir := filepath.Join(work, "beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for id, bead := range beads {
		if err := os.WriteFile(filepath.Join(beadsDir, id+".json"), []byte(bead), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	binDir, logPath = fakeGCBin(t, `case "$1 $2" in
"events --type") cat "`+eventsFile+`" ;;
"bd show") if [ -f "`+beadsDir+`/$3.json" ]; then cat "`+beadsDir+`/$3.json"; else echo "bd: no such issue: $3" >&2; exit 1; fi ;;
"mail send") echo "Sent message m-1 to human" ;;
*) echo "gc: unexpected call: $*" >&2; exit 1 ;;
esac`)
	return work, binDir, logPath
}

// drainAckLoopBead is the still-claiming bead the loop mails about.
const drainAckLoopBead = `[{"id":"gc-9","title":"misrouted bead","status":"in_progress","assignee":"gc-2","labels":["backend"]}]`

// drainAckLoopEnv is the environment of a run in work that escalates
// through the pack's own escalate.sh, with its incidents under work.
func drainAckLoopEnv(t *testing.T, work string, extra ...string) []string {
	t.Helper()
	return append([]string{
		"GC_ESCALATE_SCRIPT=" + coreEscalateScript(t),
		"GC_ESCALATE_INCIDENT_DIR=" + filepath.Join(work, "incidents"),
		"GC_CITY_PATH=" + work,
	}, extra...)
}

// drainAckLoopSends are the `gc mail send` lines of the fake gc's log.
func drainAckLoopSends(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "gc mail send ") {
			out = append(out, line)
		}
	}
	return out
}

// drainAckLoopWrites are the gc calls that would write the bead. The order
// never writes the bead, so none may appear in any run's log.
func drainAckLoopWrites(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		// The mail's own park recipe is logged as escalate.sh's -m argv and
		// is indented inside the message; a write the script made is a gc
		// invocation at the start of its own log line.
		if strings.HasPrefix(line, "gc bd set-state ") || strings.HasPrefix(line, "gc bd update ") {
			out = append(out, line)
		}
	}
	return out
}

// runDrainAckLoop runs the script once.
func runDrainAckLoop(t *testing.T, binDir string, env []string) (string, error) {
	t.Helper()
	return runPackScript(t, notifyDrainAckLoopScriptPath, binDir, escalationEnvKeys, env)
}

// TestNotifyDrainAckLoopMailsOncePerBead is the happy path (#6940): three
// qualifying cycles inside the window mail the escalation recipient once,
// naming the bead, its sessions, the template and the count, with the park
// recipe; the script writes nothing to the bead; and a window re-read that
// finds the same or a later cycle mails nothing more, because the bead is
// the incident's fingerprint.
func TestNotifyDrainAckLoopMailsOncePerBead(t *testing.T) {
	work, binDir, logPath := drainAckLoopGC(t,
		[]string{drainAckCycle1, drainAckCycle2, drainAckCycle3},
		map[string]string{"gc-9": drainAckLoopBead})
	env := drainAckLoopEnv(t, work)

	out, err := runDrainAckLoop(t, binDir, env)
	if err != nil {
		t.Fatalf("notify-drain-ack-loop: %v\n%s", err, out)
	}

	sends := drainAckLoopSends(t, logPath)
	if len(sends) != 1 {
		t.Fatalf("sends = %d, want exactly 1:\n%s", len(sends), strings.Join(sends, "\n"))
	}
	// The mail's body spans lines, so its recipe is asserted against the
	// whole log the fake gc wrote, not the send's first line.
	full, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"gc mail send human ",
		"Bead gc-9 is livelocked in a drain-ack loop (3 started claims kept in 1h) [HIGH]",
		"misrouted bead",
		"gc-2",
		"worker",
		"3 times in the last 1h",
		"gc bd set-state gc-9 hold=mayor --reason \"drain-ack loop\"",
		"gc bd update gc-9 --set-metadata gc.routed_to=",
	} {
		if !strings.Contains(string(full), want) {
			t.Fatalf("the mail does not say %q:\n%s", want, full)
		}
	}
	if writes := drainAckLoopWrites(t, logPath); len(writes) != 0 {
		t.Fatalf("the order wrote the bead:\n%s", strings.Join(writes, "\n"))
	}
	// The window is what the script asked gc for.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "gc events --type session.drain_acked_with_assigned_work --since 1h") {
		t.Fatalf("the script did not read the default 1h window:\n%s", data)
	}

	// A re-read of the same window, and a window that also holds a later
	// cycle of the same bead, mail nothing more.
	for _, events := range [][]string{
		{drainAckCycle1, drainAckCycle2, drainAckCycle3},
		{drainAckCycle1, drainAckCycle2, drainAckCycle3, drainAckLoopEvent(104, "2026-11-23T07:30:00Z", "started_claim_kept")},
	} {
		eventsFile := filepath.Join(work, "events.jsonl")
		if err := os.WriteFile(eventsFile, []byte(strings.Join(events, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := runDrainAckLoop(t, binDir, env); err != nil {
			t.Fatalf("notify-drain-ack-loop: %v\n%s", err, out)
		}
	}
	if sends := drainAckLoopSends(t, logPath); len(sends) != 1 {
		t.Fatalf("sends after the re-reads = %d, want still 1 (the bead is the fingerprint)", len(sends))
	}
}

// TestNotifyDrainAckLoopCountsCyclesPerBead pins what counts: only
// started_claim_kept events, deduplicated per (bead, session) at a
// minute's granularity, past a threshold the city can override — and a
// bead the read no longer finds looping (closed, held, reassigned) mails
// nothing, even at the cap.
func TestNotifyDrainAckLoopCountsCyclesPerBead(t *testing.T) {
	restart := drainAckLoopEvent(102, "2026-11-23T07:10:00Z", "restart_requested")
	generic := drainAckLoopEvent(102, "2026-11-23T07:10:00Z", "drain_acked_with_assigned_work")
	burstPair := drainAckLoopEvent(102, "2026-11-23T07:00:10.5Z", "started_claim_kept")
	cycle4 := drainAckLoopEvent(103, "2026-11-23T07:10:00Z", "started_claim_kept")
	cycle5 := drainAckLoopEvent(104, "2026-11-23T07:20:00Z", "started_claim_kept")
	closedBead := `[{"id":"gc-9","title":"misrouted bead","status":"closed","assignee":"gc-2","labels":["backend"]}]`
	heldBead := `[{"id":"gc-9","title":"misrouted bead","status":"in_progress","assignee":"gc-2","labels":["backend","hold:mayor"]}]`
	reassignedBead := `[{"id":"gc-9","title":"misrouted bead","status":"in_progress","assignee":"gc-7","labels":["backend"]}]`

	cases := []struct {
		name     string
		events   []string
		bead     string
		env      []string // extra env
		wantMail bool
	}{
		{name: "two cycles stay under the default cap", events: []string{drainAckCycle1, drainAckCycle2}, bead: drainAckLoopBead},
		{name: "a restart handoff is not a cycle", events: []string{drainAckCycle1, restart, drainAckCycle3}, bead: drainAckLoopBead},
		{name: "a generic kept bead is not a cycle", events: []string{drainAckCycle1, generic, drainAckCycle3}, bead: drainAckLoopBead},
		{name: "a burst for one bead and session counts once", events: []string{drainAckCycle1, burstPair, cycle4}, bead: drainAckLoopBead},
		{name: "a lower threshold is honored", events: []string{drainAckCycle1, drainAckCycle2}, bead: drainAckLoopBead, env: []string{"GC_DRAIN_ACK_LOOP_THRESHOLD=2"}, wantMail: true},
		{name: "a burst plus two more cycles reaches the cap", events: []string{drainAckCycle1, burstPair, cycle4, cycle5}, bead: drainAckLoopBead, wantMail: true},
		{name: "a bead that closed is not mailed", events: []string{drainAckCycle1, drainAckCycle2, drainAckCycle3}, bead: closedBead},
		{name: "a bead already held is not mailed", events: []string{drainAckCycle1, drainAckCycle2, drainAckCycle3}, bead: heldBead},
		{name: "a bead another session took is not mailed", events: []string{drainAckCycle1, drainAckCycle2, drainAckCycle3}, bead: reassignedBead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work, binDir, logPath := drainAckLoopGC(t, tc.events, map[string]string{"gc-9": tc.bead})
			out, err := runDrainAckLoop(t, binDir, drainAckLoopEnv(t, work, tc.env...))
			if err != nil {
				t.Fatalf("notify-drain-ack-loop: %v\n%s", err, out)
			}
			sends := drainAckLoopSends(t, logPath)
			if tc.wantMail && len(sends) != 1 {
				t.Fatalf("sends = %d, want the one mail:\n%s", len(sends), out)
			}
			if !tc.wantMail && len(sends) != 0 {
				t.Fatalf("sends = %q, want none:\n%s", sends, out)
			}
		})
	}
}

// TestNotifyDrainAckLoopHonorsTheWindowOverride pins that a configured
// window reaches gc: the window is the whole filter, and a city that
// widens or narrows it gets what it asked for.
func TestNotifyDrainAckLoopHonorsTheWindowOverride(t *testing.T) {
	work, binDir, logPath := drainAckLoopGC(t,
		[]string{drainAckCycle1, drainAckCycle2, drainAckCycle3},
		map[string]string{"gc-9": drainAckLoopBead})

	out, err := runDrainAckLoop(t, binDir, drainAckLoopEnv(t, work, "GC_DRAIN_ACK_LOOP_WINDOW=30m"))
	if err != nil {
		t.Fatalf("notify-drain-ack-loop: %v\n%s", err, out)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "--since 30m") {
		t.Fatalf("the script did not read the overridden 30m window:\n%s", data)
	}
}
