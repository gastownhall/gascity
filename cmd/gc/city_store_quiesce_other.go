//go:build !windows

package main

import "io"

// quiesceCityStoreWork does nothing outside Windows: the city stop keeps its
// upstream order.
func quiesceCityStoreWork(*managedCity, io.Writer) error { return nil }

func waitForCityStoreQuiesce(*managedCity, io.Writer) (<-chan struct{}, error) { //nolint:unparam // the Windows quiescence wait can fail
	done := make(chan struct{})
	close(done)
	return done, nil
}

func stopBeadsProviderAfterQuiesceDrain(*managedCity, string, <-chan struct{}) {}

// managedCityPlatformState is empty off Windows, where published cities carry
// no generation.
type managedCityPlatformState struct{}

// markPublished records nothing off Windows.
func (*managedCityPlatformState) markPublished(string) {}

func trackRetiringBeadStoreHandle(*controllerState, interface{ StopReconciler() }) func() {
	return func() {}
}

func afterControllerStateStoreSwapForTest() {}

func controllerStateStoreUpdateAllowed(*controllerState) bool { return true }
