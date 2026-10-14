package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// rowKey is the contract's SessionKey (CONTRACT §0): the canonical store leg
// of the session row plus its bead ID. In P2 Leg is always the sessions-class
// store's census label: legacy reconciles only rows read from that store
// (loadSessionBeadSnapshot over the sessions store), and [storage] is
// boot-latched, so the label is stable for the process.
type rowKey struct{ Leg, ID string }

const (
	v2DefaultPatrol      = 30 * time.Second
	v2DefaultStopTimeout = 5 * time.Second
)

// reconcileEnv is immutable once published: a reload publishes a new one at
// Gen+1 and never edits an old one, so a reconcile that loaded it reads one
// consistent generation however long it runs.
type reconcileEnv struct {
	Gen       uint64
	Cfg       *config.City
	SP        runtime.Provider
	ConfigRev string
	// Relaunch is SP's relaunch capability (runtime.RelaunchProvider), a
	// pass fact that survives the provider's strip from the effects' World
	// (newEffectPass), where A14 decides again (relaunches).
	Relaunch bool
}

// relaunches reports the env's provider can relaunch an agent in its warm
// box: SP's capability, or the fact a stripped env carries.
func (e *reconcileEnv) relaunches() bool {
	if e == nil {
		return false
	}
	if e.SP != nil {
		_, ok := e.SP.(runtime.RelaunchProvider)
		return ok
	}
	return e.Relaunch
}

func (e *reconcileEnv) patrol() time.Duration {
	if e == nil || e.Cfg == nil {
		return v2DefaultPatrol
	}
	return e.Cfg.Daemon.PatrolIntervalDuration()
}

func (e *reconcileEnv) shutdownTimeout() time.Duration {
	if e == nil || e.Cfg == nil {
		return v2DefaultStopTimeout
	}
	return e.Cfg.Daemon.ShutdownTimeoutDuration()
}
