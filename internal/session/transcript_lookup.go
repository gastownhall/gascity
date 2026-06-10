package session

import (
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sessionlog"
	workertranscript "github.com/gastownhall/gascity/internal/worker/transcript"
)

// ResolveCodexTranscriptBySessionOrder maps an ambiguous same-workdir Codex
// session group to a transcript by using each session's wake/start timestamp.
// It returns empty unless the target session has a unique transcript in its
// start window, preserving ambiguity for underspecified groups.
func ResolveCodexTranscriptBySessionOrder(searchPaths []string, provider, workDir, targetID string, sessions []beads.Bead) string {
	if sessionlog.ProviderFamily(provider) != "codex" || strings.TrimSpace(workDir) == "" || strings.TrimSpace(targetID) == "" {
		return ""
	}
	type anchoredSession struct {
		id     string
		start  time.Time
		tieKey string
	}
	var anchored []anchoredSession
	for _, b := range sessions {
		if b.ID == "" || strings.TrimSpace(b.Metadata["work_dir"]) != workDir {
			continue
		}
		start := transcriptStartAnchor(b)
		if start.IsZero() {
			continue
		}
		anchored = append(anchored, anchoredSession{
			id:     b.ID,
			start:  start,
			tieKey: strings.TrimSpace(b.Metadata["session_name"]),
		})
	}
	if len(anchored) < 2 {
		return ""
	}
	sort.Slice(anchored, func(i, j int) bool {
		if anchored[i].start.Equal(anchored[j].start) {
			if anchored[i].tieKey == anchored[j].tieKey {
				return anchored[i].id < anchored[j].id
			}
			return anchored[i].tieKey < anchored[j].tieKey
		}
		return anchored[i].start.Before(anchored[j].start)
	})
	for i := 1; i < len(anchored); i++ {
		if anchored[i].start.Equal(anchored[i-1].start) {
			return ""
		}
	}
	for i, item := range anchored {
		if item.id != targetID {
			continue
		}
		var end time.Time
		for j := i + 1; j < len(anchored); j++ {
			if anchored[j].start.After(item.start) {
				end = anchored[j].start
				break
			}
		}
		return workertranscript.DiscoverCodexPathInTimeWindow(searchPaths, workDir, item.start, end)
	}
	return ""
}

func transcriptStartAnchor(b beads.Bead) time.Time {
	for _, key := range []string{"last_woke_at", "pending_create_started_at", "creation_complete_at"} {
		if parsed := parseTranscriptAnchorTime(b.Metadata[key]); !parsed.IsZero() {
			return parsed
		}
	}
	return b.CreatedAt
}

func parseTranscriptAnchorTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed
	}
	return time.Time{}
}
