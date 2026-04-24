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
)

// autoSuspendChatSessions scans active chat sessions and suspends any that
// have been detached (no human attached) longer than idleTimeout.
// Called on each controller reconciliation tick when [chat_sessions] idle_timeout is set.
func autoSuspendChatSessions(store beads.Store, sp runtime.Provider, idleTimeout time.Duration, clk clock.Clock, stdout, stderr io.Writer) {
	if store == nil {
		return // no store — nothing to suspend
	}

	cityPath, _ := resolveCity()
	var cfg *config.City
	if cityPath != "" {
		cfg, _ = loadCityConfig(cityPath, stderr)
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

		handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, s.ID)
		if err != nil {
			fmt.Fprintf(stderr, "gc start: auto-suspend session %s: %v\n", s.ID, err) //nolint:errcheck // best-effort stderr
			continue
		}
		if err := handle.Stop(context.TODO()); err != nil {
			fmt.Fprintf(stderr, "gc start: auto-suspend session %s: %v\n", s.ID, err) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintf(stdout, "Session %s auto-suspended (idle %s).\n", s.ID, formatDuration(now.Sub(s.LastActive))) //nolint:errcheck // best-effort stdout
		}
	}
}
