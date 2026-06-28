package citywriteauth

import (
	"sync"
	"time"
)

// defaultSweepThreshold bounds the in-memory jti set: once it grows past this
// many live entries, the next Use sweeps expired ones before inserting.
const defaultSweepThreshold = 4096

// MemoryReplayGuard is a process-local ReplayGuard. It is the right default for
// a single supervisor process given short grant TTLs; a restart forgets
// consumed jtis, which is acceptable because expiry + request-binding bound the
// replay window. Swap in a shared store if you need cross-process durability.
//
// sweepThreshold bounds how often expired entries are reclaimed, not the live
// set size: a jti is retained until its acceptance deadline, so steady-state
// memory is roughly mint_rate * (MaxTTL + Skew). Within the intended trust
// model (only the trusted authority mints, short TTLs) that bound is small. An
// operator who raises MaxTTL or shares the guard at a high mint rate should size
// for it or add an independent size-capped eviction policy.
type MemoryReplayGuard struct {
	mu             sync.Mutex
	seen           map[string]time.Time // jti -> exp
	sweepThreshold int
}

// NewMemoryReplayGuard returns an empty in-memory guard.
func NewMemoryReplayGuard() *MemoryReplayGuard {
	return &MemoryReplayGuard{
		seen:           make(map[string]time.Time),
		sweepThreshold: defaultSweepThreshold,
	}
}

// Use records jti as consumed until exp, returning ErrReplay if already seen.
// Presence is checked before any eviction, so replay detection never depends on
// sweep timing.
func (m *MemoryReplayGuard) Use(jti string, exp time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.seen[jti]; ok {
		return ErrReplay
	}
	if len(m.seen) >= m.sweepThreshold {
		now := time.Now()
		for k, e := range m.seen {
			if now.After(e) {
				delete(m.seen, k)
			}
		}
	}
	m.seen[jti] = exp
	return nil
}
