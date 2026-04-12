//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// TestGastown_MailRoundTrip validates the full mail lifecycle between
// human and agent using a bash agent that auto-replies.
func TestGastown_MailRoundTrip(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "bash " + agentScript("loop-mail.sh")},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Human sends message to mayor.
	sendMail(t, cityDir, "mayor", "Are you alive?")

	// Wait for the mayor to reply.
	waitForMail(t, cityDir, "human", "ack from mayor", 10*time.Second)
}

// TestGastown_MailAgentToAgent validates mail routing between two agents.
func TestGastown_MailAgentToAgent(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
		{Name: "deacon", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Send from mayor (simulating agent context) to deacon.
	sendMail(t, cityDir, "deacon", "Start patrol")

	// Verify deacon has the mail.
	out, err := gc(cityDir, "mail", "inbox", "deacon")
	if err != nil {
		t.Fatalf("gc mail inbox failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Start patrol") {
		t.Errorf("expected 'Start patrol' in deacon inbox:\n%s", out)
	}
}

// TestGastown_MailCheck validates gc mail check exit codes.
func TestGastown_MailCheck(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// No mail → exit 1.
	_, err := gc(cityDir, "mail", "check", "mayor")
	if err == nil {
		t.Error("expected gc mail check to fail when inbox is empty")
	}

	// Send mail → exit 0.
	sendMail(t, cityDir, "mayor", "hello")
	_, err = gc(cityDir, "mail", "check", "mayor")
	if err != nil {
		t.Error("expected gc mail check to succeed after mail sent")
	}
}

// TestGastown_MailArchive validates that archiving removes from inbox.
func TestGastown_MailArchive(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	sendMail(t, cityDir, "mayor", "archive me")

	// Verify in inbox.
	out, err := gc(cityDir, "mail", "inbox", "mayor")
	if err != nil {
		t.Fatalf("gc mail inbox failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "archive me") {
		t.Fatalf("expected message in inbox:\n%s", out)
	}

	msgID := extractBeadID(t, out)

	// Archive it.
	out, err = gc(cityDir, "mail", "archive", msgID)
	if err != nil {
		t.Fatalf("gc mail archive failed: %v\noutput: %s", err, out)
	}

	// No longer in inbox.
	out, err = gc(cityDir, "mail", "inbox", "mayor")
	if err != nil {
		t.Fatalf("gc mail inbox after archive failed: %v\noutput: %s", err, out)
	}
	if strings.Contains(out, "archive me") {
		t.Errorf("archived message should not appear in inbox:\n%s", out)
	}
}
