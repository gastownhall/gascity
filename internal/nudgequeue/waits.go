package nudgequeue

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// WithdrawWaitNudges removes queued wait nudges that are still pending or
// in-flight, then marks their live nudge beads as terminal wait-canceled.
func WithdrawWaitNudges(store beads.Store, cityPath string, ids []string) error {
	unique := dedupeIDs(ids)
	if len(unique) == 0 || cityPath == "" {
		return nil
	}
	removed, err := withdraw(cityPath, unique)
	if err != nil {
		return err
	}
	if len(removed) == 0 || store == nil {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range removed {
		if err := markTerminal(store, id, now); err != nil {
			return err
		}
	}
	return nil
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

func withdraw(cityPath string, ids []string) ([]string, error) {
	removed := make(map[string]bool, len(ids))
	remove := make(map[string]bool, len(ids))
	for _, id := range ids {
		remove[id] = true
	}
	if err := WithState(cityPath, func(state *State) error {
		state.Pending = filterItems(state.Pending, remove, removed)
		state.InFlight = filterItems(state.InFlight, remove, removed)
		return nil
	}); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(removed))
	for _, id := range ids {
		if removed[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

func filterItems(items []Item, remove, removed map[string]bool) []Item {
	filtered := items[:0]
	for _, item := range items {
		id := item.ID
		if remove[id] {
			removed[id] = true
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func markTerminal(store beads.Store, nudgeID, now string) error {
	if nudgeID == "" {
		return nil
	}
	items, err := store.List(beads.ListQuery{Label: "nudge:" + nudgeID})
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Status == "closed" {
			continue
		}
		if err := store.SetMetadataBatch(item.ID, map[string]string{
			"state":           "failed",
			"terminal_reason": "wait-canceled",
			"commit_boundary": "delivery-withdrawn",
			"terminal_at":     now,
		}); err != nil {
			return err
		}
		if err := store.Close(item.ID); err != nil {
			return err
		}
	}
	return nil
}
