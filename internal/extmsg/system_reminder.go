package extmsg

import "github.com/gastownhall/gascity/internal/promptsafe"

// SanitizeForSystemReminder strips literal <system-reminder> open and close
// tag sequences from user-controlled text before it is interpolated into a
// <system-reminder> block, so a sender cannot break out of the legitimate
// reminder and inject attacker-controlled instructions into the receiving
// agent's prompt.
//
// It delegates to promptsafe.SanitizeForSystemReminder, the single shared
// implementation used by every construction site (mail-check injection,
// nudge-queue injection, external-message notification, and the deferred-nudge
// reminders in internal/session and internal/worker). It is retained here so
// existing extmsg callers keep a stable entry point.
//
// See gastownhall/gascity#2195.
func SanitizeForSystemReminder(s string) string {
	return promptsafe.SanitizeForSystemReminder(s)
}
