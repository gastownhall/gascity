package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// recordedSessionTransport reports the transport a session's own metadata
// records: its transport field, an "acp" provider, or recorded MCP state,
// which only ACP sessions carry. recorded is false when the session records
// none of these, so its transport must come from its template.
func recordedSessionTransport(info session.Info) (transport string, recorded bool) {
	if transport := strings.TrimSpace(info.Transport); transport != "" {
		return transport, true
	}
	if strings.TrimSpace(info.Provider) == config.SessionTransportACP {
		return config.SessionTransportACP, true
	}
	if strings.TrimSpace(info.MCPIdentity) != "" ||
		strings.TrimSpace(info.MCPServersSnapshot) != "" {
		return config.SessionTransportACP, true
	}
	return "", false
}

// templateParamsForSession returns tp as info's session launches it. A
// session whose recorded transport is ACP keeps its ACP launch when its
// template now resolves the CLI transport: the session layer routes it to
// the ACP provider by that recorded transport, so it must start with the
// template's ACP launch. swapped reports whether tp was switched to it.
func templateParamsForSession(tp TemplateParams, info session.Info) (TemplateParams, bool) {
	if tp.IsACP || tp.ACPLaunch == nil {
		return tp, false
	}
	if transport, recorded := recordedSessionTransport(info); !recorded || transport != config.SessionTransportACP {
		return tp, false
	}
	tp.IsACP = true
	tp.Command = tp.ACPLaunch.Command
	tp.MCPServers = tp.ACPLaunch.MCPServers
	return tp, true
}
