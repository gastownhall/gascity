package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
)

var (
	effectiveAPIBaseURLHook   = supervisorAPIBaseURL
	effectiveAPIClientFactory = func(baseURL string) effectiveAPIClient {
		return api.NewClient(baseURL)
	}
	effectiveAPIProbeTimeout = 150 * time.Millisecond
)

type effectiveAPIClient interface {
	ListCities() ([]api.CityInfo, error)
}

func resolveEffectiveAPIURL(cityPath string, cfg *config.City) string {
	if cfg != nil && controllerAlive(cityPath) != 0 && cfg.API.Port > 0 {
		return standaloneAPIBaseURL(cfg)
	}
	if baseURL, ok := discoverReachableSupervisorAPIBaseURL(); ok {
		return baseURL
	}
	if cfg != nil && cfg.API.Port > 0 {
		return standaloneAPIBaseURL(cfg)
	}
	return ""
}

func discoverReachableSupervisorAPIBaseURL() (string, bool) {
	baseURL, err := effectiveAPIBaseURLHook()
	if err != nil {
		return "", false
	}
	baseURL = strings.TrimRight(baseURL, "/")
	client := effectiveAPIClientFactory(baseURL)
	if !effectiveAPIClientReachable(client, effectiveAPIProbeTimeout) {
		return "", false
	}
	return baseURL, true
}

func effectiveAPIClientReachable(client effectiveAPIClient, timeout time.Duration) bool {
	if timeout <= 0 {
		_, err := client.ListCities()
		return err == nil
	}
	done := make(chan bool, 1)
	go func() {
		_, err := client.ListCities()
		done <- err == nil
	}()
	select {
	case ok := <-done:
		return ok
	case <-time.After(timeout):
		return false
	}
}
