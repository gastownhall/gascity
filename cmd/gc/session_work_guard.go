package main

import (
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func buildAssignedWorkIndex(workBeads []beads.Bead) map[string]bool {
	if workBeads == nil {
		return nil
	}
	index := make(map[string]bool, len(workBeads))
	for _, wb := range workBeads {
		if wb.Status != "open" && wb.Status != "in_progress" {
			continue
		}
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" {
			continue
		}
		index[assignee] = true
	}
	return index
}

func closeSessionBeadIfUnassigned(
	store beads.Store,
	session beads.Bead,
	assigneeHasWork map[string]bool,
	reason string,
	now time.Time,
	stderr io.Writer,
) bool {
	if sessionHasAssignedWork(session, assigneeHasWork) {
		return false
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return closeBead(store, session.ID, reason, now, stderr)
}
