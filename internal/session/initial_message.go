package session

import "strings"

// InitialMessageOverrideKey is the template_overrides key of a session's
// one-shot initial message: the user's first message, delivered with the
// first start.
const InitialMessageOverrideKey = "initial_message"

// startupNudgeSeparator separates the parts a startup nudge is composed of.
const startupNudgeSeparator = "\n\n---\n\n"

// initialMessagePrefix introduces the initial message inside a startup nudge.
const initialMessagePrefix = "User message:\n"

// AppendInitialMessage composes the startup nudge that carries msg, the
// initial message of a fresh start whose provider takes no prompt argument:
// nudge, the separator and msg under its "User message:" heading, or that
// heading and msg alone when nudge is empty.
func AppendInitialMessage(nudge, msg string) string {
	userMessage := initialMessagePrefix + msg
	if nudge != "" {
		return nudge + startupNudgeSeparator + userMessage
	}
	return userMessage
}

// SplitInitialMessage reverses AppendInitialMessage: it reports the nudge
// text was composed from, and whether text is exactly that composition of
// msg. An empty msg composes nothing.
func SplitInitialMessage(text, msg string) (turn string, ok bool) {
	if msg == "" {
		return "", false
	}
	userMessage := initialMessagePrefix + msg
	if text == userMessage {
		return "", true
	}
	turn, found := strings.CutSuffix(text, startupNudgeSeparator+userMessage)
	if !found || turn == "" {
		return "", false
	}
	return turn, true
}
