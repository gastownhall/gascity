package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

type sessionTargetHandleResolver func(string, beads.Store, runtime.Provider, *config.City, string) (worker.Handle, error)

var liveSubagentsForKill = func(ctx context.Context, handle worker.Handle) ([]worker.InFlightSubagent, error) {
	return worker.InFlightBackgroundSubagents(ctx, handle)
}

// refuseKillForLiveSubagents prints the exact work a kill would destroy. Any
// transcript lookup failure is intentionally fail-open: an operator must still
// be able to kill a damaged or history-less session.
func refuseKillForLiveSubagents(command string, resolve sessionTargetHandleResolver, cityPath string, store beads.Store, provider runtime.Provider, cfg *config.City, target string, out io.Writer) bool {
	handle, err := resolve(cityPath, store, provider, cfg, target)
	if err != nil {
		return false
	}
	live, err := liveSubagentsForKill(context.Background(), handle)
	if err != nil || len(live) == 0 {
		return false
	}
	fmt.Fprintf(out, "%s: refusing to destroy %d live background subagent(s); retry with --force:\n", command, len(live)) //nolint:errcheck
	for _, subagent := range live {
		description := strings.TrimSpace(subagent.Description)
		if description == "" {
			description = "(no description)"
		}
		age := "unknown duration"
		if !subagent.StartedAt.IsZero() {
			age = time.Since(subagent.StartedAt).Round(time.Second).String()
		}
		fmt.Fprintf(out, "  - %s (%s, running %s)\n", description, subagent.AgentID, age) //nolint:errcheck
	}
	return true
}
