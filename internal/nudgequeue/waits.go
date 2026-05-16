package nudgequeue

import (
	"errors"
	"log"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// NudgeLookupLimit bounds recovery lookups by the durable nudge ID label.
const NudgeLookupLimit = 20

// WithdrawWaitNudges removes queued wait nudges that are still pending or
// in-flight, then marks their snapshotted nudge beads as terminal wait-canceled.
func WithdrawWaitNudges(store beads.Store, cityPath string, ids []string) error {
	unique := dedupeIDs(ids)
	if len(unique) == 0 || cityPath == "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return withdraw(cityPath, unique, store, now)
}

func dedupeIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func withdraw(cityPath string, ids []string, store beads.Store, now string) error {
	remove := make(map[string]bool, len(ids))
	for _, id := range ids {
		remove[id] = true
	}

	if store == nil {
		return removeWaitNudgeIDs(cityPath, remove)
	}

	candidates := map[string][]withdrawCandidate{}
	if err := WithState(cityPath, func(state *State) error {
		candidates = queuedWaitNudgeCandidates(state, remove)
		return nil
	}); err != nil {
		return err
	}
	toRemove := make(map[withdrawCandidate]bool, len(ids))
	var firstErr error
	for _, id := range ids {
		if len(candidates[id]) == 0 {
			continue
		}
		if err := markTerminalCandidates(store, id, candidates[id], now); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, candidate := range candidates[id] {
			toRemove[candidate] = true
		}
	}
	if len(toRemove) == 0 {
		return firstErr
	}
	if err := WithState(cityPath, func(state *State) error {
		state.Pending = filterCandidateItems(state.Pending, toRemove)
		state.InFlight = filterCandidateItems(state.InFlight, toRemove)
		return nil
	}); err != nil {
		if firstErr != nil {
			return errors.Join(firstErr, err)
		}
		return err
	}
	return firstErr
}

func removeWaitNudgeIDs(cityPath string, remove map[string]bool) error {
	if len(remove) == 0 {
		return nil
	}
	return WithState(cityPath, func(state *State) error {
		state.Pending = filterItemsByID(state.Pending, remove)
		state.InFlight = filterItemsByID(state.InFlight, remove)
		return nil
	})
}

func filterItemsByID(items []Item, remove map[string]bool) []Item {
	filtered := items[:0]
	for _, item := range items {
		if remove[item.ID] {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

type withdrawCandidate struct {
	ID        string
	BeadID    string
	CreatedAt time.Time
}

func candidateForItem(item Item) withdrawCandidate {
	return withdrawCandidate{
		ID:        item.ID,
		BeadID:    item.BeadID,
		CreatedAt: item.CreatedAt.UTC(),
	}
}

func filterCandidateItems(items []Item, remove map[withdrawCandidate]bool) []Item {
	filtered := items[:0]
	for _, item := range items {
		if remove[candidateForItem(item)] {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func queuedWaitNudgeCandidates(state *State, want map[string]bool) map[string][]withdrawCandidate {
	found := make(map[string][]withdrawCandidate, len(want))
	for _, item := range state.Pending {
		if want[item.ID] {
			found[item.ID] = append(found[item.ID], candidateForItem(item))
		}
	}
	for _, item := range state.InFlight {
		if want[item.ID] {
			found[item.ID] = append(found[item.ID], candidateForItem(item))
		}
	}
	return found
}

func terminalNudgeBeads(store beads.Store, nudgeID string) ([]beads.Bead, error) {
	if nudgeID == "" {
		return nil, nil
	}
	items, err := store.List(beads.ListQuery{
		Label: "nudge:" + nudgeID,
		Limit: NudgeLookupLimit + 1,
		Sort:  beads.SortCreatedDesc,
	})
	if err != nil {
		return nil, err
	}
	if len(items) > NudgeLookupLimit {
		log.Printf("nudgequeue: nudge %q lookup capped at %d; terminalizing visible candidates", nudgeID, NudgeLookupLimit)
	}
	return items, nil
}

func markTerminalCandidates(store beads.Store, nudgeID string, candidates []withdrawCandidate, now string) error {
	legacyLookup := false
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		if candidate.BeadID == "" {
			legacyLookup = true
			continue
		}
		if seen[candidate.BeadID] {
			continue
		}
		seen[candidate.BeadID] = true
		if err := markTerminalBeadByID(store, candidate.BeadID, now); err != nil {
			return err
		}
	}
	if legacyLookup {
		return markTerminal(store, nudgeID, now)
	}
	return nil
}

func markTerminal(store beads.Store, nudgeID, now string) error {
	items, err := terminalNudgeBeads(store, nudgeID)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	if len(items) > 1 {
		log.Printf("nudgequeue: nudge %q has %d open beads", nudgeID, len(items))
	}
	for _, item := range items {
		if item.Status == "closed" {
			continue
		}
		if err := markTerminalBead(store, item, now); err != nil {
			return err
		}
	}
	return nil
}

func markTerminalBeadByID(store beads.Store, beadID, now string) error {
	if beadID == "" {
		return nil
	}
	item, err := store.Get(beadID)
	if errors.Is(err, beads.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if item.Status == "closed" {
		return nil
	}
	return markTerminalBead(store, item, now)
}

func markTerminalBead(store beads.Store, item beads.Bead, now string) error {
	if err := store.SetMetadataBatch(item.ID, map[string]string{
		"state":           "failed",
		"terminal_reason": "wait-canceled",
		"commit_boundary": "delivery-withdrawn",
		"terminal_at":     now,
	}); err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		return err
	}
	if err := store.Close(item.ID); err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}
