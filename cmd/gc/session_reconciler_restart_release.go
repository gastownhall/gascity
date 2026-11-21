package main

import (
	"context"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// confirmRuntimeReleased reports that name no longer exists on sp after a
// kill: a fresh read of name on its routed leaf since since finds it absent,
// and every fall-through hop finds nothing either (hopsQuiet). It is
// confirmStopped's composition but demands absence, not merely nothing
// running: a corpse still holds the name, and a start that finds the name
// adopts the old runtime instead of starting a fresh one. A nil ctx reads
// under context.Background; an unknown route, a backend that cannot read
// fresh, or a read that does not complete never confirms.
func confirmRuntimeReleased(ctx context.Context, sp runtime.Provider, processNames []string, name string, since time.Time) bool {
	name = strings.TrimSpace(name)
	if sp == nil || name == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	leaf, _, known := runtime.ResolveBackend(sp, name)
	if !known || leaf == nil || !runtime.FreshReadable(leaf) {
		return false
	}
	live, ok := freshRead(ctx, leaf, name, processNames, since)
	return ok && !live.Present() && hopsQuiet(ctx, sp, name, processNames, since, true)
}
