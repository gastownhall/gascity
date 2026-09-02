package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/mail"
)

// TestInjectPreviewSurfacesNewestUnread reproduces ga-18f84o: when unread mail
// exceeds mailInjectMaxMessages, formatInjectOutput clamps to messages[:limit]
// on an oldest-first slice, so the reminder shows the OLDEST N and a newly
// arrived urgent message is never surfaced. Because the stale oldest entries
// are never read, every later reminder cycle repeats the same window.
func TestInjectPreviewSurfacesNewestUnread(t *testing.T) {
	base := time.Date(2026, 8, 13, 14, 0, 0, 0, time.UTC)
	// Oldest-first, exactly as collectMailMessages returns them.
	messages := []mail.Message{
		{ID: "old-1", From: "human", Subject: "stale triage", Body: "b", CreatedAt: base},
		{ID: "old-2", From: "human", Subject: "human-hold", Body: "b", CreatedAt: base.Add(time.Hour)},
		{ID: "old-3", From: "human", Subject: "human-hold", Body: "b", CreatedAt: base.Add(2 * time.Hour)},
		{ID: "old-4", From: "human", Subject: "PR audit", Body: "b", CreatedAt: base.Add(3 * time.Hour)},
		{
			ID: "urgent-new", From: "gascity/investigator", Subject: "CORRECTION: stop before acting",
			Body: "duplicate-work stop order", CreatedAt: base.AddDate(0, 0, 19),
		},
	}

	out := formatInjectOutput(messages)

	if !strings.Contains(out, "urgent-new") {
		t.Errorf("newest unread message not surfaced in inject preview.\n"+
			"mailInjectMaxMessages=%d, unread=%d -- the clamp took the OLDEST %d.\nGot:\n%s",
			mailInjectMaxMessages, len(messages), mailInjectMaxMessages, out)
	}
}

// TestInjectPreviewPriorityStillOutranksRecency guards the priority contract
// while fixing the recency clamp above: a higher-priority message must still
// surface ahead of a newer priority-0 message, even though the window
// selection now prefers newest within a priority tier. Without this, a fix
// that just switched the clamp to "newest N overall" would silently drop the
// priority ordering that sortMailByPriority documents and that a tagged
// restart handoff (priority:1) depends on to survive the injection window.
func TestInjectPreviewPriorityStillOutranksRecency(t *testing.T) {
	base := time.Date(2026, 8, 13, 14, 0, 0, 0, time.UTC)
	messages := []mail.Message{
		{ID: "important-old", From: "human", Subject: "restart handoff", Body: "b", CreatedAt: base, Priority: 1},
		{ID: "new-1", From: "human", Subject: "chatter", Body: "b", CreatedAt: base.AddDate(0, 0, 10)},
		{ID: "new-2", From: "human", Subject: "chatter", Body: "b", CreatedAt: base.AddDate(0, 0, 11)},
		{ID: "new-3", From: "human", Subject: "chatter", Body: "b", CreatedAt: base.AddDate(0, 0, 12)},
		{ID: "new-4", From: "human", Subject: "chatter", Body: "b", CreatedAt: base.AddDate(0, 0, 13)},
	}

	out := formatInjectOutput(messages)

	if !strings.Contains(out, "important-old") {
		t.Errorf("priority>0 message dropped from inject window in favor of newer priority-0 mail.\nGot:\n%s", out)
	}
	if strings.Contains(out, "new-1") {
		t.Errorf("oldest priority-0 message should have been evicted by the recency tie-break.\nGot:\n%s", out)
	}
}
