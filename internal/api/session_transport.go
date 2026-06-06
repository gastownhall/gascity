package api

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type acpRoutingProvider interface {
	RouteACP(name string)
}

func validateSessionTransport(resolved *config.ResolvedProvider, transport string, sp runtime.Provider) (string, error) {
	transport = strings.TrimSpace(transport)
	switch transport {
	case "":
		return transport, nil
	case config.SessionTransportTmux:
		if transportSupportsTmux(sp) {
			return transport, nil
		}
		providerName := transport
		if resolved != nil && resolved.Name != "" {
			providerName = resolved.Name
		}
		return "", fmt.Errorf("provider %q requires tmux transport but the session provider cannot route tmux sessions", providerName)
	case config.SessionTransportACP:
	default:
		return "", fmt.Errorf("unknown session transport %q", transport)
	}
	providerName := ""
	if resolved != nil {
		providerName = resolved.Name
		if !resolved.SupportsACP {
			if providerName == "" {
				providerName = transport
			}
			return "", fmt.Errorf("provider %q does not support ACP transport", providerName)
		}
	}
	if transportSupportsACP(sp) {
		return transport, nil
	}
	if providerName == "" {
		providerName = transport
	}
	return "", fmt.Errorf("provider %q requires ACP transport but the session provider cannot route ACP sessions", providerName)
}

func providerSessionTransport(resolved *config.ResolvedProvider, sp runtime.Provider) (string, error) {
	if resolved == nil {
		return "", nil
	}
	return validateSessionTransport(resolved, resolved.ProviderSessionCreateTransport(), sp)
}

func transportSupportsACP(sp runtime.Provider) bool {
	if sp == nil {
		return false
	}
	if provider, ok := sp.(runtime.TransportCapabilityProvider); ok {
		return provider.SupportsTransport(config.SessionTransportACP)
	}
	if _, ok := sp.(acpRoutingProvider); ok {
		return true
	}
	return false
}

func transportSupportsTmux(sp runtime.Provider) bool {
	if provider, ok := sp.(runtime.TransportCapabilityProvider); ok {
		return provider.SupportsTransport(config.SessionTransportTmux)
	}
	return true
}
