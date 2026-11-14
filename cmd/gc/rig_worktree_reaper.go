package main

import (
	"io"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// reapDetachedRigWorktrees is a RED-step stub: it reaps nothing and reports
// nothing. The GREEN step replaces it with the detached-HEAD rig-root worktree
// reaper.
func reapDetachedRigWorktrees(
	_ string,
	_ *config.City,
	_ map[string]bool,
	_ []string,
	dryRun bool,
	_ events.Recorder,
	_ *reapSkipTracker,
	_ io.Writer,
) reapReport {
	return reapReport{DryRun: dryRun}
}
