package quota

import (
	"fmt"
	"sort"
	"time"
)

// NextAvailable returns the available account with the oldest LastUsed time
// (LRU). Skips the current account if provided. Returns an error if all
// accounts are limited or in cooldown.
func NextAvailable(state QuotaState, current string) (string, error) {
	var candidates []AccountQuota
	for _, aq := range state.Accounts {
		if aq.Status == StatusAvailable && aq.Handle != current {
			candidates = append(candidates, aq)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no available accounts (all limited or in cooldown)")
	}
	// Sort by LastUsed ascending (LRU first).
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].LastUsed.Before(candidates[j].LastUsed)
	})
	return candidates[0].Handle, nil
}

// MarkLimited transitions an account to limited status.
func MarkLimited(state *QuotaState, handle string, resetDuration time.Duration, now time.Time) {
	aq := state.GetOrCreate(handle)
	aq.Status = StatusLimited
	aq.LimitedAt = now
	if resetDuration > 0 {
		aq.ResetsAt = now.Add(resetDuration)
	}
	state.Updated = now
}

// MarkUsed updates the LastUsed timestamp and increments UseCount.
func MarkUsed(state *QuotaState, handle string, now time.Time) {
	aq := state.GetOrCreate(handle)
	aq.LastUsed = now
	aq.UseCount++
	state.Updated = now
}

// ClearAccount resets a single account to available.
func ClearAccount(state *QuotaState, handle string) {
	aq := state.Get(handle)
	if aq == nil {
		return
	}
	aq.Status = StatusAvailable
	aq.LimitedAt = time.Time{}
	aq.ResetsAt = time.Time{}
}

// ClearAll resets all accounts to available.
func ClearAll(state *QuotaState) {
	for i := range state.Accounts {
		state.Accounts[i].Status = StatusAvailable
		state.Accounts[i].LimitedAt = time.Time{}
		state.Accounts[i].ResetsAt = time.Time{}
	}
}

// ExpireStale transitions accounts whose ResetsAt has passed back to
// available. Returns the number of accounts expired.
func ExpireStale(state *QuotaState, now time.Time) int {
	expired := 0
	for i := range state.Accounts {
		aq := &state.Accounts[i]
		if aq.Status == StatusLimited && !aq.ResetsAt.IsZero() && now.After(aq.ResetsAt) {
			aq.Status = StatusAvailable
			aq.LimitedAt = time.Time{}
			aq.ResetsAt = time.Time{}
			expired++
		}
	}
	return expired
}
