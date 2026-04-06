// Package k8s implements a native Kubernetes session provider using client-go.
//
// It provides the same semantics as the exec-based gc-session-k8s script
// but eliminates subprocess overhead by making direct API calls over reused
// HTTP/2 connections. Pod manifests are compatible with gc-session-k8s
// (same labels, annotations, container names, tmux-inside-pod pattern)
// so mixed-mode migration works.
package k8s

import (
	"strings"
	"time"
	"unicode"
)

// tmuxSession is the tmux session name inside each pod (one session per pod).
const tmuxSession = "main"

// startupGracePeriod is the maximum time allowed for a pod to complete
// workspace initialization and start its tmux session. Running pods younger
// than this with dead tmux are treated as still initializing, not stale.
// Covers the full startup chain: waitForInitContainer (60s) +
// waitForPodRunning (120s) + waitForTmux (60s).
const startupGracePeriod = 240 * time.Second

// SanitizeName converts a session name to a valid K8s resource name.
// K8s names: lowercase, alphanumeric + '-', max 63 chars, must start/end
// with alphanumeric. Compatible with gc-session-k8s sanitize_name.
func SanitizeName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := b.String()

	// Trim leading dashes.
	s = strings.TrimLeft(s, "-")

	// Truncate to 63 chars.
	if len(s) > 63 {
		s = s[:63]
	}

	// Trim trailing dashes.
	s = strings.TrimRight(s, "-")

	// Return "unknown" for non-empty input that sanitized to nothing
	// (e.g., all-special-char input). Empty input returns empty.
	if s == "" && name != "" {
		return "unknown"
	}
	return s
}

// SanitizeLabel converts a value to a valid K8s label value.
// Label values: alphanumeric + '-', '_', '.', max 63 chars, must start/end
// with alphanumeric. Empty returned as "unknown". Compatible with
// gc-session-k8s sanitize_label.
func SanitizeLabel(value string) string {
	var b strings.Builder
	for _, r := range value {
		if isLabelChar(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := b.String()

	// Trim leading non-alphanumeric.
	s = strings.TrimLeftFunc(s, func(r rune) bool {
		return !isAlphanumeric(r)
	})

	// Truncate to 63 chars.
	if len(s) > 63 {
		s = s[:63]
	}

	// Trim trailing non-alphanumeric.
	s = strings.TrimRightFunc(s, func(r rune) bool {
		return !isAlphanumeric(r)
	})

	if s == "" {
		return "unknown"
	}
	return s
}

func isAlphanumeric(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isLabelChar(r rune) bool {
	return isAlphanumeric(r) || r == '-' || r == '_' || r == '.'
}
