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
