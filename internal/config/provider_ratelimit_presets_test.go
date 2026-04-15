package config

import (
	"strings"
	"testing"
)

// TestKnownProviderPatterns_HasPatterns verifies that the major builtin
// providers (claude, codex, gemini) ship with non-empty RateLimitPatterns,
// so the quota rotation system can detect rate-limiting out of the box.
func TestKnownProviderPatterns_HasPatterns(t *testing.T) {
	builtins := BuiltinProviders()
	for _, name := range []string{"claude", "codex", "gemini"} {
		spec, ok := builtins[name]
		if !ok {
			t.Fatalf("builtin provider %q missing from BuiltinProviders()", name)
		}
		if len(spec.RateLimitPatterns) == 0 {
			t.Errorf("builtin provider %q has no RateLimitPatterns; want at least 1", name)
		}
	}
}

// TestKnownProviderPatterns_Match verifies that each documented default
// rate-limit pattern for known providers matches at least one realistic
// error message from that provider. This ensures the patterns are useful
// for real-world rate-limit detection.
func TestKnownProviderPatterns_Match(t *testing.T) {
	// Realistic error messages that a provider might emit when rate-limited.
	// Each provider has at least one sample message per documented pattern.
	sampleMessages := map[string][]string{
		"claude": {
			"Error: Your account has reached its rate limit. Please wait before trying again.",
			"Error: rate limit exceeded for model claude-sonnet-4-6",
			"Too many requests. Please slow down.",
			"Error: Your usage limit has been reached for today.",
			"API rate limit hit — retrying in 30 seconds",
		},
		"codex": {
			"Error: rate limit exceeded, please retry after 60s",
			"429 Too Many Requests",
			"Rate limit reached for default-model on tokens per min",
		},
		"gemini": {
			"RESOURCE_EXHAUSTED: Quota exceeded for quota metric",
			"Error 429: quota exceeded for aiplatform.googleapis.com",
			"Rate limit exceeded. Please retry after a few seconds.",
		},
	}

	builtins := BuiltinProviders()
	for _, providerName := range []string{"claude", "codex", "gemini"} {
		spec, ok := builtins[providerName]
		if !ok {
			t.Fatalf("builtin provider %q not found", providerName)
		}
		messages := sampleMessages[providerName]
		for _, pattern := range spec.RateLimitPatterns {
			matched := false
			for _, msg := range messages {
				if strings.Contains(strings.ToLower(msg), strings.ToLower(pattern)) {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("provider %q pattern %q did not match any sample message", providerName, pattern)
			}
		}
	}
}
