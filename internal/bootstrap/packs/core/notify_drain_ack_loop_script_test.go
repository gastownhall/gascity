package core

import "testing"

// TestNotifyOnDrainAckLoopOrder pins the order's event contract: it wakes
// on the drain-ack-with-assigned-work anomaly and runs the one script.
func TestNotifyOnDrainAckLoopOrder(t *testing.T) {
	assertEventExecOrder(t, "notify-on-drain-ack-loop.toml", "session.drain_acked_with_assigned_work", "notify-drain-ack-loop.sh")
}
