//go:build !windows

package main

import (
	"io"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func refreshDrainAckedOrExitedClaimedWorkForDemand(
	_ *config.City,
	_ runtime.Provider,
	assignedWorkBeads []beads.Bead,
	assignedWorkStores []beads.Store,
	assignedWorkStoreRefs []string,
	readyAssigned map[storeScopedBeadKey]bool,
	_ *sessionBeadSnapshot,
	_ io.Writer,
) ([]beads.Bead, []beads.Store, []string, map[storeScopedBeadKey]bool) {
	return assignedWorkBeads, assignedWorkStores, assignedWorkStoreRefs, readyAssigned
}
