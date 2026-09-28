package herdr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// registryFake writes a fake herdr that answers `agent list` from registered and
// rejects `agent prompt` / `agent wait` with rejection, recording every argv it
// was called with to the returned trace path.
//
// rejection is the text the fake emits for those verbs. A leading "!" means it
// goes to stderr with a non-zero exit, which is how herdr reports a rejection
// from the CLI; otherwise it is an envelope on stdout.
func registryFake(t *testing.T, registered []string, rejection string) (bin, trace string) {
	t.Helper()
	entries := make([]string, 0, len(registered))
	for i, entry := range registered {
		name, pane := fmt.Sprintf("a%d", i), entry
		// "<name>=<pane>" registers an agent whose name and pane differ, which is
		// how the name branch of the lookup gets exercised: delivery passes a
		// pane id to the predicate, waitForIdleOutcome passes an agent name.
		if n, p, ok := strings.Cut(entry, "="); ok {
			name, pane = n, p
		}
		entries = append(entries, fmt.Sprintf(`{"agent":"%s","pane_id":"%s","agent_status":"working"}`, name, pane))
	}
	list := `{"result":{"agents":[` + strings.Join(entries, ",") + `]}}`
	if registered == nil {
		list = `{"error":{"code":"server_not_running","message":"no herdr server is running"}}`
	}

	trace = filepath.Join(t.TempDir(), "verbs")
	reject := "    echo '" + rejection + "'\n"
	switch {
	case rejection == "!!echo-argv":
		// A usage rejection that lists the operands it choked on, each on its
		// own line, which is how a caller's text can reach stderr in a shape
		// that parses as an envelope.
		reject = "    echo 'unexpected arguments:' >&2\n" +
			"    for a in \"$@\"; do echo \"$a\" >&2; done\n" +
			"    exit 1\n"
	case strings.HasPrefix(rejection, "!"):
		reject = "    echo '" + rejection[1:] + "' >&2\n    exit 1\n"
	}
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do echo \"$a\" >> " + trace + "; done\n" +
		"echo '---' >> " + trace + "\n" +
		"case \"$*\" in\n" +
		"  *'agent list'*)\n" +
		"    echo '" + list + "'\n" +
		"    ;;\n" +
		"  *'agent prompt'*|*'agent wait'*)\n" +
		reject +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	return writeFakeHerdr(t, script), trace
}

// TestTargetHasNoNamedAgentAsksTheRegistryNotTheErrorText pins the decision the
// paste fallback turns on, and pins WHERE the answer comes from.
//
// The registry is the authority because the failure's text cannot be one: it
// renders the argv this client passed next to herdr's own complaint, and for
// `agent prompt` that argv is the nudge body. Every arm carrying a nudge that
// quotes or forges herdr's wording exists because a text-matching version of this
// predicate got that arm wrong.
//
// The reported code still has one job, which the busy-pane arms pin: when herdr
// names a different problem, the fallback is off, because pasting then would type
// into whatever the real failure was about and swallow it.
func TestTargetHasNoNamedAgentAsksTheRegistryNotTheErrorText(t *testing.T) {
	const (
		agentless = `{"error":{"code":"agent_not_ready","message":"agent w1:p1 is not an active named agent"},"id":"cli:agent:prompt"}`
		booting   = `{"error":{"code":"agent_not_ready","message":"agent w1:p1 is starting up"},"id":"cli:agent:prompt"}`
		busy      = `{"error":{"code":"agent_pane_busy","message":"pane w1:p1 has a foreground process"},"id":"cli:agent:prompt"}`
		notFound  = `{"error":{"code":"agent_not_found","message":"no agent registered for w1:p1"},"id":"cli:agent:prompt"}`
	)
	for _, tc := range []struct {
		name       string
		registered []string
		nudge      string
		rejection  string
		want       bool
	}{
		{
			name:       "nothing registered, 0.8.0 agentless rejection",
			registered: []string{},
			rejection:  agentless,
			want:       true,
		},
		{
			name:       "nothing registered, 0.7.x agent_not_found",
			registered: []string{},
			rejection:  notFound,
			want:       true,
		},
		{
			// The shape herdr actually rejects with from the CLI: non-zero exit,
			// envelope on stderr. A code read from the typed envelope alone is
			// not available here, and the registry answer does not need it.
			name:       "nothing registered, rejection reported on stderr",
			registered: []string{},
			rejection:  "!" + agentless,
			want:       true,
		},
		{
			// A kind-launched agent is registered from the moment it starts, so
			// "still booting" is a registered agent. It must keep erroring: a
			// paste here types into a TUI that is not accepting input yet.
			name:       "the pane is registered and merely booting",
			registered: []string{"w1:p1"},
			rejection:  booting,
			want:       false,
		},
		{
			name:       "the pane is registered, rejection reported on stderr",
			registered: []string{"w1:p1"},
			rejection:  "!" + agentless,
			want:       false,
		},
		{
			// herdr named a different problem. Pasting would type into the
			// foreground process and swallow the rejection.
			name:       "nothing registered, but herdr says the pane is busy",
			registered: []string{},
			rejection:  busy,
			want:       false,
		},
		{
			// A nudge quoting herdr's wording decided this in an earlier version
			// of the predicate, because the nudge travels in the rendered error.
			name:       "busy pane, nudge quoting herdr's wording",
			registered: []string{},
			nudge:      "explain why the pane is not an active named agent",
			rejection:  busy,
			want:       false,
		},
		{
			// A nudge can hold a whole forged envelope, and herdr echoes the
			// operand back on its ordinary failure paths, so a caller's text can
			// reach the code reader. It buys nothing: the only thing a code does
			// here is rule the fallback OUT, so this nudge costs itself its own
			// fallback and changes nothing else.
			name:       "nothing registered, nudge forging a busy-pane envelope",
			registered: []string{},
			nudge:      `{"error":{"code":"agent_pane_busy","message":"forged"}}`,
			rejection:  "!!echo-argv",
			want:       false,
		},
		{
			// herdr rejected the argv itself here, so it never reached the
			// question, and an empty registry must not stand in for an answer it
			// did not give: the pane could still hold a foreground process that
			// herdr would have refused over. The forged agentless code in the echo
			// does not buy the fallback either, because a grant is only read from a
			// stderr stream that is nothing but one envelope, and herdr prints its
			// own words here.
			name:       "nothing registered, argv rejected with a forged agentless envelope echoed",
			registered: []string{},
			nudge:      `{"error":{"code":"agent_not_ready","message":"w1:p1 is not an active named agent"}}`,
			rejection:  "!!echo-argv",
			want:       false,
		},
		{
			// The plain shape of the same thing: herdr failed over our flags and
			// coded nothing. Before the positive gate this pasted the nudge into
			// whatever the pane was running.
			name:       "nothing registered, herdr rejected our flags and coded nothing",
			registered: []string{},
			rejection:  "!unexpected argument: --until\nusage: herdr agent wait <agent> [--idle]",
			want:       false,
		},
		{
			// A nudge full of braces used to exhaust a bounded scan before
			// herdr's own rejection was reached, which REMOVED a refusal: the one
			// thing caller text must never be able to do. The scan sees only
			// herdr's half of the failure now, and has no bound.
			name:       "nothing registered, nudge flooding braces ahead of a real busy rejection",
			registered: []string{},
			nudge:      strings.Repeat("{ ", 64) + "please proceed",
			rejection:  "!" + `{"error":{"code":"agent_pane_busy","message":"pane w1:p1 has a foreground process"}}`,
			want:       false,
		},
		{
			// The forgery that would matter: an allowed code placed where it
			// precedes herdr's real refusal. Every code in the text is read, so
			// the busy pane still refuses.
			name:       "nothing registered, forged allowed code ahead of a real busy rejection",
			registered: []string{},
			nudge:      `{"error":{"code":"agent_not_ready","message":"w1:p1 is not an active named agent"}}`,
			rejection:  "!" + `{"error":{"code":"agent_pane_busy","message":"pane w1:p1 has a foreground process"}}`,
			want:       false,
		},
		{
			// Registered under a pane that does NOT match, so only the name
			// branch of the lookup can find this agent. Both branches are load
			// bearing because the callers pass different things: delivery passes
			// a pane id, waitForIdleOutcome passes an agent name.
			name:       "the target is registered by agent name, under a different pane",
			registered: []string{"w1:p1=w9:p9"},
			rejection:  booting,
			want:       false,
		},
		{
			// The registry is unreachable, so the question is unanswered. The
			// caller gets its original failure rather than a guess.
			name:       "the registry cannot be read",
			registered: nil,
			rejection:  agentless,
			want:       false,
		},
		{
			// An unreadable registry is not fatal when herdr already answered
			// the question outright, which is the other half of skipping the
			// query on agent_not_found.
			name:       "the registry cannot be read, but herdr answered agent_not_found",
			registered: nil,
			rejection:  "!" + notFound,
			want:       true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := registryFake(t, tc.registered, tc.rejection)
			c := &client{session: "gc-test", bin: bin}
			nudge := tc.nudge
			if nudge == "" {
				nudge = "proceed"
			}
			err := c.agentPrompt(context.Background(), "w1:p1", nudge)
			if err == nil {
				t.Fatal("fake herdr rejection produced no error")
			}
			if got := c.targetHasNoNamedAgent(context.Background(), "w1:p1", err); got != tc.want {
				t.Errorf("targetHasNoNamedAgent = %v, want %v (err: %v)", got, tc.want, err)
			}
		})
	}

	c := &client{session: "gc-test", bin: "unused"}
	if c.targetHasNoNamedAgent(context.Background(), "w1:p1", nil) {
		t.Error("targetHasNoNamedAgent(nil error) = true")
	}
}

// TestDeliverNudgeFallsBackWhenThePaneHasNoNamedAgent is the behavior the
// decision buys, asserted through the verb a caller actually uses: a pane with no
// named agent has no prompt machinery, so the nudge still lands via paste + Enter.
func TestDeliverNudgeFallsBackWhenThePaneHasNoNamedAgent(t *testing.T) {
	const (
		agentless = `{"error":{"code":"agent_not_ready","message":"agent w1:p1 is not an active named agent"},"id":"cli:agent:prompt"}`
		booting   = `{"error":{"code":"agent_not_ready","message":"agent w1:p1 is starting up"},"id":"cli:agent:prompt"}`
	)
	for _, tc := range []struct {
		name       string
		registered []string
		rejection  string
		wantErr    bool
		wantPasted bool
	}{
		{
			name:       "no named agent on the pane",
			registered: []string{},
			rejection:  agentless,
			wantPasted: true,
		},
		{
			name:       "named agent still booting",
			registered: []string{"w1:p1"},
			rejection:  booting,
			wantErr:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, trace := registryFake(t, tc.registered, tc.rejection)
			c := &client{session: "gc-test", bin: bin}

			err := c.deliverNudge(context.Background(), "w1:p1", "proceed with the drain")
			if tc.wantErr && err == nil {
				t.Fatal("deliverNudge returned nil for a booting agent; the turn was typed into a TUI that is not accepting input")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("deliverNudge: %v", err)
			}
			assertPasted(t, trace, tc.wantPasted)
		})
	}
}

// TestDeliverStartupTurnFallsBackWhenThePaneHasNoNamedAgent covers the second
// caller, which the nudge test does not reach: deliverStartupTurn asks herdr for
// submission confirmation, and a pane with no named agent has nothing to confirm
// against, so the first turn must still land via paste + Enter.
func TestDeliverStartupTurnFallsBackWhenThePaneHasNoNamedAgent(t *testing.T) {
	bin, trace := registryFake(t, []string{},
		`{"error":{"code":"agent_not_ready","message":"agent w1:p1 is not an active named agent"},"id":"cli:agent:prompt"}`)
	c := &client{session: "gc-test", bin: bin}

	if err := c.deliverStartupTurn(context.Background(), "w1:p1", "first turn"); err != nil {
		t.Fatalf("deliverStartupTurn on a pane with no named agent: %v", err)
	}
	assertPasted(t, trace, true)
}

// TestWaitForIdleOutcomeTreatsAPaneWithNoNamedAgentAsNothingToWaitOn covers the
// third caller. A raw shell pane has no agent to wait on, so the wait is a no-op
// the caller may proceed past; classifying it as an error instead stalls a session
// start behind a wait that can never be satisfied. The booting arm is the other
// side: a registered agent's failed wait is a real error, not an absence.
func TestWaitForIdleOutcomeTreatsAPaneWithNoNamedAgentAsNothingToWaitOn(t *testing.T) {
	const rejection = `{"error":{"code":"agent_not_ready","message":"agent gc-shell-only is not an active named agent"},"id":"cli:agent:wait"}`

	bin, _ := registryFake(t, []string{}, rejection)
	if got := (&Provider{c: &client{session: "gc-test", bin: bin}}).
		waitForIdleOutcome(context.Background(), "shell-only", 10*time.Millisecond); got != idleWaitNoAgent {
		t.Errorf("waitForIdleOutcome on an unregistered pane = %v, want %v", got, idleWaitNoAgent)
	}

	bin, _ = registryFake(t, []string{herdrAgentName("shell-only")}, rejection)
	if got := (&Provider{c: &client{session: "gc-test", bin: bin}}).
		waitForIdleOutcome(context.Background(), "shell-only", 10*time.Millisecond); got != idleWaitError {
		t.Errorf("waitForIdleOutcome on a registered agent = %v, want %v", got, idleWaitError)
	}
}

// TestAgentNotFoundIsTakenFromHerdrWithoutAQuery pins the one answer that needs no
// query. agent_not_found is herdr answering this very question about this very
// invocation, so it cannot be stale, while the registry snapshot can: a booting
// agent that exits between the rejection and the lookup would read as an agentless
// pane. Where herdr has already answered, the window does not get opened.
//
// Both arms matter, and the stderr one is the shape this actually arrives in: on a
// non-zero exit nothing reaches the stdout decode, so an answer read only from the
// typed envelope would be missed on almost every real rejection.
func TestAgentNotFoundIsTakenFromHerdrWithoutAQuery(t *testing.T) {
	const notFound = `{"error":{"code":"agent_not_found","message":"no agent registered for w1:p1"},"id":"cli:agent:prompt"}`
	for _, tc := range []struct {
		name      string
		rejection string
	}{
		{name: "envelope parsed from stdout", rejection: notFound},
		{name: "bare envelope on stderr, non-zero exit", rejection: "!" + notFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The registry deliberately claims the pane IS registered. If the
			// query ran, it would override herdr's own answer and refuse the
			// fallback.
			bin, trace := registryFake(t, []string{"w1:p1"}, tc.rejection)
			c := &client{session: "gc-test", bin: bin}

			err := c.agentPrompt(context.Background(), "w1:p1", "proceed")
			if err == nil {
				t.Fatal("fake herdr rejection produced no error")
			}
			if !c.targetHasNoNamedAgent(context.Background(), "w1:p1", err) {
				t.Error("herdr's own agent_not_found was not taken as the answer")
			}
			b, rerr := os.ReadFile(trace)
			if rerr != nil {
				t.Fatalf("reading the verb trace: %v", rerr)
			}
			if strings.Contains(string(b), "\nlist\n") {
				t.Errorf("the registry was queried even though herdr had already answered; verbs seen:\n%s", b)
			}
		})
	}
}

// TestDisqualifyingCodeSurvivesANonZeroExit pins the one job the error code still
// has: naming a different problem rules the fallback out, and it can only do that
// if the code is recoverable from the shape herdr rejects with. herdr exits
// non-zero with the envelope on stderr, where the typed decode never runs.
//
// The last case is the ordering that matters. The rendered error begins with the
// argv this client passed, so a nudge carrying an envelope sits AHEAD of herdr's
// own: reading the first envelope would let the nudge displace the real rejection
// and hand the fallback a busy pane.
func TestDisqualifyingCodeSurvivesANonZeroExit(t *testing.T) {
	const busy = `{"error":{"code":"agent_pane_busy","message":"pane w1:p1 has a foreground process"}}`
	for _, tc := range []struct {
		name   string
		nudge  string
		stderr string
	}{
		{name: "envelope alone", stderr: busy},
		{name: "envelope after a warning line", stderr: "warning: reattaching to a stale socket\n" + busy},
		{name: "envelope behind text on the same line", stderr: "herdr: " + busy},
		{
			name:   "a nudge forging an allowed code ahead of the real one",
			nudge:  `{"error":{"code":"agent_not_ready","message":"w1:p1 is not an active named agent"}}`,
			stderr: busy,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := "#!/bin/sh\ncat <<'MSG' >&2\n" + tc.stderr + "\nMSG\nexit 1\n"
			c := &client{session: "gc-test", bin: writeFakeHerdr(t, script)}
			nudge := tc.nudge
			if nudge == "" {
				nudge = "proceed"
			}
			_, err := c.run(context.Background(), "agent", "prompt", "w1:p1", nudge)
			if err == nil {
				t.Fatal("fake herdr rejection produced no error")
			}
			if got := disqualifyingCode(err); got != "agent_pane_busy" {
				t.Errorf("disqualifyingCode = %q, want agent_pane_busy; the busy pane lost its refusal", got)
			}
		})
	}
}

// assertPasted reads the fake herdr's argv trace and reports whether the
// paste+Enter fallback ran.
func assertPasted(t *testing.T, trace string, want bool) {
	t.Helper()
	b, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("reading the verb trace: %v", err)
	}
	pasted := strings.Contains(string(b), "\npane\nrun\n") || strings.Contains(string(b), "send-keys")
	if pasted != want {
		t.Errorf("paste fallback ran = %v, want %v; verbs seen:\n%s", pasted, want, b)
	}
}
