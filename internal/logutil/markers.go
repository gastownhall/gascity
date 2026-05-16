package logutil

import "strings"

// FatalMarker prefixes fatal lines emitted for gc start output proxies.
const FatalMarker = "gc-fatal:"

// Troubleshooting URLs attached to known gc start failure causes.
const (
	URLBDOpInitTimeout     = "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#bd-op-init-timeout"
	URLPackSchemaMismatch  = "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#pack-schema-mismatch"
	URLPackV1V2Migration   = "https://docs.gascityhall.com/packv2/migration"
	URLDuplicateName       = "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#duplicate-name"
	URLUnknownField        = "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#unknown-field-agent-pool"
	URLRigPathRequired     = "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#rig-path-required"
	URLTemplateNotFound    = "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#template-not-found"
	URLDuplicateIdentity   = "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#duplicate-identity"
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
		return URLBDOpInitTimeout
	case "pack-schema-mismatch":
		return URLPackSchemaMismatch
	case "pack-v1-v2-collision":
		return URLPackV1V2Migration
	case "duplicate-name":
		return URLDuplicateName
	case "unknown-field-agent-pool":
		return URLUnknownField
	case "rig-path-required":
		return URLRigPathRequired
	case "template-not-found":
		return URLTemplateNotFound
	case "duplicate-identity":
		return URLDuplicateIdentity
	default:
		return ""
	}
}
