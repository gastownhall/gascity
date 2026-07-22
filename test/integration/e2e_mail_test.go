//go:build integration

package integration

import (
	"strings"
	"testing"
)

// TestE2E_MailSend verifies that gc mail send creates a message bead.
func TestE2E_MailSend(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "mailrecv", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	out, err := gc(cityDir, "mail", "send", "mailrecv", "hello from e2e test")
	if err != nil {
		t.Fatalf("gc mail send failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Sent message") {
		t.Errorf("expected 'Sent message' in output:\n%s", out)
	}
}

// TestE2E_MailCheckInject verifies that gc mail check --inject wraps messages
// in system-reminder tags.
func TestE2E_MailCheckInject(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "injecter", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Send a message.
	out, err := gc(cityDir, "mail", "send", "injecter", "injected test")
	if err != nil {
		t.Fatalf("gc mail send failed: %v\noutput: %s", err, out)
	}

	// Check with --inject should wrap in system-reminder.
	out, err = gc(cityDir, "mail", "check", "--inject", "injecter")
	if err != nil {
		t.Fatalf("gc mail check --inject failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "<system-reminder>") {
		t.Errorf("expected <system-reminder> in inject output:\n%s", out)
	}
}

// TestE2E_MailInbox verifies that gc mail inbox lists unread messages.
func TestE2E_MailInbox(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "inboxer", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Send two messages.
	for _, body := range []string{"first msg", "second msg"} {
		out, err := gc(cityDir, "mail", "send", "inboxer", body)
		if err != nil {
			t.Fatalf("gc mail send %q failed: %v\noutput: %s", body, err, out)
		}
	}

	// Inbox should list both.
	out, err := gc(cityDir, "mail", "inbox", "inboxer")
	if err != nil {
		t.Fatalf("gc mail inbox failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "first msg") {
		t.Errorf("inbox missing 'first msg':\n%s", out)
	}
	if !strings.Contains(out, "second msg") {
		t.Errorf("inbox missing 'second msg':\n%s", out)
	}
}

// TestE2E_MailRead verifies that gc mail read displays and closes a message.
func TestE2E_MailRead(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "reader", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Send a message.
	out, err := gc(cityDir, "mail", "send", "reader", "read me please")
	if err != nil {
		t.Fatalf("gc mail send failed: %v\noutput: %s", err, out)
	}

	// Get the message ID from inbox.
	out, err = gc(cityDir, "mail", "inbox", "reader")
	if err != nil {
		t.Fatalf("gc mail inbox failed: %v\noutput: %s", err, out)
	}
	msgID := extractFirstBeadID(t, out)

	// Read the message.
	out, err = gc(cityDir, "mail", "read", msgID)
	if err != nil {
		t.Fatalf("gc mail read failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "read me please") {
		t.Errorf("mail read output missing body:\n%s", out)
	}

	// After reading, inbox should be empty.
	_, err = gc(cityDir, "mail", "check", "reader")
	if err == nil {
		t.Error("mail check should fail after reading all messages")
	}
}

// TestE2E_MailArchive verifies that gc mail archive closes a message
// without displaying it.
func TestE2E_MailArchive(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "archiver", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Send a message.
	out, err := gc(cityDir, "mail", "send", "archiver", "archive me")
	if err != nil {
		t.Fatalf("gc mail send failed: %v\noutput: %s", err, out)
	}

	// Get message ID.
	out, err = gc(cityDir, "mail", "inbox", "archiver")
	if err != nil {
		t.Fatalf("gc mail inbox failed: %v\noutput: %s", err, out)
	}
	msgID := extractFirstBeadID(t, out)

	// Archive without display.
	out, err = gc(cityDir, "mail", "archive", msgID)
	if err != nil {
		t.Fatalf("gc mail archive failed: %v\noutput: %s", err, out)
	}

	// After archiving, inbox should be empty.
	_, err = gc(cityDir, "mail", "check", "archiver")
	if err == nil {
		t.Error("mail check should fail after archiving all messages")
	}
}

// extractFirstBeadID extracts the first bead ID from tabular output.
// Looks for the ID column (first field in the data rows). Skips the
// header line (starts with "ID"). Works with both bd-* and gc-* prefixes.
func extractFirstBeadID(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 1 && fields[0] != "ID" && fields[0] != "" {
			return fields[0]
		}
	}
	t.Fatalf("no bead ID found in output:\n%s", output)
	return ""
}
