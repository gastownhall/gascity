package main

import (
	"context"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// confirmRuntimeReleased reports that name's runtime name is proven absent
// since the restart-requested kill: the routed leaf backend reads fresh, one
// bounded fresh read completed and found nothing under the name, and every
// composite fall-through hop is quiet for an absence. It is the gate for the
// restart-requested same-pass wake: a start against a name that is merely not
// running (a corpse, an async-teardown backend still listing it) would take
// the ErrSessionExists adoption branch and silently drop the reset, so only a
// name proven gone may be started again this pass. It is confirmStopped's
// composition with the stricter verdict: absence, not "not running".
func confirmRuntimeReleased(ctx context.Context, sp runtime.Provider, processNames []string, name string, since time.Time) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	name = strings.TrimSpace(name)
	leaf, _, known := runtime.ResolveBackend(sp, name)
	if sp == nil || name == "" || !known || leaf == nil || !runtime.FreshReadable(leaf) {
		return false
	}
	live, ok := freshRead(ctx, leaf, name, processNames, since)
	return ok && !live.Present() && hopsQuiet(ctx, sp, name, processNames, since, true)
}
