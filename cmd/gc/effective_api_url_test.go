package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
)

type blockingEffectiveAPIClient struct {
	release <-chan struct{}
}

func (c blockingEffectiveAPIClient) ListCities() ([]api.CityInfo, error) {
	<-c.release
	return nil, nil
}

func TestStandaloneAPIBaseURLNormalizesBracketWildcardIPv6(t *testing.T) {
	got := standaloneAPIBaseURL(&config.City{
		API: config.APIConfig{
			Bind: "[::]",
			Port: 4567,
		},
	})
	if want := "http://[::1]:4567"; got != want {
		t.Fatalf("standaloneAPIBaseURL = %q, want %q", got, want)
	}
}

func TestDiscoverReachableSupervisorAPIBaseURLTimeout(t *testing.T) {
	origHook := effectiveAPIBaseURLHook
	origFactory := effectiveAPIClientFactory
	origTimeout := effectiveAPIProbeTimeout
	t.Cleanup(func() {
		effectiveAPIBaseURLHook = origHook
		effectiveAPIClientFactory = origFactory
		effectiveAPIProbeTimeout = origTimeout
	})

	blocked := make(chan struct{})
	effectiveAPIBaseURLHook = func() (string, error) {
		return "http://127.0.0.1:9443/", nil
	}
	effectiveAPIClientFactory = func(string) effectiveAPIClient {
		return blockingEffectiveAPIClient{release: blocked}
	}
	effectiveAPIProbeTimeout = 10 * time.Millisecond

	start := time.Now()
	baseURL, ok := discoverReachableSupervisorAPIBaseURL()
	elapsed := time.Since(start)
	close(blocked)

	if ok || baseURL != "" {
		t.Fatalf("discoverReachableSupervisorAPIBaseURL() = %q, %v; want no reachable API", baseURL, ok)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("discoverReachableSupervisorAPIBaseURL elapsed %s, want bounded by probe timeout", elapsed)
	}
}
