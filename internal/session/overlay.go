package session

import (
	"fmt"
	"regexp"
	"strings"
)

// maxPromptOverlayBytes is the maximum allowed size for a prompt override.
const maxPromptOverlayBytes = 16 * 1024 // 16KB

// envKeyPattern validates environment variable names: must start with an
// uppercase letter, contain only uppercase letters, digits, and underscores,
// and be at most 128 characters.
var envKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)

// bannedOverlayKeys are template fields that must never be overridden by
// session overlay. These are identity or lifecycle fields whose mutation
// would violate core invariants.
var bannedOverlayKeys = map[string]bool{
	"command":                    true,
	"provider":                   true,
	"session_key":                true,
	"state":                      true,
	"generation":                 true,
	"continuation_epoch":         true,
	"continuation_reset_pending": true,
	"instance_token":             true,
	"wait_hold":                  true,
	"sleep_intent":               true,
	"resume_flag":                true,
	"resume_style":               true,
	WaitIdleNudgeMetadataKey:     true,
}

// ValidateOverlay checks that all keys in overrides are permitted by the
// allowOverlay and allowEnvOverride lists. Returns an error describing the
// first violation found.
//
// Rules:
//  1. Keys in bannedOverlayKeys are always rejected.
//  2. Keys starting with "env." are validated against allowEnvOverride and
//     the envKeyPattern regex.
//  3. "prompt" requires explicit opt-in via allowOverlay.
//  4. All other keys must appear in allowOverlay.
//  5. Prompt values are capped at maxPromptOverlayBytes.
func ValidateOverlay(overrides map[string]string, allowOverlay, allowEnvOverride []string) error {
	if len(overrides) == 0 {
		return nil
	}

	// Build lookup sets.
	allowSet := make(map[string]bool, len(allowOverlay))
	for _, k := range allowOverlay {
		allowSet[k] = true
	}
	envAllowSet := make(map[string]bool, len(allowEnvOverride))
	for _, k := range allowEnvOverride {
		envAllowSet[k] = true
	}

	for key, val := range overrides {
		// Rule 1: banned keys.
		if bannedOverlayKeys[key] {
			return fmt.Errorf("overlay key %q is banned and cannot be overridden", key)
		}

		// Rule 2: env.* keys.
		if strings.HasPrefix(key, "env.") {
			envName := key[4:]
			if !envKeyPattern.MatchString(envName) {
				return fmt.Errorf("overlay env key %q does not match required pattern [A-Z][A-Z0-9_]{0,127}", envName)
			}
			if !envAllowSet[envName] {
				return fmt.Errorf("overlay env key %q is not in allow_env_override list", envName)
			}
			continue
		}

		// Rule 3: prompt requires opt-in.
		if key == "prompt" {
			if !allowSet["prompt"] {
				return fmt.Errorf("overlay key \"prompt\" requires explicit opt-in via allow_overlay")
			}
			if len(val) > maxPromptOverlayBytes {
				return fmt.Errorf("overlay prompt exceeds %d byte limit (got %d)", maxPromptOverlayBytes, len(val))
			}
			continue
		}

		// Rule 4: all other keys must be in allowOverlay.
		if !allowSet[key] {
			return fmt.Errorf("overlay key %q is not in allow_overlay list", key)
		}
	}

	return nil
}
