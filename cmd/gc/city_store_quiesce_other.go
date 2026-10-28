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

func markManagedCityPublished(string, *managedCity) {}

func trackRetiringBeadStoreHandle(*controllerState, beads.Store) func() {
	return func() {}
}

func afterControllerStateStoreSwapForTest() {}

func controllerStateStoreUpdateAllowed(*controllerState) bool { return true }
