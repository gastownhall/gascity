package main

import (
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
)

// cityStatusNow is the clock the status text renderer reads ages against.
// Tests replace it so rendered ages are deterministic.
var cityStatusNow = time.Now

// renderStuckCreatingBlock prints the abandoned-create block of gc status.
func renderStuckCreatingBlock(w io.Writer, v *api.StatusStuckCreatingView, now time.Time) {
	_, _, _ = w, v, now
}

// stuckCreatingFromSessionSnapshot builds the local-fallback block. storePresent
// says whether the city has a bead store at all.
func stuckCreatingFromSessionSnapshot(storePresent bool, cfg *config.City, snap *sessionBeadSnapshot, now time.Time) *api.StatusStuckCreatingView {
	_, _, _, _ = storePresent, cfg, snap, now
	return nil
}

// stuckCreatingWire projects the view onto the wire block gc status --json
// prints.
func stuckCreatingWire(v *api.StatusStuckCreatingView) *api.StatusStuckCreating {
	_ = v
	return nil
}
