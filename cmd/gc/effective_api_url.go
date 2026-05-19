package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
)

var effectiveAPIBaseURLHook = supervisorAPIBaseURL
var effectiveAPIClientFactory = func(baseURL string) effectiveAPIClient {
	return api.NewClient(baseURL)
}

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
	if _, err := client.ListCities(); err != nil {
		return "", false
	}
	return baseURL, true
}

func standaloneAPIBaseURL(cfg *config.City) string {
	bind := cfg.API.BindOrDefault()
	switch bind {
	case "", "0.0.0.0":
		bind = "127.0.0.1"
	case "::":
		bind = "::1"
	}
	return "http://" + net.JoinHostPort(bind, strconv.Itoa(cfg.API.Port))
}

func effectiveAPIStatusLine(cityPath string, cfg *config.City) string {
	if url := resolveEffectiveAPIURL(cityPath, cfg); url != "" {
		return fmt.Sprintf("  API:        %s\n", url)
	}
	return ""
}
