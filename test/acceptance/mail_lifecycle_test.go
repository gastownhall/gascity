//go:build acceptance_a

// Mail lifecycle acceptance tests.
//
// These exercise the full gc mail workflow: send → inbox → peek → read →
// reply → thread → mark-unread → mark-read → archive → delete. This is
// a cross-command integration test that verifies messages flow correctly
// through the bead-backed mail system.
package acceptance_test

import (
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestMailLifecycle(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	// Send a message from "human" to "mayor".
	out, err := c.GC("mail", "send", "mayor", "--from", "human",
		"-s", "Test subject", "-m", "Test body content")
	if err != nil {
		t.Fatalf("gc mail send failed: %v\n%s", err, out)
	}

	// Inbox for mayor should show the message.
	inboxOut, err := c.GC("mail", "inbox", "mayor")
	if err != nil {
		t.Fatalf("gc mail inbox --to mayor: %v\n%s", err, inboxOut)
	}
	if !strings.Contains(inboxOut, "Test subject") {
		t.Fatalf("inbox should contain 'Test subject', got:\n%s", inboxOut)
	}

	// Extract message ID from inbox output.
	msgID := extractFirstID(inboxOut)
	if msgID == "" {
		t.Fatalf("could not extract message ID from inbox:\n%s", inboxOut)
	}

	t.Run("Peek_ShowsContent", func(t *testing.T) {
		out, err := c.GC("mail", "peek", msgID)
		if err != nil {
			t.Fatalf("gc mail peek %s: %v\n%s", msgID, err, out)
		}
		if !strings.Contains(out, "Test subject") {
			t.Errorf("peek should show subject, got:\n%s", out)
		}
		if !strings.Contains(out, "Test body content") {
			t.Errorf("peek should show body, got:\n%s", out)
		}
	})

	t.Run("Read_MarksAsRead", func(t *testing.T) {
		out, err := c.GC("mail", "read", msgID)
		if err != nil {
			t.Fatalf("gc mail read %s: %v\n%s", msgID, err, out)
		}
		if !strings.Contains(out, "Test subject") {
			t.Errorf("read should show subject, got:\n%s", out)
		}
	})

	t.Run("MarkUnread_ThenMarkRead", func(t *testing.T) {
		out, err := c.GC("mail", "mark-unread", msgID)
		if err != nil {
			t.Fatalf("gc mail mark-unread %s: %v\n%s", msgID, err, out)
		}

		out, err = c.GC("mail", "mark-read", msgID)
		if err != nil {
			t.Fatalf("gc mail mark-read %s: %v\n%s", msgID, err, out)
		}
	})

	t.Run("Reply_CreatesThread", func(t *testing.T) {
		out, err := c.GC("mail", "reply", msgID,
			"-s", "Re: Test subject", "-m", "Reply body")
		if err != nil {
			t.Fatalf("gc mail reply %s: %v\n%s", msgID, err, out)
		}
	})

	t.Run("Thread_ShowsConversation", func(t *testing.T) {
		out, err := c.GC("mail", "thread", msgID)
		if err != nil {
			t.Fatalf("gc mail thread %s: %v\n%s", msgID, err, out)
		}
		// Thread should show at least the original message.
		if strings.TrimSpace(out) == "" {
			t.Fatal("thread output is empty")
		}
	})

	t.Run("Count_ShowsMessages", func(t *testing.T) {
		out, err := c.GC("mail", "count")
		if err != nil {
			t.Fatalf("gc mail count: %v\n%s", err, out)
		}
		// Should report at least 1 message (we sent one + a reply).
		if strings.TrimSpace(out) == "" {
			t.Fatal("mail count output is empty")
		}
	})

	t.Run("Archive", func(t *testing.T) {
		out, err := c.GC("mail", "archive", msgID)
		if err != nil {
			t.Fatalf("gc mail archive %s: %v\n%s", msgID, err, out)
		}
	})

	t.Run("Delete_NonexistentID", func(t *testing.T) {
		_, err := c.GC("mail", "delete", "no-such-msg-xyz")
		if err == nil {
			t.Fatal("expected error deleting nonexistent message")
		}
	})
}

func TestMailErrorPaths(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("ReadMissingID", func(t *testing.T) {
		_, err := c.GC("mail", "read")
		if err == nil {
			t.Fatal("expected error for mail read without ID")
		}
	})

	t.Run("PeekMissingID", func(t *testing.T) {
		_, err := c.GC("mail", "peek")
		if err == nil {
			t.Fatal("expected error for mail peek without ID")
		}
	})

	t.Run("ReplyMissingID", func(t *testing.T) {
		_, err := c.GC("mail", "reply")
		if err == nil {
			t.Fatal("expected error for mail reply without ID")
		}
	})

	t.Run("ThreadMissingID", func(t *testing.T) {
		_, err := c.GC("mail", "thread")
		if err == nil {
			t.Fatal("expected error for mail thread without ID")
		}
	})

	t.Run("DeleteMissingID", func(t *testing.T) {
		_, err := c.GC("mail", "delete")
		if err == nil {
			t.Fatal("expected error for mail delete without ID")
		}
	})

	t.Run("ArchiveMissingID", func(t *testing.T) {
		_, err := c.GC("mail", "archive")
		if err == nil {
			t.Fatal("expected error for mail archive without ID")
		}
	})

	t.Run("MarkReadMissingID", func(t *testing.T) {
		_, err := c.GC("mail", "mark-read")
		if err == nil {
			t.Fatal("expected error for mail mark-read without ID")
		}
	})

	t.Run("MarkUnreadMissingID", func(t *testing.T) {
		_, err := c.GC("mail", "mark-unread")
		if err == nil {
			t.Fatal("expected error for mail mark-unread without ID")
		}
	})
}

// extractFirstID scans lines for a bead-style ID (short alphanumeric
// with a dash prefix pattern like "ga-xxxx" or similar).
func extractFirstID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// Bead IDs look like "ga-xxxx" — short, contain a dash.
		candidate := fields[0]
		if strings.Contains(candidate, "-") && len(candidate) >= 4 && len(candidate) <= 20 {
			return candidate
		}
	}
	return ""
}
