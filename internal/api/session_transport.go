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
	case "", config.SessionTransportTmux:
		return transport, nil
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
		return provider.SupportsTransport("acp")
	}
	if _, ok := sp.(acpRoutingProvider); ok {
		return true
	}
	return false
}
