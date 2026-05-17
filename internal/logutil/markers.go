package logutil

import "strings"

// FatalMarker prefixes fatal lines emitted for gc start output proxies.
const FatalMarker = "gc-fatal:"

const (
	ansiBoldRed            = "\x1b[1;31m"
	ansiReset              = "\x1b[0m"
	docsBaseURL            = "https://docs.gascityhall.com/"
	legacyMigrationDocPath = "docs/packv2/migration.mdx"
)

// FormatFatalLine formats a plain fatal marker line for non-TTY output.
func FormatFatalLine(message string) string {
	return FatalMarker + " " + FormatFatalMessage(message)
}

// FormatFatalMessage appends a troubleshooting URL for known fatal causes.
func FormatFatalMessage(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		return message
	}
	if strings.Contains(message, docsBaseURL) {
		return message
	}
	if url := FatalSeeURL(message); url != "" {
		message = strings.TrimSpace(strings.ReplaceAll(message, legacyMigrationDocPath, ""))
		message = strings.TrimSpace(strings.TrimSuffix(message, "see:"))
		return message + " see: " + url
	}
	return message
}

// ParseFatalLine strips FatalMarker from a line.
func ParseFatalLine(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, FatalMarker) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, FatalMarker)), true
}

// RenderFatalLine renders a fatal line for TTY or plain output.
func RenderFatalLine(message string, tty bool) string {
	message = FormatFatalMessage(message)
	if tty {
		return ansiBoldRed + "FATAL: " + message + ansiReset
	}
	return FatalMarker + " " + message
}

// FatalCause returns the stable short cause key for a fatal message.
func FatalCause(message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "signal: killed") && (strings.Contains(lower, "bd init") || strings.Contains(lower, "beads init")):
		return "op-init-timeout"
	case strings.Contains(lower, "op_init") && (strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out")):
		return "op-init-timeout"
	case strings.Contains(lower, "pack v1/v2 layout collision"):
		return "pack-v1-v2-collision"
	case strings.Contains(lower, "pack schema") || strings.Contains(lower, "schema mismatch") || strings.Contains(lower, "schema ") && strings.Contains(lower, "not supported"):
		return "pack-schema-mismatch"
	case strings.Contains(lower, "duplicate identity"):
		return "duplicate-identity"
	case strings.Contains(lower, "duplicate name") && (strings.Contains(lower, "v1/v2") || strings.Contains(lower, "pack v1") || strings.Contains(lower, "pack v2")):
		return "pack-v1-v2-collision"
	case strings.Contains(lower, "template not found") || strings.Contains(lower, "referenced template not found"):
		return "template-not-found"
	case strings.Contains(lower, "unknown field"):
		return "unknown-field-agent-pool"
	case strings.Contains(lower, "path is required") && strings.Contains(lower, "rig"):
		return "rig-path-required"
	case strings.Contains(lower, "duplicate name"):
		return "duplicate-name"
	case strings.TrimSpace(lower) != "":
		return "startup-failed"
	default:
		return ""
	}
}

// FatalSeeURL returns the troubleshooting URL for a fatal message.
func FatalSeeURL(message string) string {
	switch FatalCause(message) {
	case "op-init-timeout":
		return WalkthroughURL["bd_op_init_timeout"]
	case "pack-schema-mismatch":
		return WalkthroughURL["pack_schema_mismatch"]
	case "pack-v1-v2-collision":
		return WalkthroughURL["duplicate_name_v1v2"]
	case "duplicate-name":
		return WalkthroughURL["duplicate_name_other"]
	case "unknown-field-agent-pool":
		return WalkthroughURL["unknown_field"]
	case "rig-path-required":
		return WalkthroughURL["rig_path_required"]
	case "template-not-found":
		return WalkthroughURL["template_not_found"]
	case "duplicate-identity":
		return WalkthroughURL["duplicate_identity"]
	default:
		return ""
	}
}
