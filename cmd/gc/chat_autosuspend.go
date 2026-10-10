package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// autoSuspendChatSessions scans active chat sessions and suspends any that
// have been detached (no human attached) longer than idleTimeout.
// Called on each controller reconciliation tick when [chat_sessions] idle_timeout is set,
// with the controller's city and config. Its suspends never wait for a session's
// runtime lease: a busy one is suspended on a later tick.
func autoSuspendChatSessions(cityPath string, cfg *config.City, store beads.Store, sp runtime.Provider, idleTimeout time.Duration, clk clock.Clock, stdout, stderr io.Writer) {
	if store == nil {
		return // no store — nothing to suspend
	}

	catalog, err := workerSessionCatalogWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: auto-suspend catalog: %v\n", err) //nolint:errcheck // best-effort stderr
		return
	}

	sessions, err := catalog.List("active", "")
	if err != nil {
		fmt.Fprintf(stderr, "gc start: auto-suspend list: %v\n", err) //nolint:errcheck // best-effort stderr
		return
	}

	now := clk.Now()
	for _, s := range sessions {
		// Skip sessions with attached terminals — human is active.
		if s.Attached {
			continue
		}
		// Check last activity time.
		if s.LastActive.IsZero() {
			continue // no activity data — skip
		}
		if now.Sub(s.LastActive) < idleTimeout {
			continue // not idle long enough
		}
		// s.Attached is a bool that reads "detached" when the probe fails;
		// re-probe so an attachment probe that cannot tell holds the stop.
		if attached, err := attachmentHolds(sp, s.SessionName); err != nil {
			fmt.Fprintf(stderr, "gc start: auto-suspend session %s: %v; not suspending\n", s.ID, err) //nolint:errcheck // best-effort stderr
			continue
		} else if attached {
			continue
		}

		handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, s.ID)
		if err != nil {
			fmt.Fprintf(stderr, "gc start: auto-suspend session %s: %v\n", s.ID, err) //nolint:errcheck // best-effort stderr
			continue
		}
		if err := handle.StopIdle(session.WithoutLeaseWait(context.Background())); err != nil {
			fmt.Fprintf(stderr, "gc start: auto-suspend session %s: %v\n", s.ID, err) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintf(stdout, "Session %s auto-suspended (idle %s).\n", s.ID, formatDuration(now.Sub(s.LastActive))) //nolint:errcheck // best-effort stdout
		}
	}
}
