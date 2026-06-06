package session

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	// MCPIdentityMetadataKey stores the stable identity used to materialize
	// MCP templates for a session.
	MCPIdentityMetadataKey = "mcp_identity"
	// MCPServersSnapshotMetadataKey stores the normalized ACP session/new MCP
	// server snapshot used to resume sessions when the current catalog cannot
	// be materialized.
	MCPServersSnapshotMetadataKey = "mcp_servers_snapshot"

	redactedMCPSnapshotValue = "__redacted__"
)

// EncodeMCPServersSnapshot returns the normalized metadata value for a
// session's persisted ACP session/new MCP server snapshot.
func EncodeMCPServersSnapshot(servers []runtime.MCPServerConfig) (string, error) {
	normalized := normalizeMCPServersSnapshotForMetadata(servers)
	if len(normalized) == 0 {
		return "", nil
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("marshal MCP server snapshot: %w", err)
	}
	return string(data), nil
}

// DecodeMCPServersSnapshot parses a persisted ACP session/new MCP server
// snapshot from session metadata.
func DecodeMCPServersSnapshot(raw string) ([]runtime.MCPServerConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var servers []runtime.MCPServerConfig
	if err := json.Unmarshal([]byte(raw), &servers); err != nil {
		return nil, fmt.Errorf("unmarshal MCP server snapshot: %w", err)
	}
	return runtime.NormalizeMCPServerConfigs(servers), nil
}

// StoredMCPSnapshotContainsRedactions reports whether a decoded persisted MCP
// snapshot contains redacted secret placeholders.
func StoredMCPSnapshotContainsRedactions(servers []runtime.MCPServerConfig) bool {
	for _, server := range servers {
		if snapshotMapContainsRedactions(server.Env) ||
			snapshotMapContainsRedactions(server.Headers) ||
			snapshotArgsContainRedactions(server.Args) ||
			strings.Contains(server.URL, redactedMCPSnapshotValue) {
			return true
		}
	}
	return false
}

// SanitizeStoredMCPSnapshotForResume strips redacted secret placeholders from
// a stored MCP snapshot while preserving any non-secret fields that can still
// help degraded resume reconstruct MCP hints.
func SanitizeStoredMCPSnapshotForResume(servers []runtime.MCPServerConfig) []runtime.MCPServerConfig {
	if len(servers) == 0 {
		return nil
	}
	normalized := runtime.NormalizeMCPServerConfigs(servers)
	for i := range normalized {
		normalized[i].Args = sanitizeStoredMCPMetadataArgs(normalized[i].Args)
		normalized[i].Env = sanitizeStoredMCPMetadataMap(normalized[i].Env)
		normalized[i].URL = sanitizeStoredMCPMetadataURL(normalized[i].URL)
		normalized[i].Headers = sanitizeStoredMCPMetadataMap(normalized[i].Headers)
	}
	return runtime.NormalizeMCPServerConfigs(normalized)
}

// WithStoredMCPMetadata returns a metadata map augmented with the stable MCP
// identity and normalized ACP session/new snapshot for the session.
func WithStoredMCPMetadata(meta map[string]string, identity string, servers []runtime.MCPServerConfig) (map[string]string, error) {
	if meta == nil {
		meta = make(map[string]string)
	}
	identity = strings.TrimSpace(identity)
	if identity != "" {
		meta[MCPIdentityMetadataKey] = identity
	}
	snapshot, err := EncodeMCPServersSnapshot(servers)
	if err != nil {
		return nil, err
	}
	if snapshot != "" {
		meta[MCPServersSnapshotMetadataKey] = snapshot
	} else if _, ok := meta[MCPServersSnapshotMetadataKey]; ok {
		meta[MCPServersSnapshotMetadataKey] = ""
	}
	return meta, nil
}

func normalizeMCPServersSnapshotForMetadata(servers []runtime.MCPServerConfig) []runtime.MCPServerConfig {
	normalized := runtime.NormalizeMCPServerConfigs(servers)
	for i := range normalized {
		normalized[i].Args = redactMCPMetadataArgs(normalized[i].Args)
		normalized[i].Env = redactMCPMetadataMap(normalized[i].Env)
		normalized[i].URL = redactMCPMetadataURL(normalized[i].URL)
		normalized[i].Headers = redactMCPMetadataMap(normalized[i].Headers)
	}
	return normalized
}

func redactMCPMetadataArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, 0, len(args))
	redactNext := false
	for _, arg := range args {
		if redactNext {
			out = append(out, redactedMCPSnapshotValue)
			redactNext = false
			continue
		}
		if isSensitiveMCPMetadataValue(arg) {
			out = append(out, redactedMCPSnapshotValue)
			continue
		}
		if redactedURL := redactMCPMetadataURL(arg); redactedURL != arg {
			out = append(out, redactedURL)
			continue
		}
		if key, value, ok := strings.Cut(arg, "="); ok && isSensitiveMCPMetadataToken(key) {
			if strings.TrimSpace(value) == "" {
				out = append(out, key+"=")
			} else {
				out = append(out, key+"="+redactedMCPSnapshotValue)
			}
			continue
		}
		if isSensitiveMCPMetadataToken(arg) && strings.HasPrefix(strings.TrimSpace(arg), "-") {
			out = append(out, arg)
			redactNext = true
			continue
		}
		out = append(out, arg)
	}
	return out
}

func redactMCPMetadataMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key := range in {
		out[key] = redactedMCPSnapshotValue
	}
	return out
}

func redactMCPMetadataURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	changed := false
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.UserPassword(redactedMCPSnapshotValue, redactedMCPSnapshotValue)
		} else {
			parsed.User = url.User(redactedMCPSnapshotValue)
		}
		changed = true
	}
	if query := parsed.Query(); len(query) > 0 {
		for key := range query {
			query.Set(key, redactedMCPSnapshotValue)
		}
		parsed.RawQuery = query.Encode()
		changed = true
	}
	if !changed {
		return raw
	}
	return parsed.String()
}

func snapshotMapContainsRedactions(in map[string]string) bool {
	for _, value := range in {
		if value == redactedMCPSnapshotValue {
			return true
		}
	}
	return false
}

func snapshotArgsContainRedactions(args []string) bool {
	for _, arg := range args {
		if strings.Contains(arg, redactedMCPSnapshotValue) {
			return true
		}
	}
	return false
}

func sanitizeStoredMCPMetadataArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		trimmed := strings.TrimSpace(arg)
		if strings.HasPrefix(trimmed, "-") &&
			isSensitiveMCPMetadataToken(trimmed) &&
			i+1 < len(args) &&
			strings.Contains(args[i+1], redactedMCPSnapshotValue) {
			i++
			continue
		}
		if !strings.Contains(arg, redactedMCPSnapshotValue) {
			out = append(out, arg)
			continue
		}
		if key, value, ok := strings.Cut(arg, "="); ok &&
			isSensitiveMCPMetadataToken(key) &&
			strings.Contains(value, redactedMCPSnapshotValue) {
			continue
		}
		if sanitizedURL := sanitizeStoredMCPMetadataURL(arg); sanitizedURL != "" && sanitizedURL != arg {
			out = append(out, sanitizedURL)
		}
	}
	return out
}

func sanitizeStoredMCPMetadataMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string)
	for key, value := range in {
		if strings.Contains(value, redactedMCPSnapshotValue) {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sanitizeStoredMCPMetadataURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, redactedMCPSnapshotValue) {
		return raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if parsed.User != nil && strings.Contains(parsed.User.String(), redactedMCPSnapshotValue) {
		parsed.User = nil
	}
	if query := parsed.Query(); len(query) > 0 {
		for key, values := range query {
			filtered := values[:0]
			for _, value := range values {
				if !strings.Contains(value, redactedMCPSnapshotValue) {
					filtered = append(filtered, value)
				}
			}
			if len(filtered) == 0 {
				query.Del(key)
				continue
			}
			query[key] = filtered
		}
		parsed.RawQuery = query.Encode()
	}
	if strings.Contains(parsed.String(), redactedMCPSnapshotValue) {
		return ""
	}
	return parsed.String()
}

func isSensitiveMCPMetadataToken(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(value, "token") ||
		strings.Contains(value, "secret") ||
		strings.Contains(value, "password") ||
		strings.Contains(value, "passwd") ||
		strings.Contains(value, "authorization") ||
		strings.Contains(value, "auth") ||
		strings.Contains(value, "bearer") ||
		strings.Contains(value, "cookie") ||
		strings.Contains(value, "api-key") ||
		strings.Contains(value, "apikey")
}

func isSensitiveMCPMetadataValue(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "authorization:") ||
		strings.HasPrefix(value, "bearer ") ||
		strings.HasPrefix(value, "basic ") ||
		strings.HasPrefix(value, "token ")
}
