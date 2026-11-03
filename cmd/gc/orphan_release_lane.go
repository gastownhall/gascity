package main

// The orphaned-pool-assignment release lane (ga-8uf72n, ruled in ga-57er0d).
//
// RED-phase skeleton: the types and signatures the lane's tests are written
// against, with inert bodies. The GREEN commit replaces every body and this
// paragraph with the lane's design.

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	// orphanReleaseBackstopInterval is how often the authoritative release pass
	// runs when nothing forces it sooner.
	orphanReleaseBackstopInterval = 5 * time.Minute

	// orphanReleaseBackstopRetryInterval is the cadence after a pass that could
	// not read every leg.
	orphanReleaseBackstopRetryInterval = 2 * time.Minute
)

// orphanReleaseInputs is the bundle of tick-built inputs one release pass reads.
type orphanReleaseInputs struct {
	store        beads.WorkStore
	sessionStore beads.SessionStore
	cfg          *config.City
	cityPath     string
	openInfos    []session.Info
	result       DesiredStateResult
	protected    map[storeScopedBeadKey]struct{}
}

// orphanReleaseReport is one pass's outcome in the terms the tick trace needs.
type orphanReleaseReport struct {
	lane       string
	reason     string
	skipped    string
	candidates int
	released   []releasedPoolAssignment
}

// fields renders the report for the reconciler trace.
func (r orphanReleaseReport) fields() map[string]any { return map[string]any{} }

// orphanReleaseLane holds the cadence and candidate state the two passes share.
type orphanReleaseLane struct {
	interval time.Duration
	retry    time.Duration
	poll     time.Duration
}

func newOrphanReleaseLane() *orphanReleaseLane {
	return &orphanReleaseLane{
		interval: orphanReleaseBackstopInterval,
		retry:    orphanReleaseBackstopRetryInterval,
		poll:     clock.Backstop(backstopPollInterval),
	}
}

// orphanReleaseLaneOf returns this runtime's lane, creating it on first use.
func (cr *CityRuntime) orphanReleaseLaneOf() *orphanReleaseLane { return newOrphanReleaseLane() }

func (l *orphanReleaseLane) observeSessions(_ []session.Info, _ *config.City) {}
func (l *orphanReleaseLane) queueAssignees(_ ...string)                       {}
func (l *orphanReleaseLane) takePending() []string                            { return nil }
func (l *orphanReleaseLane) publish(_ orphanReleaseInputs)                    {}
func (l *orphanReleaseLane) force(_ string)                                   {}
func (l *orphanReleaseLane) backstopDue(_ time.Time) (string, bool)           { return "", false }
func (l *orphanReleaseLane) noteBackstopRan(_ time.Time, _ string, _ bool)    {}
func (l *orphanReleaseLane) lastBackstop() (time.Time, string, bool)          { return time.Time{}, "", false }

func (l *orphanReleaseLane) deltaPass(_ orphanReleaseInputs, _ *deadAssigneeKeepLog) orphanReleaseReport {
	return orphanReleaseReport{lane: "delta"}
}

func (l *orphanReleaseLane) backstopPass(reason string, _ *deadAssigneeKeepLog) orphanReleaseReport {
	return orphanReleaseReport{lane: "backstop", reason: reason}
}

func (cr *CityRuntime) runOrphanReleaseBackstop(reason string) orphanReleaseReport {
	return orphanReleaseReport{lane: "backstop", reason: reason}
}

func (cr *CityRuntime) runOrphanReleaseBackstopLoop(_ context.Context, _ *orphanReleaseLane) {}
