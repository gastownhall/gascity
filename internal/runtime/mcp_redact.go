package runtime

import (
	"net/url"
	"strings"
)

// RedactedMCPValue replaces an MCP server credential wherever an MCP server
// definition is persisted (session metadata snapshots, ACP capture
// transcripts).
const RedactedMCPValue = "__redacted__"

// RedactMCPServerConfigs returns a normalized copy of servers that is safe to
// persist: every env and header value is replaced with [RedactedMCPValue]
// (templates can render any value from the agent environment into them), URL
// userinfo and query values are replaced, and secret-looking args (a
// credential-bearing value, key=value with a sensitive key, or the value after
// a sensitive flag) are replaced. The input is not modified.
func RedactMCPServerConfigs(servers []MCPServerConfig) []MCPServerConfig {
	normalized := NormalizeMCPServerConfigs(servers)
	for i := range normalized {
		normalized[i].Args = redactMCPArgs(normalized[i].Args)
		normalized[i].Env = redactMCPMap(normalized[i].Env)
		normalized[i].URL = redactMCPURL(normalized[i].URL)
		normalized[i].Headers = redactMCPMap(normalized[i].Headers)
	}
	return normalized
}

// IsSensitiveMCPName reports whether an env key, header name, flag, or
// key=value key looks like it names a credential.
func IsSensitiveMCPName(value string) bool {
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

// isSensitiveMCPValue reports whether a value itself carries a credential
// scheme (an Authorization header line or a bearer/basic/token value).
func isSensitiveMCPValue(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "authorization:") ||
		strings.HasPrefix(value, "bearer ") ||
		strings.HasPrefix(value, "basic ") ||
		strings.HasPrefix(value, "token ")
}

func redactMCPArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, 0, len(args))
	redactNext := false
	for _, arg := range args {
		if redactNext {
			out = append(out, RedactedMCPValue)
			redactNext = false
			continue
		}
		if isSensitiveMCPValue(arg) {
			out = append(out, RedactedMCPValue)
			continue
		}
		if redactedURL := redactMCPURL(arg); redactedURL != arg {
			out = append(out, redactedURL)
			continue
		}
		if key, value, ok := strings.Cut(arg, "="); ok && IsSensitiveMCPName(key) {
			if strings.TrimSpace(value) == "" {
				out = append(out, key+"=")
			} else {
				out = append(out, key+"="+RedactedMCPValue)
			}
			continue
		}
		if IsSensitiveMCPName(arg) && strings.HasPrefix(strings.TrimSpace(arg), "-") {
			out = append(out, arg)
			redactNext = true
			continue
		}
		out = append(out, arg)
	}
	return out
}

func redactMCPMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key := range in {
		out[key] = RedactedMCPValue
	}
	return out
}

func redactMCPURL(raw string) string {
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
			parsed.User = url.UserPassword(RedactedMCPValue, RedactedMCPValue)
		} else {
			parsed.User = url.User(RedactedMCPValue)
		}
		changed = true
	}
	if query := parsed.Query(); len(query) > 0 {
		for key := range query {
			query.Set(key, RedactedMCPValue)
		}
		parsed.RawQuery = query.Encode()
		changed = true
	}
	if !changed {
		return raw
	}
	return parsed.String()
}
