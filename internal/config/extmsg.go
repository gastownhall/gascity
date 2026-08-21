package config

import "strings"

// ExtMsgConfig configures the external-messaging fabric.
type ExtMsgConfig struct {
	// DefaultRoutes map inbound conversations that have no binding and no
	// group route to a configured agent, keyed by provider and optionally
	// narrowed to one adapter account. The first matching inbound message
	// binds the conversation to the agent (an agent-name binding), so the
	// route is sticky until rebound or unbound.
	DefaultRoutes []ExtMsgDefaultRoute `toml:"default_route,omitempty"`
	// ConnectedClients configures the connected-client SSE token and subscription subsystem.
	ConnectedClients ConnectedClientsConfig `toml:"connected_clients,omitempty"`
}

// ConnectedClientsConfig configures the connected-client SSE subscribe path.
type ConnectedClientsConfig struct {
	// AllowNoCredential permits client registration without a credential when
	// true. Defaults to false (credential required).
	AllowNoCredential bool `toml:"allow_no_credential,omitempty"`
	// HeartbeatInterval is the SSE keepalive interval for connected-client streams.
	// Duration string (e.g., "30s"). Defaults to "30s".
	HeartbeatInterval string `toml:"heartbeat_interval,omitempty"`
	// SubscriberBufferSize is the channel buffer depth per SSE subscriber.
	// Defaults to 64.
	SubscriberBufferSize int `toml:"subscriber_buffer_size,omitempty"`
}

// HeartbeatIntervalOrDefault returns the configured heartbeat interval, or
// 30s when unset.
func (c ConnectedClientsConfig) HeartbeatIntervalOrDefault() string {
	if c.HeartbeatInterval == "" {
		return "30s"
	}
	return c.HeartbeatInterval
}

// SubscriberBufferSizeOrDefault returns the configured buffer size, or 64
// when unset.
func (c ConnectedClientsConfig) SubscriberBufferSizeOrDefault() int {
	if c.SubscriberBufferSize <= 0 {
		return 64
	}
	return c.SubscriberBufferSize
}

// ExtMsgDefaultRoute routes unbound inbound conversations from one external
// messaging provider (and optionally a single adapter account) to a
// configured agent. The agent decides what to do with the conversation;
// the route is pure transport.
type ExtMsgDefaultRoute struct {
	// Provider is the external messaging provider name as registered by
	// the adapter (e.g. "telegram"). Required.
	Provider string `toml:"provider"`
	// AccountID narrows the route to one adapter account. Empty matches
	// every account of the provider that has no account-specific route.
	AccountID string `toml:"account_id,omitempty"`
	// Agent is the configured agent identity to route to. It must resolve
	// to a configured named session so the delivery layer can cold-wake a
	// session for it.
	Agent string `toml:"agent"`
}

// ExtMsgDefaultRouteAgent returns the configured default agent for inbound
// conversations of (provider, accountID), or "" when no route matches. An
// account-specific route takes precedence over the provider-wide route
// (empty account_id); account-specific routes never match other accounts.
//
// Provider names are matched case-insensitively (lowercased on both the
// incoming and configured side) to mirror extmsg ConversationRef
// canonicalization, so a normalized inbound posted as "Discord" still matches
// a route configured as provider = "discord". Account IDs are matched
// case-sensitively, also matching ConversationRef normalization, which trims
// but does not lowercase the account ID.
func (c *City) ExtMsgDefaultRouteAgent(provider, accountID string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	accountID = strings.TrimSpace(accountID)
	if provider == "" {
		return ""
	}
	providerWide := ""
	for i := range c.ExtMsg.DefaultRoutes {
		route := &c.ExtMsg.DefaultRoutes[i]
		if strings.ToLower(strings.TrimSpace(route.Provider)) != provider {
			continue
		}
		switch strings.TrimSpace(route.AccountID) {
		case accountID:
			return strings.TrimSpace(route.Agent)
		case "":
			providerWide = strings.TrimSpace(route.Agent)
		}
	}
	return providerWide
}
