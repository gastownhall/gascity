//go:build !windows

package main

import (
	"io"

	"github.com/gastownhall/gascity/internal/beads"
)

// quiesceCityStoreWork does nothing outside Windows: the city stop keeps its
// upstream order.
func quiesceCityStoreWork(*managedCity, io.Writer) error { return nil }

func waitForCityStoreQuiesce(*managedCity, io.Writer) (<-chan struct{}, error) {
	done := make(chan struct{})
	close(done)
	return done, nil
}

func stopBeadsProviderAfterQuiesceDrain(*managedCity, string, <-chan struct{}) {}

// managedCityPlatformState is empty off Windows, where published cities carry
// no generation. Publishing is routed through it so managedCity's embedded
// field stays in use.
type managedCityPlatformState struct{}

func (*managedCityPlatformState) markPublished() {}

func markManagedCityPublished(_ string, mc *managedCity) {
	if mc != nil {
		mc.markPublished()
	}
}

func trackRetiringBeadStoreHandle(*controllerState, beads.Store) func() {
	return func() {}
}

func afterControllerStateStoreSwapForTest() {}

func controllerStateStoreUpdateAllowed(*controllerState) bool { return true }
