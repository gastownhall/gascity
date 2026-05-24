package beads

import (
	"time"
)

// PurgeExpired removes expired ephemeral beads and closed main-tier beads whose
// retention window has elapsed. It returns the number of beads removed.
func (s *HQStore) PurgeExpired() (int, error) {
	s.counters.PurgeExpired.Add(1)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return 0, err
	}

	var ids []string
	for id, bead := range s.wisps {
		expiresAt, ok := hqBeadExpiresAt(bead)
		if ok && !expiresAt.After(now) {
			ids = append(ids, id)
		}
	}
	if s.closedTaskRetention > 0 {
		for id, bead := range s.main {
			if hqClosedTaskExpired(bead, now, s.closedTaskRetention) && !s.hasOpenChildrenLocked(id) {
				ids = append(ids, id)
			}
		}
	}
	for _, id := range ids {
		s.deleteLocked(id)
	}
	if len(ids) > 0 {
		s.counters.PurgeExpiredN.Add(int64(len(ids)))
	}
	return len(ids), nil
}

func hqBeadExpiresAt(b Bead) (time.Time, bool) {
	if len(b.Metadata) == 0 {
		return time.Time{}, false
	}
	raw := b.Metadata[hqExpiresAtMetadataKey]
	if raw == "" {
		raw = b.Metadata[hqExpiresAtMetadataAlt]
	}
	if raw == "" {
		return time.Time{}, false
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	return expiresAt, true
}

func hqClosedTaskExpired(b Bead, now time.Time, retention time.Duration) bool {
	if b.Status != "closed" {
		return false
	}
	ref := b.CreatedAt
	if len(b.Metadata) > 0 && b.Metadata[hqClosedAtMetadataKey] != "" {
		if closedAt, err := time.Parse(time.RFC3339Nano, b.Metadata[hqClosedAtMetadataKey]); err == nil {
			ref = closedAt
		}
	}
	if ref.IsZero() {
		return false
	}
	return !ref.Add(retention).After(now)
}

func (s *HQStore) hasOpenChildrenLocked(parentID string) bool {
	for _, child := range s.main {
		if child.ParentID == parentID && child.Status != "closed" && child.Status != "archived" {
			return true
		}
	}
	for _, child := range s.wisps {
		if child.ParentID == parentID && child.Status != "closed" && child.Status != "archived" {
			return true
		}
	}
	return false
}

func (s *HQStore) startTTLSweeper() {
	if s.ttlInterval <= 0 {
		return
	}
	s.ttlStop = make(chan struct{})
	s.ttlDone = make(chan struct{})
	go func() {
		defer close(s.ttlDone)
		ticker := time.NewTicker(s.ttlInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_, _ = s.PurgeExpired()
			case <-s.ttlStop:
				return
			}
		}
	}()
}

func (s *HQStore) stopTTLSweeper() {
	if s.ttlStop == nil {
		return
	}
	close(s.ttlStop)
	<-s.ttlDone
	s.ttlStop = nil
	s.ttlDone = nil
}
