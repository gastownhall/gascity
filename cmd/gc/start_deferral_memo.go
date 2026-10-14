package main

import (
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// startDeferralHoldCeiling bounds how long a start-gate deferral keeps a
// never-started create past its lease.
const startDeferralHoldCeiling = 60 * time.Minute

// startDeferralMemo is the RED skeleton: it compiles and does nothing, so the
// new tests fail by assertion. The GREEN commit gives it behavior.
type startDeferralMemo struct {
	mu            sync.Mutex
	committed     map[string]struct{}
	lastPassStart time.Time
}

// startDeferralPass is one executor run's view of the memo.
type startDeferralPass struct{}

func newStartDeferralMemo() *startDeferralMemo { return &startDeferralMemo{} }

func (m *startDeferralMemo) beginPass(_ time.Time) *startDeferralPass { return &startDeferralPass{} }

func (p *startDeferralPass) noteGateDeferred(_ string) {}

func (p *startDeferralPass) finish(_ bool) {}

// Holds reports whether info is a never-started create the memo keeps past its lease.
func (m *startDeferralMemo) Holds(_ sessionpkg.Info, _ time.Time) bool { return false }

func withStartDeferralMemo(_ *startDeferralMemo) startExecutionOption {
	return func(*startExecutionOptions) {}
}

// pendingCreateLeaseAnchor returns the instant the never-started lease runs from.
func pendingCreateLeaseAnchor(_ sessionpkg.Info) time.Time { return time.Time{} }

// pendingCreateHold combines the endpoint hold with the memo hold.
func pendingCreateHold(endpoint func(sessionpkg.Info) bool, _ *startDeferralMemo, _ clock.Clock) func(sessionpkg.Info) bool {
	return func(info sessionpkg.Info) bool { return endpoint != nil && endpoint(info) }
}
