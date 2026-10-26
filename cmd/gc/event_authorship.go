package main

import "github.com/gastownhall/gascity/internal/events"

// Event types cmd/gc declares itself have their authorship registered here,
// beside the SDK's own table (internal/events/authorship.go), so a worker
// site cannot record them unless they are decided Worker
// (engdocs/design/des-box-daemon-v0.md D13).
// TestEverySDKEventTypeHasAnAuthorship fails until a new one is listed.
func init() {
	// The start-capacity valve runs in the reconciler only.
	events.RegisterAuthorship(breakerStateChangedEventType, events.AuthorController)
}
