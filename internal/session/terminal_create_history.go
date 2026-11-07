package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// LatestEphemeralTerminalFailure returns the most recent provider-terminal
// failure stamped on an ephemeral session of template whose worker directory
// is workDir, reading open and closed history alike: a failed-create row is
// closed at once, so the open set alone never sees it. Rows stamped after
// notAfter are ignored, since a failure cannot postdate the moment it is
// asked about and a future stamp would otherwise outrank every real one.
//
// Rows without a terminal-error reason or with a missing or unparseable
// timestamp are skipped. The bool reports whether a qualifying row was found.
// A failed read is returned as an error, never as "no history", so the caller
// decides how to degrade.
func (s *Store) LatestEphemeralTerminalFailure(template, workDir string, notAfter time.Time) (time.Time, bool, error) {
	if s == nil || s.store == (beads.SessionStore{}) {
		return time.Time{}, false, nil
	}
	rows, err := ListAllSessionBeads(s.store, beads.ListQuery{
		IncludeClosed: true,
		Metadata: map[string]string{
			"template":       template,
			"session_origin": "ephemeral",
		},
	})
	if err != nil {
		return time.Time{}, false, fmt.Errorf("listing ephemeral session history for template %q: %w", template, err)
	}
	var latest time.Time
	var found bool
	for _, row := range rows {
		if strings.TrimSpace(row.Metadata["provider_terminal_error"]) == "" {
			continue
		}
		if WorkerDirFromInfo(infoFromPersistedBead(row)) != workDir {
			continue
		}
		failedAt, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(row.Metadata["provider_terminal_error_at"]))
		if parseErr != nil || failedAt.After(notAfter) {
			continue
		}
		if !found || failedAt.After(latest) {
			latest, found = failedAt, true
		}
	}
	return latest, found, nil
}
