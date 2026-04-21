package api

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/supervisor"
)

var configEffectiveAPIBaseURLHook = func() (string, error) {
	cfg, err := supervisor.LoadConfig(supervisor.ConfigPath())
	if err != nil {
		return "", err
	}
	bind := cfg.Supervisor.BindOrDefault()
	switch bind {
	case "0.0.0.0":
		bind = "127.0.0.1"
	case "::", "[::]":
		bind = "::1"
	}
	return fmt.Sprintf("http://%s", net.JoinHostPort(bind, strconv.Itoa(cfg.Supervisor.PortOrDefault()))), nil
}

var configEffectiveAPIClientFactory = func(baseURL string) effectiveAPIConfigClient {
	return NewClient(baseURL)
}

type effectiveAPIConfigClient interface {
	ListCities() ([]CityInfo, error)
}

func configEffectiveAPIURL(state State) string {
	if state == nil {
		return ""
	}
	if baseURL, ok := discoverReachableConfigSupervisorAPI(); ok {
		return baseURL
	}
	cfg := state.Config()
	if cfg == nil || cfg.API.Port <= 0 {
		return ""
	}
	bind := cfg.API.BindOrDefault()
	switch bind {
	case "", "0.0.0.0":
		bind = "127.0.0.1"
	case "::":
		bind = "::1"
	}
	return "http://" + net.JoinHostPort(bind, strconv.Itoa(cfg.API.Port))
}

func discoverReachableConfigSupervisorAPI() (string, bool) {
	defer func() {
		if recover() != nil {
			// Test environments may intentionally omit GC_HOME/real supervisor setup.
		}
	}()
	baseURL, err := configEffectiveAPIBaseURLHook()
	if err != nil {
		return "", false
	}
	baseURL = strings.TrimRight(baseURL, "/")
	client := configEffectiveAPIClientFactory(baseURL)
	if _, err := client.ListCities(); err != nil {
		return "", false
	}
	return baseURL, true
}
