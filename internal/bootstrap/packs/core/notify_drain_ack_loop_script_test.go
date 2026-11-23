package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const notifyDrainAckLoopScriptPath = "assets/scripts/notify-drain-ack-loop.sh"

// drainAckLoopBead is the misrouted bead the loop tests escalate.
const drainAckLoopBead = "gc-42"

// drainAckEvent is one session.drain_acked_with_assigned_work event, as
// `gc events` prints it, for drainAckLoopBead from session at ts.
func drainAckEvent(seq int, ts time.Time, session, reason string) string {
	return fmt.Sprintf(`{"seq":%d,"type":"session.drain_acked_with_assigned_work","ts":%q,"actor":"gc","subject":"worker","message":"session drain-acked while still assigned to work bead","session_id":%q,"payload":{"session_id":%q,"bead_id":%q,"template":"worker","bead_status":"in_progress","reason":%q}}`,
		seq, ts.UTC().Format(time.RFC3339Nano), session, session, drainAckLoopBead, reason)
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
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = fmt.Sprintf("%q", l)
	}
	return fmt.Sprintf(`[{"id":%q,"title":"misrouted work","status":%q,"assignee":%q,"labels":[%s]}]`,
		drainAckLoopBead, status, assignee, strings.Join(quoted, ","))
}

// drainAckLoopRig is one run's scratch: the fake gc (events from
// events.jsonl, the bead from bead.json) and a stub escalate.sh that
// records each call's argv, one argument per line, ending "--".
type drainAckLoopRig struct {
	work, binDir, gcLog, escalate, escalateLog string
}

func newDrainAckLoopRig(t *testing.T, events []string, bead string) drainAckLoopRig {
	t.Helper()
	r := drainAckLoopRig{work: t.TempDir()}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(r.work, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("events.jsonl", strings.Join(events, "\n")+"\n")
	write("bead.json", bead+"\n")
	r.binDir, r.gcLog = fakeGCBin(t, `case "$1 $2 $3" in
"events --type session.drain_acked_with_assigned_work") cat "`+filepath.Join(r.work, "events.jsonl")+`" ;;
"bd show `+drainAckLoopBead+`") cat "`+filepath.Join(r.work, "bead.json")+`" ;;
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
// the session, the template and the count and gives the park command; the
// script writes nothing to the bead. A second firing passes the same
// fingerprint, so escalate.sh keeps it quiet.
func TestNotifyDrainAckLoopEscalatesOnce(t *testing.T) {
	r := newDrainAckLoopRig(t, loopCycles(3), loopBeadJSON("in_progress", "gc-1"))
	if out, err := r.run(t); err != nil {
		t.Fatalf("notify-drain-ack-loop: %v\n%s", err, out)
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
		drainAckLoopBead, "misrouted work", "gc-1", "worker", "3 drain-ack cycles", "1h",
		"bd set-state " + drainAckLoopBead + " hold=mayor",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("escalation %q does not contain %q", message, want)
		}
	}
	for _, call := range r.gcCalls(t) {
		if !strings.HasPrefix(call, "gc_events_--type_session.drain_acked_with_assigned_work_--since_1h") && call != "gc_bd_show_"+drainAckLoopBead+"_--json" {
			t.Errorf("gc call %q: the script may only read the events and the bead", call)
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
// escalates nothing.
func TestNotifyDrainAckLoopStaysQuiet(t *testing.T) {
	at := time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)
	working := loopBeadJSON("in_progress", "gc-1")
	for _, c := range []struct {
		name   string
		events []string
		bead   string
	}{
		{"two cycles", loopCycles(2), working},
		{"one of three is a restart handoff", append(loopCycles(2),
			drainAckEvent(9, at.Add(time.Hour), "gc-1", "restart_requested")), working},
		{"one of three is generic assigned work", append(loopCycles(2),
			drainAckEvent(9, at.Add(time.Hour), "gc-1", "drain_acked_with_assigned_work")), working},
		{"two events of one session 10s apart count once", append(loopCycles(2),
			drainAckEvent(9, at.Add(5*time.Minute+10*time.Second), "gc-1", "started_claim_kept")), working},
		{"the bead closed since", loopCycles(3), loopBeadJSON("closed", "gc-1")},
		{"the bead is held", loopCycles(3), loopBeadJSON("in_progress", "gc-1", "hold:mayor")},
		{"the bead is held external", loopCycles(3), loopBeadJSON("in_progress", "gc-1", "hold:external")},
		{"the bead was reassigned", loopCycles(3), loopBeadJSON("in_progress", "gc-7")},
		{"the bead was released", loopCycles(3), loopBeadJSON("open", "")},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newDrainAckLoopRig(t, c.events, c.bead)
			if out, err := r.run(t); err != nil {
				t.Fatalf("notify-drain-ack-loop: %v\n%s", err, out)
			}
			if calls := r.escalations(t); len(calls) != 0 {
				t.Fatalf("escalate.sh calls = %q, want none", calls)
			}
		})
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
		{name: "the bead read fails", events: loopCycles(3), bead: working, breaks: "bead.json", want: "reading bead " + drainAckLoopBead},
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
