package api

import (
	"fmt"
	"time"
)

const statusMailCountsCacheTTL = 30 * time.Second

func (s *Server) cachedStatusMailCounts(now time.Time) mailCounts {
	s.statusMailCountsMu.Lock()
	if s.statusMailCountsSet {
		entry := s.statusMailCountsEntry
		if now.Before(s.statusMailCountsExpires) {
			s.statusMailCountsMu.Unlock()
			return entry
		}
		if !s.statusMailCountsRefreshing {
			compute := s.statusMailCountsComputeFunc()
			s.statusMailCountsRefreshing = true
			go s.refreshStatusMailCounts(compute)
		}
		s.statusMailCountsMu.Unlock()
		return entry
	}
	compute := s.statusMailCountsComputeFunc()
	s.statusMailCountsMu.Unlock()

	counts := compute()

	s.statusMailCountsMu.Lock()
	defer s.statusMailCountsMu.Unlock()
	if s.statusMailCountsSet && now.Before(s.statusMailCountsExpires) {
		return s.statusMailCountsEntry
	}
	s.statusMailCountsEntry = counts
	s.statusMailCountsSet = true
	s.statusMailCountsExpires = now.Add(statusMailCountsCacheTTL)
	return counts
}

func (s *Server) statusMailCountsComputeFunc() func() StatusMailCounts {
	if s.statusMailCountsComputer != nil {
		return s.statusMailCountsComputer
	}
	return s.computeStatusMailCounts
}

func (s *Server) refreshStatusMailCounts(compute func() StatusMailCounts) {
	defer func() {
		if recover() != nil {
			s.statusMailCountsMu.Lock()
			s.statusMailCountsRefreshing = false
			s.statusMailCountsMu.Unlock()
		}
	}()
	counts := compute()

	s.statusMailCountsMu.Lock()
	s.statusMailCountsEntry = counts
	s.statusMailCountsSet = true
	s.statusMailCountsExpires = time.Now().Add(statusMailCountsCacheTTL)
	s.statusMailCountsRefreshing = false
	s.statusMailCountsMu.Unlock()
}

func (s *Server) computeStatusMailCounts() StatusMailCounts {
	var counts StatusMailCounts
	seenProvs := make(map[string]bool)
	for _, mp := range s.state.MailProviders() {
		key := fmt.Sprintf("%p", mp)
		if seenProvs[key] {
			continue
		}
		seenProvs[key] = true
		if total, unread, err := mp.Count(""); err == nil {
			counts.Total += total
			counts.Unread += unread
		}
	}
	return counts
}
