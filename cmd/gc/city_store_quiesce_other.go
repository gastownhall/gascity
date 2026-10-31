//go:build !windows

package main

import "io"

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

// trackRetiringBeadStoreHandle tracks nothing outside Windows. It takes the
// retiring store as any because it never reads it, so the stub declares no
// raw-store parameter.
func trackRetiringBeadStoreHandle(*controllerState, any) func() {
	return func() {}
}

func afterControllerStateStoreSwapForTest() {}

func controllerStateStoreUpdateAllowed(*controllerState) bool { return true }
