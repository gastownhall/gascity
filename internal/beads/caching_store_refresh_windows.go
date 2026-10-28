//go:build windows

package beads

import (
	"fmt"
	"strings"
	"time"
)

// RefreshIDs refreshes the named rows from the backing store and absorbs them
// into the cache through the same close-reporting path as dirty cached reads.
func (c *CachingStore) RefreshIDs(ids []string) ([]Bead, error) {
	if c == nil || c.backing == nil {
		return nil, fmt.Errorf("refreshing cache rows: backing store unavailable")
	}
	ids = compactRefreshIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}

	startSeq, finishRead := c.beginSnapshotReadSeq()
	defer finishRead()

	suppressed := make(map[string]struct{})
	fetched, err := c.fetchDirtyOverlay(ids, suppressed)
	if err != nil {
		return nil, err
	}

	rows := make([]Bead, 0, len(fetched))
	notifications := make([]cacheNotification, 0)
	changed := false

	c.mu.Lock()
	if c.state != cacheLive && c.state != cachePartial {
		c.mu.Unlock()
		for _, f := range fetched {
			rows = append(rows, cloneBead(f.bead))
		}
		return rows, nil
	}

	now := time.Now()
	for _, f := range fetched {
		if c.deletedSeq[f.id] > startSeq {
			continue
		}
		if c.beadSeq[f.id] > startSeq {
			if current, ok := c.beads[f.id]; ok {
				rows = append(rows, cloneBead(current))
			}
			continue
		}
		if c.readCloseFenceBlocksFreshLocked(f.id, f.bead, startSeq) {
			if current, ok := c.beads[f.id]; ok {
				rows = append(rows, cloneBead(current))
			}
			continue
		}
		opts := absorbOpts{
			depsMode:   depsFromFields,
			seqMode:    seqClearBeadSeqOnly,
			clearDirty: true,
		}
		if f.depsFromBacking {
			opts.depsMode = depsExplicit
			opts.deps = f.deps
		}
		notification, notify, observedClose := c.closedAbsorbNotificationLocked(f.id, f.bead)
		if notify {
			notifications = append(notifications, notification)
		}
		c.absorbFreshLocked(f.id, f.bead, now, opts)
		if observedClose {
			c.fenceReportedCloseLocked(f.id)
		}
		rows = append(rows, cloneBead(f.bead))
		changed = true
	}
	for id := range suppressed {
		if c.deletedSeq[id] > startSeq || c.beadSeq[id] > startSeq {
			continue
		}
		if current, ok := c.beads[id]; ok && current.Status != "closed" && recentLocalMutation(c.localBeadAt[id], now) {
			continue
		}
		notification, notify, observedClose := c.closedEvictNotificationLocked(id)
		if notify {
			notifications = append(notifications, notification)
		}
		c.evictReadMissingLocked(id)
		if observedClose {
			c.fenceReportedCloseLocked(id)
		}
		changed = true
	}
	if changed {
		c.markFreshLocked(time.Now())
		c.updateStatsLocked()
	}
	c.mu.Unlock()
	c.notifyChanges(notifications)
	return rows, nil
}

func compactRefreshIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}
