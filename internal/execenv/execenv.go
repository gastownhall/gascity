// Package execenv centralizes environment filtering and log redaction for
// subprocess boundaries.
package execenv

import (
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Redacted is the replacement marker used when removing secrets from text.
const Redacted = "[redacted]"

// UsageMetricsDisableEnv is the process-level Gas City usage-metrics opt-out.
const UsageMetricsDisableEnv = "GC_DISABLE_USAGE_METRICS"

// UsageMetricsDisableValue is the canonical disabled value.
const UsageMetricsDisableValue = "1"

// UsageMetricsDisabledEntry is the canonical child-environment assignment.
const UsageMetricsDisabledEntry = UsageMetricsDisableEnv + "=" + UsageMetricsDisableValue

var sensitiveAssignmentRE = regexp.MustCompile(`(?i)((?:[A-Z0-9_.-]*(?:TOKEN|SECRET|PASSWORD|PRIVATE[_-]?KEY|API[_-]?KEY|ACCESS[_-]?KEY|CREDENTIALS?|OAUTH|AUTH[_-]?JSON)[A-Z0-9_.-]*|--?[A-Z0-9_.-]*(?:token|secret|password|private-key|api-key|access-key|credential|oauth)[A-Z0-9_.-]*)\s*(?:=|:|\s)\s*)([^ \t\r\n,;]+)`)

// WithUsageMetricsDisabled returns a copy of environ with every existing usage
// metrics opt-out entry replaced by one canonical disabled value. All unrelated
// entries retain their original order and multiplicity.
func WithUsageMetricsDisabled(environ []string) []string {
	return withUsageMetricsDisabledForGOOS(environ, runtime.GOOS)
}

func withUsageMetricsDisabledForGOOS(environ []string, goos string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if !usageMetricsDisableKeyEqual(key, goos) {
			out = append(out, entry)
		}
	}
	return append(out, UsageMetricsDisabledEntry)
}

func usageMetricsDisableKeyEqual(key, goos string) bool {
	if goos == "windows" {
		return strings.EqualFold(key, UsageMetricsDisableEnv)
	}
	return key == UsageMetricsDisableEnv
}

// IsSensitiveKey reports whether an environment key is likely to contain a
// secret. Callers should strip inherited values for these keys and require
// explicit config when a child process truly needs one.
func IsSensitiveKey(key string) bool {
	key = strings.ToUpper(strings.TrimSpace(key))
	if key == "" {
		return false
	}
	for _, marker := range []string{
		"PASSWORD",
		"TOKEN",
		"SECRET",
		"PRIVATE_KEY",
		"PRIVATE-KEY",
		"API_KEY",
		"API-KEY",
		"ACCESS_KEY",
		"ACCESS-KEY",
		"CREDENTIAL",
		"OAUTH",
		"AUTH_JSON",
		"AUTH-JSON",
	} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

// FilterInherited removes sensitive KEY=VALUE entries from an inherited
// environment. Explicit overrides should be appended after filtering.
func FilterInherited(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if ok && IsSensitiveKey(key) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// MergeMap filters inherited secrets, removes keys replaced by overrides, and
// appends overrides in deterministic order. Sensitive override values are kept
// because explicit configuration is the "required" path.
func MergeMap(environ []string, overrides map[string]string) []string {
	out := FilterInherited(environ)
	if len(overrides) == 0 {
		return out
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = removeEnvKey(out, key)
	}
	for _, key := range keys {
		out = append(out, key+"="+overrides[key])
	}
	return out
}

// MergeEntries is like MergeMap for already-encoded KEY=VALUE override entries.
func MergeEntries(environ, overrides []string) []string {
	out := FilterInherited(environ)
	if len(overrides) == 0 {
		return out
	}
	for _, entry := range overrides {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			out = removeEnvKey(out, key)
		}
	}
	return append(out, overrides...)
}

// RedactText replaces known secret values and common CLI/env secret assignment
// patterns in text intended for logs or events.
func RedactText(text string, envs ...[]string) string {
	if text == "" {
		return ""
	}
	for _, secret := range sensitiveValues(envs...) {
		text = strings.ReplaceAll(text, secret, Redacted)
	}
	return sensitiveAssignmentRE.ReplaceAllString(text, "${1}"+Redacted)
}

func sensitiveValues(envs ...[]string) []string {
	seen := map[string]struct{}{}
	var values []string
	for _, env := range envs {
		for _, entry := range env {
			key, value, ok := strings.Cut(entry, "=")
			if !ok || !IsSensitiveKey(key) {
				continue
			}
			value = strings.TrimSpace(value)
			if len(value) < 4 {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			values = append(values, value)
		}
	}
	sort.Slice(values, func(i, j int) bool {
		return len(values[i]) > len(values[j])
	})
	return values
}

func removeEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return out
}
