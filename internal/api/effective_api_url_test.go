package api

import (
	"testing"
	"time"
)

type blockingEffectiveAPIConfigClient struct {
	release <-chan struct{}
}

func (c blockingEffectiveAPIConfigClient) ListCities() ([]CityInfo, error) {
	<-c.release
	return nil, nil
}

func TestDiscoverReachableConfigSupervisorAPITimeout(t *testing.T) {
	origHook := configEffectiveAPIBaseURLHook
	origFactory := configEffectiveAPIClientFactory
	origTimeout := configEffectiveAPIProbeTimeout
	t.Cleanup(func() {
		configEffectiveAPIBaseURLHook = origHook
		configEffectiveAPIClientFactory = origFactory
		configEffectiveAPIProbeTimeout = origTimeout
	})

	blocked := make(chan struct{})
	configEffectiveAPIBaseURLHook = func() (string, error) {
		return "http://127.0.0.1:9443/", nil
	}
	configEffectiveAPIClientFactory = func(string) effectiveAPIConfigClient {
		return blockingEffectiveAPIConfigClient{release: blocked}
	}
	configEffectiveAPIProbeTimeout = 10 * time.Millisecond

	start := time.Now()
	baseURL, ok := discoverReachableConfigSupervisorAPI()
	elapsed := time.Since(start)
	close(blocked)

	if ok || baseURL != "" {
		t.Fatalf("discoverReachableConfigSupervisorAPI() = %q, %v; want no reachable API", baseURL, ok)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("discoverReachableConfigSupervisorAPI elapsed %s, want bounded by probe timeout", elapsed)
	}
}
