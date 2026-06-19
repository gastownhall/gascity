//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

// TestMail_BashAgent validates the full mail round-trip using a bash script
// as the agent implementation. The bash script (test/agents/loop-mail.sh)
// runs the same gc commands that a real agent would execute from
// prompts/loop-mail.md:
//
//  1. Human sends a message to the agent
//  2. Agent checks inbox, reads message, sends reply
//  3. Human receives the reply
//
// Everything goes through the front door: gc init, gc start (with a real
// city.toml config), gc mail send/inbox, gc stop.
func TestMail_BashAgent(t *testing.T) {
	agents := []agentConfig{
		{Name: "mayor", StartCommand: "bash " + agentScript("loop-mail.sh")},
	}

	var cityDir string
	if usingSubprocess() {
		cityDir = setupCityNoGuard(t, agents)
	} else {
		guard := tmuxtest.NewGuard(t)
		cityDir = setupCity(t, guard, agents)
	}

	// Human sends a message to the mayor.
	out, err := gc(cityDir, "mail", "send", "mayor", "hey, are you there?")
	if err != nil {
		t.Fatalf("gc mail send failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Sent message") {
		t.Fatalf("unexpected send output: %s", out)
	}

	// Poll for the agent's reply in the human inbox.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, _ = gc(cityDir, "mail", "inbox")
		if strings.Contains(out, "ack from mayor") {
			t.Logf("Got reply: %s", out)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Timed out — dump diagnostics.
	inbox, _ := gc(cityDir, "mail", "inbox")
	beadList, _ := gc(cityDir, "bead", "list")
	t.Fatalf("timed out waiting for agent reply\nhuman inbox:\n%s\nbead list:\n%s", inbox, beadList)
}
