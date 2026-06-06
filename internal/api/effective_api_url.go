package api

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

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

var configEffectiveAPIProbeTimeout = 150 * time.Millisecond

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
	case "::", "[::]":
		bind = "::1"
	}
	return "http://" + net.JoinHostPort(bind, strconv.Itoa(cfg.API.Port))
}

func discoverReachableConfigSupervisorAPI() (baseURL string, ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("api: config supervisor API discovery skipped after panic: %v", recovered)
			baseURL = ""
			ok = false
		}
	}()
	baseURL, err := configEffectiveAPIBaseURLHook()
	if err != nil {
		return "", false
	}
	baseURL = strings.TrimRight(baseURL, "/")
	client := configEffectiveAPIClientFactory(baseURL)
	if !configEffectiveAPIClientReachable(client, configEffectiveAPIProbeTimeout) {
		return "", false
	}
	return baseURL, true
}

func configEffectiveAPIClientReachable(client effectiveAPIConfigClient, timeout time.Duration) bool {
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
