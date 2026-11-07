package supervisor

import (
	"crypto/sha1"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"
)

var serviceNameUnsafeRE = regexp.MustCompile(`[^a-z0-9]+`)

// SanitizeServiceName lowercases name and collapses every run of characters
// outside [a-z0-9] to a single '-', trimming leading and trailing '-'.
func SanitizeServiceName(name string) string {
	name = serviceNameUnsafeRE.ReplaceAllString(strings.ToLower(name), "-")
	return strings.Trim(name, "-")
}

// ServiceSuffix returns the platform service-name suffix for a supervisor
// whose isolated GC_HOME is gcHome, which callers must already have
// normalized (pathutil.NormalizePathForCompare): the sanitized base name and
// the first 8 hex digits of the path's SHA-1, or "isolated-<hash>" when the
// base sanitizes to nothing. Returns "" for an empty gcHome.
func ServiceSuffix(gcHome string) string {
	if gcHome == "" {
		return ""
	}
	sum := sha1.Sum([]byte(gcHome))
	hash := hex.EncodeToString(sum[:])[:8]
	if base := SanitizeServiceName(filepath.Base(gcHome)); base != "" {
		return base + "-" + hash
	}
	return "isolated-" + hash
}
