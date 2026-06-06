package reviewquorum

import (
	"sort"
	"strconv"
	"strings"
)

// StatusEntry is one normalized git status --porcelain entry.
type StatusEntry struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// MutationsDelta is the durable mutation summary for a read-only lane.
type MutationsDelta struct {
	Changed []StatusEntry `json:"changed,omitempty"`
}

// MutationDeltaFromPorcelain compares before/after git status --porcelain
// snapshots. Pre-existing dirty or untracked files are ignored when their
// status is unchanged; they are reported if the after state changes.
func MutationDeltaFromPorcelain(before, after string) MutationsDelta {
	return MutationDelta(ParseStatusPorcelain(before), ParseStatusPorcelain(after))
}

// MutationDelta compares normalized status entries.
func MutationDelta(before, after []StatusEntry) MutationsDelta {
	beforeByPath := statusByPath(before)
	afterByPath := statusByPath(after)
	var changed []StatusEntry
	for _, entry := range after {
		prev, existed := beforeByPath[entry.Path]
		if existed && prev.Status == entry.Status {
			continue
		}
		changed = append(changed, entry)
	}
	for _, entry := range before {
		if _, existed := afterByPath[entry.Path]; !existed {
			changed = append(changed, StatusEntry{Path: entry.Path, Status: "clean"})
		}
	}
	sortStatusEntries(changed)
	return MutationsDelta{Changed: changed}
}

// ParseStatusPorcelain parses stable path/status pairs from git porcelain v1
// output. It accepts the preferred NUL-separated form produced by
// git status --porcelain=v1 -z and the newline form used by older callers.
// Rename/copy records use the destination path.
func ParseStatusPorcelain(output string) []StatusEntry {
	if strings.Contains(output, "\x00") {
		return parseStatusPorcelainZ(output)
	}
	var entries []StatusEntry
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" || len(line) < 3 {
			continue
		}
		status := strings.TrimSpace(line[:2])
		path := canonicalStatusPath(line[3:])
		if isRenameOrCopy(status) {
			if i := strings.LastIndex(path, " -> "); i >= 0 {
				path = strings.TrimSpace(path[i+4:])
			}
		}
		path = canonicalStatusPath(path)
		if path == "" || status == "" {
			continue
		}
		entries = append(entries, StatusEntry{Path: path, Status: status})
	}
	sortStatusEntries(entries)
	return entries
}

func parseStatusPorcelainZ(output string) []StatusEntry {
	var entries []StatusEntry
	records := strings.Split(output, "\x00")
	for i := 0; i < len(records); i++ {
		record := records[i]
		if strings.TrimSpace(record) == "" || len(record) < 3 {
			continue
		}
		status := strings.TrimSpace(record[:2])
		path := canonicalStatusPath(record[3:])
		if isRenameOrCopy(status) && i+1 < len(records) {
			i++
		}
		if path == "" || status == "" {
			continue
		}
		entries = append(entries, StatusEntry{Path: path, Status: status})
	}
	sortStatusEntries(entries)
	return entries
}

func canonicalStatusPath(path string) string {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "\"") {
		if unquoted, err := strconv.Unquote(path); err == nil {
			path = unquoted
		}
	}
	return path
}

func isRenameOrCopy(status string) bool {
	return strings.Contains(status, "R") || strings.Contains(status, "C")
}

func statusByPath(entries []StatusEntry) map[string]StatusEntry {
	byPath := make(map[string]StatusEntry, len(entries))
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	return byPath
}

func sortStatusEntries(entries []StatusEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Path == entries[j].Path {
			return entries[i].Status < entries[j].Status
		}
		return entries[i].Path < entries[j].Path
	})
}
