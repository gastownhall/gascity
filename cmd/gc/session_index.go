// session_index.go provides an in-memory index of open session beads.
// The index avoids per-tick store queries for session lookup and occupancy
// counting. Pattern matches convergenceStoreAdapter.activeIndex.
package main

import (
	"fmt"
	"io"
	"sync"

	"github.com/gastownhall/gascity/internal/session"
)

// sessionEntry holds indexed metadata for a single session bead.
type sessionEntry struct {
	template      string
	state         string
	sleepReason   string
	sessionName   string
	generation    string
	instanceToken string
	labels        []string
}

// sessionIndex is a thread-safe in-memory index of open session beads.
// Populated once at startup (populateIndex), then kept current via
// update/remove after each mutation.
type sessionIndex struct {
	mu      sync.RWMutex
	entries map[string]*sessionEntry // bead ID → entry
}

// newSessionIndex creates an empty session index.
func newSessionIndex() *sessionIndex {
	return &sessionIndex{entries: make(map[string]*sessionEntry)}
}

// populateIndex performs a one-time scan of session beads from the store
// and builds the in-memory index. Only open beads are indexed (closed and
// archived beads are skipped to keep the index small).
func (idx *sessionIndex) populateIndex(sessFront *session.Store, stderr io.Writer) {
	if !sessFront.Backed() {
		return
	}

	loaded, err := sessFront.ListAll(session.ListAllOptions{})
	if err != nil {
		fmt.Fprintf(stderr, "session index: populate: %v\n", err) //nolint:errcheck
		return
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.entries = make(map[string]*sessionEntry, len(loaded))
	for _, info := range loaded {
		state := info.MetadataState
		// Skip archived/closed — they don't affect reconciliation.
		// Check both metadata state (includes legacy "stopped" mapped to
		// "closed") and bead-level status.
		if state == "archived" || state == "closed" || info.Closed {
			continue
		}
		idx.entries[info.ID] = entryFromInfo(info)
	}
}

// snapshot returns a copy of all entries. The caller owns the returned map.
func (idx *sessionIndex) snapshot() map[string]*sessionEntry {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	cp := make(map[string]*sessionEntry, len(idx.entries))
	for k, v := range idx.entries {
		cp[k] = v
	}
	return cp
}

// byTemplate returns all entries for the given template name.
func (idx *sessionIndex) byTemplate(template string) []*sessionEntry {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var result []*sessionEntry
	for _, e := range idx.entries {
		if e.template == template {
			result = append(result, e)
		}
	}
	return result
}

// occupancy returns the count of sessions for a template that count against
// pool occupancy: creating + active + asleep + suspended + quarantined.
// Drained sessions do NOT count; they are only revived through explicit
// targeting rather than generic pool demand.
func (idx *sessionIndex) occupancy(template string) int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	count := 0
	for _, e := range idx.entries {
		if e.template != template {
			continue
		}
		if isDrainedSessionMetadata(map[string]string{
			"state":        e.state,
			"sleep_reason": e.sleepReason,
		}) {
			continue
		}
		switch e.state {
		case "start-pending", "creating", "active", "awake", "asleep", "suspended", "quarantined":
			count++
		}
	}
	return count
}

// update adds or replaces an entry in the index.
func (idx *sessionIndex) update(beadID string, entry *sessionEntry) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.entries[beadID] = entry
}

// remove deletes an entry from the index.
func (idx *sessionIndex) remove(beadID string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	delete(idx.entries, beadID)
}

// get returns the entry for a bead ID, or nil if not indexed.
func (idx *sessionIndex) get(beadID string) *sessionEntry {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.entries[beadID]
}

// entryFromInfo constructs a sessionEntry from a session.Info projection. Each
// field is a verbatim codec mirror of the raw bead metadata the index formerly
// cracked (MetadataState/SessionNameMetadata are the raw, un-normalized forms —
// NOT the closed-blanked State or the sessionNameFor-fallback SessionName).
func entryFromInfo(info session.Info) *sessionEntry {
	return &sessionEntry{
		template:      info.Template,
		state:         info.MetadataState,
		sleepReason:   info.SleepReason,
		sessionName:   info.SessionNameMetadata,
		generation:    info.Generation,
		instanceToken: info.InstanceToken,
		labels:        info.Labels,
	}
}
