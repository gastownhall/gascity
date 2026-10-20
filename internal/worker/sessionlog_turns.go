package worker

import "github.com/gastownhall/gascity/internal/sessionlog"

// Transcript turn reads (X11 TI-2b), passed through so internal/sessionlog
// stays behind the worker boundary.

// TurnSupportFor reports what provider's transcript family records about its
// turns (sessionlog.TurnSupportFor).
func (a SessionLogAdapter) TurnSupportFor(provider string) sessionlog.TurnSupport {
	return sessionlog.TurnSupportFor(provider)
}

// ScanTurns reads the turns of provider's transcript at path
// (sessionlog.ScanTurns). A torn or unreadable transcript is an error, never
// an idle scan.
func (a SessionLogAdapter) ScanTurns(provider, path string) (sessionlog.TurnScan, error) {
	return sessionlog.ScanTurns(provider, path)
}

// NewTurnFollower returns a follower of provider's transcript at path
// (sessionlog.NewTurnFollower), which reads only appended bytes.
func (a SessionLogAdapter) NewTurnFollower(provider, path string) (*sessionlog.TurnFollower, error) {
	return sessionlog.NewTurnFollower(provider, path)
}

// TurnTranscriptOwnedBy reports whether provider's transcript at path records
// the conversation of provider session key key
// (sessionlog.TurnTranscriptOwnedBy).
func (a SessionLogAdapter) TurnTranscriptOwnedBy(provider, path, key string) (bool, error) {
	return sessionlog.TurnTranscriptOwnedBy(provider, path, key)
}
