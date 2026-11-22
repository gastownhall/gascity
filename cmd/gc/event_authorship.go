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
	// gc bd runs in any gc process, an agent's included: its refusal event
	// is recorded where the refusal happens.
	events.RegisterAuthorship(bdSplitRefusalEvent, events.AuthorWorker)
	// So is the vouch-used event: any gc bd call may consult the vouch.
	events.RegisterAuthorship(bdVouchUsedEvent, events.AuthorWorker)
	// The un-inventoried-bd finding is recorded by a starting controller.
	events.RegisterAuthorship(bdUninventoriedEvent, events.AuthorController)
}
