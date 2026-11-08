package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// runtimeReading is what a confirmation of a session's runtime answered.
type runtimeReading int

const (
	// runtimeRunning: the runtime is alive.
	runtimeRunning runtimeReading = iota
	// runtimeStopped: a fresh read answered that the runtime is gone.
	runtimeStopped
	// runtimeUnknown: neither a cached nor a fresh read could answer.
	runtimeUnknown
)

// runtimeConfirm is the latch of one reconciler pass over session rows.
type runtimeConfirm struct{}

// confirm reads whether the runtime of session name in cityPath is alive.
func (c *runtimeConfirm) confirm(_ string, sp runtime.Provider, name string) runtimeReading {
	if sp.IsRunning(name) {
		return runtimeRunning
	}
	return runtimeStopped
}

// runtimeConfirmBound bounds the fresh read of one confirmation.
var runtimeConfirmBound = 3 * time.Second
