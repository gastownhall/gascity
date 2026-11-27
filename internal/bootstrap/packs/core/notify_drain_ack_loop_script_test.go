package core

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const notifyDrainAckLoopScriptPath = "assets/scripts/notify-drain-ack-loop.sh"

// drainAckLoopBead is the misrouted bead the loop tests escalate.
const drainAckLoopBead = "gc-42"

// drainAckEvent is one session.drain_acked_with_assigned_work event, as
// `gc events` prints it, for drainAckLoopBead from session at ts, held under
// the session's own ID.
func drainAckEvent(seq int, ts time.Time, session, reason string) string {
	return drainAckEventOf(drainAckLoopBead, seq, ts, session, session, reason)
}

// drainAckEventOf is one session.drain_acked_with_assigned_work event for
// bead from session at ts, held under assignee; an empty assignee is left
// out of the payload, as a gc that recorded none prints it.
func drainAckEventOf(bead string, seq int, ts time.Time, session, assignee, reason string) string {
	held := ""
	if assignee != "" {
		held = fmt.Sprintf(`"assignee":%q,`, assignee)
	}
	return fmt.Sprintf(`{"seq":%d,"type":"session.drain_acked_with_assigned_work","ts":%q,"actor":"gc","subject":"worker","message":"session drain-acked while still assigned to work bead","session_id":%q,"payload":{"session_id":%q,"bead_id":%q,%s"template":"worker","bead_status":"in_progress","reason":%q}}`,
		seq, ts.UTC().Format(time.RFC3339Nano), session, session, bead, held, reason)
}

// loopCycles is n started_claim_kept events for drainAckLoopBead from
// session gc-1, five minutes apart.
func loopCycles(n int) []string {
	const gap = 5 * time.Minute
	start := time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, drainAckEvent(i+1, start.Add(time.Duration(i)*gap), "gc-1", "started_claim_kept"))
	}
	return out
}

// loopBeadJSON is `gc bd show gc-42 --json` for the bead in status, assigned
// to assignee, with labels.
func loopBeadJSON(status, assignee string, labels ...string) string {
	return beadJSON(drainAckLoopBead, status, assignee, labels...)
}

// beadJSON is `gc bd show <id> --json` for bead id in status, assigned to
// assignee, with labels.
func beadJSON(id, status, assignee string, labels ...string) string {
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = fmt.Sprintf("%q", l)
	}
	return fmt.Sprintf(`[{"id":%q,"title":"misrouted work","status":%q,"assignee":%q,"labels":[%s]}]`,
		id, status, assignee, strings.Join(quoted, ","))
}

// drainAckLoopRig is one run's scratch: the fake gc (events from
// events.jsonl, each bead from bead-<id>.json) and a stub escalate.sh that
// records each call's argv, one argument per line, ending "--". The fake gc
// prints every event whatever --since says, unless honorSince gave it a now.
type drainAckLoopRig struct {
	work, binDir, gcLog, escalate, escalateLog string
}

// drainAckSinceFilter is the fake gc's --since: the events at or after now
// less the window, a duration like 24h or 1h30m.
const drainAckSinceFilter = `def secs: [scan("([0-9]+)([hms])")] | map((.[0] | tonumber) * ({"h": 3600, "m": 60, "s": 1}[.[1]])) | add;
def epoch: sub("\\.[0-9]+"; "") | fromdateiso8601;
select((.ts | epoch) >= (($now | epoch) - ($since | secs)))
`

func newDrainAckLoopRig(t *testing.T, events []string, bead string) drainAckLoopRig {
	t.Helper()
	r := drainAckLoopRig{work: t.TempDir()}
	r.write(t, "events.jsonl", strings.Join(events, "\n")+"\n")
	r.addBead(t, drainAckLoopBead, bead)
	r.write(t, "since.jq", drainAckSinceFilter)
	at := func(name string) string { return "'" + filepath.Join(r.work, name) + "'" }
	r.binDir, r.gcLog = fakeGCBin(t, `case "$1 $2" in
"events --type")
	if [ "$3" != session.drain_acked_with_assigned_work ] || [ "$4" != --since ]; then
		echo "fake gc: unexpected call: $*" >&2; exit 1
	fi
	if [ -f `+at("now")+` ]; then
		jq -c --arg since "$5" --arg now "$(cat `+at("now")+`)" -f `+at("since.jq")+` `+at("events.jsonl")+`
	else
		cat `+at("events.jsonl")+`
	fi ;;
"bd show") cat '`+r.work+`'/bead-"$3".json ;;
*) echo "fake gc: unexpected call: $*" >&2; exit 1 ;;
esac
`)
	r.escalateLog = filepath.Join(r.work, "escalate.log")
	r.escalate = filepath.Join(r.work, "escalate.sh")
	stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + r.escalateLog + "'; done\nprintf -- '--\\n' >> '" + r.escalateLog + "'\n"
	if err := os.WriteFile(r.escalate, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

// write puts body in the rig's file name.
func (r drainAckLoopRig) write(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.work, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// addBead is what the fake gc prints for `gc bd show <id> --json`; a bead
// the rig has no file for fails its read.
func (r drainAckLoopRig) addBead(t *testing.T, id, shown string) {
	t.Helper()
	r.write(t, "bead-"+id+".json", shown+"\n")
}

// honorSince makes the fake gc print only the events inside --since at now,
// as gc events reads the window back from the current time.
func (r drainAckLoopRig) honorSince(t *testing.T, now time.Time) {
	t.Helper()
	r.write(t, "now", now.UTC().Format(time.RFC3339))
}

func (r drainAckLoopRig) run(t *testing.T, env ...string) (string, error) {
	t.Helper()
	base := []string{"GC_ESCALATE_SCRIPT=" + r.escalate, "GC_CITY_PATH=" + r.work}
	return runPackScript(t, notifyDrainAckLoopScriptPath, r.binDir, drainAckLoopEnvKeys, append(base, env...))
}

// escalations are the stub escalate.sh's calls, each its argv.
func (r drainAckLoopRig) escalations(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(r.escalateLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	var cur []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "--" {
			calls = append(calls, cur)
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return calls
}

// gcCalls are the fake gc's logged invocations.
func (r drainAckLoopRig) gcCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(r.gcLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(data)), " ", "_"))
}

// drainAckLoopEnvKeys are the settings each loop test supplies itself.
var drainAckLoopEnvKeys = map[string]struct{}{
	"GC_ESCALATE_SCRIPT":           {},
	"GC_DRAIN_ACK_LOOP_WINDOW":     {},
	"GC_DRAIN_ACK_LOOP_THRESHOLD":  {},
	"GC_ESCALATION_RECIPIENT":      {},
	"GC_ESCALATE_INCIDENT_DIR":     {},
	"GC_ESCALATE_SEARCH_PACKS":     {},
	"GC_SYSTEM_PACKS_DIR":          {},
	"GC_ESCALATE_REMINDER_SECONDS": {},
}

// argAfter is the value that follows flag in argv.
func argAfter(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

// TestNotifyDrainAckLoopEscalatesOnce is AC-5: three started_claim_kept
// cycles of one bead inside the window, the bead still in_progress,
// assigned to the looping session and unheld, escalate once under a
// per-bead incident key and fingerprint, with a message that names the bead,
// the session, its assignee, the template, the count and the default 24h
// window and gives the park command through gc bd; the script writes nothing
// to the bead and mails without a word on stderr. A second firing passes the
// same fingerprint, so escalate.sh keeps it quiet.
func TestNotifyDrainAckLoopEscalatesOnce(t *testing.T) {
	r := newDrainAckLoopRig(t, loopCycles(3), loopBeadJSON("in_progress", "gc-1"))
	out, err := r.run(t)
	if err != nil {
		t.Fatalf("notify-drain-ack-loop: %v\n%s", err, out)
	}
	if strings.Contains(out, "notify-drain-ack-loop: ") {
		t.Errorf("output %q, want nothing from the script when it mails the loop", out)
	}
	calls := r.escalations(t)
	if len(calls) != 1 {
		t.Fatalf("escalate.sh calls = %q, want exactly one", calls)
	}
	argv := calls[0]
	if got := argAfter(argv, "--incident-key"); got != "drain-ack-loop:"+drainAckLoopBead {
		t.Errorf("--incident-key = %q, want drain-ack-loop:%s", got, drainAckLoopBead)
	}
	if got := argAfter(argv, "--fingerprint"); got != drainAckLoopBead {
		t.Errorf("--fingerprint = %q, want %s", got, drainAckLoopBead)
	}
	subject, message := argAfter(argv, "--subject"), strings.Join(argv, "\n")
	if !strings.Contains(subject, drainAckLoopBead) {
		t.Errorf("--subject = %q, want it to name the bead", subject)
	}
	for _, want := range []string{
		drainAckLoopBead, "misrouted work", "Session(s): gc-1", "Assignee: gc-1", "Template: worker",
		"3 drain-ack cycles in the last 24h",
		"  gc bd set-state " + drainAckLoopBead + " hold=mayor",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("escalation %q does not contain %q", message, want)
		}
	}
	for _, call := range r.gcCalls(t) {
		if call != "gc_events_--type_session.drain_acked_with_assigned_work_--since_24h" && call != "gc_bd_show_"+drainAckLoopBead+"_--json" {
			t.Errorf("gc call %q: the script may only read the events over the default 24h window and the bead", call)
		}
	}

	if out, err := r.run(t); err != nil {
		t.Fatalf("second firing: %v\n%s", err, out)
	}
	calls = r.escalations(t)
	if len(calls) != 2 || argAfter(calls[1], "--fingerprint") != drainAckLoopBead || argAfter(calls[1], "--incident-key") != "drain-ack-loop:"+drainAckLoopBead {
		t.Fatalf("second firing's escalate.sh calls = %q, want the same incident key and fingerprint again", calls)
	}
}

// TestNotifyDrainAckLoopStaysQuiet: what is not a loop of a parkable bead
// escalates nothing. A bead that reached the threshold but is not parkable
// is named on stderr with the reason, so a skip is never silent; below the
// threshold the run says nothing. The hold labels compare trimmed and
// case-insensitively, as the controller's dispatch-hold check does.
func TestNotifyDrainAckLoopStaysQuiet(t *testing.T) {
	at := time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)
	working := loopBeadJSON("in_progress", "gc-1")
	for _, c := range []struct {
		name   string
		events []string
		bead   string
		skip   string // the stderr reason; empty when no bead reached the threshold
	}{
		{name: "two cycles", events: loopCycles(2), bead: working},
		{name: "one of three is a restart handoff", events: append(loopCycles(2),
			drainAckEvent(9, at.Add(time.Hour), "gc-1", "restart_requested")), bead: working},
		{name: "one of three is generic assigned work", events: append(loopCycles(2),
			drainAckEvent(9, at.Add(time.Hour), "gc-1", "drain_acked_with_assigned_work")), bead: working},
		{name: "two events of one session 10s apart count once", events: append(loopCycles(2),
			drainAckEvent(9, at.Add(5*time.Minute+10*time.Second), "gc-1", "started_claim_kept")), bead: working},
		{name: "the bead closed since", events: loopCycles(3), bead: loopBeadJSON("closed", "gc-1"), skip: "it is closed, not in_progress"},
		{name: "the bead is held", events: loopCycles(3), bead: loopBeadJSON("in_progress", "gc-1", "hold:mayor"), skip: "it is on hold:mayor"},
		{name: "the bead is held external", events: loopCycles(3), bead: loopBeadJSON("in_progress", "gc-1", "hold:external"), skip: "it is on hold:external"},
		{name: "the bead is held, the label padded and in capitals", events: loopCycles(3), bead: loopBeadJSON("in_progress", "gc-1", " Hold:Mayor "), skip: "it is on hold:mayor"},
		{name: "the bead was reassigned", events: loopCycles(3), bead: loopBeadJSON("in_progress", "gc-7"), skip: "it is assigned to gc-7, not to an identity its counted cycles recorded (gc-1)"},
		{name: "the bead was released", events: loopCycles(3), bead: loopBeadJSON("open", ""), skip: "it is open, not in_progress; it is unassigned"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newDrainAckLoopRig(t, c.events, c.bead)
			out, err := r.run(t)
			if err != nil {
				t.Fatalf("notify-drain-ack-loop: %v\n%s", err, out)
			}
			if calls := r.escalations(t); len(calls) != 0 {
				t.Fatalf("escalate.sh calls = %q, want none", calls)
			}
			if c.skip == "" {
				if strings.Contains(out, "notify-drain-ack-loop: ") {
					t.Errorf("output %q, want nothing below the threshold", out)
				}
				return
			}
			if want := "notify-drain-ack-loop: not mailing bead " + drainAckLoopBead + " after 3 cycles: " + c.skip + "\n"; !strings.Contains(out, want) {
				t.Errorf("output %q, want the skip named: %q", out, want)
			}
		})
	}
}

// TestNotifyDrainAckLoopMatchesTheClaimIdentity: a seat claims its work under
// its alias, configured named identity or session name before its session
// bead ID, so the events' session_id is often not the bead's assignee. The
// bead counts as held by its loop under any assignee its counted events
// recorded, or under one of their sessions when an event recorded none (a gc
// from before the payload carried it); under any other identity it is
// skipped.
func TestNotifyDrainAckLoopMatchesTheClaimIdentity(t *testing.T) {
	cycles := func(assignee string) []string {
		start := time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)
		var out []string
		for i := 0; i < 3; i++ {
			out = append(out, drainAckEventOf(drainAckLoopBead, i+1, start.Add(time.Duration(i)*5*time.Minute), "gc-1", assignee, "started_claim_kept"))
		}
		return out
	}
	for _, c := range []struct {
		name     string
		events   []string
		assignee string // the bead's assignee when it is read again
		mailed   bool
		skip     string
	}{
		{name: "held under the alias the events recorded", events: cycles("gascity/furiosa"), assignee: "gascity/furiosa", mailed: true},
		{name: "events that recorded no assignee match by session", events: cycles(""), assignee: "gc-1", mailed: true},
		{
			name: "held under another alias since", events: cycles("gascity/furiosa"), assignee: "gascity/nux",
			skip: "it is assigned to gascity/nux, not to an identity its counted cycles recorded (gascity/furiosa, gc-1)",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newDrainAckLoopRig(t, c.events, loopBeadJSON("in_progress", c.assignee))
			out, err := r.run(t)
			if err != nil {
				t.Fatalf("notify-drain-ack-loop: %v\n%s", err, out)
			}
			calls := r.escalations(t)
			if !c.mailed {
				if len(calls) != 0 {
					t.Errorf("escalate.sh calls = %q, want none", calls)
				}
				if want := "not mailing bead " + drainAckLoopBead + " after 3 cycles: " + c.skip; !strings.Contains(out, want) {
					t.Errorf("output %q, want the skip named: %q", out, want)
				}
				return
			}
			if len(calls) != 1 {
				t.Fatalf("escalate.sh calls = %q, want one (output %q)", calls, out)
			}
			message := strings.Join(calls[0], "\n")
			for _, want := range []string{"Session(s): gc-1", "Assignee: " + c.assignee} {
				if !strings.Contains(message, want) {
					t.Errorf("escalation %q does not contain %q", message, want)
				}
			}
		})
	}
}

// TestNotifyDrainAckLoopDefaultWindowHoldsLadderPacedCycles: the work ladder
// paces a loop (nudges backing off from 3m to a 60m cap before a recycle), so
// its cycles run 70 minutes and more apart, as in the live loop this order
// was built for. The default window holds three such cycles and mails; a 1h
// window, with gc events reading the window back from now, holds only the
// last one.
func TestNotifyDrainAckLoopDefaultWindowHoldsLadderPacedCycles(t *testing.T) {
	start := time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)
	const gap = 70 * time.Minute
	var events []string
	for i := 0; i < 3; i++ {
		events = append(events, drainAckEvent(i+1, start.Add(time.Duration(i)*gap), "gc-1", "started_claim_kept"))
	}
	now := start.Add(2*gap + time.Minute) // a minute after the third cycle
	for _, c := range []struct {
		name   string
		env    []string
		mailed bool
	}{
		{name: "the default window", mailed: true},
		{name: "a 1h window", env: []string{"GC_DRAIN_ACK_LOOP_WINDOW=1h"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newDrainAckLoopRig(t, events, loopBeadJSON("in_progress", "gc-1"))
			r.honorSince(t, now)
			out, err := r.run(t, c.env...)
			if err != nil {
				t.Fatalf("notify-drain-ack-loop: %v\n%s", err, out)
			}
			calls := r.escalations(t)
			if !c.mailed {
				if len(calls) != 0 {
					t.Errorf("escalate.sh calls = %q, want none: the window holds one cycle", calls)
				}
				return
			}
			if len(calls) != 1 || !strings.Contains(argAfter(calls[0], "--message"), "3 drain-ack cycles in the last 24h") {
				t.Fatalf("escalate.sh calls = %q, want one naming 3 cycles in the last 24h (output %q)", calls, out)
			}
		})
	}
}

// TestNotifyDrainAckLoopReadsEveryBeadBeforeMailing: every looping bead is
// read before any mail is sent, so a later bead's failed read fails the run
// with no mail at all, even for an earlier bead that would be mailed.
func TestNotifyDrainAckLoopReadsEveryBeadBeforeMailing(t *testing.T) {
	const second = "gc-43" // read after gc-42; the rig has no bead file for it
	start := time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)
	events := loopCycles(3)
	for i := 0; i < 3; i++ {
		events = append(events, drainAckEventOf(second, 10+i, start.Add(time.Duration(i)*5*time.Minute), "gc-2", "gc-2", "started_claim_kept"))
	}
	r := newDrainAckLoopRig(t, events, loopBeadJSON("in_progress", "gc-1"))
	out, err := r.run(t)
	if err == nil {
		t.Fatalf("notify-drain-ack-loop succeeded, want the failed read of %s to fail it\n%s", second, out)
	}
	if !strings.Contains(out, "notify-drain-ack-loop: reading bead "+second+" failed") {
		t.Errorf("output %q does not give the failed read of %s", out, second)
	}
	if calls := r.escalations(t); len(calls) != 0 {
		t.Errorf("escalate.sh calls = %q, want none once a read failed", calls)
	}
	if calls := r.gcCalls(t); !slices.Contains(calls, "gc_bd_show_"+drainAckLoopBead+"_--json") {
		t.Errorf("gc calls = %q, want %s read before %s", calls, drainAckLoopBead, second)
	}
}

// TestNotifyDrainAckLoopCountsCyclesAcrossSessions: the cycles of one bead
// from successive seats add up, and every session is named.
func TestNotifyDrainAckLoopCountsCyclesAcrossSessions(t *testing.T) {
	at := time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)
	events := []string{
		drainAckEvent(1, at, "gc-1", "started_claim_kept"),
		drainAckEvent(2, at.Add(2*time.Minute), "gc-2", "started_claim_kept"),
		drainAckEvent(3, at.Add(4*time.Minute), "gc-3", "started_claim_kept"),
	}
	r := newDrainAckLoopRig(t, events, loopBeadJSON("in_progress", "gc-3"))
	if out, err := r.run(t); err != nil {
		t.Fatalf("notify-drain-ack-loop: %v\n%s", err, out)
	}
	calls := r.escalations(t)
	if len(calls) != 1 {
		t.Fatalf("escalate.sh calls = %q, want one", calls)
	}
	// The stub logs a multi-line message as several lines; read the call whole.
	message := strings.Join(calls[0], "\n")
	for _, want := range []string{"gc-1", "gc-2", "gc-3", "3 drain-ack cycles"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not name %q", message, want)
		}
	}
}

// TestNotifyDrainAckLoopHonorsOverrides: the threshold and the window come
// from GC_DRAIN_ACK_LOOP_THRESHOLD and GC_DRAIN_ACK_LOOP_WINDOW.
func TestNotifyDrainAckLoopHonorsOverrides(t *testing.T) {
	r := newDrainAckLoopRig(t, loopCycles(2), loopBeadJSON("in_progress", "gc-1"))
	if out, err := r.run(t, "GC_DRAIN_ACK_LOOP_THRESHOLD=2", "GC_DRAIN_ACK_LOOP_WINDOW=30m"); err != nil {
		t.Fatalf("notify-drain-ack-loop: %v\n%s", err, out)
	}
	calls := r.escalations(t)
	if len(calls) != 1 || !strings.Contains(argAfter(calls[0], "--message"), "2 drain-ack cycles") || !strings.Contains(argAfter(calls[0], "--message"), "30m") {
		t.Fatalf("escalate.sh calls = %q, want one naming 2 cycles in 30m", calls)
	}
	if calls := r.gcCalls(t); len(calls) == 0 || !strings.HasPrefix(calls[0], "gc_events_--type_session.drain_acked_with_assigned_work_--since_30m") {
		t.Fatalf("gc calls = %q, want the events read over the 30m window", calls)
	}
}

// TestNotifyDrainAckLoopFailsLoud: a setting it cannot use, or a read it
// cannot complete or decode, exits non-zero with the cause on stderr and
// mails nothing, so the order's failure is visible rather than a quiet
// "no loop". An escalation that does not land fails the run the same way,
// while an empty window stays a clean run.
func TestNotifyDrainAckLoopFailsLoud(t *testing.T) {
	working := loopBeadJSON("in_progress", "gc-1")
	for _, c := range []struct {
		name   string
		env    []string
		events []string
		bead   string
		breaks string // a rig file to remove, so its read fails
		want   string
		noGC   bool // the script must refuse before calling gc
		// escalateFails makes the stub escalate.sh exit non-zero, so the
		// one escalation attempt is the failure, not a stray send.
		escalateFails bool
		clean         bool // the run must succeed and escalate nothing
	}{
		{name: "the events read fails", events: loopCycles(3), bead: working, breaks: "events.jsonl", want: "reading session.drain_acked_with_assigned_work events"},
		{name: "the events are not JSON", events: []string{"not json"}, bead: working, want: "decoding session.drain_acked_with_assigned_work events"},
		{name: "the bead read fails", events: loopCycles(3), bead: working, breaks: "bead-" + drainAckLoopBead + ".json", want: "reading bead " + drainAckLoopBead},
		{name: "the bead is not JSON", events: loopCycles(3), bead: "not json", want: "decoding bead " + drainAckLoopBead},
		{name: "the bead read names no bead", events: loopCycles(3), bead: "[]", want: "decoding bead " + drainAckLoopBead},
		{name: "a zero threshold", env: []string{"GC_DRAIN_ACK_LOOP_THRESHOLD=0"}, events: loopCycles(3), bead: working, want: "GC_DRAIN_ACK_LOOP_THRESHOLD", noGC: true},
		{name: "a threshold that is no number", env: []string{"GC_DRAIN_ACK_LOOP_THRESHOLD=abc"}, events: loopCycles(3), bead: working, want: "GC_DRAIN_ACK_LOOP_THRESHOLD", noGC: true},
		{name: "a window that is no duration", env: []string{"GC_DRAIN_ACK_LOOP_WINDOW=forever"}, events: loopCycles(3), bead: working, want: "GC_DRAIN_ACK_LOOP_WINDOW", noGC: true},
		{name: "the escalation does not land", events: loopCycles(3), bead: working, want: "could not escalate the drain-ack loop of " + drainAckLoopBead, escalateFails: true},
		{name: "an empty window is a clean run", bead: working, clean: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newDrainAckLoopRig(t, c.events, c.bead)
			if c.breaks != "" {
				if err := os.Remove(filepath.Join(r.work, c.breaks)); err != nil {
					t.Fatal(err)
				}
			}
			if c.escalateFails {
				stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + r.escalateLog + "'; done\nprintf -- '--\\n' >> '" + r.escalateLog + "'\necho 'escalate: store unavailable' >&2\nexit 1\n"
				if err := os.WriteFile(r.escalate, []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			out, err := r.run(t, c.env...)
			if c.clean {
				if err != nil {
					t.Fatalf("notify-drain-ack-loop failed on an empty window: %v\n%s", err, out)
				}
				if calls := r.escalations(t); len(calls) != 0 {
					t.Errorf("escalate.sh calls = %q, want none", calls)
				}
				return
			}
			if err == nil {
				t.Fatalf("notify-drain-ack-loop succeeded, want a failure naming %q\n%s", c.want, out)
			}
			if !strings.Contains(out, "notify-drain-ack-loop: ") || !strings.Contains(out, c.want) {
				t.Errorf("output %q does not give the cause %q", out, c.want)
			}
			wantCalls := 0
			if c.escalateFails {
				wantCalls = 1
			}
			if calls := r.escalations(t); len(calls) != wantCalls {
				t.Errorf("escalate.sh calls = %q, want %d", calls, wantCalls)
			}
			if calls := r.gcCalls(t); c.noGC && len(calls) != 0 {
				t.Errorf("gc calls = %q, want none before the settings are valid", calls)
			}
		})
	}
}

// TestNotifyOnDrainAckLoopOrder pins the order's event contract: it wakes on
// session.drain_acked_with_assigned_work and runs notify-drain-ack-loop.sh.
func TestNotifyOnDrainAckLoopOrder(t *testing.T) {
	assertEventExecOrder(t, "notify-on-drain-ack-loop.toml", "session.drain_acked_with_assigned_work", "notify-drain-ack-loop.sh")
}
