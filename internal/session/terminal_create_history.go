package session

import "time"

// LatestEphemeralTerminalFailure is scaffolding for the red commit.
func (s *Store) LatestEphemeralTerminalFailure(template, workDir string, notAfter time.Time) (time.Time, bool, error) {
	return time.Time{}, false, nil
}
