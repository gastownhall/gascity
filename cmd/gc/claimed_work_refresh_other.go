//go:build !windows

package main

import (
	"io"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// refreshDrainAckedOrExitedClaimedWorkForDemand returns the assigned-work
// snapshot unchanged. Only Windows refreshes the claimed work of drain-acked or
// exited sessions before demand is computed.
//
// residency:allow — pass-through of the caller's own snapshot: it returns the
// bead and store-ref slices it was handed, opens no store, and consults no
// binding, namespace or leg order. Nil indexes mean no filtering; a platform
// refresh returns original indexes to retain store provenance when filtering.
func refreshDrainAckedOrExitedClaimedWorkForDemand(
	_ *config.City,
	_ runtime.Provider,
	assignedWorkBeads []beads.Bead,
	_ func(int) beads.LiveReader,
	assignedWorkStoreRefs []string,
	readyAssigned map[storeScopedBeadKey]bool,
	_ *sessionBeadSnapshot,
	_ io.Writer,
) ([]beads.Bead, []int, []string, map[storeScopedBeadKey]bool) {
	return assignedWorkBeads, nil, assignedWorkStoreRefs, readyAssigned
}
