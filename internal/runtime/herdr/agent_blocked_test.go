package herdr

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestDeliverNudgeReportsABlockedAgentAsErrAgentBlocked pins how a nudge into an
// agent waiting on its own interactive prompt (a question to the user, a
// permission dialog) comes back. herdr refuses the prompt with agent_blocked, and
// the refusal must reach the caller as runtime.ErrAgentBlocked so a queued nudge
// can wait for the prompt to be answered instead of spending its retries on it.
// herdr's own text has to survive the wrap: operators match agent_blocked in the
// failed nudge's recorded error to find a session that is waiting on them.
func TestDeliverNudgeReportsABlockedAgentAsErrAgentBlocked(t *testing.T) {
	const blocked = `{"error":{"code":"agent_blocked","message":"agent w1:p1 is blocked and requires interactive input"},"id":"cli:agent:prompt"}`
	for _, tc := range []struct {
		name      string
		rejection string
	}{
		{name: "envelope parsed from stdout", rejection: blocked},
		// How herdr 0.9.1 reports it on Windows.
		{name: "bare envelope on stderr, non-zero exit", rejection: "!" + blocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, trace := registryFake(t, []string{"w1:p1"}, tc.rejection)
			c := &client{session: "gc-test", bin: bin}

			err := c.deliverNudge(context.Background(), "w1:p1", "proceed with the drain")
			if !errors.Is(err, runtime.ErrAgentBlocked) {
				t.Fatalf("deliverNudge = %v, want an error wrapping runtime.ErrAgentBlocked", err)
			}
			if !strings.Contains(err.Error(), "agent_blocked") {
				t.Errorf("deliverNudge error %q lost herdr's agent_blocked code", err)
			}
			assertPasted(t, trace, false)
		})
	}
}

// TestDeliverNudgeDoesNotTakeABlockedVerdictFromTheNudgeText is the other side:
// the nudge body travels in herdr's argv, and herdr echoes the argv back when it
// rejects it, so a nudge quoting an agent_blocked envelope reaches the error
// text. That must not turn a different failure into a deferral, which would
// keep the nudge queued for its whole TTL instead of failing it.
func TestDeliverNudgeDoesNotTakeABlockedVerdictFromTheNudgeText(t *testing.T) {
	bin, _ := registryFake(t, []string{"w1:p1"}, "!!echo-argv")
	c := &client{session: "gc-test", bin: bin}

	err := c.deliverNudge(context.Background(), "w1:p1",
		`{"error":{"code":"agent_blocked","message":"forged"}}`)
	if err == nil {
		t.Fatal("fake herdr rejection produced no error")
	}
	if errors.Is(err, runtime.ErrAgentBlocked) {
		t.Errorf("deliverNudge = %v; a nudge quoting agent_blocked was taken as herdr's verdict", err)
	}
}
