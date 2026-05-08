package runtime

import "sort"

// MCPTransport identifies the ACP session/new transport type for an MCP server.
type MCPTransport string

const (
	// MCPTransportStdio launches the MCP server over stdio.
	MCPTransportStdio MCPTransport = "stdio"
	// MCPTransportHTTP connects to the MCP server over streamable HTTP.
	MCPTransportHTTP MCPTransport = "http"
	// MCPTransportSSE connects to the MCP server over SSE.
	MCPTransportSSE MCPTransport = "sse"
)

// MCPKeyValue is a name/value pair used for MCP env vars and HTTP headers.
type MCPKeyValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPServerConfig is the runtime-owned ACP session/new representation of one
// MCP server. Providers that do not speak ACP ignore this field.
type MCPServerConfig struct {
	Name      string
	Transport MCPTransport
	Command   string
	Args      []string
	Env       map[string]string
	URL       string
	Headers   map[string]string
}

// NormalizeMCPServerConfigs clones and deterministically sorts MCP server
// definitions so runtime configs are safe to retain and compare.
func NormalizeMCPServerConfigs(in []MCPServerConfig) []MCPServerConfig {
	if len(in) == 0 {
		return nil
	}
	out := make([]MCPServerConfig, len(in))
	for i, server := range in {
		out[i] = MCPServerConfig{
			Name:      server.Name,
			Transport: server.Transport,
			Command:   server.Command,
			Args:      append([]string(nil), server.Args...),
			Env:       cloneRuntimeStringMap(server.Env),
			URL:       server.URL,
			Headers:   cloneRuntimeStringMap(server.Headers),
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Transport != out[j].Transport {
			return out[i].Transport < out[j].Transport
		}
		if out[i].Command != out[j].Command {
			return out[i].Command < out[j].Command
		}
		return out[i].URL < out[j].URL
	})
	return out
}

func cloneRuntimeStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
