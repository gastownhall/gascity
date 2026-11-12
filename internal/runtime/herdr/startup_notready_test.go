package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestMarkersNotQuotedBy(t *testing.T) {
	markers := []string{"policy is still loading", "try again later"}
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{name: "nothing quoted", text: "run your hook", want: markers},
		{name: "one quoted", text: "if you see policy is still loading, wait", want: []string{"try again later"}},
		{name: "quoted across a wrap", text: "if you see policy is\n  still   loading, wait", want: []string{"try again later"}},
		{name: "both quoted", text: "policy is still loading; try  again\tlater", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := markersNotQuotedBy(markers, tc.text); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("markersNotQuotedBy = %q, want %q", got, tc.want)
			}
		})
	}
	if got := markersNotQuotedBy(nil, "anything"); got != nil {
		t.Errorf("markersNotQuotedBy(nil) = %q, want nil", got)
	}
}

func TestCountMarkers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		screen  string
		markers []string
		want    []int
	}{
		{name: "absent", screen: "> ", markers: []string{"still loading"}, want: []int{0}},
		{name: "one line", screen: "Managed account policy is still loading.\n> ", markers: []string{"policy is still loading"}, want: []int{1}},
		{name: "wrapped across lines", screen: "Managed account policy is\nstill loading.\n> ", markers: []string{"policy is still loading"}, want: []int{1}},
		{name: "twice", screen: "policy is still loading\npolicy is still loading\n", markers: []string{"policy is still loading"}, want: []int{2}},
		{name: "empty marker never counts", screen: "anything", markers: []string{"", "anything"}, want: []int{0, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := countMarkers(tc.screen, tc.markers); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("countMarkers = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestComposerHoldsText(t *testing.T) {
	for _, tc := range []struct {
		name         string
		screen       string
		prefix       string
		wantHolds    bool
		wantObserved bool
	}{
		{name: "bare prompt is empty", screen: "history\n> ", prefix: "> ", wantHolds: false, wantObserved: true},
		{name: "prompt with text", screen: "history\n> first turn", prefix: "> ", wantHolds: true, wantObserved: true},
		{name: "last prompt line wins over an echo", screen: "> past turn\nreply\n> ", prefix: "> ", wantHolds: false, wantObserved: true},
		{name: "last prompt line wins when it holds text", screen: "> \nreply\n> typed", prefix: "> ", wantHolds: true, wantObserved: true},
		{name: "NBSP reads as a space", screen: "history\n> typed", prefix: "> ", wantHolds: true, wantObserved: true},
		{name: "NBSP prefix", screen: "history\n> ", prefix: "> ", wantHolds: false, wantObserved: true},
		{name: "no prompt line", screen: "booting...", prefix: "> ", wantHolds: false, wantObserved: false},
		{name: "empty prefix", screen: "> typed", prefix: "", wantHolds: false, wantObserved: false},
		{name: "whitespace prefix", screen: "> typed", prefix: "  ", wantHolds: false, wantObserved: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holds, observed := composerHoldsText(tc.screen, tc.prefix)
			if holds != tc.wantHolds || observed != tc.wantObserved {
				t.Errorf("composerHoldsText = (%v, %v), want (%v, %v)", holds, observed, tc.wantHolds, tc.wantObserved)
			}
		})
	}
}

func TestStartupTimeoutMS(t *testing.T) {
	if got := startupTimeoutMS(context.Background(), 60000); got != 60000 {
		t.Errorf("no deadline: startupTimeoutMS = %d, want 60000", got)
	}

	far, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if got := startupTimeoutMS(far, 30000); got != 30000 {
		t.Errorf("far deadline: startupTimeoutMS = %d, want 30000", got)
	}

	near, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if got := startupTimeoutMS(near, 60000); got <= 0 || got > 8000 {
		t.Errorf("10s deadline: startupTimeoutMS = %d, want in (0, 8000]", got)
	}

	inside, cancel3 := context.WithTimeout(context.Background(), startupDeadlineMargin/2)
	defer cancel3()
	if got := startupTimeoutMS(inside, 60000); got > 0 {
		t.Errorf("deadline inside the margin: startupTimeoutMS = %d, want <= 0", got)
	}
}

func TestNotReadyGiveUp(t *testing.T) {
	c := &client{notReadyBudget: time.Minute}
	first := time.Now()

	if got, want := c.notReadyGiveUp(context.Background(), first), first.Add(time.Minute); !got.Equal(want) {
		t.Errorf("no deadline: notReadyGiveUp = %v, want %v", got, want)
	}

	far, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if got, want := c.notReadyGiveUp(far, first), first.Add(time.Minute); !got.Equal(want) {
		t.Errorf("far deadline: notReadyGiveUp = %v, want the budget %v", got, want)
	}

	near, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	deadline, _ := near.Deadline()
	if got, want := c.notReadyGiveUp(near, first), deadline.Add(-startupNotReadyReserve); !got.Equal(want) {
		t.Errorf("near deadline: notReadyGiveUp = %v, want the reserve %v", got, want)
	}
}

const (
	notReadyMarker = "policy is still loading"
	stalledReply   = `{"error":{"code":"agent_prompt_stalled","message":"no state change observed"}}`
	waitTimeout    = `{"error":{"code":"timeout","message":"timed out"}}`
)

// notReadyFake writes a fake herdr for the not-ready re-prompt. It counts the
// `agent prompt --wait` calls in a state file; the Nth (1-based) succeeds when
// N >= succeedOn (never when succeedOn is 0) and stalls otherwise. `pane read`
// runs screenScript, a sh snippet that sees the prompt count so far as $N.
// `agent get` reports an idle agent, `agent wait` times out, and every argv is
// appended to the returned trace.
func notReadyFake(t *testing.T, succeedOn int, screenScript string) (bin, trace string) {
	t.Helper()
	dir := t.TempDir()
	trace = filepath.Join(dir, "verbs")
	count := filepath.Join(dir, "n")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do echo \"$a\" >> " + trace + "; done\n" +
		"echo '---' >> " + trace + "\n" +
		"N=$(cat " + count + " 2>/dev/null || echo 0)\n" +
		"case \"$*\" in\n" +
		"  *'agent list'*)\n" +
		"    echo '{\"result\":{\"agents\":[{\"agent\":\"copilot\",\"name\":\"a0\",\"pane_id\":\"w1:p1\",\"agent_status\":\"idle\"}]}}'\n" +
		"    ;;\n" +
		"  *'agent get'*)\n" +
		"    echo '{\"result\":{\"agent\":{\"agent\":\"copilot\",\"name\":\"a0\",\"pane_id\":\"w1:p1\",\"agent_status\":\"idle\"}}}'\n" +
		"    ;;\n" +
		"  *'agent prompt'*)\n" +
		"    N=$((N+1)); echo $N > " + count + "\n" +
		"    if [ " + strconv.Itoa(succeedOn) + " -gt 0 ] && [ $N -ge " + strconv.Itoa(succeedOn) + " ]; then\n" +
		"      echo '{\"result\":{}}'\n" +
		"    else\n" +
		"      echo '" + stalledReply + "' >&2; exit 1\n" +
		"    fi\n" +
		"    ;;\n" +
		"  *'agent wait'*)\n" +
		"    echo '" + waitTimeout + "' >&2; exit 1\n" +
		"    ;;\n" +
		"  *'pane read'*)\n" +
		screenScript + "\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	return writeFakeHerdr(t, script), trace
}

func readTrace(t *testing.T, trace string) string {
	t.Helper()
	b, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("reading the verb trace: %v", err)
	}
	return string(b)
}

func promptCount(trace string) int {
	return strings.Count(trace, "--session\ngc-test\nagent\nprompt\n")
}

func notReadyClient(bin string, budget time.Duration) *client {
	return &client{session: "gc-test", bin: bin, rejectDelay: time.Millisecond, notReadyBudget: budget}
}

// (a) The first submit stalls and the marker appears with an empty composer on
// an idle agent: the agent consumed and discarded the turn, so it is prompted
// again, and the second attempt is taken.
func TestDeliverStartupTurnRepromptsATurnRejectedAsNotReady(t *testing.T) {
	bin, trace := notReadyFake(t, 2,
		`    if [ $N -ge 1 ]; then echo 'Managed account policy is still loading.'; fi; echo '> '`)
	c := notReadyClient(bin, time.Minute)

	if err := c.deliverStartupTurn(context.Background(), "w1:p1", "first turn", bindModeAgent, []string{notReadyMarker}, "> "); err != nil {
		t.Fatalf("deliverStartupTurn: %v", err)
	}
	got := readTrace(t, trace)
	if n := promptCount(got); n != 2 {
		t.Fatalf("prompts = %d, want 2 (one re-prompt):\n%s", n, got)
	}
	if strings.Contains(got, "send-keys") {
		t.Fatalf("a rejected turn was answered with an Enter, not a re-prompt:\n%s", got)
	}
}

// (b) A marker already on screen before the submit says nothing about this
// submit: the stall goes to the Enter recovery, never a re-prompt.
func TestDeliverStartupTurnIgnoresAMarkerAlreadyOnScreen(t *testing.T) {
	bin, trace := notReadyFake(t, 2,
		`    echo 'Managed account policy is still loading.'; echo '> '`)
	c := notReadyClient(bin, time.Minute)

	if err := c.deliverStartupTurn(context.Background(), "w1:p1", "first turn", bindModeAgent, []string{notReadyMarker}, "> "); err == nil {
		t.Fatal("deliverStartupTurn reported an unrecovered stall as delivered")
	}
	got := readTrace(t, trace)
	if n := promptCount(got); n != 1 {
		t.Fatalf("prompts = %d, want 1 (no re-prompt):\n%s", n, got)
	}
	if !strings.Contains(got, "send-keys") {
		t.Fatalf("the stall was not handed to the Enter recovery:\n%s", got)
	}
}

// (c) A marker that rises while the composer still holds the turn is not a
// rejection: a re-prompt would type the turn twice.
func TestDeliverStartupTurnDoesNotRepromptWhileTheComposerHoldsText(t *testing.T) {
	bin, trace := notReadyFake(t, 2,
		`    if [ $N -ge 1 ]; then echo 'Managed account policy is still loading.'; echo '> first turn'; else echo '> '; fi`)
	c := notReadyClient(bin, time.Minute)

	if err := c.deliverStartupTurn(context.Background(), "w1:p1", "first turn", bindModeAgent, []string{notReadyMarker}, "> "); err == nil {
		t.Fatal("deliverStartupTurn reported an unrecovered stall as delivered")
	}
	if got := readTrace(t, trace); promptCount(got) != 1 {
		t.Fatalf("prompts = %d, want 1 (no re-prompt with text in the box):\n%s", promptCount(got), got)
	}
}

// (d) An agent that keeps rejecting the turn is re-prompted only until the
// budget runs out, and the error says so.
func TestDeliverStartupTurnGivesUpWhenTheNotReadyBudgetRunsOut(t *testing.T) {
	bin, trace := notReadyFake(t, 0,
		`    i=0; while [ $i -lt $N ]; do echo 'Managed account policy is still loading.'; i=$((i+1)); done; echo '> '`)
	c := notReadyClient(bin, 200*time.Millisecond)

	err := c.deliverStartupTurn(context.Background(), "w1:p1", "first turn", bindModeAgent, []string{notReadyMarker}, "> ")
	if err == nil {
		t.Fatal("deliverStartupTurn reported a turn the agent never took as delivered")
	}
	if !strings.Contains(err.Error(), "rejected as not ready") || !strings.Contains(err.Error(), "attempts") {
		t.Fatalf("error = %v, want it to report the not-ready rejection and the attempts", err)
	}
	if got := readTrace(t, trace); promptCount(got) < 2 {
		t.Fatalf("prompts = %d, want re-prompts before giving up:\n%s", promptCount(got), got)
	}
}

// (e) herdr refusing a nudge with agent_blocked comes back as
// runtime.ErrAgentBlocked, with herdr's code still readable in the chain, and
// nothing is typed into the pane.
func TestDeliverNudgeReportsAgentBlocked(t *testing.T) {
	bin, trace := registryFake(t, []string{"w1:p1"},
		`!{"error":{"code":"agent_blocked","message":"agent w1:p1 is waiting on input"}}`)
	c := &client{session: "gc-test", bin: bin}

	err := c.deliverNudge(context.Background(), "w1:p1", "proceed")
	if !errors.Is(err, runtime.ErrAgentBlocked) {
		t.Fatalf("deliverNudge error = %v, want runtime.ErrAgentBlocked", err)
	}
	if code := herdrAnswerCode(err); code != "agent_blocked" {
		t.Fatalf("herdrAnswerCode = %q, want agent_blocked", code)
	}
	assertPasted(t, trace, false)
}
